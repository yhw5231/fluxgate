# Fluxgate

Fluxgate is a lightweight, independently deployable Go gateway that reads the existing upstream SQLite configuration and provides OpenAI-compatible proxy endpoints, bounded server-side retries, weighted routing, proxy selection, and persistent circuit-breaker state.

## Requirements

- Go 1.23 or later
- An existing upstream SQLite database containing the required site, account, token, route, channel, downstream key, proxy profile, and settings tables

The gateway uses the pure-Go `modernc.org/sqlite` driver, so normal builds do not require CGO.

## Build

From the `go-gateway` directory:

```powershell
go mod download
go build -o fluxgate.exe ./cmd/fluxgate
```

Linux and macOS:

```bash
go mod download
go build -o fluxgate ./cmd/fluxgate
```

## Start

The only environment variable normally required is the path to the existing upstream SQLite database.

PowerShell:

```powershell
$env:FLUXGATE_DATABASE_PATH = "..\data/hub.db"
$env:FLUXGATE_ADDRESS = ":8081"
go run ./cmd/fluxgate
```

Linux and macOS:

```bash
export FLUXGATE_DATABASE_PATH=../data/hub.db
export FLUXGATE_ADDRESS=:8081
go run ./cmd/fluxgate
```

The default address is `:8081`, and the default database path is `../data/hub.db` relative to the process working directory.

## Container deployment

The repository includes a multi-stage `Dockerfile`, a hardened `compose.yaml`, a `.dockerignore`, and an `.env.example` configuration template. The steps below cover the full path on a fresh machine: install the toolchain, clone the repository, place the database, configure the environment, then start and verify the service.

### Prerequisites

