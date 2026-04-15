#!/usr/bin/env bash
set -euo pipefail

CONFIG_FILE="${CONFIG_FILE:-/opt/cli-proxy-api/scripts/model-health.env}"
LOG_FILE="${LOG_FILE:-/opt/cli-proxy-api/logs/model-health-hourly.log}"
PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/model-health-hourly-wrapper.pid}"
SINGLE_MONITOR_SCRIPT="${SINGLE_MONITOR_SCRIPT:-/opt/cli-proxy-api/scripts/hourly-model-health-monitor.sh}"
MULTI_MONITOR_SCRIPT="${MULTI_MONITOR_SCRIPT:-/opt/cli-proxy-api/scripts/hourly-model-health-monitor-multi.sh}"
RUN_SINGLE_MONITOR="${RUN_SINGLE_MONITOR:-false}"
ALIGN_TO_CLOCK_HOUR="${ALIGN_TO_CLOCK_HOUR:-true}"
RUN_IMMEDIATE_ON_START="${RUN_IMMEDIATE_ON_START:-false}"
OUT_JSON="${OUT_JSON:-/opt/cli-proxy-api/static/model-health.json}"
OUT_AUTH_JSON="${OUT_AUTH_JSON:-/opt/cli-proxy-api/auths/model-health.json}"
OUT_JSON_MULTI="${OUT_JSON_MULTI:-/opt/cli-proxy-api/static/model-health-multi.json}"
OUT_AUTH_JSON_MULTI="${OUT_AUTH_JSON_MULTI:-/opt/cli-proxy-api/auths/model-health-multi.json}"

mkdir -p "$(dirname "${LOG_FILE}")"
mkdir -p "$(dirname "${OUT_JSON}")"
mkdir -p "$(dirname "${OUT_JSON_MULTI}")"

if [ -f "${CONFIG_FILE}" ]; then
  set -a
  # shellcheck disable=SC1090
  source "${CONFIG_FILE}"
  set +a
fi

INTERVAL="${CHECK_INTERVAL_SEC:-3600}"
echo "$$" > "${PID_FILE}"

now_iso() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

log_event() {
  local message="$1"
  echo "[$(now_iso)] ${message}" >>"${LOG_FILE}"
}

snapshot_valid_single() {
  local file="$1"
  [ -f "${file}" ] || return 1
  jq -e '
    type == "object"
    and (.summary | type == "object")
    and (
      (.models | type == "array")
      or (.entries | type == "array")
    )
  ' "${file}" >/dev/null 2>&1
}

snapshot_valid_multi() {
  local file="$1"
  [ -f "${file}" ] || return 1
  jq -e '
    type == "object"
    and (.summary | type == "object")
    and (.entries | type == "array")
    and ((.entries | length) > 0)
  ' "${file}" >/dev/null 2>&1
}

restore_snapshot_if_needed() {
  local label="$1"
  local dst="$2"
  local src="$3"
  local mode="$4"

  if [ -z "${src}" ] || [ ! -f "${src}" ]; then
    return 0
  fi

  case "${mode}" in
    single)
      if snapshot_valid_single "${dst}"; then
        return 0
      fi
      if ! snapshot_valid_single "${src}"; then
        return 0
      fi
      ;;
    multi)
      if snapshot_valid_multi "${dst}"; then
        return 0
      fi
      if ! snapshot_valid_multi "${src}"; then
        return 0
      fi
      ;;
    *)
      return 0
      ;;
  esac

  cp "${src}" "${dst}" 2>/dev/null || return 0
  log_event "snapshot_restored label=${label} src=${src} dst=${dst}"
}

run_one_round() {
  local run_single
  log_event "probe_round_start interval_sec=${INTERVAL} align_to_hour=${ALIGN_TO_CLOCK_HOUR}"
  run_single="$(printf '%s' "${RUN_SINGLE_MONITOR}" | tr '[:upper:]' '[:lower:]')"
  case "${run_single}" in
    1|true|yes|on)
      "${SINGLE_MONITOR_SCRIPT}" --once >>"${LOG_FILE}" 2>&1 || true
      ;;
  esac
  "${MULTI_MONITOR_SCRIPT}" --once >>"${LOG_FILE}" 2>&1 || true
  log_event "probe_round_end"
}

should_enable() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

sleep_until_next_hour() {
  local now_epoch next_hour_epoch sleep_sec
  now_epoch="$(date +%s)"
  next_hour_epoch="$(( ((now_epoch / 3600) + 1) * 3600 ))"
  sleep_sec="$((next_hour_epoch - now_epoch))"
  if [ "${sleep_sec}" -lt 1 ]; then
    sleep_sec=1
  fi
  log_event "sleep_to_next_hour seconds=${sleep_sec}"
  sleep "${sleep_sec}"
}

if should_enable "${RUN_IMMEDIATE_ON_START}"; then
  log_event "run_immediate_on_start=true"
  restore_snapshot_if_needed "single" "${OUT_JSON}" "${OUT_AUTH_JSON}" "single"
  restore_snapshot_if_needed "multi" "${OUT_JSON_MULTI}" "${OUT_AUTH_JSON_MULTI}" "multi"
  run_one_round
fi

while true; do
  restore_snapshot_if_needed "single" "${OUT_JSON}" "${OUT_AUTH_JSON}" "single"
  restore_snapshot_if_needed "multi" "${OUT_JSON_MULTI}" "${OUT_AUTH_JSON_MULTI}" "multi"
  if should_enable "${ALIGN_TO_CLOCK_HOUR}" && [ "${INTERVAL}" -eq 3600 ]; then
    sleep_until_next_hour
  else
    sleep "${INTERVAL}"
  fi
  run_one_round
done
