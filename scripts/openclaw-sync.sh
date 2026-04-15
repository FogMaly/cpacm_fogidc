#!/usr/bin/env bash
set -euo pipefail

MODE="push-model-health"
SNAPSHOT_FILE=""
EXTRA_ENV_FILE=""

usage() {
  cat <<'EOF'
Usage:
  openclaw-sync.sh --push-model-health [--file /path/to/model-health.json] [--env /path/to/env]

Environment (can be set in scripts/model-health.env):
  OPENCLAW_BASE_URL               e.g. https://openclaw.example.com
  OPENCLAW_TOKEN                  webhook token
  OPENCLAW_AUTH_MODE              bearer|x-token (default: bearer)
  OPENCLAW_AGENT_PATH             default: /hooks/agent
  OPENCLAW_TIMEOUT_SEC            default: 20
  OPENCLAW_CONNECT_TIMEOUT_SEC    default: 6
  OPENCLAW_RETRY                  default: 2
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --push-model-health)
      MODE="push-model-health"
      shift
      ;;
    --file)
      SNAPSHOT_FILE="${2:-}"
      shift 2
      ;;
    --env)
      EXTRA_ENV_FILE="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

for env_file in \
  /opt/cli-proxy-api/scripts/model-health.env \
  /opt/cli-proxy-api/scripts/openclaw-bridge.env \
  "${EXTRA_ENV_FILE}"; do
  if [ -n "${env_file}" ] && [ -f "${env_file}" ]; then
    set -a
    # shellcheck disable=SC1090
    source "${env_file}"
    set +a
  fi
done

OPENCLAW_BASE_URL="${OPENCLAW_BASE_URL:-}"
OPENCLAW_TOKEN="${OPENCLAW_TOKEN:-}"
OPENCLAW_AUTH_MODE="${OPENCLAW_AUTH_MODE:-bearer}"
OPENCLAW_AGENT_PATH="${OPENCLAW_AGENT_PATH:-/hooks/agent}"
OPENCLAW_TIMEOUT_SEC="${OPENCLAW_TIMEOUT_SEC:-20}"
OPENCLAW_CONNECT_TIMEOUT_SEC="${OPENCLAW_CONNECT_TIMEOUT_SEC:-6}"
OPENCLAW_RETRY="${OPENCLAW_RETRY:-2}"
MODEL_HEALTH_JSON="${MODEL_HEALTH_JSON:-/opt/cli-proxy-api/static/model-health.json}"

if [ "${MODE}" != "push-model-health" ]; then
  echo "unsupported mode: ${MODE}" >&2
  exit 1
fi

if [ -z "${SNAPSHOT_FILE}" ]; then
  if [ -n "${OUT_JSON:-}" ]; then
    SNAPSHOT_FILE="${OUT_JSON}"
  else
    SNAPSHOT_FILE="${MODEL_HEALTH_JSON}"
  fi
fi

if [ -z "${OPENCLAW_BASE_URL}" ] || [ -z "${OPENCLAW_TOKEN}" ]; then
  echo "openclaw sync skipped: OPENCLAW_BASE_URL or OPENCLAW_TOKEN is empty" >&2
  exit 0
fi

if [ ! -s "${SNAPSHOT_FILE}" ]; then
  echo "openclaw sync failed: snapshot file missing -> ${SNAPSHOT_FILE}" >&2
  exit 1
fi

trim_slash_base="${OPENCLAW_BASE_URL%/}"
if [[ "${OPENCLAW_AGENT_PATH}" != /* ]]; then
  OPENCLAW_AGENT_PATH="/${OPENCLAW_AGENT_PATH}"
fi
target_url="${trim_slash_base}${OPENCLAW_AGENT_PATH}"

payload="$(
  jq -cn \
    --slurpfile snapshot "${SNAPSHOT_FILE}" \
    '{
      source: "cliproxyapi",
      event: "model_health_snapshot",
      timestamp: (now | strftime("%Y-%m-%dT%H:%M:%SZ")),
      payload: ($snapshot[0] // {})
    }'
)"

do_post() {
  local mode="$1"
  local response http body
  local -a auth_header
  auth_header=()

  if [ "${mode}" = "x-token" ]; then
    auth_header=(-H "x-openclaw-token: ${OPENCLAW_TOKEN}")
  else
    auth_header=(-H "Authorization: Bearer ${OPENCLAW_TOKEN}")
  fi

  response="$(
    curl -sS --connect-timeout "${OPENCLAW_CONNECT_TIMEOUT_SEC}" -m "${OPENCLAW_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
      "${target_url}" \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json' \
      "${auth_header[@]}" \
      -d "${payload}" || true
  )"
  http="$(printf '%s' "${response}" | sed -n 's/.*__HTTP__\([0-9][0-9][0-9]\)$/\1/p')"
  body="$(printf '%s' "${response}" | sed 's/__HTTP__[0-9][0-9][0-9]$//')"
  [ -n "${http}" ] || http="000"
  printf '%s\n%s\n' "${http}" "${body}"
}

primary_mode="$(printf '%s' "${OPENCLAW_AUTH_MODE}" | tr '[:upper:]' '[:lower:]')"
if [ "${primary_mode}" != "x-token" ]; then
  primary_mode="bearer"
fi
secondary_mode="x-token"
if [ "${primary_mode}" = "x-token" ]; then
  secondary_mode="bearer"
fi

attempt_max=$((OPENCLAW_RETRY + 1))
[ "${attempt_max}" -gt 0 ] || attempt_max=1

try_mode() {
  local mode="$1"
  local n http body
  n=1
  while [ "${n}" -le "${attempt_max}" ]; do
    mapfile -t result < <(do_post "${mode}")
    http="${result[0]:-000}"
    body="${result[1]:-}"
    if [ "${http}" -ge 200 ] && [ "${http}" -lt 300 ]; then
      echo "openclaw sync ok: mode=${mode} http=${http}"
      return 0
    fi
    if [ "${http}" = "401" ] || [ "${http}" = "403" ]; then
      echo "openclaw sync auth rejected: mode=${mode} http=${http}" >&2
      return 2
    fi
    echo "openclaw sync retry: mode=${mode} http=${http} attempt=${n}/${attempt_max}" >&2
    if [ -n "${body}" ]; then
      printf '%s\n' "${body}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-200 >&2 || true
    fi
    n=$((n + 1))
    sleep 1
  done
  return 1
}

if try_mode "${primary_mode}"; then
  exit 0
fi
primary_rc=$?
if [ "${primary_rc}" -eq 2 ]; then
  if try_mode "${secondary_mode}"; then
    exit 0
  fi
fi

echo "openclaw sync failed: url=${target_url}" >&2
exit 1
