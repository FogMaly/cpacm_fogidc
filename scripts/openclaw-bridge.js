#!/usr/bin/env node
"use strict";

const fs = require("fs");
const http = require("http");
const { URL } = require("url");

const cfg = {
  host: (process.env.BRIDGE_HOST || "127.0.0.1").trim() || "127.0.0.1",
  port: Number.parseInt(process.env.BRIDGE_PORT || "35100", 10),
  bridgeToken: (process.env.BRIDGE_TOKEN || "").trim(),
  cpapiBaseURL: (process.env.CPAPI_BASE_URL || "http://127.0.0.1:34050").trim(),
  cpapiManagementKey: (process.env.CPAPI_MANAGEMENT_KEY || "").trim(),
  openclawBaseURL: (process.env.OPENCLAW_BASE_URL || "").trim(),
  openclawToken: (process.env.OPENCLAW_TOKEN || "").trim(),
  openclawAuthMode: ((process.env.OPENCLAW_AUTH_MODE || "bearer").trim() || "bearer").toLowerCase(),
  openclawAgentPath: (process.env.OPENCLAW_AGENT_PATH || "/hooks/agent").trim() || "/hooks/agent",
  openclawWakePath: (process.env.OPENCLAW_WAKE_PATH || "/hooks/wake").trim() || "/hooks/wake",
  modelHealthPath: (process.env.MODEL_HEALTH_JSON || "/opt/cli-proxy-api/static/model-health.json").trim(),
  httpTimeoutMs: Number.parseInt(process.env.HTTP_TIMEOUT_MS || "20000", 10),
};

if (!Number.isFinite(cfg.port) || cfg.port <= 0 || cfg.port > 65535) {
  console.error(`[openclaw-bridge] invalid BRIDGE_PORT: ${process.env.BRIDGE_PORT || ""}`);
  process.exit(1);
}

function sendJSON(res, statusCode, payload) {
  const body = JSON.stringify(payload);
  res.writeHead(statusCode, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
  });
  res.end(body);
}

function readSnapshot() {
  const raw = fs.readFileSync(cfg.modelHealthPath, "utf8");
  return JSON.parse(raw);
}

function normalizePath(path) {
  if (!path) return "/";
  return path.startsWith("/") ? path : `/${path}`;
}

function extractAuthToken(req) {
  const headerAuth = String(req.headers.authorization || "").trim();
  if (headerAuth) {
    const parts = headerAuth.split(/\s+/, 2);
    if (parts.length === 2 && parts[0].toLowerCase() === "bearer") {
      return parts[1].trim();
    }
    return headerAuth;
  }
  const xToken = String(req.headers["x-openclaw-token"] || "").trim();
  return xToken;
}

function ensureBridgeAuth(req, res) {
  if (!cfg.bridgeToken) return true;
  const token = extractAuthToken(req);
  if (token && token === cfg.bridgeToken) return true;
  sendJSON(res, 401, { error: "unauthorized" });
  return false;
}

async function readRequestJSON(req) {
  const chunks = [];
  for await (const chunk of req) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  const raw = Buffer.concat(chunks).toString("utf8").trim();
  if (!raw) return {};
  return JSON.parse(raw);
}

async function requestJSON(url, options) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), cfg.httpTimeoutMs);
  try {
    const response = await fetch(url, { ...options, signal: controller.signal });
    const text = await response.text().catch(() => "");
    let data = {};
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (_) {
        data = { raw: text };
      }
    }
    return { ok: response.ok, status: response.status, data };
  } finally {
    clearTimeout(timer);
  }
}

function cpapiHeaders() {
  const h = { Accept: "application/json" };
  if (!cfg.cpapiManagementKey) return h;
  h.Authorization = `Bearer ${cfg.cpapiManagementKey}`;
  h["X-Management-Key"] = cfg.cpapiManagementKey;
  return h;
}

async function callCpapi(path, method = "GET", body) {
  const base = cfg.cpapiBaseURL.replace(/\/+$/, "");
  const target = `${base}${normalizePath(path)}`;
  const headers = cpapiHeaders();
  let payload;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }
  return requestJSON(target, { method, headers, body: payload });
}