- Docker with the Compose v2 plugin — verify with `docker compose version`:
  - Windows and macOS: install [Docker Desktop](https://docs.docker.com/desktop/).
  - Linux: install [Docker Engine](https://docs.docker.com/engine/install/) together with the `docker-compose-plugin` package.
- Git — verify with `git --version`.
- The existing upstream SQLite database file (`hub.db`) from the current management server.

### Get the code

Clone the repository and enter the project directory (the clone creates a `fluxgate` folder):

```bash
git clone https://github.com/yhw5231/fluxgate.git
cd fluxgate
```

Every command below runs from this directory. If you already have a checkout, update it instead:

```bash
git pull
```

### Prepare the database

The gateway reads an existing upstream SQLite database. Create the local data directory and place the database at `data/hub.db`:

```powershell
New-Item -ItemType Directory -Force data
Copy-Item "..\data/hub.db" "data/hub.db"
```

On Linux or macOS:

```bash
mkdir -p data
cp ../data/hub.db data/hub.db
```

If no database file exists yet, the gateway creates it together with the full configuration schema and starts healthy with an empty configuration, so a first deployment comes up before any upstream data is placed; copying the management server's `hub.db` brings the real sites, routes, and downstream keys. A database that contains only part of the schema is treated as a wrong or truncated file and refuses to start with the tables it has and the ones it lacks.

The image fixes the data-directory ownership automatically: the container starts briefly as root with only the `CHOWN`, `SETUID`, and `SETGID` capabilities, the entrypoint hands the mounted directory (and everything inside it) to the runtime user and group ID `10001`, and the gateway then runs unprivileged. No manual `chown` is required, even when Docker created a missing host directory as root. New database files are created with mode `0600` (`umask 077`).

Manual steps are only needed in two cases:

- **SELinux** (CentOS/RHEL/Rocky): ownership is correct but the label is not. Enable labeling for the mount (`selinux: z` on the Compose bind mount, or `:Z` in `docker run`) and run `sudo chcon -Rt container_file_t data` once.
- **Explicit non-root startup** (`docker run --user 10001:10001` or a Kubernetes `securityContext`): the entrypoint skips its root phase, so the directory must already be writable by that identity.

To check the directory on the host at any time:

```bash
ls -ln data   # third and fourth columns should be 10001 10001
```

> **Keep the gateway the only writer of this file.** SQLite requires all
> processes that write one database to share the same host and locking
> primitives. A bind mount on Docker Desktop reaches the file through a VM file
> share, and two processes writing through it can corrupt the database: the
> symptom is an `integrity_check` failure such as a missing index entry or a
> duplicated primary key. Verified combinations:
>
> | Writer(s) | Result |
> | --- | --- |
> | Gateway only | clean, across restarts |
> | Host process only | clean |
> | Gateway **and** a host process at the same time | corrupt within a few rounds |
>
> Run the gateway against its own database copy, or run every writer on one side
> of the boundary, when another application also updates the same SQLite file.
> The gateway refuses to start on a damaged file unless
> `FLUXGATE_INTEGRITY_CHECK=false` is set.

### Configure the environment

Copy the example configuration:

```powershell
Copy-Item .env.example .env
```

On Linux or macOS:

```bash
cp .env.example .env
```

Then edit `.env` and set `FLUXGATE_MANAGEMENT_TOKEN` to a strong random secret. Generate one with OpenSSL (available in Git Bash on Windows and on Linux):

```bash
openssl rand -hex 32
```

The token is required: management endpoints and the console answer `503 management_auth_not_configured` while it is empty. Adjust `FLUXGATE_HOST_PORT` (default `8081`) and `FLUXGATE_DATA_DIR` (default `./data`) only if the defaults do not fit.

### Deploy with Docker Compose

Build and start the service:

```bash
docker compose up -d --build
```

Check its state and logs:

```bash
docker compose ps
docker compose logs -f gateway
```

Verify the health endpoints:

```bash
curl http://127.0.0.1:8081/healthz
curl http://127.0.0.1:8081/readyz
```

Open the management console at `http://127.0.0.1:8081/console/` and sign in with the
management token from `.env`.

Stop the service without deleting the mounted database:

```bash
docker compose down
```

By default, Compose publishes host port `8081`, mounts `./data` at `/data`, and configures the gateway to use `/data/hub.db`. Override the host port or data location in `.env` with `FLUXGATE_HOST_PORT` and `FLUXGATE_DATA_DIR`.

The Compose service runs with a read-only root filesystem, drops all Linux capabilities except `CHOWN`, `SETUID`, and `SETGID` (used only by the startup entrypoint to repair data-directory ownership before dropping to the unprivileged gateway user), enables `no-new-privileges`, uses a temporary `/tmp`, persists the SQLite database through a bind mount, and includes an HTTP health check.

### Upgrade an existing deployment

Pull the latest code and rebuild the image in place. The SQLite database lives in the bind-mounted data directory, so it survives both steps:

```bash
git pull
docker compose up -d --build
docker compose ps
```

### Deploy with Docker directly

Build the image:

```bash
docker build -t fluxgate:local .
```

Run it with a persistent database directory:

```bash
docker run -d \
  --name fluxgate \
  --restart unless-stopped \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  --security-opt no-new-privileges \
  --cap-drop ALL \
  --cap-add CHOWN \
  --cap-add SETUID \
  --cap-add SETGID \
  -p 8081:8081 \
  -v "$(pwd)/data:/data" \
  -e FLUXGATE_MANAGEMENT_TOKEN="replace-with-a-strong-secret" \
  fluxgate:local
```

### Validate container configuration

Validate the resolved Compose model before deployment:

```bash
docker compose config
```

Build the image independently when troubleshooting the build pipeline:

```bash
docker build --pull -t fluxgate:local .
```

## Git management

This directory is an independent Git repository whose default branch is `main`. Runtime databases, local environment files, secrets, binaries, coverage output, temporary files, and editor metadata are excluded by `.gitignore`.

Review and create the initial commit after validating the project:

```bash
git status
git add .
git commit -m "chore: initialize standalone gateway project"
```

Add the project remote when it is available:

```bash
git remote add origin YOUR_REPOSITORY_URL
git push -u origin main
```

Do not commit `.env`, SQLite database files, private keys, or generated binaries. The sanitized `.env.example` file is intentionally tracked as the configuration template.

## HTTP endpoints

Operational endpoints:

- `GET /healthz`
- `GET /readyz`

Management console:

- `GET /console/`

The management console is a self-contained, embedded web UI served directly by the
gateway binary. It requires no separate build step, static file directory, or Node
runtime. Open it in a browser and enter the configured management token to unlock
the dashboard, which shows gateway readiness and uptime, circuit-breaker state,
upstream channels, and routable models, and refreshes itself every ten seconds.

The console page itself is unauthenticated so it can render the token prompt; every
data request it makes goes to the token-protected management endpoints below. The
token is held in the browser session only, and the panel is served with a strict
Content-Security-Policy, `nosniff`, and framing protection.

Management endpoints:

- `GET /management/status`
- `GET /management/snapshot`

Management endpoints require the token configured through `FLUXGATE_MANAGEMENT_TOKEN`. Send it as either `Authorization: Bearer YOUR_MANAGEMENT_TOKEN` or `X-Management-Token: YOUR_MANAGEMENT_TOKEN`.

When exposing the console through a reverse proxy, forward `/console/` alongside the
management endpoints. The console is optional: gateways that only need the API surface
can block the path entirely without affecting any other route.

Authenticated model and proxy endpoints:

- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/responses`
- `POST /v1/messages`

Authenticated endpoints expect an enabled, unexpired downstream API key:

```http
Authorization: Bearer YOUR_DOWNSTREAM_API_KEY
```

Proxy errors use the OpenAI error envelope with a `code` field:

- `401 invalid_api_key`: missing, unknown, expired, or exhausted key.
- `403 model_not_allowed`: the key's `supported_models` denies the model.
- `400 missing_model`, `400 invalid_json`, `413 request_too_large`.
- `503 no_available_channel`: no enabled route matches the model, or no channel
  can serve it. No upstream request is attempted.
- `502 upstream_unavailable`: every attempt against the selected channels
  failed. The response carries the last upstream status.

Example model request:

```powershell
Invoke-RestMethod `
  -Uri "http://127.0.0.1:8081/v1/models" `
  -Headers @{ Authorization = "Bearer YOUR_DOWNSTREAM_API_KEY" }
```

Example chat request:

```powershell
$body = @{
  model = "gpt-5"
  messages = @(
    @{ role = "user"; content = "Hello" }
  )
} | ConvertTo-Json -Depth 10

Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:8081/v1/chat/completions" `
  -ContentType "application/json" `
  -Headers @{ Authorization = "Bearer YOUR_DOWNSTREAM_API_KEY" } `
  -Body $body
```

## Model routing

A request is served only by channels belonging to a route that actually covers
the requested model. Routes and patterns are read from `token_routes`.

Route resolution order, highest precedence first:

1. An `explicit_group` route matched by its `display_name`.
2. A route whose `model_pattern` is an exact model name.
3. A non-group route matched by its `display_name`.
4. A non-group route matched by a glob or `re:` pattern.

Patterns are case-insensitive. A glob supports `*` (any run, including empty)
and `?` (exactly one character); every other character is literal. A pattern
starting with `re:` is a regular expression. When no route matches, the request
is answered with `503` and the error code `no_available_channel`; no upstream is
contacted. The same applies when a route matches but every channel is blocked,
excluded, or in cooldown.

Candidate channels are then filtered by:

- `route_channels.source_model`: when set, the channel only serves models that
  equal it, are alias-equivalent (a `vendor/` prefix and a trailing `-free` are
  ignored), or match it as a pattern. A channel on an exact-pattern route
  inherits that pattern as its source model when the column is empty.
- The downstream key exclusions described below.
- The circuit breaker and channels already tried by the retry loop.

The model written into the upstream request body is resolved in this order:

1. If the request named the route's `display_name` and the channel has a
   `source_model`, that source model is used.
2. Otherwise, if `model_mapping` did not rewrite the name and the route pattern
   is an exact match, the channel `source_model` is used, falling back to the
   route pattern.
3. Otherwise the mapped model is used, defaulting to the requested name.

`model_mapping` entries are evaluated in declaration order: an exact key first,
then the first matching pattern. Order is preserved rather than relying on map
iteration, so overlapping patterns resolve deterministically.

### Downstream key restrictions

- `supported_models` is an **exclusion** list. A requested model matching any
  entry (exact, glob, or `re:`) is rejected with `403` and hidden from
  `GET /v1/models`. An empty or absent value excludes nothing.
- `allowed_route_ids` limits the key to the listed routes, which are addressed
  by their public name (`display_name` when set, otherwise `model_pattern`). A
  route with an exact `model_pattern` stays visible regardless of this list.
- `excluded_site_ids` removes every channel on those sites.
- `excluded_credential_refs` removes the channel identified by an
  `{"kind":"account_token","siteId":…,"accountId":…,"tokenId":…}` entry; all
  three identifiers must match, and a channel without a token is never matched.
  Malformed entries are ignored.
- `site_weight_multipliers` scales the selection weight of the named sites.
  Weight is `route_channels.weight × sites.global_weight × multiplier`, with a
  multiplier of `1` when a site has no entry.

`GET /v1/models` lists the public model names of enabled routes, minus denied
models and minus models with no channel the key may use.

## Configuration

### Core settings

- `FLUXGATE_ADDRESS`: HTTP listen address. Default: `:8081`.
- `FLUXGATE_DATABASE_PATH`: Existing upstream SQLite database path. Default: `../data/hub.db`.
- `FLUXGATE_MAX_BODY_BYTES`: Maximum accepted JSON request body size. Default: 8 MiB.
- `FLUXGATE_INTEGRITY_CHECK`: Run `PRAGMA quick_check` against the configuration database at startup and refuse to start when it fails. Default: `true`. Disable it only if start-up time on a very large database matters more than detecting a damaged file.

### Retry settings

- `FLUXGATE_MAX_ATTEMPTS`: Maximum total upstream attempts for one downstream request. Default: `8`.
- `FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL`: Maximum attempts against one channel. Default: `2`.
- `FLUXGATE_RETRY_BASE_BACKOFF`: Initial retry backoff duration. Default: `50ms`.
- `FLUXGATE_RETRY_MAX_BACKOFF`: Maximum retry backoff duration. Default: `1s`.

Retryable status codes are `408`, `409`, `425`, `429`, `500`, `502`, `503`, and `504`. Network failures may also be retried within the configured limits. Once a streaming response has been committed to the downstream client, the gateway does not switch channels.

### Circuit-breaker settings

- `FLUXGATE_BREAKER_MODE`: Default breaker mode. Supported values: `cooldown`, `disable`, `key_cooldown`, and `key_model_cooldown`. Default: `cooldown`.
- `FLUXGATE_BREAKER_THRESHOLD`: Consecutive failure threshold. Default: `3`.
- `FLUXGATE_BREAKER_BASE_COOLDOWN`: Initial cooldown duration. Default: `30s`.
- `FLUXGATE_BREAKER_MAX_COOLDOWN`: Maximum exponential cooldown duration. Default: `15m`.

Channel-specific breaker modes loaded from the existing configuration can override the default mode. Breaker state is persisted in the Go-owned `gateway_breaker_states` table and restored after restart.

### Network timeout settings

- `FLUXGATE_REQUEST_TIMEOUT`: Default per-channel request timeout. Default: `60s`.
- `FLUXGATE_CONNECT_TIMEOUT`: TCP connection timeout. Default: `10s`.
- `FLUXGATE_TLS_HANDSHAKE_TIMEOUT`: TLS handshake timeout. Default: `10s`.
- `FLUXGATE_RESPONSE_HEADER_TIMEOUT`: Upstream response-header timeout. Default: `30s`.
- `FLUXGATE_IDLE_CONN_TIMEOUT`: Upstream idle connection timeout. Default: `90s`.
- `FLUXGATE_READ_HEADER_TIMEOUT`: Downstream request-header timeout. Default: `10s`.
- `FLUXGATE_READ_TIMEOUT`: Downstream request read timeout. Default: `30s`.
- `FLUXGATE_WRITE_TIMEOUT`: Downstream response write timeout. Default: `0`, which permits long-lived streaming responses.
- `FLUXGATE_IDLE_TIMEOUT`: Downstream keep-alive idle timeout. Default: `120s`.
- `FLUXGATE_SHUTDOWN_TIMEOUT`: Graceful shutdown timeout. Default: `15s`.

Durations use Go duration syntax, such as `250ms`, `30s`, `2m`, or `1h`.

## Proxy precedence

The gateway resolves upstream proxy configuration in this order:

1. Token or key-specific proxy
2. Site-specific proxy
3. Enabled default proxy profile
4. System proxy environment
5. Direct connection

HTTP, HTTPS, SOCKS5, and SOCKS5H proxy URLs are supported. Distinct proxy decisions use isolated HTTP transports and connection pools.

## Validation

Run the complete Go validation suite from `go-gateway`:

```powershell
gofmt -l .
go test -count=1 ./...
go vet ./...
go build ./...
```

A successful `gofmt -l .` command prints no file names.

The race detector requires CGO in the current Windows toolchain. If CGO is available, run:

```powershell
$env:CGO_ENABLED = "1"
go test -race ./...
```

## Security notes

- Do not place downstream or upstream API keys in URLs, command histories, logs, or screenshots.
- The gateway does not return upstream API keys or authenticated proxy URLs through its operational endpoints.
- The console HTML shell is served without authentication but contains no gateway data; channel names, priorities, and breaker state are only returned by the token-protected management endpoints.
- Use a reverse proxy with TLS when exposing the gateway outside a trusted network.
- Restrict filesystem access to the SQLite database because it contains sensitive account and credential configuration.
- Keep operational health endpoints separate from authenticated AI proxy endpoints when applying external access-control rules.

## Current integration boundary

The Go gateway is an independent service and does not replace the existing TypeScript management server. It reuses the existing SQLite configuration, owns only its circuit-breaker persistence table, and can run alongside the current React and TypeScript application during gradual migration.