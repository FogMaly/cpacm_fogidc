#!/usr/bin/env bash
set -euo pipefail

CONFIG_FILE="${CONFIG_FILE:-/opt/cli-proxy-api/scripts/model-health.env}"
if [ -f "${CONFIG_FILE}" ]; then
  set -a
  # shellcheck disable=SC1090
  source "${CONFIG_FILE}"
  set +a
fi

MANAGEMENT_URL="${MANAGEMENT_URL:-http://127.0.0.1:34050}"
MANAGEMENT_KEY="${MANAGEMENT_KEY:-}"
CONFIG_YAML_PATH="${CONFIG_YAML_PATH:-/opt/cli-proxy-api/config.yaml}"
CHECK_INTERVAL_SEC="${CHECK_INTERVAL_SEC:-3600}"
PROBE_RETRY_COUNT="${PROBE_RETRY_COUNT:-}"
PROBE_MAX_RETRY_WAIT_SEC="${PROBE_MAX_RETRY_WAIT_SEC:-0}"

PROVIDER_PREFIX_LIST="${PROVIDER_PREFIX_LIST:-}"

OUT_JSON_MULTI="${OUT_JSON_MULTI:-/opt/cli-proxy-api/static/model-health-multi.json}"
OUT_AUTH_JSON_MULTI="${OUT_AUTH_JSON_MULTI:-/opt/cli-proxy-api/auths/model-health-multi.json}"
OUT_JSON_COMPAT="${OUT_JSON_COMPAT:-/opt/cli-proxy-api/static/model-health.json}"
OUT_AUTH_JSON_COMPAT="${OUT_AUTH_JSON_COMPAT:-/opt/cli-proxy-api/auths/model-health.json}"

MULTI_HTTP_TIMEOUT_SEC="${MULTI_HTTP_TIMEOUT_SEC:-22}"
MULTI_CURL_CONNECT_TIMEOUT_SEC="${MULTI_CURL_CONNECT_TIMEOUT_SEC:-6}"

CHECK_MODE="${CHECK_MODE:-text_only}"
CHECK_VIA_PROXY="${CHECK_VIA_PROXY:-true}"
PROXY_RESPONSES_URL="${PROXY_RESPONSES_URL:-${MANAGEMENT_URL}/v1/responses}"
PROXY_CLAUDE_MESSAGES_URL="${PROXY_CLAUDE_MESSAGES_URL:-${MANAGEMENT_URL}/v1/messages}"
PROXY_API_KEY="${PROXY_API_KEY:-}"

MAX_MODELS_PER_RUN="${MAX_MODELS_PER_RUN:-0}"
IMAGE_URL="${IMAGE_URL:-https://upload.wikimedia.org/wikipedia/commons/thumb/3/3f/Fronalpstock_big.jpg/640px-Fronalpstock_big.jpg}"
TASK_TEXT="${TASK_TEXT:-请先描述图片，再完成任务：1) 一句话总结“AI 正在改变软件开发流程”；2) 计算 37*19；3) 给出 Python fib(n)；4) 输出 JSON，包含 summary/math_result/code_language/reliability_note。}"
TEXT_FALLBACK_TASK_TEXT="${TEXT_FALLBACK_TASK_TEXT:-请直接完成：1)一句话总结AI改变软件开发流程；2)计算37*19；3)给一个Python fib(n)函数；4)最后输出JSON字段summary/math_result/code_language/reliability_note。}"
LOCK_FILE_MULTI="${LOCK_FILE_MULTI:-/opt/cli-proxy-api/logs/model-health-multi.lock}"

SINGLE_MONITOR_SCRIPT="${SINGLE_MONITOR_SCRIPT:-/opt/cli-proxy-api/scripts/hourly-model-health-monitor.sh}"
SUB2API_RETRY_MAX="${SUB2API_RETRY_MAX:-3}"
TRANSIENT_ALL_RED_RETRY_MAX="${TRANSIENT_ALL_RED_RETRY_MAX:-3}"
RETRY_SLEEP_SEC="${RETRY_SLEEP_SEC:-2}"
MODEL_HEALTH_RETAINED_MODELS="${MODEL_HEALTH_RETAINED_MODELS:-}"
MODEL_HEALTH_RETAINED_MODELS_FILE="${MODEL_HEALTH_RETAINED_MODELS_FILE:-}"

