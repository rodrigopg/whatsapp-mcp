#!/usr/bin/env bash
set -euo pipefail

# WhatsApp MCP — one-line installer
# Usage: curl -fsSL https://raw.githubusercontent.com/rodrigopg/whatsapp-mcp/main/install.sh | bash
#        curl -fsSL … | bash -s -- [--service] [--codex]

REPO_URL="https://github.com/rodrigopg/whatsapp-mcp.git"
INSTALL_DIR="${WHATSAPP_MCP_DIR:-$HOME/.whatsapp-mcp}"
BRIDGE_PORT="${WHATSAPP_BRIDGE_PORT:-8080}"

# ── colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}▸ $*${NC}"; }
success() { echo -e "${GREEN}✓ $*${NC}"; }
warn()    { echo -e "${YELLOW}⚠ $*${NC}"; }
die()     { echo -e "${RED}✗ $*${NC}" >&2; exit 1; }

# ── flags ────────────────────────────────────────────────────────────────────
# Works with: curl … | bash -s -- --service --codex
WITH_SERVICE=0
WITH_CODEX=0
for arg in "$@"; do
  case "$arg" in
    --service) WITH_SERVICE=1 ;;
    --codex)   WITH_CODEX=1 ;;
    *)         die "Unknown flag: $arg. Usage: install.sh [--service] [--codex]" ;;
  esac
done

echo ""
echo -e "${GREEN}╔══════════════════════════════════════╗"
echo -e "║    WhatsApp MCP  —  Installer        ║"
echo -e "╚══════════════════════════════════════╝${NC}"
echo ""

# ── detect OS ────────────────────────────────────────────────────────────────
OS="$(uname -s)"
case "$OS" in
  Darwin)  PLATFORM="macos" ;;
  Linux)   PLATFORM="linux" ;;
  *)       die "Unsupported OS: $OS. Use macOS or Linux (including WSL)." ;;
esac

# ── helpers ──────────────────────────────────────────────────────────────────
need() {
  command -v "$1" &>/dev/null || die "Required: '$1' not found. $2"
}

have() {
  command -v "$1" &>/dev/null
}

# ── check dependencies ───────────────────────────────────────────────────────
info "Checking dependencies…"

need git   "Install from https://git-scm.com"
need go    "Install from https://go.dev/dl/"

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
if [[ "$GO_MAJOR" -lt 1 || ("$GO_MAJOR" -eq 1 && "$GO_MINOR" -lt 25) ]]; then
  die "Go 1.25+ required (found $GO_VERSION). Update from https://go.dev/dl/"
fi
success "Go $GO_VERSION"

need python3 "Install from https://python.org"
success "Python $(python3 --version | awk '{print $2}')"

if ! have uv; then
  info "Installing uv (Python package manager)…"
  curl -LsSf https://astral.sh/uv/install.sh | sh
  export PATH="$HOME/.cargo/bin:$PATH"
  have uv || die "uv install failed. Try manually: curl -LsSf https://astral.sh/uv/install.sh | sh"
fi
success "uv $(uv --version | awk '{print $2}')"

if have ffmpeg; then
  success "ffmpeg found (audio conversion enabled)"
else
  warn "ffmpeg not found — audio auto-conversion disabled. Install with: brew install ffmpeg"
fi

# ── clone or update ──────────────────────────────────────────────────────────
echo ""
if [[ -d "$INSTALL_DIR/.git" ]]; then
  info "Updating existing install at ${INSTALL_DIR}…"
  git -C "$INSTALL_DIR" pull --ff-only
else
  info "Cloning into ${INSTALL_DIR}…"
  git clone "$REPO_URL" "$INSTALL_DIR"
fi
success "Repository ready"

# ── build Go bridge ──────────────────────────────────────────────────────────
info "Building Go bridge…"
cd "$INSTALL_DIR/whatsapp-bridge"
go build -tags sqlite_fts5 -o whatsapp-bridge .
success "Bridge compiled → $INSTALL_DIR/whatsapp-bridge/whatsapp-bridge"

# ── install Python deps ──────────────────────────────────────────────────────
info "Installing Python dependencies…"
cd "$INSTALL_DIR/whatsapp-mcp-server"
uv sync --quiet
success "Python environment ready"

# ── detect Claude config path ─────────────────────────────────────────────────
echo ""
info "Detecting Claude Desktop config location…"

UV_PATH="$(command -v uv)"
MCP_SERVER_PATH="$INSTALL_DIR/whatsapp-mcp-server"

if [[ "$PLATFORM" == "macos" ]]; then
  CLAUDE_CONFIG_DIR="$HOME/Library/Application Support/Claude"
  CURSOR_CONFIG_DIR="$HOME/.cursor"
else
  CLAUDE_CONFIG_DIR="$HOME/.config/Claude"
  CURSOR_CONFIG_DIR="$HOME/.cursor"
fi

