#!/usr/bin/env bash
set -euo pipefail

CONFIG_FILE="${CONFIG_FILE:-/opt/cli-proxy-api/scripts/model-health.env}"
PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/model-health-monitor.pid}"
LOG_FILE="${LOG_FILE:-/opt/cli-proxy-api/logs/model-health-monitor.log}"
WRAPPER_SCRIPT="${WRAPPER_SCRIPT:-/opt/cli-proxy-api/scripts/model-health-hourly-wrapper.sh}"

mkdir -p "$(dirname "${PID_FILE}")"
mkdir -p "$(dirname "${LOG_FILE}")"

existing_pid="$(pgrep -f "^bash ${WRAPPER_SCRIPT}\$" | head -n 1 || true)"
if [ -n "${existing_pid}" ] && kill -0 "${existing_pid}" 2>/dev/null; then
  echo "${existing_pid}" > "${PID_FILE}"
  echo "model-health scheduler already running (pid=${existing_pid})"
  exit 0
fi

if [ -f "${PID_FILE}" ]; then
  pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    echo "model-health scheduler already running (pid=${pid})"
    exit 0
  fi
fi

if [ -f "${CONFIG_FILE}" ]; then
  set -a
  # shellcheck disable=SC1090
  source "${CONFIG_FILE}"
  set +a
fi

new_pid=""
if command -v setsid >/dev/null 2>&1; then
  setsid -f "${WRAPPER_SCRIPT}" >>"${LOG_FILE}" 2>&1 || true
  sleep 0.3
  new_pid="$(pgrep -f "^bash ${WRAPPER_SCRIPT}\$" | head -n 1 || true)"
else
  nohup "${WRAPPER_SCRIPT}" >>"${LOG_FILE}" 2>&1 &
  new_pid="$!"
  sleep 0.3
fi

if [ -n "${new_pid}" ] && kill -0 "${new_pid}" 2>/dev/null; then
  echo "${new_pid}" > "${PID_FILE}"
  echo "model-health scheduler started (pid=${new_pid})"
  exit 0
fi

echo "failed to start model-health scheduler" >&2
exit 1
