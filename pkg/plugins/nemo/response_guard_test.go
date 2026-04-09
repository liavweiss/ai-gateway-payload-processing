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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
)

// --- NewNemoResponseGuardPlugin construction ---

func TestNewNemoResponseGuardPlugin(t *testing.T) {
	tests := []struct {
		name        string
		nemoURL     string
		timeout     int
		wantErr     bool
		wantNemoURL string
	}{
		{
			name:        "valid config",
			nemoURL:     "http://nemo:8000/v1/guardrail/checks",
			timeout:     30,
			wantNemoURL: "http://nemo:8000/v1/guardrail/checks",
		},
		{
			name:    "missing nemoURL — error",
			nemoURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewNemoResponseGuardPlugin(tt.nemoURL, tt.timeout)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, p)
			assert.Equal(t, tt.wantNemoURL, p.nemoURL)
		})
	}
}

func TestNemoResponseGuardTypedName(t *testing.T) {
	p, err := NewNemoResponseGuardPlugin("http://nemo:8000/v1/guardrail/checks", 30)
	require.NoError(t, err)

	assert.Equal(t, NemoResponseGuardPluginType, p.TypedName().Name)

	p = p.WithName("my-output-guard")
	tn := p.TypedName()
	assert.Equal(t, NemoResponseGuardPluginType, tn.Type)
	assert.Equal(t, "my-output-guard", tn.Name)
}

// --- ProcessResponse: allow / block / error ---

func TestNemoResponseGuardProcessResponse(t *testing.T) {
	const forbiddenMsg = "nemo-response-guard: blocked by NeMo guardrails"

	validResponseBody := func(content string) map[string]any {
		return map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": content,
					},
				},
			},
		}
	}

	tests := []struct {
		name            string
		serverHandler   http.HandlerFunc
		body            map[string]any
		wantErr         bool
		wantErrContains string
		wantErrCode     string
	}{
		{
			name: "allow: NeMo returns status success",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{
					"status": "success",
					"rails_status": map[string]any{
						"output-rail": map[string]any{"status": "success"},
					},
				}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			},
			body:    validResponseBody("The weather is sunny today."),
			wantErr: false,
		},
		{
			name: "block: only success allows — status allowed is rejected",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{"status": "allowed"}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			},
			body:            validResponseBody("Hello"),
			wantErr:         true,
			wantErrContains: forbiddenMsg,
			wantErrCode:     errcommon.Forbidden,
		},
		{
			name: "block: NeMo returns status blocked with per-rail detail",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{
					"status": "blocked",
					"rails_status": map[string]any{
						`huggingface detector check output $hf_model="ibm-granite/granite-guardian-hap-38m"`: map[string]any{"status": "blocked"},
					},
				}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			},
			body:            validResponseBody("You stupid moron."),
			wantErr:         true,
			wantErrContains: forbiddenMsg,
			wantErrCode:     errcommon.Forbidden,
		},
		{
			name: "block: NeMo returns empty body (fail closed)",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			},
			body:            validResponseBody("Some content"),
			wantErr:         true,
			wantErrContains: forbiddenMsg,
			wantErrCode:     errcommon.Forbidden,
		},
		{
			name: "block: NeMo returns status blocked without rails_status",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{"status": "blocked"}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			},
			body:            validResponseBody("Toxic content here"),
			wantErr:         true,
			wantErrContains: forbiddenMsg,
			wantErrCode:     errcommon.Forbidden,
		},
		{
			name: "error: NeMo returns HTTP 500",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			body:            validResponseBody("Hello"),
			wantErr:         true,
			wantErrContains: "unexpected status 500",
		},
		{
			name: "error: NeMo returns invalid JSON",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if _, err := fmt.Fprint(w, "not valid json {{{"); err != nil {
					t.Errorf("unexpected error writing test response: %v", err)
				}
			},
			body:            validResponseBody("Hello"),
			wantErr:         true,
			wantErrContains: "decode response",
		},
		{
			name:    "no-op: no choices in response — allow without calling NeMo",
			body:    map[string]any{"object": "chat.completion"},
			wantErr: false,
		},
		{
			name:    "no-op: empty choices array — allow without calling NeMo",
			body:    map[string]any{"choices": []any{}},
			wantErr: false,
		},
		{
			name: "no-op: assistant content is empty — allow without calling NeMo",
			body: map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{"role": "assistant", "content": ""},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "no-op: choices has no message or delta — allow without calling NeMo",
			body: map[string]any{
				"choices": []any{
					map[string]any{"index": 0, "finish_reason": "stop"},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL := "http://unreachable-should-not-be-called:9999"
			var srv *httptest.Server
			if tt.serverHandler != nil {
				srv = httptest.NewServer(tt.serverHandler)
				defer srv.Close()
				baseURL = srv.URL
			}

			p, err := NewNemoResponseGuardPlugin(baseURL, 30)
			require.NoError(t, err)

			resp := framework.NewInferenceResponse()
			for k, v := range tt.body {
				resp.Body[k] = v
			}

			err = p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrCode != "" {
					var infErr errcommon.Error
					require.ErrorAs(t, err, &infErr)
					assert.Equal(t, tt.wantErrCode, infErr.Code)
				}
				if tt.wantErrContains != "" {
					assert.Contains(t, err.Error(), tt.wantErrContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestNemoResponseGuardSendsCorrectPayload verifies the request sent to NeMo matches the expected format.
func TestNemoResponseGuardSendsCorrectPayload(t *testing.T) {
	var capturedReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedReq))
		require.NoError(t, json.NewEncoder(w).Encode(nemoAllowedJSON()))
	}))
	defer srv.Close()

	p, err := NewNemoResponseGuardPlugin(srv.URL, 30)
	require.NoError(t, err)

	resp := framework.NewInferenceResponse()
	resp.Body["choices"] = []any{
		map[string]any{
			"message": map[string]any{"role": "assistant", "content": "Here is the answer."},
		},
	}
	err = p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	require.NoError(t, err)

	messages, ok := capturedReq["messages"].([]any)
	require.True(t, ok, "messages should be an array")
	require.Len(t, messages, 1)
	msg := messages[0].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "Here is the answer.", msg["content"])
}

