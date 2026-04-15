#!/usr/bin/env node
"use strict";

const fs = require("fs");
const http = require("http");

const host = process.env.MODEL_HEALTH_HOST || "127.0.0.1";
const port = Number(process.env.MODEL_HEALTH_PORT || 34999);
const filePath = process.env.MODEL_HEALTH_FILE || "/opt/cli-proxy-api/static/model-health.json";

function sendJson(res, status, payload) {
  const body = JSON.stringify(payload);
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Methods": "GET,OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type, Authorization, X-Management-Key",
    "Content-Length": Buffer.byteLength(body)
  });
  res.end(body);
}

const server = http.createServer((req, res) => {
  if ((req.method || "").toUpperCase() === "OPTIONS") {
    res.writeHead(204, {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "GET,OPTIONS",
      "Access-Control-Allow-Headers": "Content-Type, Authorization, X-Management-Key",
      "Cache-Control": "no-store"
    });
    res.end();
    return;
  }

  if ((req.method || "").toUpperCase() !== "GET") {
    return sendJson(res, 405, { error: "method_not_allowed" });
  }

  const url = req.url || "/";
  if (url === "/healthz") {
    return sendJson(res, 200, { ok: true });
  }
  if (url !== "/model-health.json") {
    return sendJson(res, 404, { error: "not_found" });
  }
  fs.readFile(filePath, "utf8", (err, raw) => {
    if (err) {
      return sendJson(res, 503, { error: "unavailable", message: err.message });
    }
    try {
      const data = JSON.parse(raw);
      return sendJson(res, 200, data);
    } catch (e) {
      return sendJson(res, 500, { error: "invalid_json", message: e.message });
    }
  });
});

server.listen(port, host, () => {
  process.stdout.write(`[model-health-http] listening on http://${host}:${port}\n`);
});
