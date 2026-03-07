#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-callumj/smtp-store}"
VERSION="${VERSION:-latest}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
CONFIG_DIR="${CONFIG_DIR:-/etc/smtp-store}"
DATA_DIR="${DATA_DIR:-/var/lib/smtp-store}"
SERVICE_NAME="${SERVICE_NAME:-smtp-store}"
SERVICE_USER="${SERVICE_USER:-smtp-store}"
NO_START="false"

usage() {
  cat <<USAGE
Usage: $0 [--version <tag>] [--repo <owner/name>] [--no-start]

Examples:
  $0
  $0 --version v0.2.0
  REPO=callumj/smtp-store $0
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      VERSION="${2:-}"
      shift 2
      ;;
    --repo)
      REPO="${2:-}"
      shift 2
      ;;
    --no-start)
      NO_START="true"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -E bash "$0" "$@"
  fi
  echo "Run as root or install sudo." >&2
  exit 1
fi

for cmd in curl tar install systemctl; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
done

case "$(uname -m)" in
  x86_64|amd64)
    ARCH="amd64"
    ;;
  aarch64|arm64)
    ARCH="arm64"
    ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

ASSET="smtp-store_linux_${ARCH}.tar.gz"
if [[ "$VERSION" == "latest" ]]; then
  DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
else
  DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
fi

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

ARCHIVE_PATH="${TMP_DIR}/${ASSET}"
EXTRACT_DIR="${TMP_DIR}/extract"
mkdir -p "$EXTRACT_DIR"

echo "Downloading: ${DOWNLOAD_URL}"
curl -fL "${DOWNLOAD_URL}" -o "$ARCHIVE_PATH"

echo "Extracting release archive"
tar -xzf "$ARCHIVE_PATH" -C "$EXTRACT_DIR"

if [[ ! -f "${EXTRACT_DIR}/smtp-store" ]]; then
  echo "Release archive is missing smtp-store binary." >&2
  exit 1
fi
if [[ ! -f "${EXTRACT_DIR}/config.yaml" ]]; then
  echo "Release archive is missing config.yaml sample." >&2
  exit 1
fi

mkdir -p "$BIN_DIR" "$CONFIG_DIR" "$DATA_DIR"
install -m 0755 "${EXTRACT_DIR}/smtp-store" "${BIN_DIR}/smtp-store"

CONFIG_PATH="${CONFIG_DIR}/config.yaml"
if [[ ! -f "$CONFIG_PATH" ]]; then
  install -m 0640 "${EXTRACT_DIR}/config.yaml" "$CONFIG_PATH"
  echo "Installed sample config to ${CONFIG_PATH}"
else
  echo "Keeping existing config at ${CONFIG_PATH}"
fi

if ! getent group "$SERVICE_USER" >/dev/null 2>&1; then
  groupadd --system "$SERVICE_USER"
fi

if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  NOLOGIN_BIN="/usr/sbin/nologin"
  if [[ ! -x "$NOLOGIN_BIN" ]]; then
    NOLOGIN_BIN="/sbin/nologin"
  fi
  useradd --system --home "$DATA_DIR" --shell "$NOLOGIN_BIN" --gid "$SERVICE_USER" "$SERVICE_USER"
fi

chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
chmod 0750 "$DATA_DIR"

UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=SMTP Store (local SMTP capture service)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}
ExecStart=${BIN_DIR}/smtp-store -config ${CONFIG_PATH}
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT

chmod 0644 "$UNIT_PATH"

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
if [[ "$NO_START" == "false" ]]; then
  systemctl restart "$SERVICE_NAME"
fi

echo
echo "Install complete."
echo "Service: ${SERVICE_NAME}"
echo "Binary: ${BIN_DIR}/smtp-store"
echo "Config: ${CONFIG_PATH}"
echo "Data:   ${DATA_DIR}"
echo
if [[ "$NO_START" == "false" ]]; then
  systemctl --no-pager --full status "$SERVICE_NAME" || true
fi

echo "Next steps:"
echo "1) Edit ${CONFIG_PATH} (set web.session_secret and credentials)."
echo "2) Restart service: systemctl restart ${SERVICE_NAME}"
echo "3) Tail logs: journalctl -u ${SERVICE_NAME} -f"
