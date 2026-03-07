# smtp-store

Local Go SMTP capture server for Reolink-style camera alerts.

## Run locally

1. Copy the sample config:
```bash
cp config.example.yaml config.yaml
```
2. Edit `config.yaml` with your users, storage path, and optional TLS cert paths.
   - Set `verbose_logs: true` to log connection attempts, auth attempts, and message command flow.
3. Start the server:

```bash
go run ./cmd/smtp-store -config config.yaml
```

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
