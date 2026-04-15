#!/usr/bin/env bash
set -euo pipefail

CONFIG_FILE="${CONFIG_FILE:-/opt/cli-proxy-api/scripts/model-health.env}"
MODEL_HEALTH_SKIP_CONFIG_SOURCE="${MODEL_HEALTH_SKIP_CONFIG_SOURCE:-false}"
case "$(printf '%s' "${MODEL_HEALTH_SKIP_CONFIG_SOURCE}" | tr '[:upper:]' '[:lower:]')" in
  1|true|yes|on) ;;
  *)
    if [ -f "${CONFIG_FILE}" ]; then
      set -a
      # shellcheck disable=SC1090
      source "${CONFIG_FILE}"
      set +a
    fi
    ;;
esac

MANAGEMENT_URL="${MANAGEMENT_URL:-http://127.0.0.1:34050}"
MANAGEMENT_KEY="${MANAGEMENT_KEY:-}"
PROVIDER_PREFIX="${PROVIDER_PREFIX:-yunyi-codex}"
PROVIDER_NAME="${PROVIDER_NAME:-}"
OUT_JSON="${OUT_JSON:-/opt/cli-proxy-api/static/model-health.json}"
OUT_AUTH_JSON="${OUT_AUTH_JSON:-/opt/cli-proxy-api/auths/model-health.json}"
CONFIG_YAML_PATH="${CONFIG_YAML_PATH:-/opt/cli-proxy-api/config.yaml}"
CHECK_INTERVAL_SEC="${CHECK_INTERVAL_SEC:-3600}"
PROBE_RETRY_COUNT="${PROBE_RETRY_COUNT:-}"
PROBE_MAX_RETRY_WAIT_SEC="${PROBE_MAX_RETRY_WAIT_SEC:-0}"
HTTP_TIMEOUT_SEC="${HTTP_TIMEOUT_SEC:-45}"
CURL_CONNECT_TIMEOUT_SEC="${CURL_CONNECT_TIMEOUT_SEC:-8}"
MAX_MODELS_PER_RUN="${MAX_MODELS_PER_RUN:-0}"
CHECK_MODE="${CHECK_MODE:-text_only}"
CHECK_VIA_PROXY="${CHECK_VIA_PROXY:-false}"
PROXY_RESPONSES_URL="${PROXY_RESPONSES_URL:-${MANAGEMENT_URL}/v1/responses}"
PROXY_CLAUDE_MESSAGES_URL="${PROXY_CLAUDE_MESSAGES_URL:-${MANAGEMENT_URL}/v1/messages}"
PROXY_API_KEY="${PROXY_API_KEY:-}"
IMAGE_URL="${IMAGE_URL:-https://upload.wikimedia.org/wikipedia/commons/thumb/3/3f/Fronalpstock_big.jpg/640px-Fronalpstock_big.jpg}"
TASK_TEXT="${TASK_TEXT:-请先描述图片，再完成任务：1) 一句话总结“AI 正在改变软件开发流程”；2) 计算 37*19；3) 给出 Python fib(n)；4) 输出 JSON，包含 summary/math_result/code_language/reliability_note。}"
TEXT_FALLBACK_TASK_TEXT="${TEXT_FALLBACK_TASK_TEXT:-请直接完成：1)一句话总结AI改变软件开发流程；2)计算37*19；3)给一个Python fib(n)函数；4)最后输出JSON字段summary/math_result/code_language/reliability_note。}"
TEXT_MAX_TOKENS="${TEXT_MAX_TOKENS:-64}"
MULTIMODAL_MAX_TOKENS="${MULTIMODAL_MAX_TOKENS:-180}"
EE_MODEL_RETRY_MAX="${EE_MODEL_RETRY_MAX:-3}"
EE_MODEL_RETRY_SLEEP_SEC="${EE_MODEL_RETRY_SLEEP_SEC:-1}"
DIRECT_MODEL_RETRY_MAX="${DIRECT_MODEL_RETRY_MAX:-3}"
DIRECT_MODEL_RETRY_SLEEP_SEC="${DIRECT_MODEL_RETRY_SLEEP_SEC:-1}"
OPENCLAW_SYNC_ENABLED="${OPENCLAW_SYNC_ENABLED:-false}"
OPENCLAW_SYNC_SCRIPT="${OPENCLAW_SYNC_SCRIPT:-/opt/cli-proxy-api/scripts/openclaw-sync.sh}"
LOCK_FILE="${LOCK_FILE:-/opt/cli-proxy-api/logs/model-health-check.lock}"
DAILY_QUOTA_COOLDOWN_ENABLED="${DAILY_QUOTA_COOLDOWN_ENABLED:-true}"
DAILY_QUOTA_RESUME_TZ="${DAILY_QUOTA_RESUME_TZ:-Asia/Shanghai}"
DAILY_QUOTA_RESUME_HOUR="${DAILY_QUOTA_RESUME_HOUR:-0}"
DAILY_QUOTA_RESUME_MINUTE="${DAILY_QUOTA_RESUME_MINUTE:-0}"
DAILY_QUOTA_LOCKS_FILE="${DAILY_QUOTA_LOCKS_FILE:-/opt/cli-proxy-api/static/model-health-quota-locks.json}"
MODEL_HEALTH_RETAINED_MODELS="${MODEL_HEALTH_RETAINED_MODELS:-}"
MODEL_HEALTH_RETAINED_MODELS_FILE="${MODEL_HEALTH_RETAINED_MODELS_FILE:-}"

normalize_proxy_responses_probe_url() {
  local endpoint="$1"
  case "${endpoint}" in
    */api/provider/openai/v1/responses)
      printf '%s/v1/responses\n' "${endpoint%/api/provider/openai/v1/responses}"
      ;;
    *)
      printf '%s\n' "${endpoint}"
      ;;
  esac
}

normalize_proxy_claude_messages_probe_url() {
  local endpoint="$1"
  case "${endpoint}" in
    */api/provider/anthropic/v1/messages)
      printf '%s/v1/messages\n' "${endpoint%/api/provider/anthropic/v1/messages}"
      ;;
    *)
      printf '%s\n' "${endpoint}"
      ;;
  esac
}

PROXY_RESPONSES_URL="$(normalize_proxy_responses_probe_url "${PROXY_RESPONSES_URL}")"
PROXY_CLAUDE_MESSAGES_URL="$(normalize_proxy_claude_messages_probe_url "${PROXY_CLAUDE_MESSAGES_URL}")"

# Global single-run lock: keep probe concurrency strictly below 3 (default is 1).
mkdir -p "$(dirname "${LOCK_FILE}")"
exec 9>"${LOCK_FILE}"
if ! flock -n 9; then
  echo "model-health monitor is already running, skip overlapping run" >&2
  exit 0
fi

mkdir -p "$(dirname "${OUT_JSON}")"
if [ -n "${OUT_AUTH_JSON}" ]; then
  mkdir -p "$(dirname "${OUT_AUTH_JSON}")"
fi

now_iso() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

normalize_model_lines() {
  tr ',;' '\n\n' | sed 's/\r$//' | sed '/^[[:space:]]*#/d' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed '/^$/d'
}

protocol_list_has_responses() {
  local raw token
  raw="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
  for token in $(printf '%s' "${raw}" | tr ',;|' '   '); do
    case "$(printf '%s' "${token}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')" in
      responses|/responses|responses/compact|/responses/compact)
        return 0
        ;;
    esac
  done
  return 1
}

protocol_list_has_claude_messages() {
  local raw token
  raw="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
  for token in $(printf '%s' "${raw}" | tr ',;|' '   '); do
    case "$(printf '%s' "${token}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')" in
      messages|/messages|claude-messages|claude/messages|anthropic/messages)
        return 0
        ;;
    esac
  done
  return 1
}

probe_codex_force_chat_bridge() {
  local value normalized
  for value in "$@"; do
    normalized="$(printf '%s' "${value:-}" | tr '[:upper:]' '[:lower:]' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    [ -n "${normalized}" ] || continue
    case "${normalized}" in
      nowcoding|*nowcoding.ai*)
        return 0
        ;;
    esac
  done
  return 1
}

probe_codex_prefers_responses() {
  local provider_kind="$1"
  local supported_protocols="$2"
  shift 2

  if [ "${provider_kind}" != "codex" ]; then
    return 1
  fi
  if [ -n "${supported_protocols}" ]; then
    protocol_list_has_responses "${supported_protocols}"
    return $?
  fi
  if probe_codex_force_chat_bridge "$@"; then
    return 1
  fi
  return 0
}

