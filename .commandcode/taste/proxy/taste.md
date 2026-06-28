# proxy
- When updating CliRelay proxy API key profiles via management API, always include existing keys in the request to avoid overwriting them. Confidence: 0.85
- Do not remove API key profiles from CliRelay configuration without explicit user instruction; the proxy should auto-detect exhausted keys and cooldown/suspend them instead of removing them. Confidence: 0.85
- Configure proxy to aggregate multiple backend API key profiles behind a single client-facing API key, so clients use one key for all upstream providers. Confidence: 0.70
- Each API key provisioned in CliRelay is on a separate plan and account (not shared across keys). Confidence: 0.70
- A 'Please use Claude Code CLI' response does NOT indicate a key is out of balance; check balance properly like Claude does when health-checking upstream API keys. Confidence: 0.82
- Proxy should automatically detect 402 rate-limited keys and switch to working ones, instead of requiring manual key removal from configuration. Confidence: 0.75
