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
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
)

const (
	// nemoAllowedStatus is the top-level JSON status when a request passes all rails.
	nemoAllowedStatus = "success"
	// defaultTimeoutSec allows for CPU-based guardrail inference (2-5 min per request).
	defaultTimeoutSec = 360
	// maxNemoResponseBytes caps the NeMo response body to prevent memory exhaustion
	// from a misbehaving or compromised NeMo service (CWE-400).
	maxNemoResponseBytes = 1 << 20 // 1 MiB
)

// nemoGuardConfig is the shared JSON configuration for both nemo guard plugins.
type nemoGuardConfig struct {
	NemoURL        string `json:"nemoURL"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// nemoResponse is NeMo's JSON response from /v1/guardrail/checks.
type nemoResponse struct {
	Status      string                         `json:"status"`
	RailsStatus map[string]nemoRailStatusEntry `json:"rails_status"`
}

// nemoRailStatusEntry is one rail's outcome inside NeMo's rails_status object.
type nemoRailStatusEntry struct {
	Status string `json:"status"`
}

// nemoGuardBase holds the shared fields and HTTP logic for both guard plugins.
type nemoGuardBase struct {
	typedName  plugin.TypedName
	nemoURL    string
	httpClient *http.Client
}

// newNemoGuardBase builds the shared base from validated parameters.
func newNemoGuardBase(pluginType, nemoURL string, timeoutSeconds int) (*nemoGuardBase, error) {
	if nemoURL == "" {
		return nil, fmt.Errorf("nemoURL is required for plugin '%s'", pluginType)
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSec * time.Second
	}

	return &nemoGuardBase{
		typedName: plugin.TypedName{Type: pluginType, Name: pluginType},
		nemoURL:   nemoURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// TypedName returns the type and name tuple of this plugin instance.
func (b *nemoGuardBase) TypedName() plugin.TypedName {
	return b.typedName
}

// callNemoGuard POSTs the payload to the NeMo guardrail checks endpoint, parses the
// response, and returns an error if the content is blocked or NeMo is unreachable.
// pluginName is used as the prefix for error messages (e.g. "nemo-request-guard").
func (b *nemoGuardBase) callNemoGuard(ctx context.Context, pluginName string, payload []byte) error {
	logger := log.FromContext(ctx)
	logger.V(logutil.VERBOSE).Info("calling NeMo guardrails", "plugin", pluginName, "url", b.nemoURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.nemoURL, bytes.NewReader(payload))
	if err != nil {
		return errcommon.Error{Code: errcommon.Internal, Msg: fmt.Sprintf("%s: create request: %v", pluginName, err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("%s: call failed: %v", pluginName, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("%s: unexpected status %d", pluginName, resp.StatusCode)}
	}

	limited := io.LimitReader(resp.Body, maxNemoResponseBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("%s: read response: %v", pluginName, err)}
	}

	var nemoResp nemoResponse
	if err := json.Unmarshal(body, &nemoResp); err != nil {
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("%s: decode response: %v", pluginName, err)}
	}

	if strings.EqualFold(strings.TrimSpace(nemoResp.Status), nemoAllowedStatus) {
		logger.V(logutil.VERBOSE).Info("allowed by NeMo guardrails", "plugin", pluginName)
		return nil
	}

	railsParts := make([]string, 0, len(nemoResp.RailsStatus))
	for key, value := range nemoResp.RailsStatus {
		railsParts = append(railsParts, fmt.Sprintf("%s: %s", key, value.Status))
	}
	railsStatus := fmt.Sprintf("[ %s ]", strings.Join(railsParts, " "))

	logger.Info("blocked by NeMo guardrails", "plugin", pluginName, "railsStatus", railsStatus)
	return errcommon.Error{Code: errcommon.Forbidden, Msg: fmt.Sprintf("%s: blocked by NeMo guardrails", pluginName)}
}
