#!/usr/bin/env bash
#
# nmonit — Install script
# ==========================
# Installs the nmonit binary and configuration files.

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

BINARY_NAME="nmonit"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
CONFIG_DIR="${CONFIG_DIR:-/etc/nmonit}"
SYSTEMD_DIR="${SYSTEMD_DIR:-/etc/systemd/system}"
DATA_DIR="${DATA_DIR:-/var/lib/nmonit}"
LOG_DIR="${LOG_DIR:-/var/log/nmonit}"
USER="${NMONIT_USER:-nmonit}"

echo -e "${BLUE}╔══════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║       nmonit — Install Script           ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════╝${NC}"
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    echo -e "${YELLOW}Warning: Not running as root. Some operations may fail.${NC}"
    echo -e "${YELLOW}Try: sudo bash scripts/install.sh${NC}"
    echo ""
fi

# Build the binary
echo -e "${GREEN}→ Building nmonit...${NC}"
if ! command -v cargo &> /dev/null; then
    echo -e "${RED}Error: Rust/Cargo is not installed. Please install Rust first:${NC}"
    echo "  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh"
    exit 1
fi

cargo build --release
echo -e "${GREEN}✓ Build complete${NC}"
echo ""

# Install binary
echo -e "${GREEN}→ Installing binary to ${INSTALL_DIR}...${NC}"
mkdir -p "$INSTALL_DIR"
cp "target/release/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"
chmod 755 "$INSTALL_DIR/$BINARY_NAME"
echo -e "${GREEN}✓ Binary installed: ${INSTALL_DIR}/${BINARY_NAME}${NC}"
echo ""

# Create nmonit user
if ! id -u "$USER" &>/dev/null 2>&1; then
    echo -e "${GREEN}→ Creating nmonit system user...${NC}"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$USER" 2>/dev/null || true
    echo -e "${GREEN}✓ User created: ${USER}${NC}"
fi

# Create directories
echo -e "${GREEN}→ Creating directories...${NC}"
mkdir -p "$CONFIG_DIR"
mkdir -p "$DATA_DIR"
mkdir -p "$LOG_DIR"
chown "$USER:$USER" "$DATA_DIR"
chown "$USER:$USER" "$LOG_DIR"
echo -e "${GREEN}✓ Directories created${NC}"

# Install config
if [[ ! -f "$CONFIG_DIR/nmonit.yaml" ]]; then
    echo -e "${GREEN}→ Installing configuration...${NC}"
    cp config/nmonit.yaml "$CONFIG_DIR/nmonit.yaml"
    chmod 644 "$CONFIG_DIR/nmonit.yaml"
    echo -e "${GREEN}✓ Config installed: ${CONFIG_DIR}/nmonit.yaml${NC}"
else
    echo -e "${YELLOW}→ Config already exists at ${CONFIG_DIR}/nmonit.yaml (skipping)${NC}"
fi

# Install systemd services
echo -e "${GREEN}→ Installing systemd services...${NC}"
for service in nmonit-host.service nmonit-worker.service; do
    if [[ -f "systemd/$service" ]]; then
        cp "systemd/$service" "$SYSTEMD_DIR/$service"
        chmod 644 "$SYSTEMD_DIR/$service"
        echo -e "${GREEN}✓ Service installed: ${service}${NC}"
    fi
done

# Reload systemd
systemctl daemon-reload 2>/dev/null || true

echo ""
echo -e "${GREEN}╔══════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║       Installation Complete!             ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════╝${NC}"
echo ""
echo -e "To start the nmonit ${BLUE}host${NC}:"
echo -e "  ${YELLOW}sudo systemctl enable --now nmonit-host${NC}"
echo ""
echo -e "To start the nmonit ${BLUE}worker${NC}:"
echo -e "  ${YELLOW}sudo systemctl enable --now nmonit-worker${NC}"
echo ""
echo -e "Or run directly:"
echo -e "  ${YELLOW}nmonit host --config /etc/nmonit/nmonit.yaml${NC}"
echo -e "  ${YELLOW}nmonit worker --host <host-ip> --token <token>${NC}"
echo ""
echo -e "View cluster status:"
echo -e "  ${YELLOW}nmonit status${NC}"
echo ""
echo -e "View available models:"
echo -e "  ${YELLOW}nmonit models${NC}"
echo ""
