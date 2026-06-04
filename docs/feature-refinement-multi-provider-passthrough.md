# Feature Refinement — RHAISTRAT-XXXX — Multi-Provider API Passthrough for External Model Serving

| Field | Value |
|-------|-------|
| **Feature Jira Link** | RHAISTRAT-XXXX |
| **Status** | Draft |
| **Slack Channel / Thread** | #wg-ai-gateway-internal |
| **Feature Owner** | Jonathan Zarecki |
| **Delivery Owner** | Inference Gateway Team |
| **RFE Council Reviewer** | TBD |
| **Product** | RHOAI (managed and self-managed) |

---

## Feature Details

### Feature Overview

This feature adds **passthrough mode** to the Inference Payload Processor (IPP), allowing clients that speak a provider's native API format to send requests through the MaaS gateway without request/response translation.

**Today:** All client requests must arrive in OpenAI `/v1/chat/completions` format. The `api-translation` plugin translates to the upstream provider's format (Anthropic, Bedrock, Azure, Vertex) and translates responses back to OpenAI format. This works when the client is an OpenAI SDK consumer.

**Problem:** Increasingly, clients speak their provider's native format directly — Claude Code sends Anthropic Messages format (`/v1/messages`), OpenAI Codex sends Responses format (`/v1/responses`). Forcing these through OpenAI→provider→OpenAI translation is lossy — provider-specific features (prompt caching, extended thinking, beta flags) have no OpenAI equivalent and are stripped during translation.

**With passthrough:** The IPP detects the incoming client format from the request path, compares it to the upstream provider's declared `apiFormat` (from the ExternalModel CR), and skips translation when they match. The request and response flow through untouched, preserving all provider-specific features.

**Design reference:** [Multi-Provider API Passthrough Design Doc](docs/design-multi-provider-passthrough.md)

### The Why

Three problems drive this feature:

1. **Feature loss during translation** — Claude Code uses Anthropic-specific features (prompt caching with `cache_control`, extended thinking with `thinking` parameter, beta flags). The OpenAI→Anthropic→OpenAI translation path strips these because they have no OpenAI equivalent. Users lose capabilities they're paying for.

2. **New API format support** — OpenAI's Responses API (`/v1/responses`) is replacing Chat Completions for agentic use cases (Codex CLI). The current IPP rejects these requests entirely ("only /chat/completions input type is supported"). Adding passthrough is simpler and more maintainable than writing a full Responses API translator.

3. **Multi-tool enterprise environments** — Organizations use multiple AI coding tools (Claude Code, Codex, Cursor, aider) with different providers. MaaS should serve all of them through a single gateway without requiring tools to change their native API format.

**Customer evidence:** The MaaS dogfood environment demonstrated this with 30 engineers using both Claude Code and Codex through the same MaaS gateway. Without passthrough, Claude Code cannot connect.

### High Level Requirements

**1. Accept non-OpenAI request paths.** As a **platform admin**, I want the IPP to accept `/v1/messages` (Anthropic) and `/v1/responses` (OpenAI Responses API) in addition to `/v1/chat/completions`, so that clients using native provider SDKs can route through MaaS.

**2. Auto-detect incoming client format.** As an **IPP plugin developer**, I want the model-provider-resolver to detect the client's API format from the request path and write it to CycleState, so that downstream plugins can make format-aware decisions without additional configuration.

**3. Skip translation when formats match.** As a **platform admin**, I want the api-translation plugin to skip request/response translation when the detected incoming format matches the upstream provider's `apiFormat`, so that provider-specific features are preserved end-to-end (passthrough mode).

**4. MaaS AuthPolicy: x-api-key header support.** As a **platform admin**, I want MaaS-generated AuthConfigs to support API key authentication via the `x-api-key` HTTP header (Anthropic SDK convention), in addition to `Authorization: Bearer` (OpenAI convention), so that Claude Code can authenticate to the MaaS gateway without manual AuthConfig patches.

**5. MaaS HTTPRoute: URL path rewrite.** As a **platform admin**, I want MaaS-generated HTTPRoutes to include a URLRewrite filter that strips the model path prefix (`/llm/model-name/`) before forwarding to the upstream provider, so that providers receive clean paths (`/v1/messages`, not `/llm/ext-claude/v1/messages`).

### Non-Functional Requirements

