# CLI Proxy API

English | [中文](README_CN.md)

A CPAMC-based proxy server for CLI and agent workloads, focused on channel integration, load balancing, and external call aggregation.

It provides a stable public API under `/cpamc/*`, compatibility routes under `/api/provider/{provider}/...`, and a separate management surface under `/v0/management/*`.

It now also supports OpenAI Codex (GPT models) and Claude Code via OAuth, with local and multi-account access for compatible CLI tools and SDKs.

This repository is based on the CPAMC foundation and extends it with:

- Channel integration for third-party and custom upstreams
- Multi-account and multi-provider load balancing
- Aggregated external invocation and unified proxy routing
- Stable public APIs plus protocol compatibility layers for CLI tools

## API Surface

- Public data plane: `/cpamc/*` for stable Codex and Claude access
- Compatibility layer: `/api/provider/{provider}/...` for Amp/provider protocol routing
- Management plane: `/v0/management/*`, protected separately and not part of the public data API

## Overview

- Stable public API for Codex and Claude CLI workloads under `/cpamc/*`
- OpenAI/Gemini/Claude protocol compatibility layers for CLI integrations
- OpenAI Codex support (GPT models) via OAuth login
- Claude Code support via OAuth login
- Qwen Code support via OAuth login
- iFlow support via OAuth login
- Amp CLI and IDE extensions support with provider routing
- Streaming and non-streaming responses
- Function calling/tools support
- Multimodal input support (text and images)
- Multiple accounts with round-robin load balancing (Gemini, OpenAI, Claude, Qwen and iFlow)
- Simple CLI authentication flows (Gemini, OpenAI, Claude, Qwen and iFlow)
- Generative Language API Key support
- AI Studio Build multi-account load balancing
- Gemini CLI multi-account load balancing
- Claude Code multi-account load balancing
- Qwen Code multi-account load balancing
- iFlow multi-account load balancing
- OpenAI Codex multi-account load balancing
- OpenAI-compatible upstream providers via config (e.g., OpenRouter)
- Reusable Go SDK for embedding the proxy (see `docs/sdk-usage.md`)

## Getting Started

