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
	NemoResponseGuardPluginType = "nemo-response-guard"
)

var _ framework.ResponseProcessor = &NemoResponseGuardPlugin{}

// NemoResponseGuardPlugin calls a NeMo Guardrails service over HTTP to check model output
// using output rails. It implements ResponseProcessor to inspect responses before returning
// them to the caller.
type NemoResponseGuardPlugin struct {
	nemoGuardBase
}

// NemoResponseGuardFactory is the factory function for NemoResponseGuardPlugin.
func NemoResponseGuardFactory(name string, rawParameters json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	config := nemoGuardConfig{
		TimeoutSeconds: defaultTimeoutSec,
	}

	if len(rawParameters) > 0 {
		if err := json.Unmarshal(rawParameters, &config); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' plugin - %w", NemoResponseGuardPluginType, err)
		}
	}

	p, err := NewNemoResponseGuardPlugin(config.NemoURL, config.TimeoutSeconds)
	if err != nil {
		return nil, fmt.Errorf("failed to create '%s' plugin - %w", NemoResponseGuardPluginType, err)
	}

	return p.WithName(name), nil
}

// NewNemoResponseGuardPlugin builds a NeMo response guard plugin from validated parameters.
func NewNemoResponseGuardPlugin(nemoURL string, timeoutSeconds int) (*NemoResponseGuardPlugin, error) {
	base, err := newNemoGuardBase(NemoResponseGuardPluginType, nemoURL, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	return &NemoResponseGuardPlugin{nemoGuardBase: *base}, nil
}

// WithName sets the name of the plugin instance.
func (p *NemoResponseGuardPlugin) WithName(name string) *NemoResponseGuardPlugin {
	p.typedName.Name = name
	return p
}

// ProcessResponse calls NeMo Guardrails to evaluate output rails on the model response.
// It extracts the assistant message from the OpenAI-style response body, POSTs it to
// NeMo's /v1/guardrail/checks endpoint, and returns an errcommon.Error with Forbidden (403)
// if NeMo flags the content.
//
// NeMo always returns HTTP 200 for both allowed and blocked responses. The block/allow
// decision is conveyed through the response body "status" field.
func (p *NemoResponseGuardPlugin) ProcessResponse(ctx context.Context, _ *framework.CycleState, response *framework.InferenceResponse) error {
	if response == nil || response.Body == nil {
		return nil
	}

	content, ok := extractAssistantContent(response.Body)
	if !ok || content == "" {
		return nil
	}

	model, _ := response.Body["model"].(string)
	if model == "" {
		model = "response-guard"
	}

	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "assistant", "content": content},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("nemo-response-guard: marshal request: %v", err)}
	}

	return p.callNemoGuard(ctx, NemoResponseGuardPluginType, payload)
}

// extractAssistantContent pulls the assistant message content from an OpenAI-style
// chat completion response body. Returns the content string and true if found.
func extractAssistantContent(body map[string]any) (string, bool) {
	choices, ok := body["choices"]
	if !ok {
		return "", false
	}
	choiceSlice, ok := choices.([]any)
	if !ok || len(choiceSlice) == 0 {
		return "", false
	}
	first, ok := choiceSlice[0].(map[string]any)
	if !ok {
		return "", false
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		// streaming responses use "delta" instead of "message"
		msg, ok = first["delta"].(map[string]any)
		if !ok {
			return "", false
		}
	}
	content, ok := msg["content"].(string)
	if !ok {
		return "", false
	}
	return content, true
}