function openclawHeaders(mode) {
  const h = {
    Accept: "application/json",
    "Content-Type": "application/json",
  };
  if (!cfg.openclawToken) return h;
  if (mode === "x-token") {
    h["x-openclaw-token"] = cfg.openclawToken;
  } else {
    h.Authorization = `Bearer ${cfg.openclawToken}`;
  }
  return h;
}

async function callOpenclaw(path, body) {
  if (!cfg.openclawBaseURL) {
    return { ok: false, status: 500, data: { error: "OPENCLAW_BASE_URL is empty" } };
  }
  const base = cfg.openclawBaseURL.replace(/\/+$/, "");
  const target = `${base}${normalizePath(path)}`;
  const primary = cfg.openclawAuthMode === "x-token" ? "x-token" : "bearer";
  const fallback = primary === "bearer" ? "x-token" : "bearer";

  const one = await requestJSON(target, {
    method: "POST",
    headers: openclawHeaders(primary),
    body: JSON.stringify(body || {}),
  });
  if (one.ok) return one;
  if (one.status === 401 || one.status === 403) {
    const two = await requestJSON(target, {
      method: "POST",
      headers: openclawHeaders(fallback),
      body: JSON.stringify(body || {}),
    });
    return two;
  }
  return one;
}

function buildModelHealthEnvelope(snapshot) {
  return {
    source: "cliproxyapi",
    event: "model_health_snapshot",
    timestamp: new Date().toISOString(),
    payload: snapshot || {},
  };
}

const server = http.createServer(async (req, res) => {
  let incoming;
  try {
    incoming = new URL(req.url || "/", `http://${cfg.host}:${cfg.port}`);
  } catch (_) {
    sendJSON(res, 400, { error: "bad request" });
    return;
  }

  if (req.method === "GET" && incoming.pathname === "/health") {
    sendJSON(res, 200, {
      ok: true,
      service: "openclaw-bridge",
      time: new Date().toISOString(),
      cpapi_base_url: cfg.cpapiBaseURL,
      openclaw_base_url: cfg.openclawBaseURL || "",
    });
    return;
  }

  if (!ensureBridgeAuth(req, res)) return;

  try {
    if (req.method === "GET" && incoming.pathname === "/api/model-health") {
      const snapshot = readSnapshot();
      sendJSON(res, 200, snapshot);
      return;
    }

    if (req.method === "GET" && incoming.pathname === "/api/cpapi/usage") {
      const out = await callCpapi("/v0/management/usage");
      sendJSON(res, out.status || 500, out.data || {});
      return;
    }

    if (req.method === "GET" && incoming.pathname === "/api/cpapi/codex-api-key") {
      const out = await callCpapi("/v0/management/codex-api-key");
      sendJSON(res, out.status || 500, out.data || {});
      return;
    }

    if (req.method === "PUT" && incoming.pathname === "/api/cpapi/codex-api-key") {
      const body = await readRequestJSON(req);
      const out = await callCpapi("/v0/management/codex-api-key", "PUT", body);
      sendJSON(res, out.status || 500, out.data || {});
      return;
    }

    if (req.method === "POST" && incoming.pathname === "/api/openclaw/push/model-health") {
      let body = {};
      try {
        body = await readRequestJSON(req);
      } catch (_) {
        body = {};
      }
      let snapshot;
      if (body && body.payload && typeof body.payload === "object") {
        snapshot = body.payload;
      } else {
        snapshot = readSnapshot();
      }
      const envelope = buildModelHealthEnvelope(snapshot);
      const out = await callOpenclaw(cfg.openclawAgentPath, envelope);
      sendJSON(res, out.status || 500, out.data || {});
      return;
    }

    if (req.method === "POST" && incoming.pathname === "/api/openclaw/wake") {
      const body = await readRequestJSON(req);
      const out = await callOpenclaw(cfg.openclawWakePath, body || {});
      sendJSON(res, out.status || 500, out.data || {});
      return;
    }

    sendJSON(res, 404, { error: "not found" });
  } catch (err) {
    sendJSON(res, 500, { error: err && err.message ? err.message : "internal error" });
  }
});

server.listen(cfg.port, cfg.host, () => {
  process.stdout.write(`[openclaw-bridge] listening on http://${cfg.host}:${cfg.port}\n`);
});

server.on("error", (err) => {
  process.stderr.write(`[openclaw-bridge] failed: ${err.message}\n`);
  process.exit(1);
});