probe_codex_prefers_claude_messages() {
  local provider_kind="$1"
  local supported_protocols="$2"

  if [ "${provider_kind}" != "codex" ]; then
    return 1
  fi
  if [ -z "${supported_protocols}" ]; then
    return 1
  fi
  protocol_list_has_claude_messages "${supported_protocols}"
}

provider_prefix_env_key() {
  printf '%s' "${1:-}" | tr '[:lower:]' '[:upper:]' | sed 's/[^A-Z0-9]/_/g'
}

retained_probe_patterns() {
  local prefix_key scoped_var scoped_raw
  prefix_key="$(provider_prefix_env_key "${PROVIDER_PREFIX}")"
  scoped_var="MODEL_HEALTH_RETAINED_MODELS_${prefix_key}"
  scoped_raw="${!scoped_var-}"

  {
    if [ -n "${scoped_raw}" ]; then
      printf '%s\n' "${scoped_raw}"
    fi
    if [ -n "${MODEL_HEALTH_RETAINED_MODELS}" ]; then
      printf '%s\n' "${MODEL_HEALTH_RETAINED_MODELS}"
    fi
    if [ -n "${MODEL_HEALTH_RETAINED_MODELS_FILE}" ] && [ -f "${MODEL_HEALTH_RETAINED_MODELS_FILE}" ]; then
      cat "${MODEL_HEALTH_RETAINED_MODELS_FILE}"
    fi
  } | normalize_model_lines | awk '!seen[$0]++'
}

model_matches_any_pattern() {
  local model="$1"
  shift || true
  local pattern
  for pattern in "$@"; do
    [ -n "${pattern}" ] || continue
    case "${model}" in
      ${pattern}) return 0 ;;
    esac
  done
  return 1
}

provider_disabled_by_excluded_models() {
  local pattern normalized
  for pattern in "$@"; do
    normalized="$(printf '%s' "${pattern:-}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    [ -n "${normalized}" ] || continue
    if [ "${normalized}" = "*" ]; then
      return 0
    fi
  done
  return 1
}

filter_probe_models() {
  local excluded=()
  local candidates=()
  local seen_delim=0
  local item
  for item in "$@"; do
    if [ "${seen_delim}" -eq 0 ] && [ "${item}" = "--" ]; then
      seen_delim=1
      continue
    fi
    if [ "${seen_delim}" -eq 0 ]; then
      excluded+=("${item}")
    else
      candidates+=("${item}")
    fi
  done

  local retained=()
  mapfile -t retained < <(retained_probe_patterns)

  local filtered=()
  local model
  for model in "${candidates[@]}"; do
    [ -n "${model}" ] || continue
    if [ "${#retained[@]}" -gt 0 ] && ! model_matches_any_pattern "${model}" "${retained[@]}"; then
      continue
    fi
    if [ "${#excluded[@]}" -gt 0 ] && model_matches_any_pattern "${model}" "${excluded[@]}"; then
      continue
    fi
    filtered+=("${model}")
  done

  if [ "${#filtered[@]}" -eq 0 ]; then
    return 0
  fi
  printf '%s\n' "${filtered[@]}" | normalize_model_lines | awk '!seen[$0]++'
}

daily_quota_enabled() {
  case "$(printf '%s' "${DAILY_QUOTA_COOLDOWN_ENABLED}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

daily_quota_locks_init() {
  if ! daily_quota_enabled; then
    return 0
  fi
  mkdir -p "$(dirname "${DAILY_QUOTA_LOCKS_FILE}")"
  if [ ! -f "${DAILY_QUOTA_LOCKS_FILE}" ]; then
    echo "{}" >"${DAILY_QUOTA_LOCKS_FILE}"
    return 0
  fi
  if ! jq -e 'type == "object"' "${DAILY_QUOTA_LOCKS_FILE}" >/dev/null 2>&1; then
    echo "{}" >"${DAILY_QUOTA_LOCKS_FILE}"
  fi
}

daily_quota_next_resume_utc() {
  local tz target_day target_epoch
  tz="${DAILY_QUOTA_RESUME_TZ}"
  target_day="$(TZ="${tz}" date -d 'tomorrow' +%Y-%m-%d 2>/dev/null || true)"
  if [ -z "${target_day}" ]; then
    # Fallback: next China midnight in UTC.
    local now_epoch china_today china_tomorrow
    now_epoch="$(date -u +%s)"
    china_today="$(date -u -d "@$((now_epoch + 8 * 3600))" +%Y-%m-%d)"
    china_tomorrow="$(date -u -d "${china_today} +1 day" +%Y-%m-%d)"
    date -u -d "${china_tomorrow} 00:00:00 +0800" +"%Y-%m-%dT%H:%M:%SZ"
    return 0
  fi
  target_epoch="$(TZ="${tz}" date -d "${target_day} ${DAILY_QUOTA_RESUME_HOUR}:${DAILY_QUOTA_RESUME_MINUTE}:00" +%s 2>/dev/null || true)"
  if [ -z "${target_epoch}" ] || ! [[ "${target_epoch}" =~ ^[0-9]+$ ]]; then
    local now_epoch china_today china_tomorrow
    now_epoch="$(date -u +%s)"
    china_today="$(date -u -d "@$((now_epoch + 8 * 3600))" +%Y-%m-%d)"
    china_tomorrow="$(date -u -d "${china_today} +1 day" +%Y-%m-%d)"
    date -u -d "${china_tomorrow} 00:00:00 +0800" +"%Y-%m-%dT%H:%M:%SZ"
    return 0
  fi
  date -u -d "@${target_epoch}" +"%Y-%m-%dT%H:%M:%SZ"
}

daily_quota_set_block() {
  local prefix blocked_until reason ts tmp_file
  prefix="$1"
  blocked_until="$2"
  reason="$3"
  ts="$4"
  daily_quota_locks_init
  tmp_file="$(mktemp)"
  jq \
    --arg p "${prefix}" \
    --arg blocked_until "${blocked_until}" \
    --arg reason "${reason}" \
    --arg set_at "${ts}" \
    '.[$p] = {blocked_until: $blocked_until, reason: $reason, set_at: $set_at}' \
    "${DAILY_QUOTA_LOCKS_FILE}" >"${tmp_file}" 2>/dev/null || echo "{}" >"${tmp_file}"
  mv "${tmp_file}" "${DAILY_QUOTA_LOCKS_FILE}"
}

daily_quota_get_blocked_until() {
  local prefix
  prefix="$1"
  if ! daily_quota_enabled; then
    return 0
  fi
  daily_quota_locks_init
  jq -r --arg p "${prefix}" '.[$p].blocked_until // empty' "${DAILY_QUOTA_LOCKS_FILE}" 2>/dev/null || true
}

daily_quota_is_blocked() {
  local prefix blocked_until now_epoch blocked_epoch tmp_file
  prefix="$1"
  blocked_until="$(daily_quota_get_blocked_until "${prefix}")"
  if [ -z "${blocked_until}" ]; then
    return 1
  fi
  now_epoch="$(date -u +%s)"
  blocked_epoch="$(date -u -d "${blocked_until}" +%s 2>/dev/null || echo 0)"
  if [ "${blocked_epoch}" -gt "${now_epoch}" ]; then
    return 0
  fi
  # Auto-clean expired lock.
  daily_quota_locks_init
  tmp_file="$(mktemp)"
  jq --arg p "${prefix}" 'del(.[$p])' "${DAILY_QUOTA_LOCKS_FILE}" >"${tmp_file}" 2>/dev/null || echo "{}" >"${tmp_file}"
  mv "${tmp_file}" "${DAILY_QUOTA_LOCKS_FILE}"
  return 1
}

is_daily_quota_exhausted_error() {
  local http err body text
  http="$1"
  err="$2"
  body="$3"
  text="$(printf '%s %s' "${err}" "${body}" | tr '[:upper:]' '[:lower:]')"
  if [ "${http}" != "402" ] && [ "${http}" != "403" ]; then
    return 1
  fi
  if printf '%s' "${text}" | rg -qi 'daily_limit_reached|daily spending limit reached|quota will reset tomorrow|daily quota'; then
    return 0
  fi
  return 1
}

mirror_snapshot_for_dashboard() {
  if [ -z "${OUT_AUTH_JSON}" ]; then
    return 0
  fi
  if [ -f "${OUT_JSON}" ]; then
    cp "${OUT_JSON}" "${OUT_AUTH_JSON}" 2>/dev/null || true
  fi
}

notify_openclaw() {
  local enabled
  enabled="$(printf '%s' "${OPENCLAW_SYNC_ENABLED}" | tr '[:upper:]' '[:lower:]')"
  case "${enabled}" in
    1|true|yes|on) ;;
    *) return 0 ;;
  esac
  if [ ! -x "${OPENCLAW_SYNC_SCRIPT}" ]; then
    return 0
  fi
  "${OPENCLAW_SYNC_SCRIPT}" --push-model-health --file "${OUT_JSON}" || true
}

