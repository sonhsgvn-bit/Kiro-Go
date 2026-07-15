# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Convert Kiro accounts to OpenAI / Anthropic compatible API service.

[English](README.md) | [中文](README_CN.md)

If this project helps you, a Star would mean a lot.

## Features

- Anthropic `/v1/messages` & OpenAI `/v1/chat/completions`
- Multi-account pool with round-robin load balancing
- Auto token refresh, SSE streaming, Web admin panel
- Multiple auth: AWS Builder ID, IAM Identity Center (Enterprise SSO), SSO Token, local cache, credentials JSON
- Usage tracking, account import/export, i18n (VI / EN / CN; Vietnamese by default)
- Support configuring outbound proxy (SOCKS5 / HTTP)

## Quick Start

### Docker Compose (Recommended)

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
```

### Docker Run

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

### Build from Source

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go
```

### Deploy on Zeabur

The repo already includes a `Dockerfile`, so it builds and runs on Zeabur out of the box.

**Option 1: Dashboard (one-click)**

1. Fork this repo to your GitHub account.
2. In Zeabur, create a new service and choose **Deploy from GitHub**, then select your fork.
3. Zeabur auto-detects the `Dockerfile` and builds the image.
4. In the **Networking** tab, expose port `8080` and bind a domain.
5. In the **Variables** tab, set at least `ADMIN_PASSWORD` (admin panel password).
6. Mount a Volume at `/app/data` if you want accounts / config to survive redeploys.

**Option 2: CLI**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> Run the commands from the project root. The CLI writes `.zeabur/context.json` to remember the target project / service — it contains personal IDs, so don't commit it.

Once the service is up, open `https://<your-domain>/admin` to log in.

Config is auto-created at `data/config.json`. Mount `/app/data` for persistence. The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8080/admin`, log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

## Microsoft 365 SSO on a Remote VPS

The Enterprise SSO redirect URI is fixed at `http://localhost:3128`. Here, `localhost` means the computer running your browser, not the VPS. Before starting Microsoft 365 login, run this command on the browser computer and keep it open until login completes:

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 3128:127.0.0.1:3128 root@VPS_IP
```

If port `8080` is also private, forward both the admin panel and the callback:

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 8080:127.0.0.1:8080 \
  -L 3128:127.0.0.1:3128 root@VPS_IP
```

Then open `http://localhost:8080/admin` and start Enterprise SSO. The Compose and Docker Run examples publish VPS port `3128` on loopback only so the SSH tunnel can reach it without exposing the transient callback publicly. Publishing `3128` publicly does not replace the tunnel because the OAuth provider still redirects the browser to its own `localhost`.

## ChatGPT / OpenAI Login on a Remote VPS

The ChatGPT OAuth redirect is fixed at `http://localhost:1455/auth/callback`. You can complete a remote login in either of these ways:

1. **Manual callback:** finish signing in, copy the full `http://localhost:1455/auth/callback?...` URL from the browser address bar, paste it into the admin panel's callback field, and click **Complete**. This does not require an SSH tunnel. If port `1455` is already occupied, Start Login automatically falls back to this mode instead of failing.
2. **SSH tunnel:** run this on the browser computer and keep it open during login:

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 1455:127.0.0.1:1455 root@VPS_IP
```

To forward both Kiro Enterprise SSO and ChatGPT OAuth in one session:

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 3128:127.0.0.1:3128 \
  -L 1455:127.0.0.1:1455 root@VPS_IP
```

## Thinking Mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude-compatible requests that include a top-level `thinking` config such as `{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}` also enable thinking mode automatically. Configure output format in the admin panel under Settings - Thinking Mode.

## Outbound Proxy

For users in restricted network regions, configure an outbound proxy in the admin panel under **Settings - Outbound Proxy Settings**. Supports SOCKS5 and HTTP proxies.

The setting takes effect immediately without restarting.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config) | - |
| `KIRO_SSO_CALLBACK_BIND` | Enterprise SSO callback bind address; use `0.0.0.0` inside Docker with host port `3128` published on loopback | loopback only |
| `CODEX_CALLBACK_BIND` | ChatGPT OAuth callback bind address; use `0.0.0.0` inside Docker with host port `1455` published on loopback | loopback only |

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

## Friend Links

- [LINUX DO](https://linux.do)

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
