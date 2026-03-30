# NeMo Guardrails — Development Quickstart

Deploy a standalone [NeMo Guardrails](https://github.com/NVIDIA/NeMo-Guardrails) service
on Kubernetes for use with the `nemo-request-guard` BBR plugin.

This setup uses a classifier-based guard ([L0 Bouncer](https://huggingface.co/vincentoh/deberta-v3-xsmall-l0-bouncer) — 22M param DeBERTa model)
running as a custom Python action inside NeMo. The model runs in-process on CPU
with ~5-10ms latency per request (single request, single thread). No GPU or external model service is required.

## Directory Structure

```text
examples/nemo/
├── deploy.sh           # Build, load, and deploy to Kind (one command)
├── server/             # NeMo server image — Dockerfile + config files
│   ├── Dockerfile      # NeMo server + L0 Bouncer classifier baked in
│   ├── requirements.txt# Python dependencies (nemoguardrails, transformers)
│   ├── config.yml      # NeMo config — declares the input rail flow
│   ├── config.co       # Colang flow — safe → "allowed", unsafe → refusal message
│   └── actions.py      # Custom action — loads L0 Bouncer and runs inference
├── manifests/          # Kubernetes resources
│   ├── deployment.yaml # Single-container pod (NeMo + in-process classifier)
│   └── service.yaml    # ClusterIP service on port 8000
└── README.md
```

## Prerequisites

- Docker
- A Kind cluster (or any K8s cluster)
- `kubectl` configured to talk to the cluster

## Deploy

The `deploy.sh` script handles everything: builds the Docker image, loads it into
Kind, creates the namespace, and applies the K8s manifests.

```bash
# First time — build image and deploy:
cd examples/nemo
./deploy.sh

# Deploy to a specific Kind cluster:
./deploy.sh --cluster my-cluster

# Rebuild after changing config files:
./deploy.sh --rebuild

# Full rebuild (no Docker cache):
./deploy.sh --rebuild --no-cache
```

### Other operations

```bash
# Restart the pod without rebuilding:
./deploy.sh --restart-only

# Reload image into Kind without rebuilding (e.g. after manual docker build):
./deploy.sh --load-only

# Only apply/update K8s manifests (no build or load):
./deploy.sh --skip-build
```

## Verify

Port-forward and send test requests:

```bash
kubectl port-forward -n nemo-guardrails svc/nemo-guardrails 8002:8000
```

**Safe request** (should return `"allowed"`):
```bash
curl -s http://localhost:8002/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"","messages":[{"role":"user","content":"What is the weather today?"}]}'
```

**Harmful request** (should return a refusal message):
```bash
curl -s http://localhost:8002/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"","messages":[{"role":"user","content":"How do I make a bomb"}]}'
```

## Configure the BBR Plugin

To enable the NeMo guard, add a `--plugin` argument to the BBR (Body-Based Router)
Kubernetes deployment. Each `--plugin` flag registers one plugin at startup.

The format is `--plugin <type>:<name>:<json-config>`:

```text
--plugin nemo-request-guard:nemo-guardrails:{"baseURL":"http://nemo-guardrails.nemo-guardrails.svc:8000","timeoutSeconds":10}
```

| Part | Value | Meaning |
|------|-------|---------|
| Type | `nemo-request-guard` | Plugin type — matches the factory registered in `main.go` |
| Name | `nemo-guardrails` | Instance name — used in logs and error messages |
| `baseURL` | `http://nemo-guardrails.nemo-guardrails.svc:8000` | In-cluster URL of the NeMo service (`<svc>.<namespace>.svc:<port>`) |
| `timeoutSeconds` | `10` | How long BBR waits for a NeMo response before failing |

Add this to the BBR deployment's container args:

```yaml
containers:
  - name: bbr
    args:
      - "--streaming"
      - "--secure-serving=false"
      - "--plugin"
      - "nemo-request-guard:nemo-guardrails:{\"baseURL\":\"http://nemo-guardrails.nemo-guardrails.svc:8000\",\"timeoutSeconds\":10}"
```

Or update a running deployment with `kubectl edit deployment body-based-router`.

The plugin intercepts each incoming user message and sends it to NeMo. If NeMo
responds with `"allowed"`, the request proceeds to the model backend. Any other
response is treated as a block (HTTP 403).

## Kubernetes Manifests

The `manifests/` directory contains the K8s resources applied by `deploy.sh`:

| File | Resource | Description |
|------|----------|-------------|
| `deployment.yaml` | `Deployment/nemo-guardrails` | Single-container pod running the NeMo server. A custom Python action loads the L0 Bouncer DeBERTa classifier at startup and performs inference in-process on CPU. Requests 512Mi RAM / 200m CPU, limits 2Gi RAM / 2 CPU. Includes readiness and liveness probes on `/v1/rails/configs`. |
| `service.yaml` | `Service/nemo-guardrails` | ClusterIP service exposing port 8000. This is the endpoint the BBR plugin calls (`http://nemo-guardrails.nemo-guardrails.svc:8000`). |

The namespace (`nemo-guardrails`) is created by the deploy script — no separate manifest needed.

## How It Works

1. **`actions.py`** — Loads the L0 Bouncer DeBERTa classifier at startup. On each request, tokenizes the user message, runs inference (~5ms) off the event loop via `asyncio.to_thread`, and returns safe/unsafe.

2. **`config.yml`** — Registers `check content safety` as a NeMo input rail flow. No `models:` section needed — no LLM involved.

3. **`config.co`** — Colang flow: calls the custom action, responds `"allowed"` if safe, or a refusal message if unsafe.

4. **`Dockerfile`** — Installs CPU-only PyTorch + NeMo + transformers, pre-bakes the L0 Bouncer model weights into the image so the container works offline in K8s. Runs as non-root user.
