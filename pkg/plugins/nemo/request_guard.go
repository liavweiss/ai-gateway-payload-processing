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
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
)

const (
	// NemoRequestGuardPluginType is the plugin type identifier.
	NemoRequestGuardPluginType = "nemo-request-guard"
)

// compile-time type validation
var _ framework.RequestProcessor = &NemoRequestGuardPlugin{}

// NemoRequestGuardPlugin calls a NeMo Guardrails service over HTTP to check request content
// using input rails. It implements RequestProcessor to intercept requests before forwarding.
type NemoRequestGuardPlugin struct {
	nemoGuardBase
}

// NemoRequestGuardFactory is the factory function for NemoRequestGuardPlugin.
func NemoRequestGuardFactory(name string, rawParameters json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	config := nemoGuardConfig{
		TimeoutSeconds: defaultTimeoutSec,
	}

	if len(rawParameters) > 0 {
		if err := json.Unmarshal(rawParameters, &config); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' plugin - %w", NemoRequestGuardPluginType, err)
		}
	}

	p, err := NewNemoRequestGuardPlugin(config.NemoURL, config.TimeoutSeconds)
	if err != nil {
		return nil, fmt.Errorf("failed to create '%s' plugin - %w", NemoRequestGuardPluginType, err)
	}

	return p.WithName(name), nil
}

// NewNemoRequestGuardPlugin builds a NeMo request guard plugin from validated parameters.
// The NeMo server is expected to have a default configuration (--default-config-id).
func NewNemoRequestGuardPlugin(nemoURL string, timeoutSeconds int) (*NemoRequestGuardPlugin, error) {
	base, err := newNemoGuardBase(NemoRequestGuardPluginType, nemoURL, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	return &NemoRequestGuardPlugin{nemoGuardBase: *base}, nil
}

// ProcessRequest calls NeMo Guardrails to evaluate input rails on the incoming request.
// It extracts the last user message from the OpenAI-style body, POSTs to NeMo url,
// and returns an errcommon.Error with Forbidden (403) if NeMo flags the content.
//
// NeMo always returns HTTP 200 for both allowed and blocked requests. The block/allow
// decision is conveyed through the response body "status" field.
// "success" means the request passed all rails; "blocked" means the request is blocked.
func (p *NemoRequestGuardPlugin) ProcessRequest(ctx context.Context, _ *framework.CycleState, request *framework.InferenceRequest) error {
	model, ok := request.Body["model"].(string)
	if !ok || model == "" {
		return nil // not an inference request (e.g. API key management, model listing)
	}

	messages, err := extractMessages(request.Body)
	if err != nil {
		return errcommon.Error{Code: errcommon.BadRequest, Msg: fmt.Sprintf("malformed request body: %v", err)}
	}
	if len(messages) == 0 {
		return nil // no messages to check (e.g. non-chat request) → allow
	}

	// "model" field is required by the NeMo OpenAI-compatible API schema but is not used.
	// the guard model is defined in NeMo's config.yml.
	reqBody := map[string]any{
		"model":    model,
		"messages": messages,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("marshal request: %v", err)}
	}

	code, callErr := p.callNemoGuard(ctx, payload)
	if callErr != nil {
		if code == errcommon.Forbidden {
			return errcommon.Error{Code: code, Msg: "request blocked by NeMo guardrails"}
		}
		return errcommon.Error{Code: code, Msg: callErr.Error()}
	}
	return nil
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
