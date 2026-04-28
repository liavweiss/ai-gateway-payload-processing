# NeMo Guardrails with BBR — Using TrustyAI

Guide for wiring [NeMo Guardrails](https://github.com/NVIDIA/NeMo-Guardrails)
(deployed via the [TrustyAI Helm chart](https://github.com/trustyai-explainability/trustyai-llm-demo/tree/add-mcp-guardrails/mcp-guardrails/deploy))
to BBR's `nemo-request-guard` and `nemo-response-guard` plugins.

## Overview

BBR provides two NeMo plugins:

| Plugin | Direction | What it checks |
|--------|-----------|----------------|
| `nemo-request-guard` | Input rails | User messages **before** they reach the model |
| `nemo-response-guard` | Output rails | Model responses **before** they reach the caller |

Both plugins call NeMo's `/v1/chat/completions` endpoint and use the
`output_vars` mechanism to request `message_action` and `block_reason` from
NeMo's Colang subflows. The plugin inspects `message_action` to decide:

| Action | Behavior | HTTP |
|--------|----------|------|
| `pass` | Request/response proceeds normally | 200 |
| `block` | Blocked with reason forwarded to client | 403 |
| `modify` | Content was redacted (e.g., PII masked) — passes through | 200 |

## Prerequisites

- Kubernetes cluster with `kubectl` configured
- The `nemo-request-guard` and/or `nemo-response-guard` plugins built into your BBR image

## Step 1: Deploy NeMo Guardrails

Follow the [TrustyAI Guardrails Helm deployment guide](https://github.com/trustyai-explainability/trustyai-llm-demo/tree/add-mcp-guardrails/mcp-guardrails/deploy)
to deploy a NeMo Guardrails instance on Kubernetes.

The NeMo configuration must include Colang subflows that set the `message_action`
and `block_reason` context variables. These variables are how the plugin
distinguishes between pass, block, and redact outcomes.

### Example NeMo Configuration

Below is a sample ConfigMap with PII redaction on input. Replace the configmap
in the Helm chart with your desired configuration:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: nemo-config
data:
  config.yaml: |
    models: []
    rails:
      config:
        sensitive_data_detection:
          input:
            entities:
              - PERSON
              - EMAIL_ADDRESS
              - PHONE_NUMBER
              - CREDIT_CARD
              - US_SSN
              - IBAN_CODE
      input:
        flows:
          - redact sensitive data

  flows.co: |
    define subflow redact sensitive data
      $result = execute mask_sensitive_data(source="input", text=$user_message)
      $message_action = "pass"
      if $result != $user_message
          $block_reason = "PII redacted in input"
          $message_action = "modify"
      bot $result
      stop
```

> **Note:** The standardized catalog of configs (block, redact, etc.) is being
> finalized by the TrustyAI team. The example above is a working sample for
> PII redaction. Check the
> [TrustyAI deployment guide](https://github.com/trustyai-explainability/trustyai-llm-demo/tree/add-mcp-guardrails/mcp-guardrails/deploy)
> for the latest official configurations.

## Step 2: Verify NeMo is Working

Port-forward to the NeMo service and test with a direct `curl`:

```bash
kubectl port-forward svc/example-nemoguardrails -n trustyai-guardrails 8000:8000
```

```bash
curl -s http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "test",
    "messages": [{"role": "user", "content": "My email is john@example.com"}],
    "guardrails": {
      "options": {
        "output_vars": ["message_action", "block_reason"]
      }
    }
  }' | jq .
```

Verify the response includes `guardrails.output_data` with `message_action`
and (when applicable) `block_reason`.

## Step 3: Configure the BBR Plugins

Add one or both NeMo guard plugins to BBR's deployment args. The `--plugin` format
is `<type>:<name>:<json-config>`:

### Request Guard (input rails)

```text
--plugin nemo-request-guard:nemo-input:{"nemoURL":"http://example-nemoguardrails.trustyai-guardrails.svc:8000/v1/chat/completions","timeoutSeconds":10}
```

### Response Guard (output rails)

```text
--plugin nemo-response-guard:nemo-output:{"nemoURL":"http://example-nemoguardrails.trustyai-guardrails.svc:8000/v1/chat/completions","timeoutSeconds":10}
```

### Configuration Fields

Both plugins share the same configuration schema:

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `nemoURL` | Yes | — | Full URL of the NeMo `/v1/chat/completions` endpoint |
| `timeoutSeconds` | No | `360` | How long BBR waits for a NeMo response |
| `actionVar` | No | `message_action` | Name of the NeMo output variable for the action |
| `reasonVar` | No | `block_reason` | Name of the NeMo output variable for the reason |

The `nemoURL` is the **full endpoint URL** — the plugin POSTs directly to this URL
without appending any path.

`actionVar` and `reasonVar` allow customization if the NeMo Colang configuration
uses different variable names. The defaults (`message_action`, `block_reason`)
match the standardized catalog conventions.

### BBR Deployment Example

Add the plugin flags to your BBR deployment args:

```yaml
containers:
  - name: bbr
    args:
      - "--plugin"
      - "nemo-request-guard:nemo-input:{\"nemoURL\":\"http://example-nemoguardrails.trustyai-guardrails.svc:8000/v1/chat/completions\",\"timeoutSeconds\":10}"
      - "--plugin"
      - "nemo-response-guard:nemo-output:{\"nemoURL\":\"http://example-nemoguardrails.trustyai-guardrails.svc:8000/v1/chat/completions\",\"timeoutSeconds\":10}"
```

### ext_proc Note for Response Guard

If using `nemo-response-guard`, make sure your Envoy ext_proc configuration has
`response_body_mode: BUFFERED` and `response_header_mode: SEND` in the
`processing_mode` section. Without this, Envoy will not forward response bodies
to BBR and the response guard will silently skip processing.

## How It Works

```text
User → BBR → nemo-request-guard → NeMo (input rails) → Model Backend
                                                              │
                                                         Model response
                                                              │
User ← BBR ← nemo-response-guard ← NeMo (output rails) ←────┘

  message_action:
    "pass"   → forward request / return response
    "block"  → HTTP 403 with block_reason
    "modify" → pass through (redacted content in a future release)
```

1. BBR receives an inference request and runs the `nemo-request-guard` plugin.
2. The request guard extracts all messages from the request body and POSTs them
   to NeMo's `/v1/chat/completions` endpoint, including a `guardrails.options.output_vars`
   array requesting `message_action` and `block_reason`.
3. NeMo runs all configured **input** rails (PII redaction, etc.) and returns a
   chat-completions response with `guardrails.output_data` containing the action and reason.
4. The plugin inspects `message_action`:
   - `"pass"` → the request proceeds to the model backend.
   - `"block"` → BBR returns HTTP 403 with the `block_reason` as the error message.
   - `"modify"` → the request passes through (full redaction support is planned).
5. After the model responds, BBR runs the `nemo-response-guard` plugin.
6. The response guard extracts assistant messages from all `choices` in the
   response (checking `message.content` first, falling back to `delta.content`
   for streaming) and POSTs them to NeMo.
7. NeMo runs all configured **output** rails and the same `message_action` logic applies.

The plugin also supports MCP JSON-RPC payloads — string arguments from
`params.arguments` are extracted and sent to NeMo as a user message.

## References

- [TrustyAI Guardrails Helm Deployment](https://github.com/trustyai-explainability/trustyai-llm-demo/tree/add-mcp-guardrails/mcp-guardrails/deploy) — Kubernetes deployment guide
- [TrustyAI NeMo-Guardrails Fork](https://github.com/trustyai-explainability/NeMo-Guardrails)
- [NVIDIA NeMo Guardrails](https://github.com/NVIDIA/NeMo-Guardrails)
- [NeMo YAML schema](https://docs.nvidia.com/nemo/guardrails/latest/configure-rails/yaml-schema/index.html)
- [Presidio supported entities](https://microsoft.github.io/presidio/supported_entities/) — for PII detection configuration
