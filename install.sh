#!/usr/bin/env bash
set -euo pipefail

# WhatsApp MCP — one-line installer
# Usage: curl -fsSL https://raw.githubusercontent.com/rodrigopg/whatsapp-mcp/main/install.sh | bash

REPO_URL="https://github.com/rodrigopg/whatsapp-mcp.git"
INSTALL_DIR="${WHATSAPP_MCP_DIR:-$HOME/.whatsapp-mcp}"
BRIDGE_PORT="${WHATSAPP_BRIDGE_PORT:-8080}"

# ── colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}▸ $*${NC}"; }
success() { echo -e "${GREEN}✓ $*${NC}"; }
warn()    { echo -e "${YELLOW}⚠ $*${NC}"; }
die()     { echo -e "${RED}✗ $*${NC}" >&2; exit 1; }

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
  info "Updating existing install at $INSTALL_DIR…"
  git -C "$INSTALL_DIR" pull --ff-only
else
  info "Cloning into $INSTALL_DIR…"
  git clone "$REPO_URL" "$INSTALL_DIR"
fi
success "Repository ready"

# ── build Go bridge ──────────────────────────────────────────────────────────
info "Building Go bridge…"
cd "$INSTALL_DIR/whatsapp-bridge"
go build -o whatsapp-bridge .
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

# ── macOS launchd (optional auto-start) ──────────────────────────────────────
if [[ "$PLATFORM" == "macos" ]]; then
  PLIST_PATH="$HOME/Library/LaunchAgents/com.whatsapp-mcp.bridge.plist"
  if [[ ! -f "$PLIST_PATH" ]]; then
    cat > "$PLIST_PATH" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>com.whatsapp-mcp.bridge</string>
  <key>ProgramArguments</key>  <array><string>$INSTALL_DIR/start-bridge.sh</string></array>
  <key>RunAtLoad</key>         <false/>
  <key>KeepAlive</key>         <false/>
  <key>StandardOutPath</key>   <string>$INSTALL_DIR/bridge.log</string>
  <key>StandardErrorPath</key> <string>$INSTALL_DIR/bridge.log</string>
</dict>
</plist>
PLIST
    success "launchd plist written (not loaded — use 'launchctl load $PLIST_PATH' to auto-start)"
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