// TestNemoResponseGuardForwardsModel verifies the model field from the response body is forwarded to NeMo.
func TestNemoResponseGuardForwardsModel(t *testing.T) {
	var capturedReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedReq))
		require.NoError(t, json.NewEncoder(w).Encode(nemoAllowedJSON()))
	}))
	defer srv.Close()

	p, err := NewNemoResponseGuardPlugin(srv.URL, 30)
	require.NoError(t, err)

	resp := framework.NewInferenceResponse()
	resp.Body["model"] = "gpt-4"
	resp.Body["choices"] = []any{
		map[string]any{
			"message": map[string]any{"role": "assistant", "content": "Answer"},
		},
	}
	err = p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	require.NoError(t, err)

	assert.Equal(t, "gpt-4", capturedReq["model"])
}

// TestNemoResponseGuardBaseURLTrailingSlash ensures a trailing slash in nemoURL doesn't double up.
func TestNemoResponseGuardBaseURLTrailingSlash(t *testing.T) {
	var calledPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path
		require.NoError(t, json.NewEncoder(w).Encode(nemoAllowedJSON()))
	}))
	defer srv.Close()

	p, err := NewNemoResponseGuardPlugin(srv.URL+"//", 30)
	require.NoError(t, err)

	resp := framework.NewInferenceResponse()
	resp.Body["choices"] = []any{
		map[string]any{
			"message": map[string]any{"role": "assistant", "content": "Hello"},
		},
	}
	err = p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(calledPath, "/"), "request path should be absolute: %q", calledPath)
}

// TestNemoResponseGuardFactory verifies the factory parses JSON and sets the instance name.
func TestNemoResponseGuardFactory(t *testing.T) {
	params := json.RawMessage(`{"nemoURL":"http://nemo:8000/v1/guardrail/checks"}`)
	p, err := NemoResponseGuardFactory("my-output-guard", params, nil)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "my-output-guard", p.TypedName().Name)
	assert.Equal(t, NemoResponseGuardPluginType, p.TypedName().Type)
}

func TestNemoResponseGuardFactoryMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{name: "missing all fields", params: `{}`},
		{name: "invalid JSON", params: `{invalid`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NemoResponseGuardFactory("test", json.RawMessage(tt.params), nil)
			require.Error(t, err)
		})
	}
}

// --- nil response ---

func TestNemoResponseGuardNilResponse(t *testing.T) {
	p, err := NewNemoResponseGuardPlugin("http://nemo:8000/v1/guardrail/checks", 30)
	require.NoError(t, err)

	err = p.ProcessResponse(context.Background(), framework.NewCycleState(), nil)
	assert.NoError(t, err, "nil response should be a no-op")
}

// --- extractAssistantContent ---

func TestExtractAssistantContent(t *testing.T) {
	tests := []struct {
		name        string
		body        map[string]any
		wantContent string
		wantOK      bool
	}{
		{
			name: "standard message field",
			body: map[string]any{
				"choices": []any{
					map[string]any{"message": map[string]any{"role": "assistant", "content": "Hello"}},
				},
			},
			wantContent: "Hello",
			wantOK:      true,
		},
		{
			name: "delta field (streaming)",
			body: map[string]any{
				"choices": []any{
					map[string]any{"delta": map[string]any{"role": "assistant", "content": "Streaming chunk"}},
				},
			},
			wantContent: "Streaming chunk",
			wantOK:      true,
		},
		{
			name:        "no choices key",
			body:        map[string]any{"object": "chat.completion"},
			wantContent: "",
			wantOK:      false,
		},
		{
			name:        "empty choices",
			body:        map[string]any{"choices": []any{}},
			wantContent: "",
			wantOK:      false,
		},
		{
			name: "no message or delta in choice",
			body: map[string]any{
				"choices": []any{
					map[string]any{"index": 0},
				},
			},
			wantContent: "",
			wantOK:      false,
		},
		{
			name: "content is not a string",
			body: map[string]any{
				"choices": []any{
					map[string]any{"message": map[string]any{"content": 42}},
				},
			},
			wantContent: "",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, ok := extractAssistantContent(tt.body)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantContent, content)
		})
	}
}
