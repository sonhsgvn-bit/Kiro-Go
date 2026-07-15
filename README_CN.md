# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

将 Kiro 账号转换为 OpenAI / Anthropic 兼容的 API 服务。

[English](README.md) | 中文

如果这个项目帮到了你，欢迎点个 Star 支持一下。

## 功能特性

- Anthropic `/v1/messages` 与 OpenAI `/v1/chat/completions`
- 多账号池轮询负载均衡
- 自动 Token 刷新、SSE 流式输出、Web 管理面板
- 多种认证方式：AWS Builder ID、IAM Identity Center (企业 SSO)、SSO Token、本地缓存、凭证 JSON
- 用量追踪、账号导入导出、越英中三语（默认越南语）
- 支持设置出站代理（SOCKS5 / HTTP）

## 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
```

### Docker 运行

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -p 127.0.0.1:3128:3128 \
  -p 127.0.0.1:1455:1455 \
  -e ADMIN_PASSWORD=your_secure_password \
  -e KIRO_SSO_CALLBACK_BIND=0.0.0.0 \
  -e CODEX_CALLBACK_BIND=0.0.0.0 \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/zsecducna/kiro-go:latest
```

### 源码编译

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go
```

### 部署到 Zeabur

仓库已包含 `Dockerfile`，可直接在 Zeabur 上构建运行。

**方式一：面板一键部署**

1. Fork 本仓库到你的 GitHub 账号。
2. 在 Zeabur 新建服务，选择 **Deploy from GitHub**，绑定刚才 fork 的仓库。
3. Zeabur 自动识别 `Dockerfile` 并完成构建。
4. 在 **Networking** 标签暴露端口 `8080` 并绑定域名。
5. 在 **Variables** 标签至少设置 `ADMIN_PASSWORD`（管理面板密码）。
6. 如需持久化账号 / 配置，挂载 Volume 到 `/app/data`。

**方式二：CLI 部署**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> 命令需在项目根目录执行。CLI 会生成 `.zeabur/context.json` 记录目标 project / service，包含个人 ID，请勿提交。

部署完成后访问 `https://<你的域名>/admin` 登录管理面板。

首次运行会在 `data/config.json` 自动生成配置，挂载 `/app/data` 以持久化。默认管理密码为 `changeme`，生产环境请务必通过 `ADMIN_PASSWORD` 环境变量或在管理面板中修改。

## 使用方法

访问 `http://localhost:8080/admin` 登录、添加账号，然后调用 API：

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好！"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"你好！"}]}'
```

## 在远程 VPS 上登录 Microsoft 365 SSO

Enterprise SSO 的回调地址固定为 `http://localhost:3128`。这里的 `localhost` 指运行浏览器的电脑，而不是 VPS。开始 Microsoft 365 登录前，请在浏览器所在电脑执行以下命令，并保持 SSH 会话开启直到登录完成：

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 3128:127.0.0.1:3128 root@VPS_IP
```

如果 VPS 的 `8080` 端口也未公开，可以同时转发管理面板和 SSO 回调：

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 8080:127.0.0.1:8080 \
  -L 3128:127.0.0.1:3128 root@VPS_IP
```

然后访问 `http://localhost:8080/admin` 并启动 Enterprise SSO。Compose 和 Docker Run 示例只把 VPS 的 `3128` 发布到回环地址，SSH 隧道可以访问该端口，同时避免把临时回调直接暴露到公网。仅把 `3128` 暴露到公网不能替代 SSH 隧道，因为 OAuth 仍然会把浏览器重定向到浏览器电脑自己的 `localhost`。

## 在远程 VPS 上登录 ChatGPT / OpenAI

ChatGPT OAuth 的回调地址固定为 `http://localhost:1455/auth/callback`。远程登录有两种完成方式：

1. **手动回调：** 登录完成后，从浏览器地址栏复制完整的 `http://localhost:1455/auth/callback?...` URL，粘贴到管理面板的回调输入框，然后点击 **Complete**。此方式不需要 SSH 隧道。如果端口 `1455` 已被占用，Start Login 会自动回退到该模式，而不会直接失败。
2. **SSH 隧道：** 在浏览器电脑执行以下命令，并在登录期间保持会话开启：

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 1455:127.0.0.1:1455 root@VPS_IP
```

也可以在同一个 SSH 会话中同时转发 Kiro Enterprise SSO 和 ChatGPT OAuth：

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 3128:127.0.0.1:3128 \
  -L 1455:127.0.0.1:1455 root@VPS_IP
```

## 思考模式

在模型名后加后缀（默认 `-thinking`）即可启用，例如 `claude-sonnet-4.5-thinking`。Claude 兼容请求如果带有顶层 `thinking` 配置，例如 `{"type":"enabled","budget_tokens":2048}` 或 `{"type":"adaptive"}`，也会自动启用 thinking 模式。输出格式可在管理面板「设置 - Thinking 模式」中配置。

## 出站代理

可在管理面板「设置 - 出站代理设置」中配置代理。支持 SOCKS5 和 HTTP 代理。

设置保存后即时生效，无需重启服务。

## 环境变量

| 变量 | 说明 | 默认值 |
|-----|------|-------|
| `CONFIG_PATH` | 配置文件路径 | `data/config.json` |
| `ADMIN_PASSWORD` | 管理面板密码（覆盖配置文件） | - |
| `KIRO_SSO_CALLBACK_BIND` | Enterprise SSO 回调监听地址；Docker 内使用 `0.0.0.0`，并只把宿主机 `3128` 发布到回环地址 | 仅回环地址 |
| `CODEX_CALLBACK_BIND` | ChatGPT OAuth 回调监听地址；Docker 内使用 `0.0.0.0`，并只把宿主机 `1455` 发布到回环地址 | 仅回环地址 |

## 参与贡献

欢迎友好交流。遇到问题时，建议先让 Claude Code、Codex 等工具帮忙排查一下，大部分问题都能自己解决。如果能直接提个 PR 就更好了。

## 友情链接

- [LINUX DO](https://linux.do)

## 免责声明

本项目仅供学习和研究目的使用，与 Amazon、AWS 或 Kiro 没有任何关联。用户需自行确保使用行为符合所有适用的服务条款和法律法规，使用风险自负。

## 许可证

[MIT](LICENSE)
