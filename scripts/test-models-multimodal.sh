#!/usr/bin/env bash
set -euo pipefail

# Config (can be overridden by environment variables)
PROXY_BASE="${PROXY_BASE:-http://127.0.0.1:34050}"
API_KEY="${API_KEY:-}"
MODEL_PREFIX="${MODEL_PREFIX:-yunyi-codex/}"
MODELS_CSV="${MODELS_CSV:-}"
TASK_FILE="${TASK_FILE:-/opt/cli-proxy-api/scripts/multimodal-task.txt}"
TASK_TEXT="${TASK_TEXT:-}"
IMAGE_URL="${IMAGE_URL:-}"
MAX_TOKENS="${MAX_TOKENS:-260}"
TIMEOUT_SEC="${TIMEOUT_SEC:-45}"
OUTPUT_TSV="${OUTPUT_TSV:-/tmp/yunyi_multimodal_compare.tsv}"
OUTPUT_MD="${OUTPUT_MD:-/tmp/yunyi_multimodal_compare.md}"

if [ -z "${API_KEY}" ]; then
  echo "ERROR: API_KEY is required." >&2
  exit 1
fi

if [ -n "${TASK_TEXT}" ]; then
  task="${TASK_TEXT}"
elif [ -f "${TASK_FILE}" ]; then
  task="$(cat "${TASK_FILE}")"
else
  echo "ERROR: TASK_FILE not found: ${TASK_FILE}" >&2
  exit 1
fi

if [ -n "${MODELS_CSV}" ]; then
  IFS=',' read -r -a models <<<"${MODELS_CSV}"
else
  mapfile -t models < <(
    curl -sS "${PROXY_BASE}/v1/models" \
      -H "Authorization: Bearer ${API_KEY}" \
      | jq -r '.data[].id' \
      | rg "^${MODEL_PREFIX}" \
      | sort -u
  )
fi

if [ "${#models[@]}" -eq 0 ]; then
  echo "ERROR: no models found for prefix '${MODEL_PREFIX}'" >&2
  exit 1
fi

printf 'model\thttp\tresponse_model\tfinish_reason\tusage\treply\terror\n' >"${OUTPUT_TSV}"

for model in "${models[@]}"; do
  if [ -n "${IMAGE_URL}" ]; then
    payload="$(jq -nc \
      --arg m "${model}" \
      --arg t "${task}" \
      --arg i "${IMAGE_URL}" \
      --argjson mt "${MAX_TOKENS}" \
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
  else
    payload="$(jq -nc \
      --arg m "${model}" \
      --arg t "${task}" \
      --argjson mt "${MAX_TOKENS}" \
      '{
        model: $m,
        messages: [{role: "user", content: $t}],
        temperature: 0.2,
        max_tokens: $mt
      }'
    )"
  fi

  resp="$(
    curl -sS -m "${TIMEOUT_SEC}" -w '\n__HTTP__%{http_code}' \
      "${PROXY_BASE}/v1/chat/completions" \
      -H "Authorization: Bearer ${API_KEY}" \
      -H 'Content-Type: application/json' \
      -d "${payload}" || true
  )"

  http="$(printf '%s' "${resp}" | sed -n 's/.*__HTTP__\([0-9][0-9][0-9]\)$/\1/p')"
  body="$(printf '%s' "${resp}" | sed 's/__HTTP__[0-9][0-9][0-9]$//')"
  [ -n "${http}" ] || http="000"

  response_model="$(printf '%s' "${body}" | jq -r '.model // empty' 2>/dev/null || true)"
  finish_reason="$(printf '%s' "${body}" | jq -r '.choices[0].finish_reason // empty' 2>/dev/null || true)"
  usage="$(printf '%s' "${body}" | jq -r '[.usage.prompt_tokens,.usage.completion_tokens,.usage.total_tokens] | map(select(.!=null)) | join("/")' 2>/dev/null || true)"
  reply="$(printf '%s' "${body}" | jq -r '.choices[0].message.content // empty' 2>/dev/null || true)"
  err="$(printf '%s' "${body}" | jq -r '.error.message // .message // empty' 2>/dev/null || true)"

  reply="$(printf '%s' "${reply}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-200)"
  err="$(printf '%s' "${err}" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | cut -c1-200)"

  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "${model}" "${http}" "${response_model}" "${finish_reason}" "${usage}" "${reply}" "${err}" \
    >>"${OUTPUT_TSV}"

  sleep 0.5
done

ok_count="$(awk -F'\t' 'NR>1 && $2=="200"{c++} END{print c+0}' "${OUTPUT_TSV}")"
total_count="$(awk 'END{print NR-1}' "${OUTPUT_TSV}")"

{
  echo "| model | http | response_model | finish_reason | usage(p/c/t) | reply(截断) |"
  echo "|---|---:|---|---|---|---|"
  awk -F'\t' 'NR>1 {printf("| %s | %s | %s | %s | %s | %s |\n",$1,$2,($3==""?"-":$3),($4==""?"-":$4),($5==""?"-":$5),($6==""?"-":$6))}' "${OUTPUT_TSV}"
  echo
  echo "Total: ${total_count}, HTTP 200: ${ok_count}"
  if [ -n "${IMAGE_URL}" ]; then
    echo "Mode: multimodal (text + image_url)"
  else
    echo "Mode: text-only composite task"
  fi
} >"${OUTPUT_MD}"

echo "Done. total=${total_count} http200=${ok_count}"
echo "TSV: ${OUTPUT_TSV}"
echo "MD : ${OUTPUT_MD}"
