# Fluxgate

Fluxgate is a lightweight, independently deployable Go gateway that reads the existing upstream SQLite configuration and provides OpenAI-compatible proxy endpoints, bounded server-side retries, weighted routing, proxy selection, and persistent circuit-breaker state. It ships with an embedded management console that both shows the running state and edits that configuration: upstreams with their keys and models, the routing the gateway derives from them, client keys, proxy profiles, and the retry, failover, and circuit-breaker policy the gateway applies while it runs.

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

`FLUXGATE_ADMIN_PASSWORD` is optional: leave it empty and the gateway creates
`admin` / `admin` on first start, forcing a password change at first sign-in. Set it to
choose that initial password yourself, in which case the account is ready to use with no
forced change, because the password is one you picked rather than a published default.
Either way the variable has no effect once the account exists, so it can be removed from
`.env` afterwards.

`FLUXGATE_MANAGEMENT_TOKEN` is optional and no longer used for console sign-in. Set it
only if scripts or automation need to call the management endpoints directly. Generate
one with OpenSSL (available in Git Bash on Windows and on Linux):

```bash
openssl rand -hex 32
```

Adjust `FLUXGATE_HOST_PORT` (default `8081`) and `FLUXGATE_DATA_DIR` (default `./data`)
only if the defaults do not fit.

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

Open the management console at `http://127.0.0.1:8081/console/` and sign in. A gateway
with no administrator accepts `admin` / `admin` once and then requires a new password.

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
  -e FLUXGATE_ADMIN_PASSWORD="replace-with-a-strong-password" \
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
runtime. Open it in a browser, sign in with the administrator account, and the
dashboard shows gateway readiness and uptime, circuit-breaker state, upstream
channels, and routable models, refreshing itself every ten seconds. The interface is
in Simplified Chinese; the endpoints it calls are unchanged, and the gateway's own
error codes stay stable for automation while the console renders them in Chinese.

### Managing the configuration from the console

The console is also where the configuration is edited. Its main view is 上游
(upstreams): one row per upstream service, holding everything that upstream needs
— a name, an API address, a weight, request headers, an optional proxy, its keys,
and the models it serves. Adding an upstream and picking its models is all it takes
to route them; the routes appear by themselves. Every accepted write reloads the
configuration and hands the result to the routing engine, so an upstream added in
the console serves traffic on the next request.

The views are 概览 (the live dashboard), 上游 (upstreams), 路由 (the routing table
the gateway derived, shown read-only), 客户端密钥 (client keys), and 设置 (the
administrator account, the runtime policy, and proxy profiles).

The upstream form is built around the four things that actually vary between
upstreams:

- **Keys are pasted in one go.** One key per line, as many as needed. The stored
  keys are shown masked and are matched by position: an untouched line keeps the
  key it stands for, a changed line replaces it, an added line is stored, and a
  removed line retires that key.
- **Key selection has two modes.** 可用优先 (available first) prefers the first key
  and moves to the next one only when it fails or is cooling down; 轮询 (round
  robin) spreads requests across every key by weight.
- **Models are picked, not typed.** 获取模型 asks the upstream itself for its model
  list, so the operator selects from what it really serves; a model can also be
  typed in by hand. Each selected model takes an optional upstream model name —
  the spelling that upstream knows, when it differs from the name clients use.
- **Routing is derived.** Selecting a model creates its route and one line per key;
  deselecting it removes them once no other upstream serves that model. A route the
  operator wrote by hand through the API is never pruned by a console edit.

Each editable table has an 添加 button, and every row has 编辑 and 删除 actions;
client keys additionally have 轮换, which mints a new value and invalidates the old
one immediately. The 概览 view lists the recorded circuits and can clear one (恢复)
or all of them (全部恢复), which is how a cooled-down or disabled line returns to
service before its cooldown would have expired.

A few rules the console enforces, all of them on the server rather than in the
browser:

- **Secrets are write-only.** An upstream key or client key is stored as written
  but returned as a mask showing its last four characters. The form for an existing
  row starts with the mask and leaves the stored value alone unless a new value is
  typed, so a mask can never be saved back over a credential. A client key is shown
  in full exactly once, when it is created or rotated.
- **References are checked.** A channel must name a route and an account, ids have
  to exist, and a delete is refused while other rows still point at what it would
  remove — the refusal says how many and of which kind. Deleting an upstream does
  take its credentials, its lines, and the routes that only existed for it, and the
  console reports how many went.
