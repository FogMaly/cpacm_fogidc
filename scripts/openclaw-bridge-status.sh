#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${ENV_FILE:-/opt/cli-proxy-api/scripts/openclaw-bridge.env}"
PID_FILE="${PID_FILE:-/opt/cli-proxy-api/logs/openclaw-bridge.pid}"
LOG_FILE="${LOG_FILE:-/opt/cli-proxy-api/logs/openclaw-bridge.log}"

if [ -f "${ENV_FILE}" ]; then
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
fi

host="${BRIDGE_HOST:-127.0.0.1}"
port="${BRIDGE_PORT:-35100}"

if [ -f "${PID_FILE}" ]; then
  pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    echo "bridge: running (pid=${pid})"
  else
    echo "bridge: stopped (stale pid file)"
  fi
else
  echo "bridge: stopped"
fi

echo "listen: http://${host}:${port}"

if [ -f "${LOG_FILE}" ]; then
  echo "log: ${LOG_FILE}"
fi