- **Backward compatibility:** Existing OpenAI-only deployments must be unaffected. If no `IncomingAPIFormatKey` is set in CycleState, api-translation behaves exactly as before.
- **No performance overhead:** Passthrough mode removes processing (skips translation), it does not add any.
- **No new CRD fields required:** The existing `ExternalProviderRef.apiFormat` field (from RHAISTRAT-1720) is sufficient to declare the upstream format. The incoming format is auto-detected from the request path.
- **Streaming support:** Passthrough must work for both non-streaming and SSE streaming responses. The BBR framework SSE fix (upstream PR #138) is a prerequisite.

### Out-of-Scope

- **Model override / transparent model swapping** — changing the backend model without client awareness. Tracked separately.
- **Parameter normalization** — stripping incompatible parameters when overriding models across capability tiers (e.g., Opus → Sonnet). Tracked separately.
- **External metering / usage tracking** — token-level usage recording to external storage. Separate feature.
- **Unified entry point / body-based routing** — routing based on the `model` field in the request body rather than URL path. Tracked in RHAISTRAT-1540.
- **New provider translation plugins** — adding translation support for new providers (e.g., Gemini native). This is api-translation scope, not passthrough scope.

### Acceptance Criteria

- IPP accepts requests on `/v1/messages`, `/v1/responses`, and `/v1/chat/completions` paths for ExternalModel routes
- Model-provider-resolver writes `incoming-api-format` and `api-format` to CycleState
- api-translation skips request and response translation when `incoming-api-format == api-format`
- Claude Code can send native Anthropic requests through MaaS gateway to an Anthropic ExternalModel — response is native Anthropic format (not translated to OpenAI)
- OpenAI Codex can send Responses API requests through MaaS gateway to an OpenAI ExternalModel
- Existing OpenAI → Anthropic translation path still works (formats differ → translate)
- MaaS AuthPolicy supports `x-api-key` header authentication natively (no manual patches)
- MaaS HTTPRoutes include URLRewrite filter for clean upstream paths
- Unit tests cover format detection, passthrough logic, and CycleState propagation
- E2E tests validate passthrough for Anthropic and OpenAI providers

### Risks & Assumptions

**Risks:**

- **MaaS AuthPolicy dependency** — passthrough requires MaaS AuthConfig changes (`x-api-key` support). Until the MaaS team ships these, deployments need manual AuthConfig patches and operators scaled to 0. This is the primary blocker for GA-quality passthrough.
- **SSE framework dependency** — streaming passthrough requires the BBR framework SSE fix (PR #138 to llm-d). Until merged, the IPP uses a fork with a replace directive in go.mod.
- **Wasm plugin token extraction** — the Kuadrant Wasm plugin uses `responseBodyJSON("/usage/total_tokens")` which only works for OpenAI format. Anthropic responses use different field names (`input_tokens` + `output_tokens`). Token-based rate limiting via Limitador does not work for Anthropic passthrough. Tracked in Kuadrant issue #1864.

**Assumptions:**

- RHAISTRAT-1720 (ExternalProvider/ExternalModel CRD redesign) is merged and the `apiFormat` field is available on ExternalProviderRef.
- The BBR framework (RHAISTRAT-1320) is stable enough to support the passthrough plugin chain.
- Clients using passthrough accept that MaaS-level features (usage dashboards, rate limiting) may have reduced fidelity for non-OpenAI formats until the Wasm plugin supports multi-format token extraction.

### Supporting Documentation

- [Multi-Provider API Passthrough Design Doc](docs/design-multi-provider-passthrough.md) — architecture, data flow, changes required
- RHAISTRAT-1720 — Separated Provider and Model Configuration (prerequisite — merged)
- RHAISTRAT-1320 — Pluggable BBR Framework for IGW (prerequisite)
- RHAISTRAT-1540 — Unified Entry Point / Body-Based Routing (future, complementary)
- [Kuadrant #1864](https://github.com/Kuadrant/kuadrant-operator/issues/1864) — Extend TokenRateLimitPolicy to support non-OpenAI endpoints
- [BBR Framework PR #138](https://github.com/llm-d/llm-d-inference-payload-processor/pull/138) — SSE streaming response parsing

---

## New Feature / Component Prerequisites & Dependencies

| Field | Answer |
|-------|--------|
| **Architecture Review Check** | NO — this is an extension of existing plugin behavior, not a new architectural pattern |
| **Licence Validation** | NO — no new upstream projects or sub-projects |
| **Accelerator/Package Support** | NO — pure Go binary, no new dependencies |
| **UXD Support** | NO |
| **Performance Team Support** | NO — passthrough removes processing, does not add it |
| **Documentation Support** | YES — provider passthrough mode needs to be documented for platform admins configuring ExternalModels |

### Add'l Dependencies

| Dependency | Owner | Status |
|------------|-------|--------|
| MaaS AuthPolicy: `x-api-key` header support | MaaS team (MaaS team) | Not started — required for production passthrough without workarounds |
| MaaS HTTPRoute: URLRewrite filter | MaaS team | Not started |
| BBR Framework SSE fix | llm-d upstream (PR #138) | Open PR |
| Kuadrant Wasm multi-format token extraction | Kuadrant team (#1864) | Open issue |

### TestOps Support

Are there dependency operators which customers will need to enable this feature? **YES** — Kuadrant (Authorino for auth, Limitador for rate limiting), same as existing ExternalModel flow. No new operators.

Will this feature need to onboard e2e tests for Build Validation pipelines? **YES** — passthrough E2E tests for Anthropic and OpenAI Responses API paths.

---

## High Level Plan

| Team | Start Date | EPIC | Dependencies | T-Shirt Size | Approval |
|------|-----------|------|--------------|-------------|----------|
| Inference Gateway (Inference Gateway Team) | June 2026 | TBD | RHAISTRAT-1720 (merged) | S | |
| MaaS (MaaS team) | TBD | TBD | AuthPolicy template changes | S | |

---

## Open Questions

1. **MaaS AuthPolicy timeline** — when can the MaaS team add native `x-api-key` header support to the AuthPolicy template? This is the primary blocker for removing the manual patching workaround.

2. **Wasm plugin multi-format support** — Kuadrant #1864 tracks extending the Wasm plugin to support non-OpenAI response formats for token extraction. Without this, token-based rate limiting only works for OpenAI-format responses. Is this on the Kuadrant roadmap for 3.5/3.6?

3. **Responses API as a first-class format** — OpenAI is deprecating Chat Completions in favor of the Responses API. Should `openai-responses` be treated as a separate `apiFormat` value, or should it be considered a variant of `openai`? This affects how api-translation handles the format mismatch.