- **Values are typed and bounded.** Model mappings, custom headers, exclusion
  lists, and weight multipliers are validated as JSON of the expected shape; a URL
  has to be one the transport can actually use; text has a byte bound. A rejected
  field is answered with `400 invalid_configuration` carrying the field name, a
  stable `reason` code, and its parameters, which the console renders in Chinese
  and highlights on the form.
- **A write is same-origin.** The console authenticates with a cookie, so a
  state-changing request — including an upstream probe — is only accepted from the
  gateway's own origin. The management token remains accepted, because a script
  carries it in a header a page cannot set.

Every write is logged as `console_configuration_created`, `…_updated`, `…_deleted`,
`…_key_rotated`, `console_policy_updated`, or `console_breakers_reset` with the
resource, the row id, and the client address. Values are never logged, because a
write carries credentials.

The console page itself is unauthenticated so it can render the sign-in form; every
data request it makes — reads and writes alike — goes to the authenticated management
endpoints below. Sign-in issues an HTTP-only session cookie, so no credential is kept
in JavaScript-accessible storage, and the panel is served with a strict
Content-Security-Policy, `nosniff`, and framing protection.

### Sign in and the default password

A gateway that has no administrator creates one on first start with the username
`admin` and the password `admin`. Sign in with those credentials and the console
immediately asks for a new password. Until that password is changed the account can
reach nothing: every data endpoint answers `403 password_change_required`, so the
well-known default is only ever good for setting a real password. The startup log
records `default_admin_account_created` when this happens.

There is no minimum length and no complexity rule. The only rejected values are an
empty password (sign-in requires one, so it could never be used again) and input
longer than 4096 bytes (a bound that keeps hashing cheap under abuse). Passwords
longer than 72 bytes work correctly: bcrypt stops at 72, so the gateway hashes a
SHA-256 of the password first and the full value stays significant.

### Change or recover the password

Change it from the account page, reached from the gear (设置) in the top bar. The form
requires the current password, so a stolen session cookie alone cannot lock the
operator out, and it disables autofill so a password manager cannot silently rotate
the stored credential. Sessions survive the change, so you stay signed in.

To set the password without the console — before first use, or to recover a lost one —
use the CLI on the host that holds the database:

```bash
docker compose exec gateway /app/fluxgate admin reset
```

Or for a binary deployment:

```bash
./fluxgate admin reset --database ./data/hub.db
```

The password is read from a terminal prompt with echo disabled, so it never appears in
the process list or your shell history. `reset` clears the change-required flag and
revokes every session, because an operator running it has already chosen the password
deliberately. Related commands:

```bash
./fluxgate admin status   # report whether an account exists
./fluxgate admin create   # create the account explicitly instead of at startup
```

Set `FLUXGATE_ADMIN_PASSWORD` to supply the password non-interactively. The container
entrypoint uses it with `admin create` on first start, which is the convenient path for
Compose; the account needs no forced change because the password came from you rather
than from the published default. Remove the variable from `.env` once the account
exists.

Sign-in is rate limited per client address: after 5 failed attempts further attempts
are refused with `429` for 15 minutes, including attempts with the correct password.
Every attempt is logged as `console_login_succeeded`, `console_login_failed`, or
`console_login_locked`, and password changes as `console_password_changed` or
`console_password_change_failed`; passwords and session tokens are never logged.

Management endpoints:

- `GET /management/status`
- `GET /management/snapshot`
- `GET /management/configuration`
- `POST /management/configuration/{resource}`
- `PUT /management/configuration/{resource}/{id}`
- `DELETE /management/configuration/{resource}/{id}`
- `POST /management/configuration/keys/{id}/rotate`
- `PUT /management/policy`
- `POST /management/upstreams/models`
- `POST /management/breakers/reset`
- `GET /management/session`
- `POST /management/login`
- `POST /management/logout`
- `POST /management/password`

`{resource}` is one of `upstreams`, `sites`, `accounts`, `tokens`, `routes`,
`channels`, `keys`, or `proxies`. `GET /management/configuration` returns every row of
each resource with secrets masked, the writable field list of each resource, the models
the gateway currently routes, and the runtime policy with its field list, so the console
builds its tables and forms from one payload. A successful write answers with the stored
row and the reloaded payload, so the console repaints from a single round trip; when a
key value was minted it is returned once as `generated.key`. A create needs the fields
the resource marks as required and takes the schema default for the rest; an update
applies only the fields the request carries, so a partial update from automation never
clears what it did not mention.

