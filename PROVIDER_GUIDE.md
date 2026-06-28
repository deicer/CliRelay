# CliRelay Provider Management Guide

This guide explains how to manage AI providers on the CliRelay proxy server.

## Server Info

| Item | Value |
|------|-------|
| URL | `http://31.56.177.191:8317` |
| Management Key | `21338f61854c924c1630657d3e3db89d` |

## Architecture

CliRelay is a proxy that aggregates multiple AI providers behind a single API endpoint.

```
Client (Claude Code / OpenAI client / any LLM tool)
    ↓ single client API key (e.g. sk-aerolink)
CliRelay Proxy
    ↓ balances between upstream keys
├─ Provider 1 key → upstream API
├─ Provider 2 key → upstream API
└─ Provider N key → upstream API
```

- **Client API keys** authenticate clients (`/v0/management/api-key-entries`)
- **Provider keys** hold upstream API credentials (`/v0/management/claude-api-key`, `gemini-api-key`, etc.)

## Supported Provider Types

| Management endpoint | Provider type | Auth method |
|---------------------|---------------|-------------|
| `claude-api-key` | Anthropic Claude | `x-api-key` / `Authorization: Bearer` |
| `gemini-api-key` | Google Gemini | `?key=` query param |
| `codex-api-key` | OpenAI Codex | `Authorization: Bearer` |
| `openai-compatibility` | Any OpenAI-compatible | `Authorization: Bearer` |
| `vertex-api-key` | Vertex-compatible | `x-goog-api-key` header |
| `bedrock-api-key` | AWS Bedrock | Access key + secret |
| `opencode-go-api-key` | OpenCode Go | `Authorization: Bearer` |

## How to Add a Provider

### Step 1: Determine the provider type

1. Check the upstream API's models endpoint: `GET {base_url}/v1/models` or `GET {base_url}/models`
2. Check which auth header it expects (`Authorization: Bearer` = openai-compatible, `x-api-key` = claude, `x-goog-api-key` = vertex)
3. Make a test request to confirm format

### Step 2: Read current config

Always **read first** before writing — `PUT` replaces ALL entries.

```bash
curl -s -H "X-Management-Key: 21338f61854c924c1630657d3e3db89d" \
  http://31.56.177.191:8317/v0/management/{provider-type}
```

### Step 3: Build the body and PUT

#### Claude API key (Anthropic-compatible, uses `x-api-key`)

```json
[
  {
    "api-key": "YOUR_API_KEY",
    "base-url": "https://api.example.com",
    "models": [
      {"name": "model-name-upstream", "alias": "model-alias-client"},
      {"name": "model-2-upstream", "alias": "model-2-alias"}
    ]
  }
]
```

Endpoint: `PUT /v0/management/claude-api-key`

#### OpenAI-compatible provider (uses `Authorization: Bearer`)

```json
[
  {
    "name": "provider-name",
    "base-url": "https://api.example.com/v1",
    "api-key-entries": [
      {"api-key": "YOUR_API_KEY"}
    ],
    "models": [
      {"name": "model-name", "alias": "short-alias"}
    ]
  }
]
```

Endpoint: `PUT /v0/management/openai-compatibility`

### Step 4: Optional — add client API keys

```bash
curl -s -X PUT "http://31.56.177.191:8317/v0/management/api-key-entries" \
  -H "X-Management-Key: 21338f61854c924c1630657d3e3db89d" \
  -H "Content-Type: application/json" \
  -d '[{"key": "sk-something", "name": "Username"}]'
```

## CRITICAL Rules

### Rule 1: PUT replaces ALL, not appends
When using `PUT`, you must include ALL existing entries in the array. To add one:
1. `GET` the current config
2. Append your new entry to the array
3. `PUT` the full array back

### Rule 2: Base URL format
- For `claude-api-key`: `https://api.example.com` (NO trailing slash, NO `/v1`)
- For `openai-compatibility`: `https://api.example.com/v1` (WITH `/v1`)

### Rule 3: `excluded-models: ["*"]` blocks everything
If `excluded-models` is set, it blocks models from this provider. Wildcards supported: `"*"`, `"gpt-*"`, `*-mini`, `*flash*`. Never include `excluded-models` unless you specifically want to block models.

### Rule 4: Prefix works per-provider
A `prefix` on a provider key means models are accessed as `prefix/model-name`. Without prefix, models appear directly. Multiple provider keys with same models and no prefix = load balancing.

### Rule 5: Aliases
`models[].alias` is optional. When set, the model is available under the alias name. When empty/absent, the model name is used directly.

### Rule 6: Verify after changes
```bash
# Check models are visible
curl -s -H "Authorization: Bearer sk-aerolink" \
  http://31.56.177.191:8317/v1/models | python3 -m json.tool

# Test actual request
curl -s http://31.56.177.191:8317/v1/messages \
  -H "Authorization: Bearer sk-aerolink" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}'
```

## Current Providers (June 2026)

| Provider | Keys | Models |
|----------|------|--------|
| Aerolink (Claude) | 2 keys | claude-sonnet-4-6, claude-opus-4-8/4-7/4-6, claude-haiku-4-5 |
| OpenCode (various) | OAuth | qwen, kimi, glm, minimax, deepseek, mimo, hy |

## Current Client Keys

| Name | Key |
|------|-----|
| Aerolink User | `sk-aerolink` |
| Женя | `sk-dx6gah88dih65iaucwg1izczohxvlh5z` |
| Влад | `sk-ozvqeu1pidg7a5qsfnswg3siyb0ogg5a` |
| Гена | `sk-usoh8bbo5yaxus3mdryfq50qch9a09er` |

## Common Patterns

### Add a new Claude-compatible provider with 2 keys
```bash
# 1. Read existing
EXISTING=$(curl -s -H "X-Management-Key: KEY" http://SERVER/v0/management/claude-api-key)

# 2. Build new array (existing + new)
curl -s -X PUT "http://SERVER/v0/management/claude-api-key" \
  -H "X-Management-Key: KEY" \
  -H "Content-Type: application/json" \
  -d '[
    ...existing entries...,
    {
      "api-key": "new-key-1",
      "base-url": "https://api.new-provider.com",
      "models": [{"name": "model-x", "alias": "model-x"}]
    },
    {
      "api-key": "new-key-2",
      "base-url": "https://api.new-provider.com",
      "models": [{"name": "model-x", "alias": "model-x"}]
    }
  ]'
```

### Test upstream API directly before adding
```bash
# Claude/Anthropic format
curl -s "https://api.example.com/v1/messages" \
  -H "x-api-key: YOUR_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'

# OpenAI format
curl -s "https://api.example.com/v1/chat/completions" \
  -H "Authorization: Bearer YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'
```