mkdir -p "$(dirname "${OUT_JSON_MULTI}")"
if [ -n "${OUT_AUTH_JSON_MULTI}" ]; then
  mkdir -p "$(dirname "${OUT_AUTH_JSON_MULTI}")"
fi
if [ -n "${OUT_JSON_COMPAT}" ]; then
  mkdir -p "$(dirname "${OUT_JSON_COMPAT}")"
fi
if [ -n "${OUT_AUTH_JSON_COMPAT}" ]; then
  mkdir -p "$(dirname "${OUT_AUTH_JSON_COMPAT}")"
fi

# Global lock: avoid overlapping multi-prefix jobs.
mkdir -p "$(dirname "${LOCK_FILE_MULTI}")"
exec 9>"${LOCK_FILE_MULTI}"
if ! flock -n 9; then
  echo "model-health multi monitor is already running, skip overlapping run" >&2
  exit 0
fi

now_iso() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

mirror_snapshot() {
  if [ -z "${OUT_AUTH_JSON_MULTI}" ]; then
    :
  elif [ -f "${OUT_JSON_MULTI}" ]; then
    cp "${OUT_JSON_MULTI}" "${OUT_AUTH_JSON_MULTI}" 2>/dev/null || true
  fi

  if [ -n "${OUT_JSON_COMPAT}" ] && [ -f "${OUT_JSON_MULTI}" ]; then
    cp "${OUT_JSON_MULTI}" "${OUT_JSON_COMPAT}" 2>/dev/null || true
  fi
  if [ -n "${OUT_AUTH_JSON_COMPAT}" ] && [ -f "${OUT_JSON_MULTI}" ]; then
    cp "${OUT_JSON_MULTI}" "${OUT_AUTH_JSON_COMPAT}" 2>/dev/null || true
  fi
}

write_error_snapshot() {
  local reason="$1"
  local ts
  ts="$(now_iso)"
  if [ -f "${OUT_JSON_MULTI}" ] && jq -e '.entries | type == "array" and length > 0' "${OUT_JSON_MULTI}" >/dev/null 2>&1; then
    echo "model-health multi monitor warning: ${reason}, keep previous snapshot" >&2
    mirror_snapshot
    return 0
  fi
  jq -n \
    --arg updated_at "${ts}" \
    --arg reason "${reason}" \
    --argjson interval "${CHECK_INTERVAL_SEC}" \
    '{
      updated_at: $updated_at,
      interval_sec: $interval,
      summary: {total: 0, green: 0, yellow: 0, red: 0},
      error: $reason,
      entries: [],
      by_model: {}
    }' >"${OUT_JSON_MULTI}.tmp"
  mv "${OUT_JSON_MULTI}.tmp" "${OUT_JSON_MULTI}"
  mirror_snapshot
}

normalize_prefix_lines() {
  sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed '/^$/d'
}

list_targets_from_config() {
  local config_path="${1:-}"
  if [ -z "${config_path}" ] || [ ! -f "${config_path}" ]; then
    return 0
  fi
  ruby -ryaml -e '
    begin
      cfg = YAML.load_file(ARGV[0]) || {}
    rescue
      cfg = {}
    end
    out = []
    codex = cfg["codex-api-key"]
    codex = [] unless codex.is_a?(Array)
    codex.each do |entry|
      next unless entry.is_a?(Hash)
      prefix = String(entry["prefix"] || "").strip
      out << ["codex", prefix, ""] unless prefix.empty?
    end
    compat = cfg["openai-compatibility"]
    compat = [] unless compat.is_a?(Array)
    compat.each do |entry|
      next unless entry.is_a?(Hash)
      prefix = String(entry["prefix"] || "").strip
      name = String(entry["name"] || "").strip
      next if prefix.empty?
      out << ["openai-compatibility", prefix, name]
    end
    claude = cfg["claude-api-key"]
    claude = [] unless claude.is_a?(Array)
    claude.each do |entry|
      next unless entry.is_a?(Hash)
      prefix = String(entry["prefix"] || "").strip
      out << ["claude", prefix, ""] unless prefix.empty?
    end
    out.each { |row| puts row.join("\t") }
  ' "${config_path}" 2>/dev/null | normalize_prefix_lines
}

