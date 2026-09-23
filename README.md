# Fluxgate

Fluxgate is a lightweight, independently deployable Go gateway that reads the existing upstream SQLite configuration and provides OpenAI-compatible proxy endpoints, bounded server-side retries, priority-then-weight routing, proxy selection, and persistent circuit-breaker state. It ships with an embedded management console that both shows the running state and edits that configuration: upstreams with their priorities, weights, keys and models, the routing the gateway derives from them, client keys, proxy profiles, and the retry, failover, and circuit-breaker policy the gateway applies while it runs.

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

### Container networking

The service is pinned to Docker's default bridge network (`network_mode: bridge`), so Compose does not create a project network for it, and it must stay that way. A self-created bridge brings its own networking with it — an embedded DNS resolver plus its own forwarding and NAT rules — and if any part of that does not work on the host, the container has no working outbound path even though the host does. Name resolution either goes unanswered or the container's packets leave and are silently dropped, and the console then reports every upstream as unreachable — `无法连接该上游 ... context deadline exceeded` — while `curl` on the host answers the same address instantly. The default bridge reuses the host's existing network, so the container behaves the way the host does.

When an upstream is reported as unreachable, compare the two paths before changing any gateway setting:

```bash
curl -sS -m 20 -o /dev/null -w '%{http_code} %{time_total}\n' https://upstream.example.com/v1/models
docker exec fluxgate wget -T 20 -O /dev/null https://upstream.example.com/v1/models; echo "exit=$?"
```

An HTTP status in both cases means the address and the credential are worth checking instead; an exit code of `8` from `wget` is also a successful connection (the server answered an error), whereas a hang is the container's outbound path.

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
  --network bridge \
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

