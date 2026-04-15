#!/usr/bin/env bash
set -euo pipefail

PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/model-health-monitor.pid}"
WRAPPER_SCRIPT="${WRAPPER_SCRIPT:-/opt/cli-proxy-api/scripts/model-health-hourly-wrapper.sh}"
MONITOR_SCRIPT="${MONITOR_SCRIPT:-/opt/cli-proxy-api/scripts/hourly-model-health-monitor.sh}"

if [ ! -f "${PID_FILE}" ]; then
  true
fi

pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
  kill "${pid}" || true
fi

for p in $(pgrep -f "^bash ${WRAPPER_SCRIPT}\$" || true); do
  kill "${p}" || true
done

for p in $(pgrep -f "^bash ${MONITOR_SCRIPT}( |$)" || true); do
  kill "${p}" || true
done

sleep 0.2

rm -f "${PID_FILE}"
echo "model-health scheduler stopped"
