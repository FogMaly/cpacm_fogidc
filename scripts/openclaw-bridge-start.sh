#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${ENV_FILE:-/opt/cli-proxy-api/scripts/openclaw-bridge.env}"
PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/openclaw-bridge.pid}"
LOG_FILE="${LOG_FILE:-/opt/cli-proxy-api/logs/openclaw-bridge.log}"
SCRIPT="${SCRIPT:-/opt/cli-proxy-api/scripts/openclaw-bridge.js}"

mkdir -p "$(dirname "${PID_FILE}")"
mkdir -p "$(dirname "${LOG_FILE}")"

existing_pid="$(pgrep -f '^node /opt/cli-proxy-api/scripts/openclaw-bridge.js$' | head -n 1 || true)"
if [ -n "${existing_pid}" ] && kill -0 "${existing_pid}" 2>/dev/null; then
  echo "${existing_pid}" > "${PID_FILE}"
  echo "openclaw bridge already running (pid=${existing_pid})"
  exit 0
fi

if [ -f "${PID_FILE}" ]; then
  pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    echo "openclaw bridge already running (pid=${pid})"
    exit 0
  fi
fi

if [ -f "${ENV_FILE}" ]; then
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
fi

nohup node "${SCRIPT}" >>"${LOG_FILE}" 2>&1 &
new_pid="$!"
echo "${new_pid}" > "${PID_FILE}"
sleep 0.3
if kill -0 "${new_pid}" 2>/dev/null; then
  echo "openclaw bridge started (pid=${new_pid})"
  exit 0
fi

rm -f "${PID_FILE}"
echo "failed to start openclaw bridge" >&2
exit 1
