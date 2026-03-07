# smtp-store

Local Go SMTP capture server for Reolink-style camera alerts.
Includes a built-in web UI to browse stored messages and attachments.

## Run locally

1. Copy the sample config:
```bash
cp config.example.yaml config.yaml
```
2. Edit `config.yaml` with your SMTP users, UI users, storage path, and optional TLS cert paths.
   - Set `verbose_logs: true` to log connection attempts, auth attempts, and message command flow.
   - Set a strong `web.session_secret` value.
3. Start the server:

```bash
go run ./cmd/smtp-store -config config.yaml
```

By default:
- SMTP listens on `127.0.0.1:2525`
- Web UI listens on `0.0.0.0:8080`

UI routes:
- `/login`
- `/` dashboard (recipient tree + recent feed)
- `/browse/*path`
- `/view/*path`
- `/download/*path`

## Build

Build local binary:

```bash
make build
```

Cross-build archives for macOS and Linux on `amd64` + `arm64`:

```bash
make cross-build
```

Artifacts are written to `dist/` as `smtp-store_<os>_<arch>.tar.gz`.

## Test

```bash
make test
```

## systemd (Linux)

An example unit file is included at `deploy/systemd/smtp-store.service`.

### Automated install script

You can run the installer directly on a Linux host (including LXC with systemd):

```bash
curl -fsSL https://raw.githubusercontent.com/callumj/smtp-store/main/scripts/install-linux.sh | sudo bash
```

Install a specific release tag:

```bash
curl -fsSL https://raw.githubusercontent.com/callumj/smtp-store/main/scripts/install-linux.sh | sudo bash -s -- --version v0.2.0
```

Example install steps:

```bash
# build and install binary
make build
sudo install -m 0755 bin/smtp-store /usr/local/bin/smtp-store

# create service user and directories
sudo useradd --system --home /var/lib/smtp-store --shell /usr/sbin/nologin smtp-store || true
sudo mkdir -p /etc/smtp-store /var/lib/smtp-store
sudo cp config.example.yaml /etc/smtp-store/config.yaml
sudo chown -R smtp-store:smtp-store /var/lib/smtp-store
sudo chmod 0750 /var/lib/smtp-store

# install unit and start
sudo cp deploy/systemd/smtp-store.service /etc/systemd/system/smtp-store.service
sudo systemctl daemon-reload
sudo systemctl enable --now smtp-store
sudo systemctl status smtp-store
```

If exposed beyond localhost, run the UI behind HTTPS (reverse proxy preferred).
Cookie security automatically sets `Secure` when requests arrive over TLS (including `X-Forwarded-Proto: https`).

Logs:

```bash
journalctl -u smtp-store -f
```