`upstreams` is the composite resource the console's upstream form writes, and the only
one whose fields are assembled from several tables. Alongside the columns of `sites` it
carries four synthetic fields: `keys` (a JSON array of strings, one per key, matched by
position), `key_mode` (`available_first` or `round_robin`), `models` (the exposed model
names the upstream serves), and `model_mapping` (a JSON object of exposed name to the
name that upstream knows). Writing `keys` or `models` creates the account, the token
rows, the routes, and the channels behind them; the route ids the console created are
recorded in the settings table under `gateway.managed_routes`, so an edit prunes only
what it owns. The tables themselves remain addressable one by one for a detail the form
does not show, such as a forced endpoint or a per-key proxy.

`PUT /management/policy` stores runtime policy overrides, and `POST
/management/breakers/reset` clears one recorded circuit (`scope` of `channel`, `key`, or
`key_model` with the identifying fields) or every circuit (`scope: "all"`). `POST
/management/upstreams/models` asks an upstream for its model list: the body carries the
`url`, one `key`, and any `headers` from the form before it is saved, or an `id` and a
masked key so the gateway probes with the key it already stores. The key is used for
that one outbound request and is never part of a response.

The write endpoints refuse a request that changes nothing valid: `400` with
`invalid_configuration` for a bad value (the body names the `field` and a stable
`reason`), `404 unknown_resource` or `configuration_not_found`, `409
configuration_referenced` when a delete would orphan other rows, `409
configuration_conflict` for a duplicate value such as a client key, and `403
cross_origin_rejected` for a state-changing request from another origin.

`/management/session`, `/management/login`, and `/management/logout` manage the console
session itself and are therefore not themselves protected by one. `/management/password`
requires a session but works while a password change is pending, which is the only way
out of that state. Every other management endpoint accepts either a console session
cookie or the configured management token, so existing automation keeps
working. Send the token as either `Authorization: Bearer YOUR_MANAGEMENT_TOKEN` or
`X-Management-Token: YOUR_MANAGEMENT_TOKEN`.

When exposing the console through a reverse proxy, forward `/console/` alongside the
management endpoints. The console is optional: gateways that only need the API surface
can block the path entirely without affecting any other route.

Any `GET` the gateway does not otherwise serve redirects to the console, so a bare domain
(`https://gateway.example/`) and an unknown path such as `/admin` both open the console
instead of returning a bare `404`. Requests under the API namespaces (`/v1/`,
`/management/`, `/console/`) keep their `404`, because a mistyped endpoint is a client
mistake rather than a browser navigation.

The console is built to survive a proxy that mounts it under a path prefix and strips
that prefix before forwarding: the redirect target is relative, and the console loads
its assets and calls `/management/*` relative to the URL the page was served from. With
this nginx configuration:

```nginx
location /gateway/ {
    proxy_pass http://127.0.0.1:8081/;   # trailing slash strips the /gateway prefix
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # Needed so https://host/gateway (no trailing slash) keeps the prefix.
    proxy_set_header X-Forwarded-Prefix /gateway;
}
```

`https://host/gateway/` redirects to `/gateway/console/`, which then loads
`/gateway/console/styles.css`, `/gateway/console/app.js`, and `/gateway/management/*`.
Because the redirect is resolved by the browser against the public URL, no
`proxy_redirect` rewriting is needed, and the `/v1/...` and `/management/...` routes stay
reachable as long as the proxy forwards them to the gateway.

`X-Forwarded-Prefix` matters for the slash-less form. When a visitor opens `https://host/gateway`
without the trailing slash, the browser treats `gateway` as a file name and resolves a
relative redirect against the parent directory, which would drop the prefix and land on
`/console/`. The proxy is the only party that still knows the public prefix, so it
reports it in `X-Forwarded-Prefix` and the gateway uses it. A value that is not a plain
absolute path (an absolute URL, a `..` segment, a backslash, or a CR/LF) is ignored and
the relative form is used instead, so a forged header cannot redirect off-origin or
inject a response header.

Streaming responses (server-sent events) require proxy buffering to be off. Add
`proxy_buffering off;` to the locations serving `/v1/chat/completions`,
`/v1/responses`, or `/v1/messages` when clients send `stream: true`.

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
  inherits that pattern as its source model when the column is empty. A channel
  that names its own model on an exact-pattern route is a candidate for that
  route regardless, which is what lets one exposed model reach several upstreams
  that each spell it differently.
