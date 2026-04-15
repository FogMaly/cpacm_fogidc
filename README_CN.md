# CLI 代理 API

[English](README.md) | 中文

一个面向 CLI 与 Agent 场景的代理服务器，提供稳定公共接口与协议兼容层。

当前提供三层接口能力：`/cpamc/*` 作为稳定公共接口，`/api/provider/{provider}/...` 作为兼容接入层，`/v0/management/*` 作为独立管理面。

现已支持通过 OAuth 登录接入 OpenAI Codex（GPT 系列）和 Claude Code，并支持本地或多账户方式接入兼容客户端与 SDK。

## 赞助商

[![bigmodel.cn](https://assets.router-for.me/chinese-4.7.png)](https://www.bigmodel.cn/claude-code?ic=RRVJPB5SII)

本项目由 Z智谱 提供赞助, 他们通过 GLM CODING PLAN 对本项目提供技术支持。

GLM CODING PLAN 是专为AI编码打造的订阅套餐，每月最低仅需20元，即可在十余款主流AI编码工具如 Claude Code、Cline、Roo Code 中畅享智谱旗舰模型GLM-4.7，为开发者提供顶尖的编码体验。

智谱AI为本软件提供了特别优惠，使用以下链接购买可以享受九折优惠：https://www.bigmodel.cn/claude-code?ic=RRVJPB5SII

---

<table>
<tbody>
<tr>
<td width="180"><a href="https://www.packyapi.com/register?aff=cliproxyapi"><img src="./assets/packycode.png" alt="PackyCode" width="150"></a></td>
<td>感谢 PackyCode 对本项目的赞助！PackyCode 是一家可靠高效的 API 中转服务商，提供 Claude Code、Codex、Gemini 等多种服务的中转。PackyCode 为本软件用户提供了特别优惠：使用<a href="https://www.packyapi.com/register?aff=cliproxyapi">此链接</a>注册，并在充值时输入 "cliproxyapi" 优惠码即可享受九折优惠。</td>
</tr>
<tr>
<td width="180"><a href="https://www.aicodemirror.com/register?invitecode=TJNAIF"><img src="./assets/aicodemirror.png" alt="AICodeMirror" width="150"></a></td>
<td>感谢 AICodeMirror 赞助了本项目！AICodeMirror 提供 Claude Code / Codex / Gemini CLI 官方高稳定中转服务，支持企业级高并发、极速开票、7×24 专属技术支持。 Claude Code / Codex / Gemini 官方渠道低至 3.8 / 0.2 / 0.9 折，充值更有折上折！AICodeMirror 为 CLIProxyAPI 的用户提供了特别福利，通过<a href="https://www.aicodemirror.com/register?invitecode=TJNAIF">此链接</a>注册的用户，可享受首充8折，企业客户最高可享 7.5 折！</td>
</tr>
</tbody>
</table>


## 接口分层

- 公共数据面：`/cpamc/*`，提供稳定的 Codex 与 Claude 访问能力
- 兼容接入层：`/api/provider/{provider}/...`，用于 Amp / Provider 协议适配
- 管理面：`/v0/management/*`，独立鉴权，不属于公共数据接口

## 功能特性

- 为 Codex 与 Claude CLI 场景提供稳定公共 API（`/cpamc/*`）
- 提供 OpenAI/Gemini/Claude 协议兼容层，用于 CLI 集成与适配
- 新增 OpenAI Codex（GPT 系列）支持（OAuth 登录）
- 新增 Claude Code 支持（OAuth 登录）
- 新增 Qwen Code 支持（OAuth 登录）
- 新增 iFlow 支持（OAuth 登录）
- 支持流式与非流式响应
- 函数调用/工具支持
- 多模态输入（文本、图片）
- 多账户支持与轮询负载均衡（Gemini、OpenAI、Claude、Qwen 与 iFlow）
- 简单的 CLI 身份验证流程（Gemini、OpenAI、Claude、Qwen 与 iFlow）
- 支持 Gemini AIStudio API 密钥
- 支持 AI Studio Build 多账户轮询
- 支持 Gemini CLI 多账户轮询
- 支持 Claude Code 多账户轮询
- 支持 Qwen Code 多账户轮询
- 支持 iFlow 多账户轮询
- 支持 OpenAI Codex 多账户轮询
- 通过配置接入上游 OpenAI 兼容提供商（例如 OpenRouter）
- 可复用的 Go SDK（见 `docs/sdk-usage_CN.md`）

## 新手入门

CLIProxyAPI 用户手册： [https://help.router-for.me/](https://help.router-for.me/cn/)

## 管理 API 文档

请参见 [MANAGEMENT_API_CN.md](https://help.router-for.me/cn/management/api)

## Amp CLI 支持

CLIProxyAPI 已内置对 [Amp CLI](https://ampcode.com) 和 Amp IDE 扩展的支持，可让你使用自己的 Google/ChatGPT/Claude OAuth 订阅来配合 Amp 编码工具：

- 提供商路由别名，兼容 Amp 的 API 路径模式（`/api/provider/{provider}/v1...`）
- 独立管理代理，处理 OAuth 认证和账号功能
- 智能模型回退与自动路由
- 以安全为先的设计，管理端点仅限 localhost

**→ [Amp CLI 完整集成指南](https://help.router-for.me/cn/agent-client/amp-cli.html)**

## OpenClaw 对接

OpenClaw 对接 `cpamc` 时，推荐将 Codex 与 Claude 拆成两个独立 provider，不要用单个 provider 同时承载两种协议。

推荐方式：

- `cpamc-codex`
  - `baseUrl: http://127.0.0.1:34050/cpamc/codex`
  - 原生 Codex / OpenAI Responses 入口
  - 优先使用 `POST /cpamc/codex/responses`
- `cpamc-claude`
  - `baseUrl: http://127.0.0.1:34050/cpamc/claude`
  - 原生 Claude Messages 入口
  - 优先使用 `POST /cpamc/claude/messages`
  - Claude CLI 不要复用 Codex 路由，也不要走 `chat/completions`

按通道拆分后的公共接口：

- Codex 通道
  - `GET /cpamc/codex/models`
  - `GET /cpamc/codex/health`
  - `POST /cpamc/codex/completions`
  - `POST /cpamc/codex/responses`
- Claude 通道
  - `GET /cpamc/claude/models`
  - `GET /cpamc/claude/health`
  - `POST /cpamc/claude/messages`
  - `POST /cpamc/claude/messages/count_tokens`
- 兼容聚合入口仍保留：
  - `GET /cpamc/models`
  - `GET /cpamc/health`

原因：

- `cpamc/codex/*` 是原生 Codex / OpenAI 风格入口
- `cpamc/claude/*` 是原生 Claude Messages 风格入口
- 原生 Claude CLI 流量必须走 `/v1/messages` 或 `/cpamc/claude/messages`
- 原生 Codex 流量应走 `/v1/responses` 或 `/cpamc/codex/responses`
- 原生 Claude 流量发到 `/chat/completions` 现在会被主动拒绝，以确保两套协议彻底分开
- 如果把 Claude 模型塞进只适配 Codex 的 provider，OpenClaw 在 fallback 到 Claude 时可能继续走错协议

不推荐：

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

推荐：

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

如果你同时还接了第三方 OpenAI 兼容上游，例如阿里云百炼，建议让 OpenClaw 保持多 provider 结构：

- `bailian` 作为主 provider
- `cpamc-codex` 作为 Codex 类模型备用
- `cpamc-claude` 作为 Claude 类模型备用

模型探测与重启保留：

- 立即执行一次全量探测：
  - `./scripts/hourly-model-health-monitor-multi.sh --once`
- 查看探测状态：
  - `./scripts/model-health-status.sh`
- 探测快照写入路径：
  - `/opt/cli-proxy-api/static/model-health-multi.json`
  - `/opt/cli-proxy-api/auths/model-health-multi.json`
- 服务重启时，如果 `static` 快照缺失或损坏，调度器会先从 `auths` 镜像快照恢复，再进入下一轮探测。

## Nowcoding 说明

- `nowcoding` 按 Codex 通道接入，但底层走 `claude-messages`。
- 对外只暴露 `nowcoding/gpt-5.4`。
- 上游配置使用 `claude-sonnet-4-6`，通过 alias 映射为 `gpt-5.4`。
- 除非渠道方明确支持，否则不要在本地额外声明 `o4-mini`、`gpt-5.4-pro` 这类模型。

## SDK 文档

- 外部对接实操（CPAMC / Provider）：[docs/external_integration_CN.md](docs/external_integration_CN.md)
- 使用文档：[docs/sdk-usage_CN.md](docs/sdk-usage_CN.md)
- 高级（执行器与翻译器）：[docs/sdk-advanced_CN.md](docs/sdk-advanced_CN.md)
- 认证: [docs/sdk-access_CN.md](docs/sdk-access_CN.md)
- 凭据加载/更新: [docs/sdk-watcher_CN.md](docs/sdk-watcher_CN.md)
- 自定义 Provider 示例：`examples/custom-provider`

## 贡献

欢迎贡献！请随时提交 Pull Request。

1. Fork 仓库
2. 创建您的功能分支（`git checkout -b feature/amazing-feature`）
3. 提交您的更改（`git commit -m 'Add some amazing feature'`）
4. 推送到分支（`git push origin feature/amazing-feature`）
5. 打开 Pull Request

## 谁与我们在一起？

这些项目基于 CLIProxyAPI:

### [vibeproxy](https://github.com/automazeio/vibeproxy)

一个原生 macOS 菜单栏应用，让您可以使用 Claude Code & ChatGPT 订阅服务和 AI 编程工具，无需 API 密钥。

### [Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator)

一款基于浏览器的 SRT 字幕翻译工具，可通过 CLI 代理 API 使用您的 Gemini 订阅。内置自动验证与错误修正功能，无需 API 密钥。

### [CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)

CLI 封装器，用于通过 CLIProxyAPI OAuth 即时切换多个 Claude 账户和替代模型（Gemini, Codex, Antigravity），无需 API 密钥。

### [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)

基于 macOS 平台的原生 CLIProxyAPI GUI：配置供应商、模型映射以及OAuth端点，无需 API 密钥。

### [Quotio](https://github.com/nguyenphutrong/quotio)

原生 macOS 菜单栏应用，统一管理 Claude、Gemini、OpenAI、Qwen 和 Antigravity 订阅，提供实时配额追踪和智能自动故障转移，支持 Claude Code、OpenCode 和 Droid 等 AI 编程工具，无需 API 密钥。

### [CodMate](https://github.com/loocor/CodMate)

原生 macOS SwiftUI 应用，用于管理 CLI AI 会话（Claude Code、Codex、Gemini CLI），提供统一的提供商管理、Git 审查、项目组织、全局搜索和终端集成。集成 CLIProxyAPI 为 Codex、Claude、Gemini、Antigravity 和 Qwen Code 提供统一的 OAuth 认证，支持内置和第三方提供商通过单一代理端点重路由 - OAuth 提供商无需 API 密钥。

### [ProxyPilot](https://github.com/Finesssee/ProxyPilot)

原生 Windows CLIProxyAPI 分支，集成 TUI、系统托盘及多服务商 OAuth 认证，专为 AI 编程工具打造，无需 API 密钥。

### [Claude Proxy VSCode](https://github.com/uzhao/claude-proxy-vscode)

一款 VSCode 扩展，提供了在 VSCode 中快速切换 Claude Code 模型的功能，内置 CLIProxyAPI 作为其后端，支持后台自动启动和关闭。

### [ZeroLimit](https://github.com/0xtbug/zero-limit)

Windows 桌面应用，基于 Tauri + React 构建，用于通过 CLIProxyAPI 监控 AI 编程助手配额。支持跨 Gemini、Claude、OpenAI Codex 和 Antigravity 账户的使用量追踪，提供实时仪表盘、系统托盘集成和一键代理控制，无需 API 密钥。

### [CPA-XXX Panel](https://github.com/ferretgeek/CPA-X)

面向 CLIProxyAPI 的 Web 管理面板，提供健康检查、资源监控、日志查看、自动更新、请求统计与定价展示，支持一键安装与 systemd 服务。

### [CLIProxyAPI Tray](https://github.com/kitephp/CLIProxyAPI_Tray)

Windows 托盘应用，基于 PowerShell 脚本实现，不依赖任何第三方库。主要功能包括：自动创建快捷方式、静默运行、密码管理、通道切换（Main / Plus）以及自动下载与更新。

### [霖君](https://github.com/wangdabaoqq/LinJun)

霖君是一款用于管理AI编程助手的跨平台桌面应用，支持macOS、Windows、Linux系统。统一管理Claude Code、Gemini CLI、OpenAI Codex、Qwen Code等AI编程工具，本地代理实现多账户配额跟踪和一键配置。

### [CLIProxyAPI Dashboard](https://github.com/itsmylife44/cliproxyapi-dashboard)

一个面向 CLIProxyAPI 的现代化 Web 管理仪表盘，基于 Next.js、React 和 PostgreSQL 构建。支持实时日志流、结构化配置编辑、API Key 管理、Claude/Gemini/Codex 的 OAuth 提供方集成、使用量分析、容器管理，并可通过配套插件与 OpenCode 同步配置，无需手动编辑 YAML。

> [!NOTE]  
> 如果你开发了基于 CLIProxyAPI 的项目，请提交一个 PR（拉取请求）将其添加到此列表中。

## 更多选择

以下项目是 CLIProxyAPI 的移植版或受其启发：

### [9Router](https://github.com/decolua/9router)

基于 Next.js 的实现，灵感来自 CLIProxyAPI，易于安装使用；自研格式转换（OpenAI/Claude/Gemini/Ollama）、组合系统与自动回退、多账户管理（指数退避）、Next.js Web 控制台，并支持 Cursor、Claude Code、Cline、RooCode 等 CLI 工具，无需 API 密钥。

> [!NOTE]  
> 如果你开发了 CLIProxyAPI 的移植或衍生项目，请提交 PR 将其添加到此列表中。

## 许可证

此项目根据 MIT 许可证授权 - 有关详细信息，请参阅 [LICENSE](LICENSE) 文件。

## 写给所有中国网友的

QQ 群：188637136

或

Telegram 群：https://t.me/CLIProxyAPI
