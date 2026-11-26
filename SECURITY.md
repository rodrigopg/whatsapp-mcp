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
- A file sent by path (`send_file`, `send_audio_message`) is read by the MCP server on its own machine and uploaded to the bridge as bytes (`/api/send/file`, capped at 100 MB, held in memory, never written to disk by the bridge). Before reading, the MCP server checks the path after resolving symlinks: only files inside the Downloads folder or the temp folder (no hidden names), files `download_media` saved, or inside the folders listed in `MEDIA_ALLOWED_ROOTS` when it is set; regular files only, 100 MB at most. The file is opened one folder at a time without following symlinks and checked on the open file, and the error returned to the caller carries no path. A client that still sends `media_path` to `/api/send` gets the same rule on the bridge's disk, where nothing inside `store/` passes except downloaded media. Do not keep the install folder, backups or secrets inside Downloads or the temp folder.
- Downloads (`/api/download/file`) serve only the media the bridge stored for the given message and chat, looked up in its database, opened without following links, with fixed error texts. The MCP server saves it in `WHATSAPP_DOWNLOAD_DIR` (`0700` folder, `0600` files) under `<message id>_<file name>`; a name from WhatsApp that is not a plain file name is refused.
- The data directory is `0700` and its files `0600`; the pairing QR image is written there, not to a shared temp folder.
- Audio transcription is opt-in; nothing is sent to a third-party speech API unless you configure it.
- This is an unofficial client (whatsmeow, WhatsApp Web multi-device protocol). WhatsApp may rate-limit or ban accounts that automate; use at your own risk.
- Prompt injection: the MCP server is subject to the [lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Any incoming message is untrusted text an LLM may read, and the LLM can send messages and read private data, so a crafted message could exfiltrate data. Review tool calls that send or forward content.
- Secrets (`transcription.env`, tokens, session databases) are gitignored and must never be committed.
