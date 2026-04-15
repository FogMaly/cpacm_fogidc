#!/usr/bin/env node

/*
Local OAuth relay helper for remote WebUI login.

Usage:
  CPAPI_RELAY_BASE="http://<server-host>:34050" node scripts/oauth-local-relay.js

Optional env:
  CPAPI_LOCAL_HOST   (default: 127.0.0.1)
  CPAPI_LOCAL_PORT   (default: 51121)
  CPAPI_CALLBACK_PATH (default: /oauth-callback)
*/

const http = require("http");
const { URL } = require("url");

const relayBaseRaw = (process.env.CPAPI_RELAY_BASE || process.argv[2] || "").trim();
if (!relayBaseRaw) {
  console.error("CPAPI_RELAY_BASE is required. Example:");
  console.error('  CPAPI_RELAY_BASE="http://127.0.0.1:34050" node scripts/oauth-local-relay.js');
  process.exit(1);
}

let relayBase;
try {
  relayBase = new URL(relayBaseRaw);
} catch (err) {
  console.error(`Invalid CPAPI_RELAY_BASE: ${relayBaseRaw}`);
  process.exit(1);
}

const host = (process.env.CPAPI_LOCAL_HOST || "127.0.0.1").trim() || "127.0.0.1";
const port = Number.parseInt(process.env.CPAPI_LOCAL_PORT || "51121", 10);
const callbackPath = (process.env.CPAPI_CALLBACK_PATH || "/oauth-callback").trim() || "/oauth-callback";
const managementKey = (process.env.CPAPI_MANAGEMENT_KEY || "").trim();
const defaultProvider = (process.env.CPAPI_OAUTH_PROVIDER || "antigravity").trim() || "antigravity";

if (!Number.isFinite(port) || port <= 0 || port > 65535) {
  console.error(`Invalid CPAPI_LOCAL_PORT: ${process.env.CPAPI_LOCAL_PORT || ""}`);
  process.exit(1);
}

function sendText(res, statusCode, body) {
  res.writeHead(statusCode, {
    "Content-Type": "text/plain; charset=utf-8",
    "Cache-Control": "no-store",
  });
  res.end(body);
}

function sendHtml(res, statusCode, title, message, targetURL) {
  const safeTitle = String(title || "OAuth relay").replace(/[<>&]/g, "");
  const safeMessage = String(message || "").replace(/[<>&]/g, "");
  const target = String(targetURL || `${relayBase.origin}/management.html`);
  const html = `<!doctype html><html><head><meta charset="utf-8"><title>${safeTitle}</title><script>
setTimeout(function(){ try { window.location.replace(${JSON.stringify(target)}); } catch(_) { window.location.href = ${JSON.stringify(target)}; } }, 500);
</script></head><body><h1>${safeTitle}</h1><p>${safeMessage}</p><p><a href="${target}">Back to management</a></p></body></html>`;
  res.writeHead(statusCode, {
    "Content-Type": "text/html; charset=utf-8",
    "Cache-Control": "no-store",
  });
  res.end(html);
}

async function postJSON(targetURL, body, headers) {
  const response = await fetch(targetURL, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  });
  const text = await response.text().catch(() => "");
  let payload = {};
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch (err) {}
  }
  return { response, payload, text };
}

async function submitCallbackToServer(callbackURL, provider) {
  const payload = { provider, redirect_url: callbackURL };

  const relayEndpoint = new URL("/oauth-relay", relayBase.origin).toString();
  const relayResp = await postJSON(relayEndpoint, payload, {
    "Content-Type": "application/json",
    Accept: "application/json",
  });
  if (relayResp.response.ok) {
    return { ok: true, mode: "relay", detail: "ok" };
  }
  if (relayResp.response.status !== 404 && relayResp.response.status !== 405) {
    const msg = relayResp.payload.error || relayResp.payload.message || relayResp.text || `HTTP ${relayResp.response.status}`;
    return { ok: false, mode: "relay", detail: msg };
  }

  if (!managementKey) {
    return { ok: false, mode: "relay", detail: "oauth-relay unavailable and CPAPI_MANAGEMENT_KEY is not set" };
  }

  const legacyEndpoint = new URL("/v0/management/oauth-callback", relayBase.origin).toString();
  const legacyResp = await postJSON(legacyEndpoint, payload, {
    "Content-Type": "application/json",
    Accept: "application/json",
    Authorization: `Bearer ${managementKey}`,
    "X-Management-Key": managementKey,
  });
  if (legacyResp.response.ok) {
    return { ok: true, mode: "legacy", detail: "ok" };
  }
  const legacyMsg = legacyResp.payload.error || legacyResp.payload.message || legacyResp.text || `HTTP ${legacyResp.response.status}`;
  return { ok: false, mode: "legacy", detail: legacyMsg };
}

const server = http.createServer(async (req, res) => {
  let incoming;
  try {
    incoming = new URL(req.url || "/", `http://${host}:${port}`);
  } catch (err) {
    sendText(res, 400, "Bad request");
    return;
  }

  if (incoming.pathname !== callbackPath) {
    sendText(
      res,
      200,
      `OAuth relay helper is running on http://${host}:${port}${callbackPath}\n` +
        `Relay target: ${relayBase.origin}/oauth-relay\n` +
        `Provider(default): ${defaultProvider}\n` +
        `Fallback legacy endpoint: ${relayBase.origin}/v0/management/oauth-callback${managementKey ? " (enabled)" : " (disabled: CPAPI_MANAGEMENT_KEY not set)"}\n`
    );
    return;
  }

  const callbackURL = new URL(callbackPath, `http://${host}:${port}`);
  for (const [key, value] of incoming.searchParams.entries()) {
    callbackURL.searchParams.set(key, value);
  }

  const inferredProvider = incoming.searchParams.get("provider");
  const provider = (inferredProvider && inferredProvider.trim()) || defaultProvider;

  try {
    const result = await submitCallbackToServer(callbackURL.toString(), provider);
    if (result.ok) {
      sendHtml(res, 200, "OAuth relay completed", `Callback submitted via ${result.mode}. Redirecting...`, `${relayBase.origin}/management.html`);
      return;
    }
    sendHtml(res, 502, "OAuth relay failed", `Failed to submit callback: ${result.detail}`, `${relayBase.origin}/management.html`);
  } catch (err) {
    sendHtml(res, 500, "OAuth relay failed", `Unexpected error: ${err && err.message ? err.message : "unknown error"}`, `${relayBase.origin}/management.html`);
  }
});

server.listen(port, host, () => {
  console.log(`[oauth-local-relay] listening on http://${host}:${port}${callbackPath}`);
  console.log(`[oauth-local-relay] forwarding to ${relayBase.origin}/oauth-relay`);
  if (!managementKey) {
    console.log("[oauth-local-relay] CPAPI_MANAGEMENT_KEY not set; legacy fallback is disabled.");
  }
});

server.on("error", (err) => {
  console.error(`[oauth-local-relay] failed: ${err.message}`);
  process.exit(1);
});
