#!/usr/bin/env bash
set -euo pipefail

PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/openclaw-bridge.pid}"

stop_pid() {
  local pid="$1"
  if [ -z "${pid}" ]; then
    return 1
  fi
  if kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    sleep 0.2
    if kill -0 "${pid}" 2>/dev/null; then
      kill -9 "${pid}" 2>/dev/null || true
    fi
    return 0
  fi
  return 1
}

stopped=0
if [ -f "${PID_FILE}" ]; then
  pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if stop_pid "${pid}"; then
    echo "openclaw bridge stopped (pid=${pid})"
    stopped=1
  fi
  rm -f "${PID_FILE}"
fi

while IFS= read -r pid; do
  if stop_pid "${pid}"; then
    echo "openclaw bridge stopped (pid=${pid})"
    stopped=1
  fi
done < <(pgrep -f '^node /opt/cli-proxy-api/scripts/openclaw-bridge.js$' || true)

if [ "${stopped}" -eq 0 ]; then
  echo "openclaw bridge not running"
fi
