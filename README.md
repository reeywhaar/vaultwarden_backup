# vaultwarden_backup

A tiny sidecar container that backs up a [Vaultwarden](https://github.com/dani-garcia/vaultwarden)
instance: it snapshots the database and data files into an encrypted, password-protected
zip archive, keeps a local copy, and (optionally) uploads it to a cloud provider through
[backio](https://github.com/reeywhaar/backio).

Runs on a loop (every 2 hours) and applies a retention policy both locally and remotely.

## ⚠️ Important: SQLite only

**This tool only works with a Vaultwarden instance backed by SQLite** (the default).
It runs `sqlite3 .backup` against `/data/db.sqlite3`. It does **not** support MySQL/MariaDB
or PostgreSQL backends. If you use one of those, this image will not back up your database.

## What gets backed up

Everything under the mounted `/data` directory that Vaultwarden uses:

- `db.sqlite3` — the database (via `sqlite3 .backup`, a consistent hot backup)
- `rsa_key.*` — RSA key files (`.pem`/`.der`, any version) — if present
- `config.json` — admin-page configuration — if present
- `attachments/` — if present
- `sends/` — if present

Any of the optional items that don't exist are simply skipped.

These are packed into `vaultwarden-YYYYMMDD_HHMMSS.zip`, AES-256 encrypted and protected
with `BACKUP_PASSWORD`.

## Retention policy

Applied on every run, to both local (`/backups`) and remote (backio) archives:

- 3 most recent
- 1 from the previous calendar day
- 1 weekly (newest archive at least 7 days old)
- 1 monthly (newest archive at least 30 days old)

Everything else is deleted.

## Configuration

| Variable              | Required | Default              | Description                                                                                            |
| --------------------- | -------- | -------------------- | ------------------------------------------------------------------------------------------------------ |
| `BACKUP_PASSWORD`     | ✅ yes   | —                    | Password used to encrypt the zip archive. **Keep it safe — without it the backups are unrecoverable.** |
| `BACKIO_SUBDIRECTORY` | ✅ yes   | —                    | Remote path (subdirectory) where archives are stored on the provider.                                  |
| `BACKUP_TOKEN`        | no       | —                    | backio bearer token. If unset, remote upload is skipped and backups stay local only.                   |
| `BACKIO_URL`          | no       | `http://backio:8080` | URL of the backio service.                                                                             |
| `BACKIO_PROVIDER`     | no       | `gdrive`             | rclone remote name configured in backio.                                                               |

### Volumes

| Mount      | Description                                                   |
| ---------- | ------------------------------------------------------------- |
| `/data`    | Vaultwarden's data directory. Mount it **read-only** (`:ro`). |
| `/backups` | Where local archives are written and rotated.                 |

## Remote backups with backio

Remote upload is handled by [backio](https://github.com/reeywhaar/backio), a small service
that receives backup archives over HTTP and forwards them to any rclone-configured cloud
provider (Google Drive, S3, Backblaze, …).

### 1. Run backio

backio needs an rclone config (base64-encoded) and shares a Docker network with this
container so it's reachable at `http://backio:8080`.

```bash
docker network create backup-net

docker run -d \
  --name backio \
  --network backup-net \
  -v backio-data:/data \
  -e RCLONE_CONF_BASE64="$RCLONE_CONF_BASE64" \
  ghcr.io/reeywhaar/backio:latest
```

For Google Drive, backio ships a `./setup-gdrive.sh` helper that walks you through the OAuth
flow and produces the base64 rclone config. See the [backio README](https://github.com/reeywhaar/backio)
for details on other providers.

### 2. Create a token

backio uses scoped bearer tokens. Issue one that grants this backup job the permissions it
needs (`create`, `read`, `delete`) on the provider + subdirectory it will use:

```bash
docker exec backio /backio issue-token "gdrive vaultwarden create,read,delete"
```

- `gdrive` — the provider (must match `BACKIO_PROVIDER`)
- `vaultwarden` — the subdirectory (must match `BACKIO_SUBDIRECTORY`)
- `create,read,delete` — required so the job can upload, list, and prune old archives

Manage tokens with:

```bash
docker exec backio /backio list-tokens
docker exec backio /backio delete-token <token>
```

Put the printed token into `BACKUP_TOKEN`.

## Example `docker-compose.yml`

See [docker-compose.example.yml](docker-compose.example.yml) for a full example.

```yaml
services:
  vaultwarden:
    image: vaultwarden/server:latest
    container_name: vaultwarden
    restart: unless-stopped
    volumes:
      - ./data/:/data/
    environment:
      - SIGNUPS_ALLOWED=false

  backup:
    image: ghcr.io/reeywhaar/vaultwarden_backup:latest
    container_name: vaultwardenbackup
    restart: unless-stopped
    depends_on:
      - vaultwarden
    volumes:
      - ./data/:/data/:ro
      - ./backups/:/backups/
    networks:
      - default
      - backup-net
    environment:
      - BACKUP_PASSWORD=change-me-and-store-it-safely
      - BACKIO_SUBDIRECTORY=vaultwarden
      # remote upload (optional) — omit BACKUP_TOKEN for local-only backups
      - BACKIO_URL=http://backio:8080
      - BACKIO_PROVIDER=gdrive
      - BACKUP_TOKEN=your-backio-token

networks:
  # shared with the backio container
  backup-net:
    external: true
```

## Building & publishing

```bash
docker build -t ghcr.io/reeywhaar/vaultwarden_backup:latest .
docker push ghcr.io/reeywhaar/vaultwarden_backup:latest
```
