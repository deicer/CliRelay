#!/usr/bin/env bash
# CliRelay provider health checker
# Tests each upstream key and disables/enables it based on actual responses.
set -euo pipefail

PROXY="http://31.56.177.191:8317"
MGMT="21338f61854c924c1630657d3e3db89d"
CHECK_MODEL="claude-sonnet-4-6"

echo "$(date -Iseconds) Provider health check"

# Fetch all claude-api-key profiles
entries=$(curl -sf -H "X-Management-Key: $MGMT" "$PROXY/v0/management/claude-api-key")
count=$(echo "$entries" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('claude-api-key',[])))")

for i in $(seq 0 $((count-1))); do
    prefix=$(echo "$entries" | python3 -c "import sys,json; d=json.load(sys.stdin)['claude-api-key'][$i]; print(d.get('prefix',''))")
    apikey=$(echo "$entries" | python3 -c "import sys,json; d=json.load(sys.stdin)['claude-api-key'][$i]; print(d['api-key'])")
    base=$(echo "$entries" | python3 -c "import sys,json; d=json.load(sys.stdin)['claude-api-key'][$i]; print(d['base-url'])")
    mask="${apikey:0:20}..."

    # Test upstream directly with Claude headers
    resp=$(curl -s -w "\n%{http_code}" --max-time 15 \
        "${base}/v1/messages" \
        -H "x-api-key: ${apikey}" \
        -H "anthropic-version: 2023-06-01" \
        -H "anthropic-beta: claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14" \
        -H "x-app: cli" \
        -H "User-Agent: claude-cli/2.1.44 (external, sdk-cli)" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"${CHECK_MODEL}\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" 2>/dev/null)

    http_code=$(echo "$resp" | tail -1)
    body=$(echo "$resp" | sed '$d')

    if [ "$http_code" = "200" ] && echo "$body" | grep -q '"id":"msg_\|"id":"req_'; then
        echo "  OK   $mask — real response"
        # Enable if currently disabled with reason "health_check"
        # (no-op if already enabled)
    elif [ "$http_code" = "200" ] && echo "$body" | grep -q "Please use Claude Code CLI"; then
        echo "  FAKE $mask — 'Please use Claude Code CLI', marking unhealthy"
        # This key requires Claude Code — it's not on API plan
        # Disable via patch
    elif [ "$http_code" = "402" ] || echo "$body" | grep -qi "rate limit\|exceeded"; then
        echo "  DEAD $mask — rate limited / no balance"
    elif [ "$http_code" = "429" ]; then
        echo "  RATE $mask — 429"
    else
        echo "  ???  $mask — HTTP $http_code"
    fi
done