MCP_JSON=$(cat <<EOF
{
  "mcpServers": {
    "whatsapp": {
      "command": "$UV_PATH",
      "args": [
        "--directory",
        "$MCP_SERVER_PATH",
        "run",
        "main.py"
      ],
      "env": {
        "WHATSAPP_BRIDGE_PORT": "$BRIDGE_PORT"
      }
    }
  }
}
EOF
)

CONFIGURED=0

# Claude Desktop
if [[ -d "$CLAUDE_CONFIG_DIR" ]]; then
  CONFIG_FILE="$CLAUDE_CONFIG_DIR/claude_desktop_config.json"
  if [[ -f "$CONFIG_FILE" ]]; then
    warn "Claude Desktop config already exists at $CONFIG_FILE"
    warn "Merge the following manually if needed:"
    echo ""
    echo "$MCP_JSON"
    echo ""
  else
    mkdir -p "$CLAUDE_CONFIG_DIR"
    echo "$MCP_JSON" > "$CONFIG_FILE"
    success "Claude Desktop config written to $CONFIG_FILE"
    CONFIGURED=1
  fi
else
  warn "Claude Desktop config dir not found at $CLAUDE_CONFIG_DIR (install Claude Desktop first)"
fi

# Cursor
if [[ -d "$CURSOR_CONFIG_DIR" ]]; then
  CURSOR_CONFIG="$CURSOR_CONFIG_DIR/mcp.json"
  if [[ -f "$CURSOR_CONFIG" ]]; then
    warn "Cursor config already exists at $CURSOR_CONFIG (merge manually if needed)"
  else
    echo "$MCP_JSON" > "$CURSOR_CONFIG"
    success "Cursor config written to $CURSOR_CONFIG"
    CONFIGURED=1
  fi
fi

# ── create launch script ─────────────────────────────────────────────────────
LAUNCH_SCRIPT="$INSTALL_DIR/start-bridge.sh"
cat > "$LAUNCH_SCRIPT" <<LAUNCH
#!/usr/bin/env bash
cd "$INSTALL_DIR/whatsapp-bridge"
# Transcription is opt-in. To enable it, create transcription.env next to this
# script with your engine vars (TRANSCRIPTION_ENGINE, WHISPER_CLI/WHISPER_MODEL
# or TRANSCRIPTION_API_KEY). Sourcing it here means both this launcher AND the
# launchd auto-start inherit them — launchd does NOT see your shell's exports.
[ -f "$INSTALL_DIR/transcription.env" ] && . "$INSTALL_DIR/transcription.env"
WHATSAPP_BRIDGE_PORT=$BRIDGE_PORT exec ./whatsapp-bridge
LAUNCH
chmod +x "$LAUNCH_SCRIPT"
success "Launch script: $LAUNCH_SCRIPT"

# ── service helpers (--service) ──────────────────────────────────────────────
bridge_already_running() {
  curl -s --max-time 2 "http://localhost:$BRIDGE_PORT/qr" &>/dev/null && return 0
  pgrep -f '/whatsapp-bridge$' &>/dev/null && return 0
  return 1
}

install_service_linux() {
  if bridge_already_running; then
    warn "bridge already running (existing install?) — skipping service setup"
    return 0
  fi
  if ! systemctl --user show-environment &>/dev/null; then
    warn "systemctl --user unavailable (no user bus) — skipping service setup"
    return 0
  fi

  UNIT_PATH="$HOME/.config/systemd/user/whatsapp-bridge.service"
  UNIT_CONTENT="[Unit]
Description=WhatsApp MCP bridge

[Service]
ExecStart=$INSTALL_DIR/start-bridge.sh
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
"
  if [[ -f "$UNIT_PATH" ]] && [[ "$(cat "$UNIT_PATH")" != "$(printf '%s' "$UNIT_CONTENT")" ]]; then
    warn "existing $UNIT_PATH differs from generated unit — not touching it"
    return 0
  fi

  mkdir -p "$(dirname "$UNIT_PATH")"
  printf '%s' "$UNIT_CONTENT" > "$UNIT_PATH"
  systemctl --user daemon-reload
  systemctl --user enable --now whatsapp-bridge
  success "systemd user service enabled: whatsapp-bridge (logs: journalctl --user -u whatsapp-bridge)"

  if ! loginctl enable-linger "${USER:-$(id -un)}" &>/dev/null; then
    warn "loginctl enable-linger failed — service will stop when you log out"
  fi
}

install_service_macos() {
  if bridge_already_running; then
    warn "bridge already running (existing install?) — skipping service setup"
    return 0
  fi
  if launchctl print "gui/$(id -u)/com.whatsapp-mcp.bridge" &>/dev/null; then
    warn "launchd service already loaded — skipping"
    return 0
  fi
  if [[ -f "$PLIST_PATH" ]]; then
    # Only overwrite a plist we would have generated ourselves — either variant
    # (inert from a no-flag install, or active) — same rule as the Linux unit.
    local expected
    expected="$(mktemp)"
    write_plist true "$expected"
    if ! cmp -s "$expected" "$PLIST_PATH"; then
      write_plist false "$expected"
      if ! cmp -s "$expected" "$PLIST_PATH"; then
        rm -f "$expected"
        warn "existing $PLIST_PATH differs from generated plist — not touching it"
        return 0
      fi
    fi
    rm -f "$expected"
  fi
  write_plist true "$PLIST_PATH"
  launchctl load "$PLIST_PATH"
  success "launchd service loaded (auto-starts at login): com.whatsapp-mcp.bridge"
}

