#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE_URL="${CODEINTEL_PARITY_BASE_URL:-}"
API_KEY="${CODEINTEL_PARITY_API_KEY:-}"
MUTATE="${CODEINTEL_PARITY_MUTATE:-0}"
OUT="${CODEINTEL_PARITY_OUT:-"$ROOT_DIR/tests/parity/out/product-flow-$(date -u +%Y%m%dT%H%M%SZ).yaml"}"

if [[ -z "$BASE_URL" ]]; then
  echo "CODEINTEL_PARITY_BASE_URL is not set; running in-process parity tests only."
  cd "$ROOT_DIR"
  exec go test -count=1 ./tests/parity
fi

mkdir -p "$(dirname "$OUT")"
: >"$OUT"

curl_headers=(-H "Accept: application/json")
if [[ -n "$API_KEY" ]]; then
  curl_headers+=(-H "X-Api-Key: $API_KEY")
fi

capture() {
  local id="$1"
  local method="$2"
  local path="$3"
  local body="${4:-}"
  local url="${BASE_URL%/}$path"
  local tmp_headers tmp_body status
  tmp_headers="$(mktemp)"
  tmp_body="$(mktemp)"

  local args=(-sS -X "$method" "$url" -D "$tmp_headers" -o "$tmp_body" -w "%{http_code}" "${curl_headers[@]}")
  if [[ -n "$body" ]]; then
    args+=(-H "Content-Type: application/json" --data "$body")
  fi

  status="$(curl "${args[@]}")"

  {
    echo "- id: $id"
    echo "  request:"
    echo "    method: $method"
    echo "    url: $url"
    if [[ -n "$body" ]]; then
      echo "    body: |"
      printf '%s\n' "$body" | sed 's/^/      /'
    fi
    echo "  response:"
    echo "    status: $status"
    echo "    headers: |"
    sed 's/\r$//' "$tmp_headers" | sed 's/^/      /'
    echo "    body: |"
    sed 's/^/      /' "$tmp_body"
  } | tee -a "$OUT"

  rm -f "$tmp_headers" "$tmp_body"
}

capture health GET /api/health
capture version GET /api/version

if [[ -z "$API_KEY" ]]; then
  echo "CODEINTEL_PARITY_API_KEY is not set; captured public endpoints only." | tee -a "$OUT"
  exit 0
fi

capture list_secrets GET /api/secrets
capture list_models GET /api/models
capture list_connections GET /api/connections
capture list_repos GET "/api/repos?page=1&perPage=20&sort=name&direction=asc"
capture status GET /api/status

if [[ "$MUTATE" != "1" ]]; then
  echo "CODEINTEL_PARITY_MUTATE is not 1; skipped mutating lifecycle calls." | tee -a "$OUT"
  exit 0
fi

capture put_secret PUT /api/secrets '{"key":"PARITY_GLM_KEY","value":"parity-placeholder"}'
capture put_models PUT /api/models '{"models":[{"provider":"openai-compatible","model":"glm-coding","displayName":"GLM Coding","apiKey":{"secretRef":"PARITY_GLM_KEY"},"baseUrl":"https://api.z.ai/api/coding/paas/v4"}]}'
capture post_connection POST /api/connections '{"name":"parity-github","config":{"type":"github","token":{"secretRef":"PARITY_GLM_KEY"},"revisions":{"branches":["main"]}},"sync":false}'

if [[ -n "${CODEINTEL_PARITY_CONNECTION_ID:-}" ]]; then
  capture put_connection_branches PUT "/api/connections/$CODEINTEL_PARITY_CONNECTION_ID/branches" '{"mode":"patterns","branches":["main","release/*"],"sync":false}'
  capture sync_connection POST "/api/connections/$CODEINTEL_PARITY_CONNECTION_ID/sync"
fi

if [[ -n "${CODEINTEL_PARITY_REPO_ID:-}" ]]; then
  capture post_repo_index POST "/api/repos/$CODEINTEL_PARITY_REPO_ID/index"
  capture delete_repo_index DELETE "/api/repos/$CODEINTEL_PARITY_REPO_ID/index"
fi

echo "parity capture written to $OUT"