write_empty_snapshot() {
  local reason="$1"
  local base_url="${2:-}"
  local supported_protocols="${3:-}"
  local source_mode="${4:-}"
  local disabled="${5:-false}"
  local ts
  local snapshot_provider
  ts="$(now_iso)"
  snapshot_provider="${PROVIDER_NAME:-${PROVIDER_PREFIX}}"
  jq -n \
    --arg updated_at "${ts}" \
    --arg provider_prefix "${snapshot_provider}" \
    --arg base_url "${base_url}" \
    --arg supported_protocols "${supported_protocols}" \
    --arg source_mode "${source_mode}" \
    --arg reason "${reason}" \
    --argjson disabled "${disabled}" \
    --argjson interval "${CHECK_INTERVAL_SEC}" \
    '{
      updated_at: $updated_at,
      provider_prefix: $provider_prefix,
      base_url: $base_url,
      supported_protocols: $supported_protocols,
      source_mode: $source_mode,
      disabled: $disabled,
      interval_sec: $interval,
      summary: {total: 0, green: 0, yellow: 0, red: 0},
      error: $reason,
      models: []
    }' >"${OUT_JSON}.tmp"
  mv "${OUT_JSON}.tmp" "${OUT_JSON}"
  mirror_snapshot_for_dashboard
  notify_openclaw
}

write_error_snapshot() {
  local reason="$1"
  if [ -f "${OUT_JSON}" ] && jq -e '.models | type == "array" and length > 0' "${OUT_JSON}" >/dev/null 2>&1; then
    echo "model-health monitor warning: ${reason}, keep previous snapshot" >&2
    mirror_snapshot_for_dashboard
    notify_openclaw
    return 0
  fi
  write_empty_snapshot "${reason}"
}