- The downstream key exclusions described below.
- The circuit breaker and channels already tried by the retry loop.

The model written into the upstream request body is resolved in this order:

1. If the request named the route's `display_name` and the channel has a
   `source_model`, that source model is used.
2. Otherwise, if the channel names its own model on an exact-pattern route, that
   is the name its upstream receives.
3. Otherwise, if `model_mapping` did not rewrite the name and the route pattern
   is an exact match, the channel `source_model` is used, falling back to the
   route pattern.
4. Otherwise the mapped model is used, defaulting to the requested name.

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
- `FLUXGATE_MANAGEMENT_TOKEN`: Optional bearer token accepted by the management endpoints alongside a console session. Empty by default; console sign-in does not use it.
- `FLUXGATE_ADMIN_PASSWORD`: Initial password for the console administrator account. Empty by default, which makes the gateway create `admin` / `admin` and require a change at first sign-in. The container entrypoint passes it to `admin create` on first start. Ignored once the account exists.
- `FLUXGATE_ADMIN_USERNAME`: Username for the console administrator account. Default: `admin`.
- `FLUXGATE_MAX_BODY_BYTES`: Maximum accepted JSON request body size. Default: 8 MiB.
- `FLUXGATE_INTEGRITY_CHECK`: Run `PRAGMA quick_check` against the configuration database at startup and refuse to start when it fails. Default: `true`. Disable it only if start-up time on a very large database matters more than detecting a damaged file.

### Retry settings

- `FLUXGATE_MAX_ATTEMPTS`: Maximum total upstream attempts for one downstream request. Default: `8`.
- `FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL`: Maximum attempts against one channel. Default: `2`.
- `FLUXGATE_RETRY_BASE_BACKOFF`: Initial retry backoff duration. Default: `50ms`.
- `FLUXGATE_RETRY_MAX_BACKOFF`: Maximum retry backoff duration. Default: `1s`.
- `FLUXGATE_FAILOVER_ENABLED`: Whether a failed request may move to another channel at all. Default: `true`. With it off, the request keeps retrying the channel it started on, which is what makes one upstream's behavior observable.
- `FLUXGATE_FAILOVER_CROSS_UPSTREAM`: Whether failover may reach another upstream, or only another key of the same one. Default: `true`.

Retryable status codes are `408`, `409`, `425`, `429`, `500`, `502`, `503`, and `504`. Network failures may also be retried within the configured limits. Once a streaming response has been committed to the downstream client, the gateway does not switch channels.

### Circuit-breaker settings

- `FLUXGATE_BREAKER_MODE`: Default breaker mode. Supported values: `cooldown`, `disable`, `key_cooldown`, and `key_model_cooldown`. Default: `cooldown`.
- `FLUXGATE_BREAKER_THRESHOLD`: Consecutive failure threshold. Default: `3`.
- `FLUXGATE_BREAKER_BASE_COOLDOWN`: Initial cooldown duration. Default: `30s`.
- `FLUXGATE_BREAKER_MAX_COOLDOWN`: Maximum exponential cooldown duration. Default: `15m`.
- `FLUXGATE_BREAKER_COOLDOWN_MULTIPLIER`: Factor the cooldown of a repeatedly failing circuit grows by, until the maximum. Default: `2`.

Channel-specific breaker modes loaded from the existing configuration can override the default mode. Breaker state is persisted in the Go-owned `gateway_breaker_states` table and restored after restart. A `disable` circuit does not return to service by itself; it is cleared from the console (恢复) or through `POST /management/breakers/reset`.

### Runtime policy

The retry, failover, and circuit-breaker settings above can be changed while the
gateway runs, from the console's 设置 page. A change is stored in the configuration
database's `settings` table under a `gateway.`-prefixed key, is additive to whatever
else that shared table holds, and takes effect for the next request rather than at the
next restart. Clearing a value removes its row, which puts the environment's value
back; restoring a section's defaults does that for every value in it.

