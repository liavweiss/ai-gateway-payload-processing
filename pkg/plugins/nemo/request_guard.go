/*
Copyright 2026 The opendatahub.io Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nemo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
)

const (
	// NemoRequestGuardPluginType is the plugin type identifier.
	NemoRequestGuardPluginType = "nemo-request-guard"
	// nemoAllowedResponse is the expected response content from NeMo when a request passes all rails.
	nemoAllowedResponse = "allowed"
	// defaultTimeoutSec allows for CPU-based LLM inference (2-5 min per request).
	defaultTimeoutSec = 360
	// maxNemoResponseBytes caps the NeMo response body to prevent memory exhaustion
	// from a misbehaving or compromised NeMo service (CWE-400).
	maxNemoResponseBytes = 1 << 20 // 1 MiB
)

// compile-time type validation
var _ framework.RequestProcessor = (*NemoRequestGuardPlugin)(nil)

// NemoRequestGuardPlugin calls a NeMo Guardrails service over HTTP to check request content
// using input rails. It implements RequestProcessor to intercept requests before forwarding.
type NemoRequestGuardPlugin struct {
	typedName         plugin.TypedName
	nemoServerBaseURL string
	nemoEndpointURL   string
	httpClient        *http.Client
}

// nemoRequestGuardConfig is the JSON configuration for the plugin.
type nemoRequestGuardConfig struct {
	BaseURL        string `json:"baseURL"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// NemoRequestGuardFactory is the factory function for NemoRequestGuardPlugin.
func NemoRequestGuardFactory(name string, rawParameters json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	var cfg nemoRequestGuardConfig

	if len(rawParameters) > 0 {
		if err := json.Unmarshal(rawParameters, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' plugin - %w", NemoRequestGuardPluginType, err)
		}
	}

	p, err := NewNemoRequestGuardPlugin(cfg.BaseURL, cfg.TimeoutSeconds)
	if err != nil {
		return nil, fmt.Errorf("failed to create '%s' plugin - %w", NemoRequestGuardPluginType, err)
	}

	return p.WithName(name), nil
}

// NewNemoRequestGuardPlugin builds a NeMo request guard plugin from validated parameters.
// The NeMo server is expected to have a default configuration (--default-config-id).
func NewNemoRequestGuardPlugin(nemoServerBaseURL string, timeoutSeconds int) (*NemoRequestGuardPlugin, error) {
	if nemoServerBaseURL == "" {
		return nil, fmt.Errorf("baseURL is required in %s plugin", NemoRequestGuardPluginType)
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSec * time.Second
	}

	endpointURL := strings.TrimRight(nemoServerBaseURL, "/") + "/v1/chat/completions"

	return &NemoRequestGuardPlugin{
		typedName:         plugin.TypedName{Type: NemoRequestGuardPluginType, Name: NemoRequestGuardPluginType},
		nemoServerBaseURL: nemoServerBaseURL,
		nemoEndpointURL:   endpointURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *NemoRequestGuardPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

// WithName sets the name of the plugin instance.
func (p *NemoRequestGuardPlugin) WithName(name string) *NemoRequestGuardPlugin {
	p.typedName.Name = name
	return p
}

// ProcessRequest calls NeMo Guardrails to evaluate input rails on the incoming request.
// It extracts the last user message from the OpenAI-style body, POSTs to NeMo
// /v1/chat/completions, and returns an errcommon.Error with Forbidden (403) if NeMo flags
// the content, which the framework converts into an Envoy ImmediateResponse.
//
// NeMo always returns HTTP 200 for both allowed and blocked requests. The block/allow
// decision is conveyed through the response body: choices[0].message.content equal to
// "allowed" means the request passed all rails; any other content is a block reason.
func (p *NemoRequestGuardPlugin) ProcessRequest(ctx context.Context, _ *framework.CycleState, request *framework.InferenceRequest) error {
	logger := log.FromContext(ctx)

	if request == nil || request.Body == nil {
		return errcommon.Error{Code: errcommon.Internal, Msg: "nemo-request-guard: invalid inference request: request/body must be non-nil"}
	}

	messages, err := extractMessages(request.Body)
	if err != nil {
		return errcommon.Error{Code: errcommon.BadRequest, Msg: fmt.Sprintf("nemo-request-guard: malformed request body: %v", err)}
	}
	if len(messages) == 0 {
		return nil // no messages to check (e.g. non-chat request) → allow
	}

	// "model" is required by the NeMo OpenAI-compatible API schema but is not used
	// for guard-only configurations — the guard model is defined in NeMo's config.yml.
	reqBody := map[string]any{
		"model":    "",
		"messages": messages,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("nemo-request-guard: marshal request: %v", err)}
	}

	logger.V(logutil.VERBOSE).Info("calling NeMo guardrails", "url", p.nemoEndpointURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.nemoEndpointURL, bytes.NewReader(payload))
	if err != nil {
		return errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("nemo-request-guard: create request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("nemo-request-guard: call failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("nemo-request-guard: unexpected status %d", resp.StatusCode)}
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxNemoResponseBytes)).Decode(&out); err != nil {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("nemo-request-guard: decode response: %v", err)}
	}

	content := ""
	if len(out.Choices) > 0 {
		content = strings.TrimSpace(out.Choices[0].Message.Content)
	}

	if strings.EqualFold(content, nemoAllowedResponse) {
		logger.V(logutil.VERBOSE).Info("request allowed by NeMo guardrails")
		return nil
	}

	logger.Info("request blocked by NeMo guardrails", "reason", content)
	return errcommon.Error{Code: errcommon.Forbidden, Msg: fmt.Sprintf("nemo-request-guard: request blocked: %s", content)}
}

// extractMessages pulls OpenAI-style "messages" from the body and returns the last user message
// as a single-element slice for the input-rail check. Falls back to all messages if no user
// message is found.
func extractMessages(body map[string]any) ([]map[string]string, error) {
	raw, ok := body["messages"]
	if !ok {
		return nil, nil
	}
	slice, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("messages is not an array")
	}
	var messages []map[string]string
	for _, m := range slice {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		messages = append(messages, map[string]string{"role": role, "content": content})
	}
	var lastUser []map[string]string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" {
			lastUser = messages[i : i+1]
			break
		}
	}
	if len(lastUser) > 0 {
		return lastUser, nil
	}
	if len(messages) > 0 {
		return messages, nil
	}
	return nil, nil
}
