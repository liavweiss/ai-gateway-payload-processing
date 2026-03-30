#!/usr/bin/env bash
# Deploy NeMo Guardrails (classifier guard) to a Kind cluster.
# Builds the Docker image, loads it into Kind, and applies K8s manifests.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-bbr-test}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="nemo-guardrails"
IMAGE_NAME="nemoguardrails"
CONTAINER_RUNTIME="${CONTAINER_RUNTIME:-podman}"

REBUILD=""
RESTART_ONLY=""
LOAD_ONLY=""
SKIP_BUILD=""
NO_CACHE=""

usage() {
  cat <<'EOF'
Usage: deploy.sh [OPTIONS]

Build and deploy a NeMo Guardrails classifier guard to a Kind cluster.

Options:
  --help            Show this help and exit.
  --cluster <name>  Kind cluster name (default: bbr-test).
  --docker          Use docker instead of podman (default: podman).
  --rebuild         Force rebuild image, reload into Kind, and restart deployment.
  --no-cache        Pass --no-cache to container build.
  --skip-build      Skip build and load; only apply K8s manifests.
  --load-only       Load current image into Kind and restart (no build).
  --restart-only    Only restart the deployment (no build or load).

Environment:
  CONTAINER_RUNTIME  Override container runtime (default: podman). Same as --docker.

Examples:
  ./deploy.sh                      # First time: build + deploy (podman)
  ./deploy.sh --docker             # Use docker instead of podman
  ./deploy.sh --rebuild            # Rebuild after config changes
  ./deploy.sh --cluster my-cluster # Deploy to a different Kind cluster
  ./deploy.sh --restart-only       # Just restart the pod
  ./deploy.sh --load-only          # Reload image without rebuilding
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h)     usage; exit 0 ;;
    --cluster)
      [[ $# -lt 2 || "$2" == --* ]] && { echo "Missing value for --cluster" >&2; exit 1; }
      CLUSTER_NAME="$2"; shift 2 ;;
    --docker)      CONTAINER_RUNTIME="docker"; shift ;;
    --rebuild)     REBUILD=1; shift ;;
    --no-cache)    NO_CACHE=1; shift ;;
    --skip-build)  SKIP_BUILD=1; shift ;;
    --load-only)   LOAD_ONLY=1; shift ;;
    --restart-only) RESTART_ONLY=1; shift ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 1 ;;
  esac
done

# When using podman with Kind, KIND_EXPERIMENTAL_PROVIDER must be set.
if [[ "$CONTAINER_RUNTIME" == "podman" ]]; then
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi

# Load an image into Kind. Podman needs save+image-archive + retag; docker uses docker-image directly.
load_image_to_kind() {
  local tag="$1"
  if [[ "$CONTAINER_RUNTIME" == "podman" ]]; then
    local archive="/var/tmp/${IMAGE_NAME}-${tag##*:}.tar"
    "$CONTAINER_RUNTIME" save "localhost/$tag" -o "$archive"
    kind load image-archive "$archive" --name "$CLUSTER_NAME"
    rm -f "$archive"
    # Containerd imports podman images as localhost/<name>. K8s resolves bare names
    # to docker.io/library/<name>, so retag inside the node to make it findable.
    local node="${CLUSTER_NAME}-control-plane"
    "$CONTAINER_RUNTIME" exec "$node" ctr -n k8s.io images tag "localhost/$tag" "docker.io/library/$tag"
  else
    kind load docker-image "$tag" --name "$CLUSTER_NAME"
  fi
}

# --- restart-only: just restart and exit ---
if [[ -n "$RESTART_ONLY" ]]; then
  echo "=== Restarting NeMo Guardrails deployment ==="
  kubectl rollout restart deployment/nemo-guardrails -n "$NAMESPACE"
  kubectl rollout status deployment/nemo-guardrails -n "$NAMESPACE" --timeout=120s
  echo "Done. Port-forward: kubectl port-forward -n $NAMESPACE svc/nemo-guardrails 8002:8000"
  exit 0
