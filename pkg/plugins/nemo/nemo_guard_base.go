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
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
)

const (
	defaultTimeoutSec    = 360
	maxNemoResponseBytes = 1 << 20 // 1 MiB

	defaultActionVar = "message_action"
	defaultReasonVar = "block_reason"

	ActionPass   = "pass"
	ActionBlock  = "block"
	ActionModify = "modify"
)

// nemoGuardConfig is the configuration for nemo guard plugins.
type nemoGuardConfig struct {
	NemoURL        string `json:"nemoURL"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	ActionVar      string `json:"actionVar"`
	ReasonVar      string `json:"reasonVar"`
}

// nemoCompletionResponse is NeMo's JSON response from /v1/chat/completions.
type nemoCompletionResponse struct {
	Choices    []nemoChoice       `json:"choices"`
	Guardrails nemoGuardrailsData `json:"guardrails"`
}

type nemoChoice struct {
	Message nemoMessage `json:"message"`
}

type nemoMessage struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}

type nemoGuardrailsData struct {
	ConfigID   string         `json:"config_id"`
	OutputData map[string]any `json:"output_data"`
}

// nemoGuardResult holds the parsed outcome from a /v1/chat/completions call.
type nemoGuardResult struct {
	Action  string // ActionPass, ActionBlock, or ActionModify
	Reason  string // human-readable reason (empty for pass)
	Content string // response content from choices[0].message.content
}

// nemoGuardBase holds the shared fields and HTTP logic for nemo guard plugins.
type nemoGuardBase struct {
	nemoURL    string
	httpClient *http.Client
	actionVar  string
	reasonVar  string
}

func newNemoGuardBase(nemoURL string, timeoutSeconds int, actionVar, reasonVar string) (*nemoGuardBase, error) {
	if nemoURL == "" {
		return nil, errors.New("nemoURL is required")
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSec * time.Second
	}
	if actionVar == "" {
		actionVar = defaultActionVar
	}
	if reasonVar == "" {
		reasonVar = defaultReasonVar
	}

	return &nemoGuardBase{
		nemoURL:    nemoURL,
		httpClient: &http.Client{Timeout: timeout},
		actionVar:  actionVar,
		reasonVar:  reasonVar,
	}, nil
}

// callNemoGuard POSTs a chat-completions request to NeMo with output_vars,
// parses the response, and returns a nemoGuardResult describing the action taken.
// The caller provides the model and messages; this method adds the guardrails.options
// wrapper with the configured output variable names.
func (b *nemoGuardBase) callNemoGuard(ctx context.Context, model string, messages any) (*nemoGuardResult, error) {
	logger := log.FromContext(ctx).V(logutil.DEFAULT)
	log.FromContext(ctx).V(logutil.VERBOSE).Info("calling NeMo guardrails", "url", b.nemoURL)

	reqBody := map[string]any{
		"model":    model,
		"messages": messages,
		"guardrails": map[string]any{
			"options": map[string]any{
				"output_vars": []string{b.actionVar, b.reasonVar},
			},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		logger.Error(err, "failed to marshal NeMo request")
		return nil, errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("failed to marshal nemo request: %v", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.nemoURL, bytes.NewReader(payload))
	if err != nil {
		logger.Error(err, "failed to create NeMo request")
		return nil, errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("failed to create nemo request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		logger.Error(err, "NeMo guardrail call failed")
		return nil, errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("nemo call failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		logger.Error(nil, "NeMo guardrail unexpected status", "statusCode", resp.StatusCode)
		return nil, errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("unexpected status %d from NeMo", resp.StatusCode)}
	}

	limited := io.LimitReader(resp.Body, maxNemoResponseBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		logger.Error(err, "failed to read NeMo response")
		return nil, errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("failed to read nemo response: %v", err)}
	}

	var nemoResp nemoCompletionResponse
	if err := json.Unmarshal(body, &nemoResp); err != nil {
		logger.Error(err, "failed to decode NeMo response")
		return nil, errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("failed to decode nemo response: %v", err)}
	}

	result := &nemoGuardResult{Action: ActionPass}

	if len(nemoResp.Choices) > 0 {
		result.Content = nemoResp.Choices[0].Message.Content
	}

	if od := nemoResp.Guardrails.OutputData; od != nil {
		if action, ok := od[b.actionVar].(string); ok && action != "" {
			result.Action = action
		}
		if reason, ok := od[b.reasonVar].(string); ok {
			result.Reason = reason
		}
	}

	logger.Info("NeMo guardrails result", "action", result.Action, "reason", result.Reason)
	return result, nil
}
