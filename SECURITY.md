# Security policy

## Reporting a vulnerability

Do not open a public issue. Use GitHub's private vulnerability reporting:
**Security tab > Report a vulnerability** on this repository. Include steps to reproduce and the affected version or commit.

## Supported versions

Only the latest `main`. Fixes land there; there are no backport branches.

## Security model

- The bridge is a local service that holds your full WhatsApp session and message history (SQLite on disk). Treat its data directory like a password.
- The REST API binds to `127.0.0.1` by default.
- Binding beyond loopback (`BIND_ADDR`) requires `API_AUTH_TOKEN`; the bridge refuses to start without it. A VPN or private network is not a substitute for the token.
- Without a token (loopback only), every `/api/*` request must carry a loopback `Host` header (or a name in `API_ALLOWED_HOSTS`), and requests made by a web page (`Origin` present, or a cross-site `Sec-Fetch-Site`) are refused. With a token, the bearer token is required on every `/api/*` request and compared in constant time.
- Media paths are validated: components with separators or `..` are rejected, not silently sanitized.
- A file sent by path (`media_path`) is checked after resolving symlinks: nothing inside the bridge's `store/` except downloaded media, no hidden path components unless the folder is listed in `MEDIA_ALLOWED_ROOTS`, regular files only, 100 MB at most. With `MEDIA_ALLOWED_ROOTS` set, only files inside those folders are sent.
- The data directory is `0700` and its files `0600`; the pairing QR image is written there, not to a shared temp folder.
- Audio transcription is opt-in; nothing is sent to a third-party speech API unless you configure it.
- This is an unofficial client (whatsmeow, WhatsApp Web multi-device protocol). WhatsApp may rate-limit or ban accounts that automate; use at your own risk.
- Prompt injection: the MCP server is subject to the [lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Any incoming message is untrusted text an LLM may read, and the LLM can send messages and read private data, so a crafted message could exfiltrate data. Review tool calls that send or forward content.
- Secrets (`transcription.env`, tokens, session databases) are gitignored and must never be committed.