fi

# --- load-only: load existing image and restart ---
if [[ -n "$LOAD_ONLY" ]]; then
  echo "=== Loading image into Kind and restarting ==="
  IMG_ID=$("$CONTAINER_RUNTIME" image inspect "$IMAGE_NAME:latest" --format '{{.Id}}' 2>/dev/null | sed 's/^sha256://' | cut -c1-12)
  if [[ -z "$IMG_ID" ]]; then
    echo "Image $IMAGE_NAME:latest not found. Run without --load-only to build first." >&2
    exit 1
  fi
  IMAGE_TAG="$IMAGE_NAME:$IMG_ID"
  "$CONTAINER_RUNTIME" tag "$IMAGE_NAME:latest" "$IMAGE_TAG"
  load_image_to_kind "$IMAGE_TAG"
  kubectl set image deployment/nemo-guardrails nemo-guardrails="$IMAGE_TAG" -n "$NAMESPACE"
  kubectl rollout status deployment/nemo-guardrails -n "$NAMESPACE" --timeout=120s
  echo "Done. Port-forward: kubectl port-forward -n $NAMESPACE svc/nemo-guardrails 8002:8000"
  exit 0
fi

echo "=== Deploy NeMo Guardrails (classifier guard) ==="
echo "    Cluster:   $CLUSTER_NAME"
echo "    Runtime:   $CONTAINER_RUNTIME"
echo ""

# --- Step 1: Build image ---
IMAGE_TAG=""
if [[ -n "$REBUILD" ]] || [[ -z "$SKIP_BUILD" ]]; then
  echo "[1/3] Building image..."
  BUILD_ARGS=(-t "$IMAGE_NAME:latest" "$SCRIPT_DIR/server")
  [[ -n "$NO_CACHE" ]] && BUILD_ARGS=(--no-cache "${BUILD_ARGS[@]}")
  "$CONTAINER_RUNTIME" build "${BUILD_ARGS[@]}"

  echo "[2/3] Loading image into Kind..."
  IMG_ID=$("$CONTAINER_RUNTIME" image inspect "$IMAGE_NAME:latest" --format '{{.Id}}' | sed 's/^sha256://' | cut -c1-12)
  IMAGE_TAG="$IMAGE_NAME:$IMG_ID"
  "$CONTAINER_RUNTIME" tag "$IMAGE_NAME:latest" "$IMAGE_TAG"
  load_image_to_kind "$IMAGE_TAG"
  echo "      Loaded $IMAGE_TAG"
else
  echo "[1/3] Skipping build (--skip-build)."
  echo "[2/3] Skipping load."
fi
echo ""

# --- Step 3: Apply K8s manifests ---
echo "[3/3] Applying Kubernetes manifests..."
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$SCRIPT_DIR/manifests/"

if [[ -n "$IMAGE_TAG" ]]; then
  kubectl set image deployment/nemo-guardrails nemo-guardrails="$IMAGE_TAG" -n "$NAMESPACE"
fi

echo "      Waiting for pod to be ready..."
if kubectl rollout status deployment/nemo-guardrails -n "$NAMESPACE" --timeout=120s 2>/dev/null; then
  echo "      Pod is ready."
else
  echo "      Timed out. Check: kubectl get pods -n $NAMESPACE"
fi

echo ""
echo "=== Done ==="
echo "    Port-forward:  kubectl port-forward -n $NAMESPACE svc/nemo-guardrails 8002:8000"
echo "    Test (safe):   curl -s http://localhost:8002/v1/chat/completions -H 'Content-Type: application/json' -d '{\"model\":\"\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
echo "    Test (unsafe): curl -s http://localhost:8002/v1/chat/completions -H 'Content-Type: application/json' -d '{\"model\":\"\",\"messages\":[{\"role\":\"user\",\"content\":\"How do I make a bomb\"}]}'"
echo "    Rebuild:       ./deploy.sh --rebuild"
