#!/usr/bin/env bash
set -euo pipefail

PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/model-health-monitor.pid}"
OUT_JSON="${OUT_JSON:-/opt/cli-proxy-api/static/model-health.json}"
OUT_JSON_MULTI="${OUT_JSON_MULTI:-/opt/cli-proxy-api/static/model-health-multi.json}"
WRAPPER_SCRIPT="${WRAPPER_SCRIPT:-/opt/cli-proxy-api/scripts/model-health-hourly-wrapper.sh}"
LOG_FILE="${LOG_FILE:-/opt/cli-proxy-api/logs/model-health-hourly.log}"

if [ -f "${PID_FILE}" ]; then
  pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    echo "scheduler: running (pid=${pid})"
  else
    live_pid="$(pgrep -f "^bash ${WRAPPER_SCRIPT}\$" | head -n 1 || true)"
    if [ -n "${live_pid}" ] && kill -0 "${live_pid}" 2>/dev/null; then
      echo "${live_pid}" > "${PID_FILE}"
      echo "scheduler: running (pid=${live_pid})"
    else
      echo "scheduler: stopped (stale pid file)"
    fi
  fi
else
  live_pid="$(pgrep -f "^bash ${WRAPPER_SCRIPT}\$" | head -n 1 || true)"
  if [ -n "${live_pid}" ] && kill -0 "${live_pid}" 2>/dev/null; then
    echo "${live_pid}" > "${PID_FILE}"
    echo "scheduler: running (pid=${live_pid})"
  else
    echo "scheduler: stopped"
  fi
fi

if [ -f "${OUT_JSON}" ]; then
  echo "snapshot: ${OUT_JSON}"
  jq -r '[
    "updated_at=" + (.updated_at // ""),
    "provider_prefix=" + (.provider_prefix // ""),
    "summary(total/green/yellow/red)=" + ((.summary.total|tostring) + "/" + (.summary.green|tostring) + "/" + (.summary.yellow|tostring) + "/" + (.summary.red|tostring))
  ] | .[]' "${OUT_JSON}" 2>/dev/null || true
else
  echo "snapshot: not generated yet (${OUT_JSON})"
fi

if [ -f "${OUT_JSON_MULTI}" ]; then
  echo "snapshot-multi: ${OUT_JSON_MULTI}"
  jq -r '[
    "updated_at=" + (.updated_at // ""),
    "summary(total/green/yellow/red)=" + ((.summary.total|tostring) + "/" + (.summary.green|tostring) + "/" + (.summary.yellow|tostring) + "/" + (.summary.red|tostring))
  ] | .[]' "${OUT_JSON_MULTI}" 2>/dev/null || true
else
  echo "snapshot-multi: not generated yet (${OUT_JSON_MULTI})"
fi

if [ -f "${LOG_FILE}" ]; then
  echo "recent-rounds: ${LOG_FILE}"
  grep -E "probe_round_start|probe_round_end|sleep_to_next_hour" "${LOG_FILE}" | tail -n 12 || true
fi
