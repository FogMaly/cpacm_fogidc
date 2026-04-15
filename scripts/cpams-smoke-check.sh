#!/usr/bin/env bash
set -euo pipefail

CONFIG_FILE="${CONFIG_FILE:-/opt/cli-proxy-api/scripts/model-health.env}"

if [ -f "${CONFIG_FILE}" ]; then
  set -a
  # shellcheck disable=SC1090
  source "${CONFIG_FILE}"
  set +a
fi

BASE_URL="${BASE_URL:-${MANAGEMENT_URL:-http://127.0.0.1:34050}}"
API_KEY="${API_KEY:-${PROXY_API_KEY:-}}"
MGMT_KEY="${MGMT_KEY:-${MANAGEMENT_KEY:-}}"

if [ -z "${API_KEY}" ]; then
  echo "[FAIL] missing API_KEY/PROXY_API_KEY" >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

echo "[INFO] base_url=${BASE_URL}"

check_cpams_status() {
  local channel="$1"
  local out="${tmp_dir}/status_${channel}.json"

  if [ -z "${MGMT_KEY}" ]; then
    echo "[WARN] skip /v0/management/cpams/status?channel=${channel} (missing MGMT_KEY)"
    return 0
  fi

  local code
  code="$(
    curl -sS -o "${out}" -w '%{http_code}' \
      -H "x-management-key: ${MGMT_KEY}" \
      "${BASE_URL}/v0/management/cpams/status?channel=${channel}"
  )"

  if [ "${code}" != "200" ]; then
    echo "[FAIL] cpams status ${channel}: http=${code}, body=$(cat "${out}")" >&2
    exit 1
  fi

  local snapshot_path
  snapshot_path="$(jq -r '.snapshot_path // ""' "${out}" 2>/dev/null || true)"
  local ttl
  ttl="$(jq -r '.token_ttl_seconds // 0' "${out}" 2>/dev/null || true)"
  echo "[PASS] cpams status ${channel}: ttl=${ttl}s snapshot=${snapshot_path}"
}

check_channel_request() {
  local method="$1"
  local path="$2"
  local body="$3"
  local body_expect="$4"
  local max_attempts=3
  local attempt

  for attempt in $(seq 1 "${max_attempts}"); do
    local headers="${tmp_dir}/headers_$(echo "${path}" | tr '/?' '__')_${attempt}.txt"
    local out="${tmp_dir}/body_$(echo "${path}" | tr '/?' '__')_${attempt}.json"

    local code
    code="$(
      curl -sS -D "${headers}" -o "${out}" -w '%{http_code}' \
        -X "${method}" \
        -H "Authorization: Bearer ${API_KEY}" \
        -H 'content-type: application/json' \
        "${BASE_URL}${path}" \
        -d "${body}"
    )"

    if [ "${code}" != "200" ]; then
      if [ "${attempt}" -lt "${max_attempts}" ]; then
        echo "[WARN] ${path}: http=${code}, retry ${attempt}/${max_attempts}" >&2
        sleep 1
        continue
      fi
      echo "[FAIL] ${path}: http=${code}" >&2
      cat "${out}" >&2 || true
      exit 1
    fi

    if rg -qi "^x-cpams-" "${headers}"; then
      echo "[FAIL] ${path}: public response should not expose X-Cpams-* headers" >&2
      cat "${headers}" >&2 || true
      exit 1
    fi

    if [ -n "${body_expect}" ] && ! rg -q "${body_expect}" "${out}"; then
      echo "[FAIL] ${path}: unexpected response body" >&2
      cat "${out}" >&2 || true
      exit 1
    fi

    echo "[PASS] ${path}: attempt=${attempt}"
    return 0
  done
}

check_cpams_status "codex"
check_cpams_status "claude"

check_channel_request \
  "POST" \
  "/cpamc/codex/responses" \
  '{"model":"gpt-5.4","input":"reply with exactly OK","max_output_tokens":16}' \
  '"object":"response"'

check_channel_request \
  "POST" \
  "/cpamc/claude/messages" \
  '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"reply with exactly OK"}],"max_tokens":16}' \
  '"type":"message"'

echo "[OK] cpamc smoke check passed"
