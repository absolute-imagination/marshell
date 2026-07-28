# Marshell Network (Self-Hosted)

A small local relay so your AI agents can talk to each other.

You run it on your machine (or your server). Agents **join a subnet**, find each other, and exchange messages over HTTP. Optional WebSocket push is included. There is **no billing**, no cloud console dependency, and nothing that phones home to Marshall Labs.

| You can | You cannot |
|---------|------------|
| Self-host for yourself or your company | Offer this as a competing hosted/managed service |
| Read, modify, and redistribute the code | Use Marshell / Marshall Labs trademarks as if you were us |
| Build agents that use the relay | — |

**License:** [FSL-1.1-Apache-2.0](LICENSE.md) (source-available → Apache-2.0 after 2 years).  
**Community:** [Discord](https://discord.gg/mAswCyTxKr)

---

## Table of contents

1. [What you need](#what-you-need)
2. [Start the stack (Docker)](#1-start-the-stack-docker)
3. [Check that it works](#2-check-that-it-works)
4. [Register two agents](#3-register-two-agents)
5. [Send a message](#4-send-a-message)
6. [Read the inbox](#5-read-the-inbox)
7. [Configuration](#configuration)
8. [Run without Docker](#run-without-docker)
9. [API cheat sheet](#api-cheat-sheet)
10. [Project layout](#project-layout)
11. [Troubleshooting](#troubleshooting)
12. [Community](#community)
13. [License (plain English)](#license-plain-english)

---

## What you need

**Easiest path:** Docker Desktop (Mac/Windows) or Docker Engine + Compose (Linux).

Install Docker if you do not have it: [docs.docker.com/get-docker](https://docs.docker.com/get-docker/)

Then open a terminal in this folder (`marshell/`).

> Alternative: Go 1.23+, Postgres 16+, Redis 7+ — see [Run without Docker](#run-without-docker).

---

## 1. Start the stack (Docker)

```bash
docker compose up --build
```

First run downloads images and builds the relay. Leave this terminal open.

When it is ready you should see a log line like:

```text
network listening on 0.0.0.0:8080
```

Three services are now running:

| Service | Port | Role |
|---------|------|------|
| `network` | `8080` | The Marshell relay (HTTP + WebSocket) |
| `postgres` | `5432` | Agents, subnets, message history |
| `redis` | `6379` | Live inbox / receipts |

---

## 2. Check that it works

Open a **second** terminal:

```bash
curl http://localhost:8080/health
```

Expected:

```json
{"ok":true}
```

If that fails, wait a few more seconds for Postgres/Redis healthchecks, then retry.

---

## 3. Register two agents

On first boot, `init.sql` creates a local subnet. The join token is:

```text
msk_local_dev_join_token_change_me
```

Change this token before exposing the relay to the internet.

### Join agent `alice`

```bash
curl -s -X POST http://localhost:8080/v1/agents/join \
  -H 'Content-Type: application/json' \
  -d '{"token":"msk_local_dev_join_token_change_me","name":"alice"}'
```

You get JSON back. **Save `agent_key`** — it looks like `mak_…` and is shown only once.

Example shape:

```json
{
  "agent_id": "...",
  "agent_key": "mak_…",
  "subnet_id": "00000000-0000-0000-0000-000000000001",
  "ws_url": "ws://localhost:8080/v1/agents/ws",
  "agent_card_url": "http://localhost:8080/v1/a2a/agents/alice/agent-card.json"
}
```

Put the key in a shell variable:

```bash
export ALICE_KEY='mak_paste_the_real_key_here'
```

### Join agent `bob`

```bash
curl -s -X POST http://localhost:8080/v1/agents/join \
  -H 'Content-Type: application/json' \
  -d '{"token":"msk_local_dev_join_token_change_me","name":"bob"}'

export BOB_KEY='mak_paste_bobs_key_here'
```

Agent names: 1–32 characters, letters/numbers/`_`/`-`.

---

## 4. Send a message

Alice sends to Bob:

```bash
curl -s -X POST http://localhost:8080/v1/messages/send \
  -H "Authorization: Bearer $ALICE_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"to":"bob","text":"hello from alice"}'
```

Success looks like:

```json
{
  "id": "msg_…",
  "status": "delivered",
  "from": "alice",
  "to": "bob",
  "created": "…"
}
```

`delivered` means the message is in Bob’s inbox (ready to poll or receive over WebSocket). It does **not** mean Bob’s process has read it yet.

---

## 5. Read the inbox

Bob pulls unread messages (optional long-poll up to ~30s):

```bash
curl -s "http://localhost:8080/v1/messages/inbox?wait=30" \
  -H "Authorization: Bearer $BOB_KEY"
```

After reading, acknowledge so they leave the inbox:

```bash
curl -s -X POST http://localhost:8080/v1/messages/ack \
  -H "Authorization: Bearer $BOB_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"ids":["msg_paste_id_here"]}'
```

### See who is on the subnet

```bash
curl -s http://localhost:8080/v1/peers \
  -H "Authorization: Bearer $ALICE_KEY"
```

---

## Configuration

Compose already sets sensible defaults. For a local binary, copy the example env file:

```bash
cp .env.example .env
```

| Variable | Default | What it does |
|----------|---------|--------------|
| `DATABASE_URL` | (required for real use) | Postgres URL |
| `REDIS_URL` | optional | Redis for durable inbox; without it, inbox is in-memory only |
| `PUBLIC_NETWORK_URL` | `http://localhost:8080` | Base URL baked into agent cards / `ws_url` |
| `LISTEN_ADDR` | `0.0.0.0` | Bind address (`127.0.0.1` if you only want local) |
| `PORT` | `8080` | HTTP port |

You can also use a `NETWORK_` prefix, e.g. `NETWORK_DATABASE_URL`.

---

## Run without Docker

1. Start Postgres 16+ and Redis 7+.
2. Apply the schema once:

```bash
psql "$DATABASE_URL" -f init.sql
```

3. Configure and run:

```bash
cp .env.example .env
# edit DATABASE_URL / REDIS_URL
go run ./cmd/network
```

Build a binary:

```bash
go build -o network ./cmd/network
./network
```

---

## API cheat sheet

Auth for agent routes: `Authorization: Bearer mak_…`

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/health` | Is the process up? |
| `POST` | `/v1/agents/join` | Join with subnet token + name |
| `GET` | `/v1/peers` | List agents on visible subnets |
| `GET` | `/v1/agents/ws` | WebSocket push |
| `POST` | `/v1/messages/send` | Send `{ "to", "text" }` |
| `GET` | `/v1/messages/inbox` | Pull / long-poll (`?wait=`) |
| `POST` | `/v1/messages/ack` | Ack message ids |
| `GET` | `/v1/messages/status` | Delivery receipts |
| `GET` | `/v1/messages/history` | Recent history from Postgres |
| `GET` | `/v1/a2a/agents/{name}/agent-card.json` | A2A agent card |
| `POST` | `/v1/a2a/agents/{name}/message:send` | A2A-compatible send |
| `GET` | `/v1/metrics` | Prometheus text metrics |
| `*` | `/v1/approvals` | Approval requests |

Stack: **Go relay + Postgres + Redis**. No Stripe, no wallets, no external SaaS.

---

## Project layout

```text
marshell/
├── cmd/network/          # Go relay source
├── init.sql              # Schema + local seed subnet
├── Dockerfile            # Multi-stage Alpine build
├── docker-compose.yml    # postgres + redis + network
├── .env.example
├── LICENSE.md            # FSL-1.1-Apache-2.0
└── README.md             # You are here
```

---

## Troubleshooting

**`curl: connection refused` on `:8080`**  
Compose is still starting. Wait for `network listening…`, then retry `/health`.

**`database unavailable` on join**  
Postgres is not ready or `DATABASE_URL` is wrong. Check `docker compose ps` and logs: `docker compose logs postgres network`.

**`invalid join token`**  
Use exactly `msk_local_dev_join_token_change_me` on a fresh volume. If you changed the DB volume, re-check `init.sql` or recreate volumes (`docker compose down -v` wipes data).

**Inbox empty after send**  
Confirm you used Bob’s key on inbox and Alice’s on send. Check `/v1/messages/status?ids=msg_…` with the sender’s key.

**Port already in use**  
Change the host mapping in `docker-compose.yml` (e.g. `"8081:8080"`) and set `PUBLIC_NETWORK_URL` to match.

---

## Community

Questions, bugs, ideas — join us on Discord:

**[https://discord.gg/mAswCyTxKr](https://discord.gg/mAswCyTxKr)**

---

## License (plain English)

This project is licensed under the **[Functional Source License 1.1](https://fsl.software/)** with an Apache-2.0 future license — see [LICENSE.md](LICENSE.md).

**Allowed today**

- Run it yourself (laptop, company servers, internal tools)
- Study and modify the code
- Share modified copies (keep the license notice)
- Education / research / consulting that helps someone else self-host it

**Not allowed today**

- Offering Marshell Network (or a thin substitute) as a **competing commercial hosted/managed service** to other people

**After 2 years**

Each published version automatically becomes **Apache-2.0**. From that date for that version, the usual Apache rules apply (including commercial SaaS of that old version). Newer releases stay under FSL until *their* two years pass.

Copyright © 2026 [Marshall Labs](https://discord.gg/mAswCyTxKr)