pick_provider_entry_from_payloads() {
  local codex_payload="${1:-{}}"
  local compat_payload="${2:-{}}"
  local claude_payload="${3:-{}}"
  jq -nc \
    --arg p "${PROVIDER_PREFIX}" \
    --arg provider_name "${PROVIDER_NAME}" \
    --arg codex "${codex_payload}" \
    --arg compat "${compat_payload}" \
    --arg claude "${claude_payload}" '
    def parse_json($raw):
      (try ($raw | fromjson) catch {});
    def normalize_codex_entry:
      {
        "api-key": (."api-key" // ""),
        "base-url": (."base-url" // ""),
        "models": (.models // []),
        "excluded-models": (."excluded-models" // []),
        "supported-protocols": (."supported-protocols" // ""),
        "headers": (.headers // {}),
        "prefix": (.prefix // ""),
        "provider-kind": "codex"
      };
    def normalize_compat_entry:
      {
        "name": (.name // ""),
        "api-key": ((."api-key-entries" // [] | map(."api-key" // empty) | map(select(length > 0)) | first) // ""),
        "base-url": (."base-url" // ""),
        "models": (.models // []),
        "excluded-models": (."excluded-models" // []),
        "headers": (.headers // {}),
        "prefix": (.prefix // ""),
        "provider-kind": "openai-compatibility"
      };
    def normalize_claude_entry:
      {
        "api-key": (."api-key" // ""),
        "base-url": (."base-url" // ""),
        "models": (.models // []),
        "excluded-models": (."excluded-models" // []),
        "headers": (.headers // {}),
        "prefix": (.prefix // ""),
        "provider-kind": "claude"
      };
    (
      ((parse_json($codex)["codex-api-key"] // []) | map(normalize_codex_entry))
      +
      ((parse_json($compat)["openai-compatibility"] // []) | map(normalize_compat_entry))
      +
      ((parse_json($claude)["claude-api-key"] // []) | map(normalize_claude_entry))
    )
    | map(
        .name = (.name | tostring)
        | ."api-key" = (."api-key" | tostring)
        | ."base-url" = (."base-url" | tostring)
        | .prefix = (.prefix | tostring)
      )
    | map(select(."api-key" != "" and ."base-url" != ""))
    | (
        if ($provider_name | length) > 0 then
          ([ .[] | select(."provider-kind" == "openai-compatibility" and .name == $provider_name) ] | first)
        else
          null
        end
      )
      // ([ .[] | select(.prefix == $p) ] | first)
      // (
        if ($provider_name | length) == 0 and ($p | length) == 0 then
          first
        else
          null
        end
      )
      // null
  '
}

pick_provider_entry_from_config() {
  local config_path="$1"
  ruby -rjson -ryaml -e '
    cfg = YAML.load_file(ARGV[0]) || {}
    provider_name = String(ARGV[2] || "")
    codex = cfg["codex-api-key"]
    codex = [] unless codex.is_a?(Array)
    codex = codex.select { |x| x.is_a?(Hash) }.map do |x|
      {
        "api-key" => String(x["api-key"] || ""),
        "base-url" => String(x["base-url"] || ""),
        "models" => x["models"].is_a?(Array) ? x["models"] : [],
        "excluded-models" => x["excluded-models"].is_a?(Array) ? x["excluded-models"] : [],
        "supported-protocols" => String(x["supported-protocols"] || ""),
        "headers" => x["headers"].is_a?(Hash) ? x["headers"] : {},
        "prefix" => String(x["prefix"] || ""),
        "provider-kind" => "codex"
      }
    end

    compat = cfg["openai-compatibility"]
    compat = [] unless compat.is_a?(Array)
    compat = compat.select { |x| x.is_a?(Hash) }.map do |x|
      key_entries = x["api-key-entries"]
      key_entries = [] unless key_entries.is_a?(Array)
      api_key = key_entries.find { |k| k.is_a?(Hash) && String(k["api-key"]).strip != "" }
      {
        "name" => String(x["name"] || ""),
        "api-key" => String(api_key && api_key["api-key"] || ""),
        "base-url" => String(x["base-url"] || ""),
        "models" => x["models"].is_a?(Array) ? x["models"] : [],
        "excluded-models" => x["excluded-models"].is_a?(Array) ? x["excluded-models"] : [],
        "headers" => x["headers"].is_a?(Hash) ? x["headers"] : {},
        "prefix" => String(x["prefix"] || ""),
        "provider-kind" => "openai-compatibility"
      }
    end

    claude = cfg["claude-api-key"]
    claude = [] unless claude.is_a?(Array)
    claude = claude.select { |x| x.is_a?(Hash) }.map do |x|
      {
        "api-key" => String(x["api-key"] || ""),
        "base-url" => String(x["base-url"] || ""),
        "models" => x["models"].is_a?(Array) ? x["models"] : [],
        "excluded-models" => x["excluded-models"].is_a?(Array) ? x["excluded-models"] : [],
        "headers" => x["headers"].is_a?(Hash) ? x["headers"] : {},
        "prefix" => String(x["prefix"] || ""),
        "provider-kind" => "claude"
      }
    end

    arr = (codex + compat + claude).select { |x| String(x["api-key"]).strip != "" && String(x["base-url"]).strip != "" }
    prefix = String(ARGV[1] || "")
    entry = nil
    if !provider_name.empty?
      entry = arr.find do |x|
        x.is_a?(Hash) &&
          String(x["provider-kind"]).strip == "openai-compatibility" &&
          String(x["name"]).strip == provider_name
      end
    end
    entry = arr.find { |x| x.is_a?(Hash) && String(x["prefix"]).strip == prefix } if entry.nil?
    entry = arr.first if entry.nil? && prefix.empty? && provider_name.empty?
    if entry.nil?
      puts "null"
    else
      puts JSON.generate(entry)
    end
  ' "${config_path}" "${PROVIDER_PREFIX}" "${PROVIDER_NAME}" 2>/dev/null || true
}

health_status() {
  local http="$1"
  local reply="$2"
  local err="$3"
  local body_excerpt="$4"

  if [ "${http}" != "200" ]; then
    echo "red"
    return
  fi
  if [ -z "${reply}" ]; then
    echo "red"
    return
  fi
  if printf '%s' "${reply}" | rg -qi '703|fib|json|summary|数学|代码'; then
    echo "green"
    return
  fi
  if [ -n "${err}" ] || [ -n "${body_excerpt}" ]; then
    echo "red"
    return
  fi
  echo "red"
}

build_openai_chat_endpoint() {
  local raw="$1"
  local trimmed
  trimmed="$(printf '%s' "${raw}" | sed 's#/*$##')"
  if [ -z "${trimmed}" ]; then
    printf '%s' ""
    return
  fi
  case "${trimmed}" in
    */chat/completions)
      printf '%s' "${trimmed}"
      ;;
    */v1)
      printf '%s/chat/completions' "${trimmed}"
      ;;
    *)
      printf '%s/v1/chat/completions' "${trimmed}"
      ;;
  esac
}

build_openai_responses_endpoint() {
  local raw="$1"
  local trimmed
  trimmed="$(printf '%s' "${raw}" | sed 's#/*$##')"
  if [ -z "${trimmed}" ]; then
    printf '%s' ""
    return
  fi
  case "${trimmed}" in
    */responses)
      printf '%s' "${trimmed}"
      ;;
    */v1)
      printf '%s/responses' "${trimmed}"
      ;;
    *)
      printf '%s/v1/responses' "${trimmed}"
      ;;
  esac
}

build_claude_messages_endpoint() {
  local raw="$1"
  local trimmed
  trimmed="$(printf '%s' "${raw}" | sed 's#/*$##')"
  if [ -z "${trimmed}" ]; then
    printf '%s' ""
    return
  fi
  case "${trimmed}" in
    */v1/messages|*/messages)
      printf '%s' "${trimmed}"
      ;;
    */v1)
      printf '%s/messages' "${trimmed}"
      ;;
    *)
      printf '%s/v1/messages' "${trimmed}"
      ;;
  esac
}

probe_stateful_responses_endpoint() {
  local endpoint="$1"
  local api_key="$2"
  local model="$3"
  shift 3
  local -a extra_headers=("$@")

  local endpoint codeword first_payload second_payload
  local resp1 body1 http1 id1 call1 type1 err1
  local resp2 body2 http2 reply2 err2

  if [ -z "${endpoint}" ]; then
    printf '%s\t%s\n' "skip" "responses_endpoint_missing"
    return
  fi

  codeword="BANANA-731"
  first_payload="$(jq -nc \
    --arg m "${model}" \
    --arg codeword "${codeword}" \
    '{
      model: $m,
      tool_choice: "required",
      tools: [
        {
          type: "function",
          name: "get_token",
          description: "Return the fixed token",
          parameters: {
            type: "object",
            properties: {},
            additionalProperties: false
          }
        }
      ],
      input: [
        {
          type: "message",
          role: "user",
          content: [
            {
              type: "input_text",
              text: ("Call get_token and then wait for the tool result. The tool will return " + $codeword + ".")
            }
          ]
        }
      ]
    }'
  )"
  resp1="$(
    curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
      "${endpoint}" \
      -H "Authorization: Bearer ${api_key}" \
      -H 'Content-Type: application/json' \
      "${extra_headers[@]}" \
      -d "${first_payload}" || true
  )"
  http1="$(printf '%s' "${resp1}" | sed -n 's/.*__HTTP__\([0-9][0-9][0-9]\)[[:space:]]*$/\1/p')"
  body1="$(printf '%s' "${resp1}" | sed 's/[[:space:]]*__HTTP__[0-9][0-9][0-9][[:space:]]*$//')"
  [ -n "${http1}" ] || http1="000"
  id1="$(printf '%s' "${body1}" | jq -r '.id // empty' 2>/dev/null || true)"
  call1="$(printf '%s' "${body1}" | jq -r '.output[]? | select(.type=="function_call") | .call_id // empty' 2>/dev/null | head -n1 || true)"
  type1="$(printf '%s' "${body1}" | jq -r '.output[0].type // empty' 2>/dev/null || true)"
  err1="$(printf '%s' "${body1}" | jq -r '.error.message // .message // empty' 2>/dev/null || true)"

  if [ "${http1}" != "200" ] || [ -n "${err1}" ] || [ -z "${id1}" ] || [ -z "${call1}" ] || [ "${type1}" != "function_call" ]; then
    printf '%s\t%s\n' "failed" "responses_bootstrap_failed"
    return
  fi

  second_payload="$(jq -nc \
    --arg m "${model}" \
    --arg prev "${id1}" \
    --arg call "${call1}" \
    --arg codeword "${codeword}" \
    '{
      model: $m,
      previous_response_id: $prev,
      input: [
        {
          type: "function_call_output",
          call_id: $call,
          output: $codeword
        }
      ]
    }'
  )"
  resp2="$(
    curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
      "${endpoint}" \
      -H "Authorization: Bearer ${api_key}" \
      -H 'Content-Type: application/json' \
      "${extra_headers[@]}" \
      -d "${second_payload}" || true
  )"
  http2="$(printf '%s' "${resp2}" | sed -n 's/.*__HTTP__\([0-9][0-9][0-9]\)[[:space:]]*$/\1/p')"
  body2="$(printf '%s' "${resp2}" | sed 's/[[:space:]]*__HTTP__[0-9][0-9][0-9][[:space:]]*$//')"
  [ -n "${http2}" ] || http2="000"
  reply2="$(printf '%s' "${body2}" | jq -r '[.output[]? | select(.type=="message") | .content[]? | select(.type=="output_text") | .text] | join(" ")' 2>/dev/null || true)"
  err2="$(printf '%s' "${body2}" | jq -r '.error.message // .message // empty' 2>/dev/null || true)"

  if [ "${http2}" = "200" ] && [ -z "${err2}" ] && printf '%s' "${reply2}" | rg -q 'BANANA-731'; then
    printf '%s\t%s\n' "ok" "stateful_responses_ok"
    return
  fi

  printf '%s\t%s\n' "failed" "responses_continuation_failed"
}

probe_stateful_responses() {
  local base_url="$1"
  local api_key="$2"
  local model="$3"
  shift 3
  local endpoint
  endpoint="$(build_openai_responses_endpoint "${base_url}")"
  probe_stateful_responses_endpoint "${endpoint}" "${api_key}" "${model}" "$@"
}

provider_header_args_from_entry() {
  local entry_json="$1"
  jq -r '
    (.headers // {})
    | to_entries[]
    | select(
        (.key | ascii_downcase) as $key
        | $key != "authorization"
        and $key != "x-api-key"
        and $key != "anthropic-version"
        and $key != "content-type"
      )
    | @base64
  ' <<<"${entry_json}" 2>/dev/null || true
}

check_once() {
  local mgmt_codex_payload mgmt_compat_payload mgmt_claude_payload entry base_url api_key
  local provider_kind provider_label provider_name_entry quota_scope ts items_file all_models_json
  local supported_protocols
  local source_mode use_proxy blocked_until
  local provider_header_args encoded_header header_key header_value

  ts="$(now_iso)"
  items_file="$(mktemp)"
  trap 'rm -f "${items_file}"' RETURN

  source_mode="management_api"
  entry=""
  mgmt_codex_payload="{}"
  mgmt_compat_payload="{}"
  mgmt_claude_payload="{}"
  if [ -n "${MANAGEMENT_KEY}" ]; then
    mgmt_codex_payload="$(
      curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m 20 "${MANAGEMENT_URL}/v0/management/codex-api-key" \
        -H "x-management-key: ${MANAGEMENT_KEY}" || true
    )"
    if ! jq -e . >/dev/null 2>&1 <<<"${mgmt_codex_payload}"; then
      mgmt_codex_payload="{}"
    fi

    mgmt_compat_payload="$(
      curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m 20 "${MANAGEMENT_URL}/v0/management/openai-compatibility" \
        -H "x-management-key: ${MANAGEMENT_KEY}" || true
    )"
    if ! jq -e . >/dev/null 2>&1 <<<"${mgmt_compat_payload}"; then
      mgmt_compat_payload="{}"
    fi

    mgmt_claude_payload="$(
      curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m 20 "${MANAGEMENT_URL}/v0/management/claude-api-key" \
        -H "x-management-key: ${MANAGEMENT_KEY}" || true
    )"
    if ! jq -e . >/dev/null 2>&1 <<<"${mgmt_claude_payload}"; then
      mgmt_claude_payload="{}"
    fi

    entry="$(pick_provider_entry_from_payloads "${mgmt_codex_payload}" "${mgmt_compat_payload}" "${mgmt_claude_payload}")"
  fi
  if [ -z "${entry}" ] || [ "${entry}" = "null" ]; then
    source_mode="config_yaml"
    entry="$(pick_provider_entry_from_config "${CONFIG_YAML_PATH}")"
  fi
  if [ -z "${entry}" ] || [ "${entry}" = "null" ]; then
    write_error_snapshot "provider_entry_not_found"
    return 1
  fi

  base_url="$(jq -r '.["base-url"] // empty' <<<"${entry}")"
  api_key="$(jq -r '.["api-key"] // empty' <<<"${entry}")"
  provider_kind="$(jq -r '."provider-kind" // "codex"' <<<"${entry}")"
  supported_protocols="$(jq -r '.["supported-protocols"] // empty' <<<"${entry}")"
  provider_name_entry="$(jq -r '.name // empty' <<<"${entry}")"
  provider_label="${PROVIDER_PREFIX}"
  if [ "${provider_kind}" = "openai-compatibility" ]; then
    if [ -n "${provider_name_entry}" ]; then
      provider_label="${provider_name_entry}"
    elif [ -n "${PROVIDER_NAME}" ]; then
      provider_label="${PROVIDER_NAME}"
    fi
  fi
  quota_scope="${provider_label}"
  if [ -z "${base_url}" ] || [ -z "${api_key}" ]; then
    write_error_snapshot "provider_entry_missing_base_url_or_api_key"
    return 1
  fi

  provider_header_args=()
  while IFS= read -r encoded_header; do
    [ -n "${encoded_header}" ] || continue
    header_key="$(printf '%s' "${encoded_header}" | base64 -d 2>/dev/null | jq -r '.key // empty' 2>/dev/null || true)"
    header_value="$(printf '%s' "${encoded_header}" | base64 -d 2>/dev/null | jq -r '.value // empty' 2>/dev/null || true)"
    [ -n "${header_key}" ] || continue
    provider_header_args+=(-H "${header_key}: ${header_value}")
  done < <(provider_header_args_from_entry "${entry}")

  local -a raw_model_entries
  local -a models
  local -A model_display_by_upstream

  mapfile -t excluded_models < <(jq -r '.["excluded-models"][]? // empty' <<<"${entry}" | normalize_model_lines | awk '!seen[$0]++')
  if provider_disabled_by_excluded_models "${excluded_models[@]}"; then
    write_empty_snapshot "provider_disabled" "${base_url}" "${supported_protocols}" "${source_mode}" true
    return 1
  fi
  mapfile -t raw_model_entries < <(
    jq -r '
      .models[]?
      | select((.name // "" | tostring | length) > 0)
      | [(.name | tostring), (.alias // "" | tostring)]
      | @tsv
    ' <<<"${entry}" \
      | awk -F'\t' '!seen[$1]++'
  )
  models=()
  for raw_model_entry in "${raw_model_entries[@]}"; do
    local upstream_model alias_model display_model
    upstream_model="${raw_model_entry%%$'\t'*}"
    alias_model=""
    if [[ "${raw_model_entry}" == *$'\t'* ]]; then
      alias_model="${raw_model_entry#*$'\t'}"
    fi
    upstream_model="$(printf '%s' "${upstream_model}" | normalize_model_lines | head -n 1)"
    if [ -z "${upstream_model}" ]; then
      continue
    fi
    display_model="$(printf '%s' "${alias_model}" | normalize_model_lines | head -n 1)"
    if [ -z "${display_model}" ]; then
      display_model="${upstream_model}"
    fi
    models+=("${upstream_model}")
    model_display_by_upstream["${upstream_model}"]="${display_model}"
  done
  if [ "${#models[@]}" -eq 0 ]; then
    write_error_snapshot "provider_models_empty"
    return 1
  fi
  mapfile -t models < <(filter_probe_models "${excluded_models[@]}" -- "${models[@]}")
  if [ "${#models[@]}" -eq 0 ]; then
    write_error_snapshot "provider_probe_models_empty_after_filter"
    return 1
  fi
  if [ "${MAX_MODELS_PER_RUN}" -gt 0 ] && [ "${#models[@]}" -gt "${MAX_MODELS_PER_RUN}" ]; then
    models=("${models[@]:0:${MAX_MODELS_PER_RUN}}")
  fi

  all_models_json="$(
    {
      for upstream_model in "${models[@]}"; do
        local display_model_json
        display_model_json="${model_display_by_upstream["${upstream_model}"]:-${upstream_model}}"
        jq -nc \
          --arg upstream_model "${upstream_model}" \
          --arg display_model "${display_model_json}" \
          '{
            upstream_model: $upstream_model,
            display_model: (if ($display_model | length) > 0 then $display_model else $upstream_model end)
          }'
      done
    } | jq -s '.'
  )"

  blocked_until="$(daily_quota_get_blocked_until "${quota_scope}")"
  if daily_quota_is_blocked "${quota_scope}"; then
    jq -n \
      --arg updated_at "${ts}" \
      --arg provider_prefix "${provider_label}" \
      --arg base_url "${base_url}" \
      --arg supported_protocols "${supported_protocols}" \
      --arg source_mode "${source_mode}" \
      --arg image_url "${IMAGE_URL}" \
      --arg task_text "${TASK_TEXT}" \
      --arg blocked_until "${blocked_until}" \
      --argjson interval "${CHECK_INTERVAL_SEC}" \
      --argjson all_models "${all_models_json}" \
      '{
        updated_at: $updated_at,
        provider_prefix: $provider_prefix,
        base_url: $base_url,
        supported_protocols: $supported_protocols,
        source_mode: $source_mode,
        interval_sec: $interval,
        task: {
          mode: "multimodal_text+image_url",
          image_url: $image_url,
          prompt: $task_text
        },
        models: (
          $all_models
          | map({
              model: ($provider_prefix + "/" + .display_model),
              upstream_model: .upstream_model,
              status: "red",
              reason: ("daily_quota_exhausted_until_" + $blocked_until),
              http: 402,
              response_model: "",
              finish_reason: "",
              usage: "",
              reply_excerpt: "",
              error: "daily quota exhausted",
              body_excerpt: "",
              tested_at: $updated_at
            })
        )
      }
      | .summary = {
          total: (.models | length),
          green: 0,
          yellow: 0,
          red: (.models | length)
        }
      ' >"${OUT_JSON}.tmp"
    mv "${OUT_JSON}.tmp" "${OUT_JSON}"
    mirror_snapshot_for_dashboard
    notify_openclaw
    return 0
  fi

  use_proxy="false"
  case "$(printf '%s' "${CHECK_VIA_PROXY}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes|on)
      if [ "${provider_kind}" = "codex" ] && probe_codex_prefers_claude_messages "${provider_kind}" "${supported_protocols}"; then
        use_proxy="true"
      fi
      ;;
    *)
      use_proxy="false"
      ;;
  esac
  if [ "${use_proxy}" = "true" ]; then
    source_mode="local_proxy_responses"
    if [ -z "${PROXY_API_KEY}" ]; then
      write_error_snapshot "proxy_api_key_missing"
      return 1
    fi
  fi

  emit_snapshot() {
    local tested_json
    tested_json="$(jq -s '.' "${items_file}")"
    jq -n \
      --arg updated_at "${ts}" \
      --arg provider_prefix "${provider_label}" \
      --arg base_url "${base_url}" \
      --arg supported_protocols "${supported_protocols}" \
      --arg source_mode "${source_mode}" \
      --arg image_url "${IMAGE_URL}" \
      --arg task_text "${TASK_TEXT}" \
      --argjson interval "${CHECK_INTERVAL_SEC}" \
      --argjson all_models "${all_models_json}" \
      --argjson tested "${tested_json}" \
      '
      ($tested | reduce .[] as $t ({}; .[$t.upstream_model] = $t)) as $tested_map
      | {
          updated_at: $updated_at,
          provider_prefix: $provider_prefix,
          base_url: $base_url,
          supported_protocols: $supported_protocols,
          source_mode: $source_mode,
          interval_sec: $interval,
          task: {
            mode: "multimodal_text+image_url",
            image_url: $image_url,
            prompt: $task_text
          },
          models: (
            $all_models
            | map(
                if $tested_map[.upstream_model] != null then
                  $tested_map[.upstream_model]
                else
                  {
                    model: ($provider_prefix + "/" + .display_model),
                    upstream_model: .upstream_model,
                    status: "yellow",
                    reason: "pending_current_run",
                    http: 0,
                    response_model: "",
                    finish_reason: "",
                    usage: "",
                    reply_excerpt: "",
                    error: "",
                    body_excerpt: "",
                    tested_at: $updated_at
                  }
                end
              )
          )
        }
      | .summary = {
          total: (.models | length),
          green: (.models | map(select(.status=="green")) | length),
          yellow: (.models | map(select(.status=="yellow")) | length),
          red: (.models | map(select(.status=="red")) | length)
        }
      ' >"${OUT_JSON}.tmp"
    mv "${OUT_JSON}.tmp" "${OUT_JSON}"
    mirror_snapshot_for_dashboard
    notify_openclaw
  }

  local model payload resp body http response_model finish_reason usage reply err body_excerpt status reason
  local text_payload t_resp t_body t_http t_response_model t_finish_reason t_usage t_reply t_err t_body_excerpt
  local run_multimodal prefix_norm text_retry_max text_retry_attempt text_retry_sleep_sec daily_quota_hit channel_daily_blocked channel_blocked_until
  prefix_norm="$(printf '%s' "${PROVIDER_PREFIX}" | tr '[:upper:]' '[:lower:]')"
  channel_daily_blocked="false"
  channel_blocked_until=""
  text_retry_max=1
  text_retry_sleep_sec="${EE_MODEL_RETRY_SLEEP_SEC}"
  if [[ "${PROBE_RETRY_COUNT}" =~ ^[0-9]+$ ]]; then
    text_retry_max=$((PROBE_RETRY_COUNT + 1))
  else
    if [ "${use_proxy}" = "false" ]; then
      text_retry_max="${DIRECT_MODEL_RETRY_MAX}"
      text_retry_sleep_sec="${DIRECT_MODEL_RETRY_SLEEP_SEC}"
    fi
    if [ "${prefix_norm}" = "sub2api" ] || [ "${prefix_norm}" = "ee" ]; then
      text_retry_max="${EE_MODEL_RETRY_MAX}"
      text_retry_sleep_sec="${EE_MODEL_RETRY_SLEEP_SEC}"
    fi
    if [ "${provider_kind}" = "claude" ] && [ "${text_retry_max}" -lt 3 ]; then
      text_retry_max=3
    fi
    if ! [[ "${text_retry_max}" =~ ^[0-9]+$ ]]; then
      text_retry_max=3
    fi
  fi
  if [ "${text_retry_max}" -lt 1 ]; then
    text_retry_max=1
  fi

  run_multimodal="false"
  if [ "${use_proxy}" = "false" ]; then
    case "$(printf '%s' "${CHECK_MODE}" | tr '[:upper:]' '[:lower:]')" in
      multimodal|multimodal_first|text_then_multimodal)
        run_multimodal="true"
        ;;
      text_only|text_first|*)
        run_multimodal="false"
        ;;
    esac
  fi

  for model in "${models[@]}"; do
    local display_model proxy_model
    display_model="${model_display_by_upstream["${model}"]:-${model}}"
    proxy_model="${PROVIDER_PREFIX}/${display_model}"
    if [ "${channel_daily_blocked}" = "true" ]; then
      jq -nc \
        --arg model "${provider_label}/${display_model}" \
        --arg upstream_model "${model}" \
        --arg blocked_until "${channel_blocked_until}" \
        --arg tested_at "${ts}" \
        '{
          model: $model,
          upstream_model: $upstream_model,
          status: "red",
          reason: ("daily_quota_exhausted_until_" + $blocked_until),
          http: 402,
          response_model: "",
          finish_reason: "",
          usage: "",
          reply_excerpt: "",
          error: "daily quota exhausted",
          body_excerpt: "",
          text_fallback_http: 402,
          text_fallback_response_model: "",
          text_fallback_finish_reason: "",
          text_fallback_usage: "",
          text_fallback_reply_excerpt: "",
          text_fallback_error: "daily quota exhausted",
          text_fallback_body_excerpt: "",
          tested_at: $tested_at
        }' >>"${items_file}"
      continue
    fi

    http="000"
    response_model=""
    finish_reason=""
    usage=""
    reply=""
    err=""
    body_excerpt=""
    status="red"
    reason=""

    t_http="000"
    t_response_model=""
    t_finish_reason=""
    t_usage=""
    t_reply=""
    t_err=""
    t_body_excerpt=""
    local responses_continuation_status responses_continuation_reason
    local codex_use_responses codex_use_claude_messages
    responses_continuation_status=""
    responses_continuation_reason=""
    codex_use_responses="false"
    codex_use_claude_messages="false"
    if probe_codex_prefers_responses "${provider_kind}" "${supported_protocols}" "${provider_label}" "${provider_name_entry}" "${base_url}"; then
      codex_use_responses="true"
    elif probe_codex_prefers_claude_messages "${provider_kind}" "${supported_protocols}"; then
      codex_use_claude_messages="true"
    fi

    if [ "${use_proxy}" = "true" ]; then
      if [ "${provider_kind}" = "claude" ]; then
        text_payload="$(jq -nc \
          --arg m "${proxy_model}" \
          --arg t "${TEXT_FALLBACK_TASK_TEXT}" \
          --argjson mt "${TEXT_MAX_TOKENS}" \
          '{
            model: $m,
            max_tokens: $mt,
            messages: [
              {
                role: "user",
                content: $t
              }
            ]
          }'
        )"
      else
        text_payload="$(jq -nc \
          --arg m "${proxy_model}" \
          --arg t "${TEXT_FALLBACK_TASK_TEXT}" \
          '{
            model: $m,
            input: $t
          }'
        )"
      fi
    else
      if [ "${provider_kind}" = "codex" ] && [ "${codex_use_responses}" = "true" ]; then
        text_payload="$(jq -nc \
          --arg m "${model}" \
          --arg t "${TEXT_FALLBACK_TASK_TEXT}" \
          '{
            model: $m,
            input: [
              {
                type: "message",
                role: "user",
                content: [
                  {
                    type: "input_text",
                    text: $t
                  }
                ]
              }
            ]
          }'
        )"
      elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_claude_messages}" = "true" ]; then
        text_payload="$(jq -nc \
          --arg m "${model}" \
          --arg t "${TEXT_FALLBACK_TASK_TEXT}" \
          --argjson mt "${TEXT_MAX_TOKENS}" \
          '{
            model: $m,
            max_tokens: $mt,
            messages: [
              {
                role: "user",
                content: $t
              }
            ]
          }'
        )"
      else
        text_payload="$(jq -nc \
          --arg m "${model}" \
          --arg t "${TEXT_FALLBACK_TASK_TEXT}" \
          --argjson mt "${TEXT_MAX_TOKENS}" \
          '{
            model: $m,
            messages: [
              {
                role: "user",
                content: $t
              }
            ],
            temperature: 0.2,
            max_tokens: $mt
          }'
        )"
      fi
    fi

    text_retry_attempt=1
    while [ "${text_retry_attempt}" -le "${text_retry_max}" ]; do
      if [ "${use_proxy}" = "true" ]; then
        if [ "${provider_kind}" = "claude" ]; then
          t_resp="$(
            curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
              "${PROXY_CLAUDE_MESSAGES_URL}" \
              -H "Authorization: Bearer ${PROXY_API_KEY}" \
              -H 'Content-Type: application/json' \
              -d "${text_payload}" || true
          )"
        else
          t_resp="$(
            curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
              "${PROXY_RESPONSES_URL}" \
              -H "Authorization: Bearer ${PROXY_API_KEY}" \
              -H 'Content-Type: application/json' \
              -d "${text_payload}" || true
          )"
        fi
      else
        if [ "${provider_kind}" = "claude" ]; then
          local_claude_endpoint="$(build_claude_messages_endpoint "${base_url}")"
          t_resp="$(
            curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
              "${local_claude_endpoint}" \
              -H "x-api-key: ${api_key}" \
              -H 'anthropic-version: 2023-06-01' \
              -H 'Content-Type: application/json' \
              "${provider_header_args[@]}" \
              -d "${text_payload}" || true
          )"
        elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_responses}" = "true" ]; then
          local_openai_endpoint="$(build_openai_responses_endpoint "${base_url}")"
          t_resp="$(
            curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
              "${local_openai_endpoint}" \
              -H "Authorization: Bearer ${api_key}" \
              -H 'Content-Type: application/json' \
              "${provider_header_args[@]}" \
              -d "${text_payload}" || true
          )"
        elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_claude_messages}" = "true" ]; then
          local_claude_endpoint="$(build_claude_messages_endpoint "${base_url}")"
          t_resp="$(
            curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
              "${local_claude_endpoint}" \
              -H "Authorization: Bearer ${api_key}" \
              -H 'anthropic-version: 2023-06-01' \
              -H 'Content-Type: application/json' \
              "${provider_header_args[@]}" \
              -d "${text_payload}" || true
          )"
        else
          local_openai_endpoint="$(build_openai_chat_endpoint "${base_url}")"
          t_resp="$(
            curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
              "${local_openai_endpoint}" \
              -H "Authorization: Bearer ${api_key}" \
              -H 'Content-Type: application/json' \
              "${provider_header_args[@]}" \
              -d "${text_payload}" || true
          )"
        fi
      fi

      t_http="$(printf '%s' "${t_resp}" | sed -n 's/.*__HTTP__\([0-9][0-9][0-9]\)[[:space:]]*$/\1/p')"
      t_body="$(printf '%s' "${t_resp}" | sed 's/[[:space:]]*__HTTP__[0-9][0-9][0-9][[:space:]]*$//')"
      [ -n "${t_http}" ] || t_http="000"

      if [ "${provider_kind}" = "claude" ]; then
        t_response_model="$(printf '%s' "${t_body}" | jq -r '.model // empty' 2>/dev/null || true)"
        t_finish_reason="$(printf '%s' "${t_body}" | jq -r '.stop_reason // .stopReason // .type // empty' 2>/dev/null || true)"
        t_usage="$(printf '%s' "${t_body}" | jq -r '[.usage.input_tokens,.usage.output_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        t_reply="$(printf '%s' "${t_body}" | jq -r 'if (.content | type) == "array" then (.content | map(.text // .input_text // empty) | join(" ")) else (.content // empty) end' 2>/dev/null || true)"
      elif [ "${use_proxy}" = "true" ]; then
        t_response_model="$(printf '%s' "${t_body}" | jq -r '.model // empty' 2>/dev/null || true)"
        t_finish_reason="$(printf '%s' "${t_body}" | jq -r '.status // .output[0].finish_reason // empty' 2>/dev/null || true)"
        t_usage="$(printf '%s' "${t_body}" | jq -r '[.usage.input_tokens,.usage.output_tokens,.usage.total_tokens,.usage.prompt_tokens,.usage.completion_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        t_reply="$(printf '%s' "${t_body}" | jq -r '.output_text // .output[0].content[0].text // empty' 2>/dev/null || true)"
      elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_responses}" = "true" ]; then
        t_response_model="$(printf '%s' "${t_body}" | jq -r '.model // empty' 2>/dev/null || true)"
        t_finish_reason="$(printf '%s' "${t_body}" | jq -r '.status // .output[0].status // empty' 2>/dev/null || true)"
        t_usage="$(printf '%s' "${t_body}" | jq -r '[.usage.input_tokens,.usage.output_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        t_reply="$(printf '%s' "${t_body}" | jq -r '[.output[]? | select(.type=="message") | .content[]? | select(.type=="output_text") | .text] | join(" ")' 2>/dev/null || true)"
      elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_claude_messages}" = "true" ]; then
        t_response_model="$(printf '%s' "${t_body}" | jq -r '.model // empty' 2>/dev/null || true)"
        t_finish_reason="$(printf '%s' "${t_body}" | jq -r '.stop_reason // .stopReason // .type // empty' 2>/dev/null || true)"
        t_usage="$(printf '%s' "${t_body}" | jq -r '[.usage.input_tokens,.usage.output_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        t_reply="$(printf '%s' "${t_body}" | jq -r 'if (.content | type) == "array" then (.content | map(.text // .input_text // empty) | join(" ")) else (.content // empty) end' 2>/dev/null || true)"
      else
        t_response_model="$(printf '%s' "${t_body}" | jq -r '.model // empty' 2>/dev/null || true)"
        t_finish_reason="$(printf '%s' "${t_body}" | jq -r '.choices[0].finish_reason // empty' 2>/dev/null || true)"
        t_usage="$(printf '%s' "${t_body}" | jq -r '[.usage.prompt_tokens,.usage.completion_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        t_reply="$(printf '%s' "${t_body}" | jq -r '.choices[0].message.content // empty' 2>/dev/null || true)"
      fi
      t_err="$(printf '%s' "${t_body}" | jq -r '.error.message // .message // empty' 2>/dev/null || true)"

      t_reply="$(printf '%s' "${t_reply}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-200)"
      t_err="$(printf '%s' "${t_err}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-160)"
      t_body_excerpt="$(printf '%s' "${t_body}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-160)"

      if [ "${t_http}" = "200" ] && [ -z "${t_err}" ] && { [ -n "${t_reply}" ] || [ -n "${t_usage}" ]; }; then
        break
      fi
      if [ "${text_retry_attempt}" -lt "${text_retry_max}" ]; then
        local sleep_sec
        if [ "${provider_kind}" = "claude" ] && printf '%s %s' "${t_err}" "${t_body_excerpt}" | rg -qi 'cooldown|service_busy|retry shortly'; then
          text_retry_sleep_sec=3
        fi
        sleep_sec="${text_retry_sleep_sec}"
        if [[ "${PROBE_MAX_RETRY_WAIT_SEC}" =~ ^[0-9]+$ ]] && [ "${PROBE_MAX_RETRY_WAIT_SEC}" -gt 0 ] && [ "${sleep_sec}" -gt "${PROBE_MAX_RETRY_WAIT_SEC}" ]; then
          sleep_sec="${PROBE_MAX_RETRY_WAIT_SEC}"
        fi
        sleep "${sleep_sec}"
      fi
      text_retry_attempt=$((text_retry_attempt + 1))
    done

    http="${t_http}"
    response_model="${t_response_model}"
    finish_reason="${t_finish_reason}"
    usage="${t_usage}"
    reply="${t_reply}"
    err="${t_err}"
    body_excerpt="${t_body_excerpt}"

    if [ "${t_http}" = "200" ] && [ -z "${t_err}" ] && { [ -n "${t_reply}" ] || [ -n "${t_usage}" ]; }; then
      status="green"
      reason="text_only_ok"
      if [ "${provider_kind}" = "codex" ]; then
        local stateful_probe_result stateful_probe_status stateful_probe_reason
        if [ "${use_proxy}" = "true" ] && [ -n "${PROXY_RESPONSES_URL}" ] && [ -n "${PROXY_API_KEY}" ]; then
          stateful_probe_result="$(probe_stateful_responses_endpoint "${PROXY_RESPONSES_URL}" "${PROXY_API_KEY}" "${proxy_model}")"
        elif [ "${codex_use_responses}" = "true" ]; then
          stateful_probe_result="$(probe_stateful_responses "${base_url}" "${api_key}" "${model}" "${provider_header_args[@]}")"
        elif [ "${codex_use_claude_messages}" = "true" ]; then
          stateful_probe_result="$(printf '%s\t%s\n' "skip" "claude_messages_no_native_responses_continuation")"
        else
          stateful_probe_result="$(printf '%s\t%s\n' "skip" "proxy_responses_probe_unavailable")"
        fi
        stateful_probe_status="${stateful_probe_result%%$'\t'*}"
        stateful_probe_reason="${stateful_probe_result#*$'\t'}"
        if [ "${stateful_probe_status}" = "ok" ]; then
          responses_continuation_status="ok"
          responses_continuation_reason="${stateful_probe_reason}"
        elif [ "${stateful_probe_status}" = "failed" ]; then
          responses_continuation_status="failed"
          responses_continuation_reason="${stateful_probe_reason}"
        elif [ "${stateful_probe_status}" = "skip" ]; then
          responses_continuation_status="skip"
          responses_continuation_reason="${stateful_probe_reason}"
        fi
      fi
    else
      if [ "${t_http}" = "200" ] && [ -z "${t_err}" ]; then
        status="red"
        reason="response_without_text_or_usage"
      else
        status="red"
        reason="${t_err:-${t_body_excerpt:-http_${t_http}}}"
        if [ "${t_http}" = "000" ] && [ -z "${t_err}" ] && [ -z "${t_body_excerpt}" ]; then
          reason="timeout_or_network_error"
        fi
      fi
    fi

    daily_quota_hit="false"
    if is_daily_quota_exhausted_error "${t_http}" "${t_err}" "${t_body}"; then
      blocked_until="$(daily_quota_next_resume_utc)"
      daily_quota_set_block "${quota_scope}" "${blocked_until}" "daily_quota_exhausted" "${ts}"
      channel_daily_blocked="true"
      channel_blocked_until="${blocked_until}"
      daily_quota_hit="true"
      status="red"
      reason="daily_quota_exhausted_until_${blocked_until}"
      http="${t_http}"
      response_model="${t_response_model}"
      finish_reason="${t_finish_reason}"
      usage="${t_usage}"
      reply="${t_reply}"
      err="${t_err}"
      body_excerpt="${t_body_excerpt}"
    fi

    # Optional multimodal probe: only run when explicitly enabled.
    if [ "${run_multimodal}" = "true" ] && [ "${daily_quota_hit}" != "true" ]; then
      if [ "${provider_kind}" = "codex" ] && [ "${codex_use_responses}" = "true" ]; then
        payload="$(jq -nc \
          --arg m "${model}" \
          --arg t "${TASK_TEXT}" \
          --arg i "${IMAGE_URL}" \
          '{
            model: $m,
            input: [
              {
                type: "message",
                role: "user",
                content: [
                  {type: "input_text", text: $t},
                  {type: "input_image", image_url: $i}
                ]
              }
            ]
          }'
        )"

        local_openai_endpoint="$(build_openai_responses_endpoint "${base_url}")"
        resp="$(
          curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
            "${local_openai_endpoint}" \
            -H "Authorization: Bearer ${api_key}" \
            -H 'Content-Type: application/json' \
            "${provider_header_args[@]}" \
            -d "${payload}" || true
        )"
      elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_claude_messages}" = "true" ]; then
        payload="$(jq -nc \
          --arg m "${model}" \
          --arg t "${TASK_TEXT}" \
          --arg i "${IMAGE_URL}" \
          --argjson mt "${MULTIMODAL_MAX_TOKENS}" \
          '{
            model: $m,
            max_tokens: $mt,
            messages: [
              {
                role: "user",
                content: [
                  {type: "text", text: $t},
                  {type: "image", source: {type: "url", url: $i}}
                ]
              }
            ]
          }'
        )"

        local_claude_endpoint="$(build_claude_messages_endpoint "${base_url}")"
        resp="$(
          curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
            "${local_claude_endpoint}" \
            -H "Authorization: Bearer ${api_key}" \
            -H 'anthropic-version: 2023-06-01' \
            -H 'Content-Type: application/json' \
            "${provider_header_args[@]}" \
            -d "${payload}" || true
        )"
      else
        payload="$(jq -nc \
          --arg m "${model}" \
          --arg t "${TASK_TEXT}" \
          --arg i "${IMAGE_URL}" \
          --argjson mt "${MULTIMODAL_MAX_TOKENS}" \
          '{
            model: $m,
            messages: [
              {
                role: "user",
                content: [
                  {type: "text", text: $t},
                  {type: "image_url", image_url: {url: $i}}
                ]
              }
            ],
            temperature: 0.2,
            max_tokens: $mt
          }'
        )"

        resp="$(
          curl -sS --connect-timeout "${CURL_CONNECT_TIMEOUT_SEC}" -m "${HTTP_TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
            "${base_url}/chat/completions" \
            -H "Authorization: Bearer ${api_key}" \
            -H 'Content-Type: application/json' \
            "${provider_header_args[@]}" \
            -d "${payload}" || true
        )"
      fi

      http="$(printf '%s' "${resp}" | sed -n 's/.*__HTTP__\([0-9][0-9][0-9]\)[[:space:]]*$/\1/p')"
      body="$(printf '%s' "${resp}" | sed 's/[[:space:]]*__HTTP__[0-9][0-9][0-9][[:space:]]*$//')"
      [ -n "${http}" ] || http="000"

      response_model="$(printf '%s' "${body}" | jq -r '.model // empty' 2>/dev/null || true)"
      if [ "${provider_kind}" = "codex" ] && [ "${codex_use_responses}" = "true" ]; then
        finish_reason="$(printf '%s' "${body}" | jq -r '.status // .output[0].status // empty' 2>/dev/null || true)"
        usage="$(printf '%s' "${body}" | jq -r '[.usage.input_tokens,.usage.output_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        reply="$(printf '%s' "${body}" | jq -r '[.output[]? | select(.type=="message") | .content[]? | select(.type=="output_text") | .text] | join(" ")' 2>/dev/null || true)"
      elif [ "${provider_kind}" = "codex" ] && [ "${codex_use_claude_messages}" = "true" ]; then
        finish_reason="$(printf '%s' "${body}" | jq -r '.stop_reason // .stopReason // .type // empty' 2>/dev/null || true)"
        usage="$(printf '%s' "${body}" | jq -r '[.usage.input_tokens,.usage.output_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        reply="$(printf '%s' "${body}" | jq -r 'if (.content | type) == "array" then (.content | map(.text // .input_text // empty) | join(" ")) else (.content // empty) end' 2>/dev/null || true)"
      else
        finish_reason="$(printf '%s' "${body}" | jq -r '.choices[0].finish_reason // empty' 2>/dev/null || true)"
        usage="$(printf '%s' "${body}" | jq -r '[.usage.prompt_tokens,.usage.completion_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
        reply="$(printf '%s' "${body}" | jq -r '.choices[0].message.content // empty' 2>/dev/null || true)"
      fi
      err="$(printf '%s' "${body}" | jq -r '.error.message // .message // empty' 2>/dev/null || true)"

      reply="$(printf '%s' "${reply}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-200)"
      err="$(printf '%s' "${err}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-160)"
      body_excerpt="$(printf '%s' "${body}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-160)"

      if [ "${status}" = "green" ]; then
        if [ "${http}" = "200" ] && [ -n "${reply}" ]; then
          reason="multimodal_ok"
        else
          reason="text_only_ok"
        fi
      else
        status="$(health_status "${http}" "${reply}" "${err}" "${body_excerpt}")"
        if [ "${status}" = "green" ]; then
          reason="multimodal_ok"
        else
          reason="${err:-response_uncertain}"
          if [ "${http}" = "000" ]; then
            reason="timeout_or_network_error"
          fi
        fi
      fi
    fi

    jq -nc \
      --arg model "${provider_label}/${display_model}" \
      --arg upstream_model "${model}" \
      --arg status "${status}" \
      --arg reason "${reason}" \
      --arg response_model "${response_model}" \
      --arg finish_reason "${finish_reason}" \
      --arg usage "${usage}" \
      --arg reply_excerpt "${reply}" \
      --arg error "${err}" \
      --arg body_excerpt "${body_excerpt}" \
      --arg text_fallback_response_model "${t_response_model}" \
      --arg text_fallback_finish_reason "${t_finish_reason}" \
      --arg text_fallback_usage "${t_usage}" \
      --arg text_fallback_reply_excerpt "${t_reply}" \
      --arg text_fallback_error "${t_err}" \
      --arg text_fallback_body_excerpt "${t_body_excerpt}" \
      --arg responses_continuation_status "${responses_continuation_status}" \
      --arg responses_continuation_reason "${responses_continuation_reason}" \
      --arg tested_at "${ts}" \
      --argjson http "$((10#${http}))" \
      --argjson text_fallback_http "$((10#${t_http}))" \
      '{
        model: $model,
        upstream_model: $upstream_model,
        status: $status,
        reason: $reason,
        http: $http,
        response_model: $response_model,
        finish_reason: $finish_reason,
        usage: $usage,
        reply_excerpt: $reply_excerpt,
        error: $error,
        body_excerpt: $body_excerpt,
        text_fallback_http: $text_fallback_http,
        text_fallback_response_model: $text_fallback_response_model,
        text_fallback_finish_reason: $text_fallback_finish_reason,
        text_fallback_usage: $text_fallback_usage,
        text_fallback_reply_excerpt: $text_fallback_reply_excerpt,
        text_fallback_error: $text_fallback_error,
        text_fallback_body_excerpt: $text_fallback_body_excerpt,
        tested_at: $tested_at
      }
      | if $responses_continuation_status != "" then .responses_continuation_status = $responses_continuation_status else . end
      | if $responses_continuation_reason != "" then .responses_continuation_reason = $responses_continuation_reason else . end
      ' >>"${items_file}"

    sleep 0.4
  done
  emit_snapshot
  return 0
}

run_mode="${1:-loop}"
if [ "${run_mode}" = "--once" ]; then
  check_once
  exit 0
fi

while true; do
  check_once || true
  sleep "${CHECK_INTERVAL_SEC}"
done