`--network bridge` is explicit on purpose: the container has to resolve upstream names the way the host does. Do not move it onto a network you created yourself — see [Container networking](#container-networking).

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
dashboard shows gateway readiness, uptime, the revision the binary was built
from, circuit-breaker state, upstream lines, routable models, and the record of
the requests it served, refreshing itself every ten seconds. It opens in the
scheme the browser prefers, light or dark, and the sun/moon button in the top bar
switches between the two at any time; that choice is remembered for the next
visit. The interface is
in Simplified Chinese; the endpoints it calls are unchanged, and the gateway's own
error codes stay stable for automation while the console renders them in Chinese.

### Managing the configuration from the console

The console is also where the configuration is edited. Its main view is 上游
(upstreams): one row per upstream service, holding everything that upstream needs
— a name, an API address, a priority, a weight, request headers, an optional
proxy, its keys, and the models it serves. Adding an upstream and picking its
models is all it takes to route them; the routes appear by themselves. Every
accepted write reloads the configuration and hands the result to the routing
engine, so an upstream added in the console serves traffic on the next request.

The views are 概览 (the live dashboard), 上游 (upstreams), 路由 (the routing table
the gateway derived), 客户端密钥 (client keys), and 设置. The settings view is
grouped by function and shows one group at a time: 账号 (the administrator
account), 运行策略 (the runtime policy), and 代理 (proxy profiles).

The 路由 view answers one question per model and shows the details behind it. A
collapsed row is a model and its headline: which upstreams serve it, how it is
matched, how many lines it has, and how those lines currently stand (可用 / 冷却 /
已熔断 / 已停用, with the counts). Expanding it lists the lines themselves — one per
upstream key — with the upstream, the masked key, the model name that upstream knows,
the priority, the weight, the state, and the remaining cooldown counted down to the
second. The priority column shows the line's own, and the upstream's when it is not
the default, because the gateway compares the upstream's first. A line the gateway is
holding out of rotation can be brought back from there (恢复), which clears every
circuit that can hold it. The view refreshes itself every ten seconds, like 概览,
because a line's state changes without an operator doing anything.

The 上游 view shows the same lines from the upstream's side. The priority and the
weight are both edited in place: type a number in the row, press Enter or 保存, and
the change is stored and applied for the next request. A 线路状态 column summarizes
how that upstream's lines currently stand, and expanding a row lists them.

The upstream form is built around the four things that actually vary between
upstreams:

- **The address is completed, not corrected later.** A trailing slash is dropped and a
  bare host is completed to its versioned API root, so `https://api.example.com`,
  `https://api.example.com/`, `https://api.example.com/v1` and
  `https://api.example.com/v1/` are stored as one address; a full endpoint pasted from a
  provider's documentation (`…/v1/chat/completions`) is trimmed back to the version root.
  A path the operator wrote is kept as written, because an upstream may be mounted under
  one (`https://example.com/openai`), and the probe tries both the versioned and the bare
  listing path for whichever address it is given.
- **Keys are pasted in one go.** One key per line, as many as needed. The stored
  keys are shown masked and are matched by position: an untouched line keeps the
  key it stands for, a changed line replaces it, an added line is stored, and a
  removed line retires that key.
- **Key selection has two modes.** 可用优先 (available first) prefers the first key
  and moves to the next one only when it fails or is cooling down; 加权随机
  (weighted random) spreads requests across every key in proportion to its weight.
- **Models are picked, not typed.** 获取模型 asks the upstream itself for its model
  list, so the operator selects from what it really serves; a model can also be
  typed in by hand. A listing entry names one model: the name is read from the
  entry's identifier (`id`, or `model`/`name` when that is all an entry carries), and
  a display label a platform writes beside the identifier is not offered as a second
  model — it is what that platform's own console shows, not a name it would accept.
  A picked name is filed under the model's canonical name — the
  part after any `channel/` path, without a known `:variant` suffix, lowercased — so
  `cline-free/deepseek-v4.1-flash:free` and `DeepSeek-V4.1-Flash` are one model
  exposed once, and the spelling the upstream listed is carried as the name to send
  it. Each selected model takes an optional upstream model name, prefilled with that
  spelling and editable, so it can be pointed somewhere else. The
  probe goes out the way a real request to that upstream would: through the proxy
  in the form, or the default proxy profile when the form leaves it open, and the
  answer names the address that served the listing. A probe that fails says which
  address it tried, which way the request left the gateway, and what the error was,
  so a wrong address and a missing proxy can be told apart.
- **Routing is derived.** Selecting a model creates its route and one line per key;
  deselecting it removes them once no other upstream serves that model. A route the
  operator wrote by hand through the API is never pruned by a console edit.

An upstream added from the console is created enabled, so its lines serve traffic
as soon as it is saved. Client keys are created enabled too, and their restrictions —
排除模型 (denied models), 限定路由 (allowed routes), and 排除上游 (excluded upstreams) —
are picked from the routes, upstreams, and models the gateway already holds rather than
typed as JSON; a value stored by hand that is no longer in those lists stays selected,
so editing a key cannot drop a restriction by accident.

Each editable table has an 添加 button, and every row has 编辑 and 删除 actions;
client keys additionally have 轮换, which mints a new value and invalidates the old
one immediately. A line that is cooling down, or disabled by a `disable` circuit, is
brought back from 概览 (where every recorded circuit is listed and can be cleared one
by one or all at once with 全部恢复) or from the line itself on the 路由 and 上游 views
(恢复), so it returns to service before its cooldown would have expired.

A few rules the console enforces, all of them on the server rather than in the
browser:

- **Secrets are write-only.** An upstream key or client key is stored as written
  but returned as a mask showing its last four characters. The form for an existing
  row starts with the mask and leaves the stored value alone unless a new value is
  typed, so a mask can never be saved back over a credential. A client key is shown
  in full exactly once, when it is created or rotated.
- **Credential fields refuse to be filled in.** A credential input stays read-only
  until it is focused and anything that appears in it without a keystroke is cleared,
  because a browser or password manager would otherwise fill it with the console's own
  saved login and that value would be stored as a key.
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

Change it from 设置 → 账号 in the top bar. The form requires the current password,
so a stolen session cookie alone cannot lock the operator out, and it disables
autofill so a password manager cannot silently rotate the stored credential.
Sessions survive the change, so you stay signed in.

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
- `POST /management/configuration/keys/{id}/reveal`
- `PUT /management/policy`
- `POST /management/upstreams/models`
- `POST /management/breakers/reset`
- `GET /management/requests`
- `DELETE /management/requests`
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

A client key stays readable after it was created: `POST
/management/configuration/keys/{id}/reveal` answers with the stored value, which is how
the console's 显示 and 复制 actions work for the operator who lost one. The listing is
never unmasked — a listing is fetched on every refresh and kept in the browser, while
this answers one request for one key and is recorded in the audit log as
`console_key_revealed` with the row id and the client address, never with the value. The
gate is the one writes use: a console credential, a configured write store, and the
gateway's own origin. Upstream keys cannot be read this way; those belong to the
upstream, and the gateway is the only party that needs them.

`GET /management/snapshot` reports the running state: the routable models, the recorded
circuits, and one entry per line with its effective `enabled` flag alongside a `state`
block saying what the gateway would do with it right now — `ready`, `cooling` (with the
`blocked_until` deadline and the `cooldown_level` it reached), `disabled` (a `disable`
circuit, which only an operator clears), or `inactive` (the configuration itself holds
the line out of rotation). A circuit is filed by the credential the line presents, so
the state is resolved per line by the gateway and the `scope` it names is where the
circuit lives; the credential itself is never part of a line's entry.

`upstreams` is the composite resource the console's upstream form writes, and the only
one whose fields are assembled from several tables. Alongside the columns of `sites` it
carries five synthetic fields: `keys` (a JSON array of strings, one per key, matched by
position), `key_mode` (`available_first` or `round_robin`), `models` (the exposed model
names the upstream serves), `model_mapping` (a JSON object of exposed name to the
name that upstream knows), and `priority` (the whole number selection compares first).
Writing `keys` or `models` creates the account, the token
rows, the routes, and the channels behind them; the route ids the console created are
recorded in the settings table under `gateway.managed_routes`, so an edit prunes only
what it owns. A selected model is exposed under its canonical name — the part after any
`channel/` path, without a known `:variant` suffix, lowercased — while the spelling it was
picked by becomes the name that upstream receives, so two upstreams that call one model
different things meet on one route. `priority` is stored in the settings table under
`gateway.upstream_priorities` as a JSON object of upstream id to whole number, because
it is not a `sites` column; an upstream the object does not mention has priority `0`.
The tables themselves remain addressable one by one for a detail the form
does not show, such as a forced endpoint or a per-key proxy.

`PUT /management/policy` stores runtime policy overrides, and `POST
/management/breakers/reset` clears recorded circuits: `scope: "all"` clears every one,
while a `channel` scope naming a `channel_id` clears every circuit that can hold that
line out of rotation — the line's own, the circuit shared by every line presenting the
same credential, and that credential's per-model circuits — because bringing one line
back is what an operator means and which circuit filed the failure is the gateway's to
know. The `key` and `key_model` scopes stay available for automation that read a scope
from the snapshot. `POST /management/upstreams/models` asks an upstream for its model
list: the body carries the `url`, one `key`, the `proxy_url`, and any `headers` from the
form before it is saved, or an `id` and a masked key so the gateway probes with the key
it already stores. An empty `proxy_url` probes through the default proxy profile, which
is how the gateway would reach that upstream anyway. The key is used for that one
outbound request and is never part of a response. The answer names the `endpoint` that
served the listing; a failure answers with a stable `reason` and its `params`, including
the `endpoint` that was tried, the `proxy_source` (`direct`, `system`, or `default` with
`proxy_url`) the request left through, and the underlying error.

The write endpoints refuse a request that changes nothing valid: `400` with
`invalid_configuration` for a bad value (the body names the `field` and a stable
`reason`), `404 unknown_resource` or `configuration_not_found`, `409
configuration_referenced` when a delete would orphan other rows, `409
configuration_conflict` for a duplicate value such as a client key, and `403
cross_origin_rejected` for a state-changing request from another origin.

### Request records

Every proxied request leaves a record. A client only ever sees a status code and a
gateway error code; the upstream's own message — `insufficient balance`, `invalid api
key`, `model not found` — is what names the cause of a failure, and the record is where
it is kept. The console shows it under 请求记录, and `GET /management/requests` returns
it as JSON.

`GET /management/requests` accepts:

| Query | Meaning |
| --- | --- |
| `limit` | How many records to return, newest first. Default `200`, at most `2000`. |
| `since` | Return only records newer than this row id, which is how a caller polls. |
| `failed=1` | Only the requests that were not served. |
| `model` | Only the requests for one model, compared without case. |

Each record carries the request id, the time, the path and client address, the client key
by name (never by secret), the requested model, whether it streamed, the final status,
the gateway error code and message, the duration, and one entry per upstream attempt:
the line by name, the model that was sent, the upstream's status, the duration, whether
the attempt was retried, and as much of the upstream's own response body as explains the
failure (512 bytes, whitespace collapsed, with the credential the attempt presented
redacted if the upstream echoed it). No upstream credential appears anywhere in a
record: the line an attempt ran on identifies the key, and the gateway is the only party
that ever holds the secret.

The answer also carries `retention` — whether records are being kept and how many — so a
view can say how far back the log reaches rather than implying it holds everything ever
served. `DELETE /management/requests` empties the log.

A proxied response carries `X-Fluxgate-Request-Id`, and a failed one carries the same id
inside its error body:

```json
{
  "error": {
    "code": "upstream_unavailable",
    "message": "upstream request failed after attempt 2 with status 429: {\"error\":{\"message\":\"insufficient balance\"}}",
    "type": "gateway_error",
    "request_id": "9f2c1a7b4e8d0356",
    "attempts": 2,
    "upstream_status": 429,
    "upstream_message": "{\"error\":{\"message\":\"insufficient balance\"}}"
  }
}
```

The upstream's answer travels to the client as well as to the log, because an opaque
`502 upstream_unavailable` costs an operator a trip through the gateway's console for
something the upstream had already spelled out. Quote the `request_id` to find the full
record, including the lines that were tried before the one that failed. A client of the
gateway is an authenticated caller of the operator's own deployment, which is why its own
upstream's error text is shared with it; the credential that attempt presented is redacted
from that text.

The record is on by default and keeps the newest 1000 requests; see
`FLUXGATE_REQUEST_LOG_ENABLED` and `FLUXGATE_REQUEST_LOG_KEEP`, or the 请求记录 section
of 设置 → 运行策略 in the console.

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
`/gateway/console/styles.css`, `/gateway/console/theme.js`, `/gateway/console/app.js`,
and `/gateway/management/*`.
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

### Upstream addresses

A channel's base address is the upstream's API root — the value an OpenAI client would be
configured with, such as `https://api.example.com/v1`, or
`https://example.com/openai/v1` for an upstream mounted under a sub-path. The gateway's
own routes already carry the version segment, so it is not repeated: a request for
`/v1/chat/completions` reaches `<base>/chat/completions`. A base address with a path is
honored in full, so an upstream mounted under `/openai` is called under `/openai` and not
at its host root; a bare host keeps the request path as it arrived, because the upstream
schema stores a site as a bare host and the version segment is then the only thing naming
the API.

`sites.forced_upstream_endpoint` overrides that address for upstream requests when it is
set, which is how a deployment whose stored address is not the one requests have to go to
says so. It is not on the console's upstream form; it is written through
`PUT /management/configuration/sites/{id}`. The console's model probe asks the address in
the form, not the forced endpoint, so a probe before saving answers for what was typed.

### Model names

One model is routinely spelled several ways, so a request is matched by model
identity rather than as a string. Case, the channel path before the last `/`, a
known variant suffix after the last `:` (`:free`, `:nitro`, `:thinking`, `:online`,
`:extended`, `:floor`, `:beta`, `:self-moderated`), and a trailing `-free` are naming
rather than a
different model, so `DeepSeek-V4.1-Flash`, `cline-free/deepseek-v4.1-flash`, and
`cline-free/deepseek-v4.1-flash:free` all reach the route that exposes
`deepseek-v4.1-flash`, and that name is what the gateway exposes and lists. Route
patterns, group display names, `source_model` checks, `model_mapping` keys, and a
downstream key's deny list are all compared this way, so a restriction written for
the plain name covers every decorated spelling of it. A `re:` pattern is the
exception: it is matched against the name as sent, because it is written against
what the client spells out.

Only those variant tags are read as naming, because a colon also writes the channel
group a platform serves a model from — `cn:deepseek-v4.1-flash` — and there the name
after the colon is the model itself. Such a name is kept whole: it keeps its own
identity instead of collapsing into its channel group, and every model that group
serves stays a model of its own. A suffix the gateway does not know is kept for the
same reason, so a name is never reduced to something no upstream ever wrote. To
expose a friendlier name for one, map it (`model_mapping`) or give the route a
display name.

The upstream still receives the spelling it knows: a channel's `source_model` (or
the name the console stored when the model was picked) is written into the request
body, so one exposed model can reach several upstreams that each call it something
different.

Candidate channels are then filtered by:

- `route_channels.source_model`: when set, the channel only serves models that
  equal it, are the same model under another spelling (case, channel path, known
  variant suffix, `-free`), or match it as a pattern. A channel on an exact-pattern route
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

### Channel selection

Selection is by priority first and by weight second, which is what lets an
operator say "use this upstream first" and "spread what is left":

1. The highest **upstream priority** with a usable line wins. Priority travels
   with the upstream (`priority` on the `upstreams` resource), not with a line,
   so every line of a preferred upstream is tried before any line of a
   lower-priority one.
2. Inside that, the highest **line priority** (`route_channels.priority`) wins.
   This is the key mode: 可用优先 gives an upstream's keys descending priorities,
   which orders them within their upstream rather than across upstreams.
3. Among the lines that tie on both, one is drawn **at random**, each line's
   chance proportional to its effective weight: `route_channels.weight ×
   sites.global_weight × site_weight_multipliers[site]`, with a line weight of
   `0` treated as `10` and a site weight of `0` as `1`. A priority tier is
   therefore not a share of traffic — a lower tier is reached only when every
   line above it is unusable — while weight divides the traffic of one tier.

A line that is disabled, blocked by a circuit, excluded by the downstream key, or
already tried by the retry loop is filtered out before the tiers are computed, so
a request falls to the next tier instead of failing while a preferred line cools
down. An upstream that has never been given a priority has `0`, which is what
every upstream had before priorities existed.

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
  multiplier of `1` when a site has no entry, and it decides the draw among lines
  of one priority tier.

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

The threshold counts **failed requests**, not failed attempts: a request that is retried
eight times against one line counts once against that line's circuit. The retry budget is
how hard the gateway tries to serve one request; the threshold is how much evidence an
operator wants before a line is taken out of rotation, and a single request that never
succeeded is one piece of evidence. Without that rule one `429` would walk a threshold of
three up three cooldown levels and hold the line for minutes.

Channel-specific breaker modes loaded from the existing configuration can override the default mode. Breaker state is persisted in the Go-owned `gateway_breaker_states` table and restored after restart. A `disable` circuit does not return to service by itself; it is cleared from the console (恢复) or through `POST /management/breakers/reset`.

### Request-log settings

- `FLUXGATE_REQUEST_LOG_ENABLED`: Whether the gateway records the requests it serves. Default: `true`. With it off, nothing new is recorded; existing records stay readable and can be cleared from the console.
- `FLUXGATE_REQUEST_LOG_KEEP`: How many records the log holds. Default: `1000`. The newest records are kept and the rest are dropped as new ones are written, so a long-running gateway cannot grow the shared database without limit.

The log is a gateway-owned table (`gateway_request_log`), created at startup beside
`gateway_breaker_states`. It is written once per proxied request, after the response, and
a write that fails is reported in the process log as `request_log_write_failed` — it never
changes what the client was told.

### Runtime policy

The retry, failover, and circuit-breaker settings above can be changed while the
gateway runs, from 设置 → 运行策略 in the console. A change is stored in the configuration
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
| `gateway.request_log.enabled` | Whether the gateway records the requests it serves. |
| `gateway.request_log.keep` | How many records the log holds, newest kept. |

A policy value is validated twice: on its own against the field's range or choice list,
and as a whole against the rules that tie two values together — a per-line attempt
budget may not exceed the total one, a cooldown ceiling may not sit below its floor. A
request that breaks one is answered with `400 invalid_configuration` naming the field to
change, and nothing is stored. The same parser reads the table at startup, so a row that
was edited by hand and cannot be read is reported once in the log as
`policy_setting_ignored` and the value it tried to set keeps its environment default.

Two settings the console writes are worth knowing about: it records the routes it
created in `gateway.managed_routes`, and the priority of each upstream in
`gateway.upstream_priorities`. Both are the console's bookkeeping rather than gateway
configuration, and the console is the only thing that prunes or clears them.
`gateway.upstream_priorities` is a JSON object of upstream id to whole number; an
upstream it does not mention has priority `0`, and setting an upstream back to `0`
removes its entry rather than storing one.

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
- The gateway does not return upstream API keys or authenticated proxy URLs through its operational endpoints. The console's configuration listing masks every stored secret to its last four characters, and a client key is returned in full only in the response that created or rotated it, or in the response to a deliberate `…/keys/{id}/reveal` read, which is audited by row and never by value. A recorded circuit names the lines it holds out of rotation rather than the credential it is filed under, so the snapshot never carries an upstream key either.
- A request record names the client key by its row and the upstream by the line an attempt ran on; it holds no credential. It does hold the beginning of an upstream's error response, which is the point of the log, so treat `GET /management/requests` (and the database file) with the same care as the rest of the configuration.
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
configuration and owns three additional tables of its own: `gateway_breaker_states`
for circuit-breaker persistence, `gateway_request_log` for the record of served
requests, and the `gateway_admin_*` tables for the console
credential and its sessions.

The console can also edit that configuration, which makes the gateway a writer of
the shared file. SQLite requires every writer of one database to share the same host
and locking primitives, so when both applications are in use, run each against its
own database copy or retire one of them as a writer; the warning in
[Prepare the database](#prepare-the-database) explains the corruption this prevents.
A write replaces only the columns the request names, so a column the other
application owns is left as it is.