resolve_targets() {
  local manual_list config_list
  manual_list="$(
    printf '%s' "${PROVIDER_PREFIX_LIST}" | tr ',' '\n' | normalize_prefix_lines \
      | awk '{print "manual\t" $0 "\t"}'
  )"
  if [ -n "${manual_list}" ]; then
    printf '%s\n' "${manual_list}" | normalize_prefix_lines | awk -F'\t' '!seen[$1 FS $2 FS $3]++'
    return 0
  fi

  config_list="$(list_targets_from_config "${CONFIG_YAML_PATH}" || true)"
  printf '%s\n' "${config_list}" | normalize_prefix_lines | awk -F'\t' '!seen[$1 FS $2 FS $3]++'
}

probe_one_prefix() {
  local prefix="$1"
  local out_json="$2"
  local provider_name="${3:-}"
  local prefix_norm max_attempts attempt total green error_field daily_quota_locked transient_all_red

  prefix_norm="$(printf '%s' "${prefix}" | tr '[:upper:]' '[:lower:]')"
  if [[ "${PROBE_RETRY_COUNT}" =~ ^[0-9]+$ ]]; then
    max_attempts=$((PROBE_RETRY_COUNT + 1))
  else
    max_attempts="${TRANSIENT_ALL_RED_RETRY_MAX}"
    if [ "${prefix_norm}" = "sub2api" ] || [ "${prefix_norm}" = "ee" ]; then
      max_attempts="${SUB2API_RETRY_MAX}"
    fi
    if ! [[ "${max_attempts}" =~ ^[0-9]+$ ]]; then
      max_attempts=3
    fi
  fi
  if [ "${max_attempts}" -lt 1 ]; then
    max_attempts=1
  fi

  attempt=1
  while [ "${attempt}" -le "${max_attempts}" ]; do
    (
      export MANAGEMENT_URL MANAGEMENT_KEY CONFIG_YAML_PATH
      export PROVIDER_PREFIX="${prefix}"
      export PROVIDER_NAME="${provider_name}"
      export OUT_JSON="${out_json}"
      export OUT_AUTH_JSON=""
      export CHECK_INTERVAL_SEC
      export HTTP_TIMEOUT_SEC="${MULTI_HTTP_TIMEOUT_SEC}"
      export CURL_CONNECT_TIMEOUT_SEC="${MULTI_CURL_CONNECT_TIMEOUT_SEC}"
      export CHECK_MODE CHECK_VIA_PROXY PROXY_RESPONSES_URL PROXY_API_KEY PROXY_CLAUDE_MESSAGES_URL
      export DAILY_QUOTA_COOLDOWN_ENABLED DAILY_QUOTA_RESUME_TZ DAILY_QUOTA_RESUME_HOUR DAILY_QUOTA_RESUME_MINUTE DAILY_QUOTA_LOCKS_FILE
      export MAX_MODELS_PER_RUN IMAGE_URL TASK_TEXT TEXT_FALLBACK_TASK_TEXT MODEL_HEALTH_RETAINED_MODELS MODEL_HEALTH_RETAINED_MODELS_FILE
      export OPENCLAW_SYNC_ENABLED=false MODEL_HEALTH_SKIP_CONFIG_SOURCE=true
      "${SINGLE_MONITOR_SCRIPT}" --once
    ) >/dev/null 2>&1

    if [ -s "${out_json}" ] && jq -e '.summary | type == "object"' "${out_json}" >/dev/null 2>&1; then
      total="$(jq -r '.summary.total // 0' "${out_json}" 2>/dev/null || echo 0)"
      green="$(jq -r '.summary.green // 0' "${out_json}" 2>/dev/null || echo 0)"
      error_field="$(jq -r '.error // empty' "${out_json}" 2>/dev/null || echo '')"
      daily_quota_locked="$(
        jq -r '[.models[]?.reason // "" | tostring | startswith("daily_quota_exhausted_until_")] | any' "${out_json}" 2>/dev/null || echo false
      )"
      transient_all_red="$(
        jq -r '
          (.summary.total // 0) > 0
          and (.summary.green // 0) == 0
          and (
            [.models[]? | (((.error // "") + " " + (.reason // "")) | ascii_downcase)
              | test("token invalida|timeout|timed out|network_error|network error|http_5|service_busy|retry shortly|temporarily unavailable")
            ] | any
          )
        ' "${out_json}" 2>/dev/null || echo false
      )"

      if [ "${daily_quota_locked}" = "true" ]; then
        return 0
      fi

      if [ "${total}" -gt 0 ]; then
        if [ "${green}" -gt 0 ] || [ -n "${error_field}" ]; then
          return 0
        fi
        if [ "${attempt}" -ge "${max_attempts}" ] || [ "${transient_all_red}" != "true" ]; then
          return 0
        fi
      fi
    fi

    if [ "${attempt}" -lt "${max_attempts}" ]; then
      local retry_sleep_sec
      retry_sleep_sec="${RETRY_SLEEP_SEC}"
      if [[ "${PROBE_MAX_RETRY_WAIT_SEC}" =~ ^[0-9]+$ ]] && [ "${PROBE_MAX_RETRY_WAIT_SEC}" -gt 0 ] && [ "${retry_sleep_sec}" -gt "${PROBE_MAX_RETRY_WAIT_SEC}" ]; then
        retry_sleep_sec="${PROBE_MAX_RETRY_WAIT_SEC}"
      fi
      echo "model-health multi monitor retry: prefix=${prefix} attempt=${attempt}/${max_attempts}" >&2
      sleep "${retry_sleep_sec}"
    fi
    attempt=$((attempt + 1))
  done

  # Keep the last probe output even if all-red; it's still a real result.
  [ -s "${out_json}" ]
}

build_multi_snapshot() {
  local ts="$1"
  shift
  jq -s \
    --arg updated_at "${ts}" \
    --argjson interval "${CHECK_INTERVAL_SEC}" \
    '
    def safe_models: (.models // []);
    def safe_summary:
      .summary // {
        total: (safe_models | length),
        green: (safe_models | map(select(.status == "green")) | length),
        yellow: (safe_models | map(select(.status == "yellow")) | length),
        red: (safe_models | map(select(.status == "red")) | length)
      };

    {
      updated_at: $updated_at,
      interval_sec: $interval,
      entries: (
        map({
          updated_at: (.updated_at // $updated_at),
          provider_prefix: (.provider_prefix // ""),
          base_url: (.base_url // ""),
          supported_protocols: (.supported_protocols // ""),
          source_mode: (.source_mode // ""),
          summary: safe_summary,
          models: safe_models
        })
      ),
      by_model: (
        reduce (map(safe_models) | add | .[]) as $m ({}; .[$m.model] = $m)
      )
    }
    | .summary = {
        total: (.entries | map(.summary.total // 0) | add // 0),
        green: (.entries | map(.summary.green // 0) | add // 0),
        yellow: (.entries | map(.summary.yellow // 0) | add // 0),
        red: (.entries | map(.summary.red // 0) | add // 0)
      }
    ' "$@"
}

check_once() {
  local ts tmp_dir targets_raw
  ts="$(now_iso)"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "${tmp_dir}"' RETURN

  targets_raw="$(resolve_targets)"
  if [ -z "${targets_raw}" ]; then
    write_error_snapshot "provider_prefix_list_empty"
    return 1
  fi

  local files=()
  while IFS=$'\t' read -r target_kind prefix provider_name; do
    [ -n "${prefix}" ] || continue
    local safe_name out_json
    safe_name="$(printf '%s' "${provider_name:-${prefix}}" | tr '/:@ ' '____')"
    out_json="${tmp_dir}/${safe_name}.json"
    if probe_one_prefix "${prefix}" "${out_json}" "${provider_name}" && [ -s "${out_json}" ]; then
      files+=("${out_json}")
    fi
  done <<<"${targets_raw}"

  if [ "${#files[@]}" -eq 0 ]; then
    write_error_snapshot "all_prefix_probe_failed"
    return 1
  fi

  build_multi_snapshot "${ts}" "${files[@]}" >"${OUT_JSON_MULTI}.tmp"
  mv "${OUT_JSON_MULTI}.tmp" "${OUT_JSON_MULTI}"
  mirror_snapshot
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
