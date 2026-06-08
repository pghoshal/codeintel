#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE="${CODEINTEL_PRODUCT_NAMESPACE:-}"

find_cmd() {
  local name="$1"
  shift
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
    return 0
  fi
  local candidate
  for candidate in "$@"; do
    if [[ -x "$candidate" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  echo "product-gate: required command '$name' not found" >&2
  exit 2
}

require_env() {
  if [[ -z "${!1:-}" ]]; then
    echo "product-gate: required env '$1' is not set" >&2
    exit 2
  fi
}

DOCKER_BIN="$(find_cmd docker /opt/homebrew/bin/docker /usr/local/bin/docker /Applications/Docker.app/Contents/Resources/bin/docker)"
KIND_BIN="$(find_cmd kind /opt/homebrew/bin/kind /usr/local/bin/kind)"
KUBECTL_BIN="$(find_cmd kubectl /opt/homebrew/bin/kubectl /usr/local/bin/kubectl /Applications/Docker.app/Contents/Resources/bin/kubectl)"
GIT_BIN="$(find_cmd git /opt/homebrew/bin/git /usr/local/bin/git /usr/bin/git)"

if [[ -n "${CODEINTEL_GO_BIN:-}" ]]; then
  GO_BIN="$CODEINTEL_GO_BIN"
elif command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
elif [[ -x "/opt/homebrew/Cellar/go@1.23/1.23.6/libexec/bin/go" ]]; then
  GO_BIN="/opt/homebrew/Cellar/go@1.23/1.23.6/libexec/bin/go"
else
  echo "product-gate: required command 'go' not found; set CODEINTEL_GO_BIN" >&2
  exit 2
fi
export PATH="$(dirname "$GO_BIN"):$(dirname "$DOCKER_BIN"):$(dirname "$KIND_BIN"):$(dirname "$KUBECTL_BIN"):$(dirname "$GIT_BIN"):$PATH"

require_env CODEINTEL_PRODUCT_BASE_URL
require_env CODEINTEL_PRODUCT_NAMESPACE
require_env CODEINTEL_PRODUCT_LIFECYCLE_CMD

if ! "$DOCKER_BIN" info >/dev/null 2>&1; then
  echo "product-gate: Docker daemon is not reachable" >&2
  exit 2
fi

if ! "$KUBECTL_BIN" get namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "product-gate: namespace '$NAMESPACE' is not reachable in the current kubectl context" >&2
  exit 2
fi

if ! "$KIND_BIN" get clusters >/dev/null 2>&1; then
  echo "product-gate: kind is installed but no cluster list is reachable" >&2
  exit 2
fi

ARTIFACT_DIR="${CODEINTEL_PRODUCT_ARTIFACT_DIR:-/tmp/codeintel-product-gate-$(date +%Y%m%d%H%M%S)}"
mkdir -p "$ARTIFACT_DIR"
export CODEINTEL_PRODUCT_ARTIFACT_DIR="$ARTIFACT_DIR"
export CODEINTEL_PRODUCT_LIFECYCLE_REPORT="${CODEINTEL_PRODUCT_LIFECYCLE_REPORT:-$ARTIFACT_DIR/lifecycle-report.md}"
export CODEINTEL_PRODUCT_LIFECYCLE_ENV="${CODEINTEL_PRODUCT_LIFECYCLE_ENV:-$ARTIFACT_DIR/lifecycle.env}"

echo "product-gate: namespace=$NAMESPACE baseURL=$CODEINTEL_PRODUCT_BASE_URL artifactDir=$ARTIFACT_DIR"
cd "$ROOT_DIR"
go test -count=1 ./tests/product_quality

echo "product-gate: running lifecycle command"
bash -lc "$CODEINTEL_PRODUCT_LIFECYCLE_CMD"
if [[ ! -s "$CODEINTEL_PRODUCT_LIFECYCLE_REPORT" ]]; then
  echo "product-gate: lifecycle command did not write CODEINTEL_PRODUCT_LIFECYCLE_REPORT=$CODEINTEL_PRODUCT_LIFECYCLE_REPORT" >&2
  exit 2
fi

if [[ -s "$CODEINTEL_PRODUCT_LIFECYCLE_ENV" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$CODEINTEL_PRODUCT_LIFECYCLE_ENV"
  set +a
fi
require_env CODEINTEL_PRODUCT_API_KEY
require_env CODEINTEL_PRODUCT_ORG_DOMAIN

echo "product-gate: running live real-repo gate for org=$CODEINTEL_PRODUCT_ORG_DOMAIN"
go test -count=1 -tags=realrepo ./tests/product_quality