| Setting | Meaning |
| --- | --- |
| `gateway.failover.enabled` | Whether a failed request may move to another line at all. With it off, the request stays on the line it started with. |
| `gateway.failover.cross_upstream` | Whether it may move to another upstream, or only to another key of the same one. |
| `gateway.retry.max_attempts` | Total upstream attempts for one request. |
| `gateway.retry.max_attempts_per_channel` | Attempts against one line before it is given up. |
| `gateway.retry.statuses` | JSON array of status codes that are retried. |
| `gateway.retry.base_backoff_ms` | Initial wait before a retry, in milliseconds. |
| `gateway.retry.max_backoff_ms` | Upper bound of that wait, in milliseconds. |
| `gateway.breaker.mode` | `cooldown`, `disable`, `key_cooldown`, or `key_model_cooldown`. |
| `gateway.breaker.threshold` | Consecutive failures before a line is held out. |
| `gateway.breaker.base_cooldown_seconds` | First cooldown, in seconds. |
| `gateway.breaker.max_cooldown_seconds` | Upper bound of the cooldown, in seconds. |
| `gateway.breaker.cooldown_multiplier` | Growth factor applied per consecutive trip. |

A policy value is validated twice: on its own against the field's range or choice list,
and as a whole against the rules that tie two values together — a per-line attempt
budget may not exceed the total one, a cooldown ceiling may not sit below its floor. A
request that breaks one is answered with `400 invalid_configuration` naming the field to
change, and nothing is stored. The same parser reads the table at startup, so a row that
was edited by hand and cannot be read is reported once in the log as
`policy_setting_ignored` and the value it tried to set keeps its environment default.

Two settings the console does not write are worth knowing about: the console records the
routes it created in `gateway.managed_routes`, and it is the only thing that prunes
them.

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
- The gateway does not return upstream API keys or authenticated proxy URLs through its operational endpoints. The console's configuration listing masks every stored secret to its last four characters, and a client key is returned in full only in the response that created or rotated it.
- The console HTML shell is served without authentication but contains no gateway data; channel names, priorities, and breaker state are only returned by the authenticated management endpoints.
- Configuration writes are authenticated like any other management request, validated field by field against the tables they touch, and refused from a foreign origin. Because the console authenticates with a cookie, `SameSite=Lax` and the origin check are what keep a cross-site page from driving a write; a request that carries the management token in a header sends no `Origin` and is accepted, which is the automation path.
- Writes are audited by event with the resource, the row id, and the client address, and never with the values, because a request body carries credentials.
- The administrator password is stored as a bcrypt hash and never in plaintext; session tokens are stored only as SHA-256 hashes, so a leaked database cannot be replayed as a live session. Console sign-in is rate limited per client address and every attempt is logged.
- A gateway with no administrator starts with `admin` / `admin`. That credential cannot read anything until it is changed, because the data endpoints answer `403 password_change_required` to an account holding it. Someone who finds the console before you sign in can therefore still claim the account by setting its password, so on a publicly reachable deployment sign in and change it promptly, or set `FLUXGATE_ADMIN_PASSWORD` to a value only you know before the first start.
- There is no password strength requirement by design: the gateway will accept a one-character password. Strength is the operator's decision, and the rate limiter plus the audit log are the compensating controls.
- Console sessions last 12 hours and are revoked by `admin reset` and by signing out. Set `FLUXGATE_MANAGEMENT_TOKEN` only when automation needs to call the management endpoints; leaving it unset means a console session is the only way in.
- Use a reverse proxy with TLS when exposing the gateway outside a trusted network. Forward `X-Forwarded-Proto` so the session cookie is marked `Secure` behind TLS termination.
- The gateway trusts `X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Forwarded-Prefix` from its peer. Only place it behind a proxy that overwrites those headers, or the login rate limiter can be evaded by spoofing `X-Forwarded-For`. A forged `X-Forwarded-Prefix` cannot escape the origin — unsafe values are rejected — but an accepted one steers where the console redirect lands.
- Restrict filesystem access to the SQLite database because it contains sensitive account and credential configuration.
- Keep operational health endpoints separate from authenticated AI proxy endpoints when applying external access-control rules.

## Current integration boundary

The Go gateway is an independent service that can run alongside the existing React
and TypeScript application during gradual migration. It reads the same SQLite
configuration and owns two additional tables of its own: `gateway_breaker_states`
for circuit-breaker persistence and the `gateway_admin_*` tables for the console
credential and its sessions.

The console can also edit that configuration, which makes the gateway a writer of
the shared file. SQLite requires every writer of one database to share the same host
and locking primitives, so when both applications are in use, run each against its
own database copy or retire one of them as a writer; the warning in
[Prepare the database](#prepare-the-database) explains the corruption this prevents.
A write replaces only the columns the request names, so a column the other
application owns is left as it is.