CLIProxyAPI Guides: [https://help.router-for.me/](https://help.router-for.me/)

## Management API

see [MANAGEMENT_API.md](https://help.router-for.me/management/api)

## Amp CLI Support

CLIProxyAPI includes integrated support for [Amp CLI](https://ampcode.com) and Amp IDE extensions, enabling you to use your Google/ChatGPT/Claude OAuth subscriptions with Amp's coding tools:

- Provider route aliases for Amp's API patterns (`/api/provider/{provider}/v1...`)
- Separate management proxy for OAuth authentication and account features
- Smart model fallback with automatic routing
- **Model mapping** to route unavailable models to alternatives (e.g., `claude-opus-4.5` → `claude-sonnet-4`)
- Security-first design with localhost-only management endpoints

**→ [Complete Amp CLI Integration Guide](https://help.router-for.me/agent-client/amp-cli.html)**

## OpenClaw Integration

When integrating OpenClaw with `cpamc`, treat Codex and Claude as two separate providers. Do not put both protocol families behind a single `cpamc` provider entry.

Recommended layout:

- `cpamc-codex`
  - `baseUrl: http://127.0.0.1:34050/cpamc/codex`
  - native Codex/OpenAI Responses surface
  - prefer `POST /cpamc/codex/responses`
- `cpamc-claude`
  - `baseUrl: http://127.0.0.1:34050/cpamc/claude`
  - native Claude Messages surface
  - prefer `POST /cpamc/claude/messages`
  - do not reuse the Codex route or `chat/completions` for Claude CLI traffic

Channel-split public endpoints:

- Codex channel
  - `GET /cpamc/codex/models`
  - `GET /cpamc/codex/health`
  - `POST /cpamc/codex/completions`
  - `POST /cpamc/codex/responses`
- Claude channel
  - `GET /cpamc/claude/models`
  - `GET /cpamc/claude/health`
  - `POST /cpamc/claude/messages`
  - `POST /cpamc/claude/messages/count_tokens`
- Combined compatibility views remain available:
  - `GET /cpamc/models`
  - `GET /cpamc/health`

Why:

- `cpamc/codex/*` is the native Codex/OpenAI-style surface
- `cpamc/claude/*` is the native Claude Messages-style surface
- native Claude CLI traffic must use `/v1/messages` or `/cpamc/claude/messages`
- native Codex traffic should use `/v1/responses` or `/cpamc/codex/responses`
- native Claude traffic sent to `/chat/completions` is now rejected on purpose so protocol families stay separated
- if Claude models are placed under a provider that only targets the Codex route, OpenClaw may fallback to Claude models while still using the wrong protocol path

Not recommended:

```json
{
  "models": {
    "providers": {
      "cpamc": {
        "baseUrl": "http://127.0.0.1:34050/cpamc/codex",
        "api": "openai-completions",
        "models": [
          { "id": "gpt-5.4", "name": "gpt-5.4" },
          { "id": "claude-sonnet-4-6", "name": "claude-sonnet-4-6" }
        ]
      }
    }
  }
}
```

Recommended:

```json
{
  "models": {
    "providers": {
      "cpamc-codex": {
        "baseUrl": "http://127.0.0.1:34050/cpamc/codex",
        "apiKey": "YOUR_PROXY_KEY",
        "api": "openai-completions",
        "authHeader": true,
        "models": [
          { "id": "gpt-5.4", "name": "gpt-5.4" },
          { "id": "gpt-5.3-codex", "name": "gpt-5.3-codex" }
        ]
      },
      "cpamc-claude": {
        "baseUrl": "http://127.0.0.1:34050/cpamc/claude",
        "apiKey": "YOUR_PROXY_KEY",
        "authHeader": true,
        "models": [
          { "id": "claude-opus-4-6", "name": "claude-opus-4-6" },
          { "id": "claude-sonnet-4-6", "name": "claude-sonnet-4-6" }
        ]
      }
    }
  }
}
```

If you also use third-party OpenAI-compatible upstreams such as Alibaba Cloud Bailian, keep a multi-provider layout:

- `bailian` as the primary provider
- `cpamc-codex` as the Codex backup provider
- `cpamc-claude` as the Claude backup provider

Model health probe persistence:

- Run one full probe immediately:
  - `./scripts/hourly-model-health-monitor-multi.sh --once`
- Check probe status:
  - `./scripts/model-health-status.sh`
- Snapshots are written to:
  - `/opt/cli-proxy-api/static/model-health-multi.json`
  - `/opt/cli-proxy-api/auths/model-health-multi.json`
- On restart, if static snapshots are missing/corrupted, the scheduler restores from the auth mirror snapshot before the next probe round.

## Nowcoding Notes

- Treat `nowcoding` as a Codex channel carried over `claude-messages`.
- Publicly expose only `nowcoding/gpt-5.4`.
- Configure upstream as `claude-sonnet-4-6` with alias `gpt-5.4`.
- Do not locally advertise extra Codex models such as `o4-mini` or `gpt-5.4-pro` unless the provider explicitly adds them.

## SDK Docs

- External Integration Playbook (CN): [docs/external_integration_CN.md](docs/external_integration_CN.md)
- Usage: [docs/sdk-usage.md](docs/sdk-usage.md)
- Advanced (executors & translators): [docs/sdk-advanced.md](docs/sdk-advanced.md)
- Access: [docs/sdk-access.md](docs/sdk-access.md)
- Watcher: [docs/sdk-watcher.md](docs/sdk-watcher.md)
- Custom Provider Example: `examples/custom-provider`

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## Who is with us?

Those projects are based on CLIProxyAPI:

### [vibeproxy](https://github.com/automazeio/vibeproxy)

Native macOS menu bar app to use your Claude Code & ChatGPT subscriptions with AI coding tools - no API keys needed

### [Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator)

Browser-based tool to translate SRT subtitles using your Gemini subscription via CLIProxyAPI with automatic validation/error correction - no API keys needed

### [CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)

CLI wrapper for instant switching between multiple Claude accounts and alternative models (Gemini, Codex, Antigravity) via CLIProxyAPI OAuth - no API keys needed

### [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)

Native macOS GUI for managing CLIProxyAPI: configure providers, model mappings, and endpoints via OAuth - no API keys needed.

### [Quotio](https://github.com/nguyenphutrong/quotio)

Native macOS menu bar app that unifies Claude, Gemini, OpenAI, Qwen, and Antigravity subscriptions with real-time quota tracking and smart auto-failover for AI coding tools like Claude Code, OpenCode, and Droid - no API keys needed.

### [CodMate](https://github.com/loocor/CodMate)

Native macOS SwiftUI app for managing CLI AI sessions (Codex, Claude Code, Gemini CLI) with unified provider management, Git review, project organization, global search, and terminal integration. Integrates CLIProxyAPI to provide OAuth authentication for Codex, Claude, Gemini, Antigravity, and Qwen Code, with built-in and third-party provider rerouting through a single proxy endpoint - no API keys needed for OAuth providers.

### [ProxyPilot](https://github.com/Finesssee/ProxyPilot)

Windows-native CLIProxyAPI fork with TUI, system tray, and multi-provider OAuth for AI coding tools - no API keys needed.

### [Claude Proxy VSCode](https://github.com/uzhao/claude-proxy-vscode)

VSCode extension for quick switching between Claude Code models, featuring integrated CLIProxyAPI as its backend with automatic background lifecycle management.

### [ZeroLimit](https://github.com/0xtbug/zero-limit)

Windows desktop app built with Tauri + React for monitoring AI coding assistant quotas via CLIProxyAPI. Track usage across Gemini, Claude, OpenAI Codex, and Antigravity accounts with real-time dashboard, system tray integration, and one-click proxy control - no API keys needed.

### [CPA-XXX Panel](https://github.com/ferretgeek/CPA-X)

A lightweight web admin panel for CLIProxyAPI with health checks, resource monitoring, real-time logs, auto-update, request statistics and pricing display. Supports one-click installation and systemd service.

### [CLIProxyAPI Tray](https://github.com/kitephp/CLIProxyAPI_Tray)

A Windows tray application implemented using PowerShell scripts, without relying on any third-party libraries. The main features include: automatic creation of shortcuts, silent running, password management, channel switching (Main / Plus), and automatic downloading and updating.

### [霖君](https://github.com/wangdabaoqq/LinJun)

霖君 is a cross-platform desktop application for managing AI programming assistants, supporting macOS, Windows, and Linux systems. Unified management of Claude Code, Gemini CLI, OpenAI Codex, Qwen Code, and other AI coding tools, with local proxy for multi-account quota tracking and one-click configuration.

### [CLIProxyAPI Dashboard](https://github.com/itsmylife44/cliproxyapi-dashboard)

A modern web-based management dashboard for CLIProxyAPI built with Next.js, React, and PostgreSQL. Features real-time log streaming, structured configuration editing, API key management, OAuth provider integration for Claude/Gemini/Codex, usage analytics, container management, and config sync with OpenCode via companion plugin - no manual YAML editing needed.

> [!NOTE]  
> If you developed a project based on CLIProxyAPI, please open a PR to add it to this list.

## More choices

Those projects are ports of CLIProxyAPI or inspired by it:

### [9Router](https://github.com/decolua/9router)

A Next.js implementation inspired by CLIProxyAPI, easy to install and use, built from scratch with format translation (OpenAI/Claude/Gemini/Ollama), combo system with auto-fallback, multi-account management with exponential backoff, a Next.js web dashboard, and support for CLI tools (Cursor, Claude Code, Cline, RooCode) - no API keys needed.

> [!NOTE]  
> If you have developed a port of CLIProxyAPI or a project inspired by it, please open a PR to add it to this list.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
