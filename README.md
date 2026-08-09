# smtp-store

Local Go SMTP capture server for Reolink-style camera alerts.
Includes a built-in web UI to browse stored messages and attachments.

## Run locally

1. Copy the sample config:
```bash
cp config.example.yaml config.yaml
```
2. Edit `config.yaml` with your SMTP users, UI users, storage path, optional TLS cert paths, and classification settings.
   - Set `verbose_logs: true` to log connection attempts, auth attempts, and message command flow.
   - Set a strong `web.session_secret` value.
   - Set `classification.api_key` for Gemini.
   - Set `index_path` to a local-disk SQLite path when `storage_root` is a NAS/network mount.
   - Enable `spool` on local disk when `storage_root` is a NAS/network mount and camera alerts should survive short storage outages.
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

Dashboard performance:
- The web UI can maintain a local SQLite metadata index via `index_path`.
- Keep `index_path` on local disk, for example `/var/lib/smtp-store/index.sqlite`, so dashboard recent-file queries do not recursively scan a network-mounted `storage_root`.
- When `index_path` is empty, the UI falls back to walking `storage_root` on demand.

Local durable spool:
- When `spool.enabled` is true, SMTP messages are accepted into a local disk queue first, then a background worker writes them to `storage_root`.
- This is intended for NAS-backed storage where short outages, reboots, or stale CIFS mounts would otherwise make cameras drop alert emails.
- Classification, SQLite indexing, and MQTT notifications run only after a spooled message is successfully written to final storage.
- Keep `spool.path` on local LXC/host disk, for example `/var/lib/smtp-store/spool`.
- `spool.max_bytes` bounds accepted queued messages; once full, SMTP returns a temporary failure instead of risking local disk exhaustion.

Example:

```yaml
spool:
  enabled: true
  path: /var/lib/smtp-store/spool
  max_bytes: 10737418240
  flush_interval: 30s
```

Detection metadata:
- Video attachments are asynchronously classified for person, animal, and vehicle detections.
- Sidecars are stored as `<video_filename>.detections.json`.
- Thumbnails for successfully classified videos are stored as `<video_filename>.thumb.jpg`.
- UI tables show detection badges (`Person`, `Animal`, `Vehicle`) or states (`pending`, `failed`, `skipped`, `none`).

Dependencies for classification:
- `ffmpeg` must be installed for frame sampling.

## Home Assistant MQTT

`smtp-store` can publish Home Assistant MQTT discovery, motion state, and detection events after classification.

Example config:

```yaml
mqtt:
  enabled: true
  host: 192.168.52.57
  port: 1883
  username: ""
  password: ""
  client_id: smtp-store
  topic_prefix: smtp-store
  discovery_prefix: homeassistant
  qos: 1
  motion_reset_after: 60s
  public_base_url: https://smtp-store.lake.jonesswimclub.com
  media_token: change-this-long-random-media-token
  notify_categories:
    - person
    - animal
    - vehicle
```

Published topics:
- `homeassistant/event/smtp_store_<camera>/config`
- `homeassistant/binary_sensor/smtp_store_<camera>_motion/config`
- `smtp-store/<camera>/event`
- `smtp-store/<camera>/motion/state`

Detection event payloads include `event_type`, `camera`, `detections`, `video_url`, `thumbnail_url`, and category flags such as `has_vehicle`. The thumbnail URL uses `/media/...?...token=...` so Home Assistant automations can pass it to mobile app notifications as an image attachment.

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