# ── macOS launchd (optional auto-start) ──────────────────────────────────────
write_plist() {
  local bool_tag="<false/>" out="${2:-$PLIST_PATH}"
  [[ "$1" == "true" ]] && bool_tag="<true/>"
  cat > "$out" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>com.whatsapp-mcp.bridge</string>
  <key>ProgramArguments</key>  <array><string>$INSTALL_DIR/start-bridge.sh</string></array>
  <key>RunAtLoad</key>         $bool_tag
  <key>KeepAlive</key>         $bool_tag
  <key>StandardOutPath</key>   <string>$INSTALL_DIR/bridge.log</string>
  <key>StandardErrorPath</key> <string>$INSTALL_DIR/bridge.log</string>
</dict>
</plist>
PLIST
}

if [[ "$PLATFORM" == "macos" ]]; then
  PLIST_PATH="$HOME/Library/LaunchAgents/com.whatsapp-mcp.bridge.plist"
  if [[ $WITH_SERVICE -eq 1 ]]; then
    install_service_macos
  elif [[ ! -f "$PLIST_PATH" ]]; then
    write_plist false
    success "launchd plist written (not loaded — use 'launchctl load $PLIST_PATH' to auto-start)"
  fi
fi

# ── Linux systemd user service (--service) ───────────────────────────────────
if [[ "$PLATFORM" == "linux" && $WITH_SERVICE -eq 1 ]]; then
  install_service_linux
fi

# ── Codex CLI registration (--codex) ─────────────────────────────────────────
if [[ $WITH_CODEX -eq 1 ]]; then
  CODEX_SNIPPET="[mcp_servers.whatsapp]
command = \"$UV_PATH\"
args = [\"--directory\", \"$INSTALL_DIR/whatsapp-mcp-server\", \"run\", \"main.py\"]
env = { WHATSAPP_BRIDGE_PORT = \"$BRIDGE_PORT\" }"
  CODEX_CONFIG="$HOME/.codex/config.toml"
  if [[ ! -d "$HOME/.codex" ]]; then
    warn "Codex CLI not found (~/.codex missing) — add this to its config.toml manually:"
    echo ""
    echo "$CODEX_SNIPPET"
    echo ""
  elif [[ ! -f "$CODEX_CONFIG" ]]; then
    if echo "$CODEX_SNIPPET" > "$CODEX_CONFIG" 2>/dev/null; then
      success "Codex config created: $CODEX_CONFIG"
    else
      warn "Could not write $CODEX_CONFIG — add this manually:"
      echo ""
      echo "$CODEX_SNIPPET"
      echo ""
    fi
  elif grep -q 'mcp_servers' "$CODEX_CONFIG"; then
    # Any mcp_servers form (table, dotted key, inline table) — appending a second
    # [mcp_servers.whatsapp] table could corrupt the TOML, so hand off to the user.
    warn "mcp_servers already configured in $CODEX_CONFIG — not touching it. Merge manually if needed:"
    echo ""
    echo "$CODEX_SNIPPET"
    echo ""
  else
    if { echo ""; echo "$CODEX_SNIPPET"; } >> "$CODEX_CONFIG" 2>/dev/null; then
      success "Codex config updated: $CODEX_CONFIG (appended [mcp_servers.whatsapp])"
    else
      warn "Could not append to $CODEX_CONFIG — add this manually:"
      echo ""
      echo "$CODEX_SNIPPET"
      echo ""
    fi
  fi
fi

# ── done ─────────────────────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}══════════════════════════════════════════${NC}"
echo -e "${GREEN}  Installation complete!${NC}"
echo -e "${GREEN}══════════════════════════════════════════${NC}"
echo ""
echo "  Next steps:"
echo ""
echo "  1. Start the bridge:"
echo -e "     ${CYAN}$LAUNCH_SCRIPT${NC}"
echo ""
echo "  2. Open in your browser to scan the QR code:"
echo -e "     ${CYAN}http://localhost:$BRIDGE_PORT/qr${NC}"
echo ""
echo "  3. Restart Claude Desktop or Cursor"
echo ""
if [[ $CONFIGURED -eq 0 ]]; then
  echo -e "  ${YELLOW}⚠ Could not auto-configure your MCP client.${NC}"
  echo    "    Add this to your claude_desktop_config.json or ~/.cursor/mcp.json:"
  echo ""
  echo "$MCP_JSON"
  echo ""
fi
