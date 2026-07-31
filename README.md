# hosted-html-plans

A single native binary, `plans`, that gives you a personal HTML "plans"
service: agents push HTML deliverables (plans, roadmaps, reports) to it over
a simple open API, you read them from any of your devices over LAN or your
Tailscale tailnet, and a built-in homepage lets you list, search, rename,
delete, and (on-demand only) publicly share individual plans. No Docker, no
database, no auth tokens — reachability on a trusted network (LAN/tailnet)
*is* the auth boundary, and every write is an immutable new version so
nothing is ever silently lost. Full design rationale lives in `plan.html`
(architecture, access modes, service management) and `agent-loop.html`
(versioning, concurrent writes, the agent-integration skills) — open either
in a browser to read them properly.

## Install

There are two halves — the **server** (the box that stores and serves plans)
and the **client** (any machine where you or an agent authors them). One
script opens a checkbox picker where either or both can be selected:

```bash
U=https://raw.githubusercontent.com/ayushdeolasee/hosted-html-plans/main/install.sh

curl -fsSL $U | bash                       # interactive Server/Client picker
curl -fsSL $U | bash -s -- --server    # on the box: binary + background service
curl -fsSL $U | bash -s -- --client    # scripted client install
```

Neither needs a checkout. Explicit flags skip the first picker; passing both
flags installs both sides.

### `--server`

Detects the OS and architecture, downloads the matching binary from the
latest GitHub release, installs it (to `/usr/local/bin` when possible), and
registers it as a service — launchd on macOS, systemd on Linux. If no release
asset matches the platform and Go is present, it builds from source instead.
It also makes `plans` available on `PATH`, introduces the CLI, and guides
first-time Tailscale authentication. Once approved, it prints the complete
client-install command with the real tailnet URL already filled in. The
Tailscale step can be skipped without affecting the LAN service.

| Flag | Effect |
|---|---|
| `--binary PATH` | Install this local binary instead of fetching one. |
| `--version TAG` | Pin a release tag (default: latest). |
| `--bin-dir DIR` | Where the binary lands. |
| `--user` | Linux: user-level systemd unit, no root needed. |
| `--print` | Dry run — show the unit and the commands, change nothing. |
| `--uninstall` | Stop the service, deregister it, remove the binary. |

A system-level systemd unit needs root; the installer uses `sudo` when it
has to and tells you if it can't.

### `--client`

Installs the `push-plan` command (to `~/.local/bin`, or `/usr/local/bin` if
that's writable and `~/.local/bin` isn't on `PATH`), then launches
`npx skills add` for the two agent skills. The skills CLI detects supported
agents and lets you independently choose Claude Code, Codex, Cursor, or any
other supported destination. The skills are installed globally so they are
available across projects.

| Flag | Effect |
|---|---|
| `--url URL` | Set the preferred server URL without prompting. |
| `--lan-url URL` / `--tailscale-url URL` | Set either address without prompting. |
| `--bin-dir DIR` | Override the `push-plan` command destination. |
| `--agent NAME` | Skip the agent picker and target one agent; repeat for multiple agents. |
| `--all-agents` | Install the skills for every supported agent without prompting. |
| `--cli-only` / `--skills-only` | Install one half. |
| `--uninstall` | Remove the command and the skills. |

For example, a Codex-only scripted skill install is:

```bash
curl -fsSL $U | bash -s -- --client --skills-only --agent codex
```

Node.js/npm is required for skill installation; `--cli-only` does not require
it.

During an interactive client install, the installer asks for two optional
addresses:

- A LAN IP or cloud-accessible URL. A raw IP or hostname becomes
  `http://<value>:8080`.
- A Tailscale hostname or URL. A raw hostname becomes `https://<value>`.

Both are retained in `~/.config/plans/config.json` as `lan_url` and
`tailscale_url`. The legacy `push_url` is also populated for compatibility;
at runtime the CLI tries Tailscale first and LAN/cloud second.

When a Tailscale address is configured, the client installer checks whether
this device accepts Tailscale DNS. If it is disabled, the installer offers to
run `tailscale set --accept-dns=true` and verifies the result. Declining leaves
network settings untouched and prints the manual macOS path: open **Tailscale
> Settings** and enable **Use Tailscale DNS settings**. Without that setting,
MagicDNS names such as `plans.<tailnet>.ts.net` may not resolve, but a configured
LAN/cloud address remains available as the fallback.

### Building it yourself

```bash
make build           # compiles ./plans for your current machine
make cross           # cross-compiles darwin/linux × arm64/amd64 into dist/
make test            # go test ./... (+ the generated-artifact staleness guard)
make release TAG=v0.1.0   # cross-compile and publish a GitHub release
```

`plans` is a single static binary (`CGO_ENABLED=0`) — copying it anywhere is
the entire deployment step. `make release` is what keeps
`install.sh --server` working: it uploads all four binaries under the exact
names the installer looks for (`plans-<os>-<arch>`).

To install from a checkout instead of a release — the flow when you're
editing this repo — use the dev wrapper, which regenerates `install.sh` first
so you're testing exactly what ships:

```bash
make install                 # client half from this checkout
make install-server          # server half, using ./plans (skips the fetch)
make install-server ARGS=--print
```

### Managing the service directly

```bash
plans service status      # running? which listeners? tailnet auth state? funnel on?
plans service install     # what `install.sh --server` calls for you
plans service uninstall   # stop + remove the service registration
plans run                 # foreground mode — development/debugging only
```

- **macOS** — installs a launchd `LaunchAgent` at
  `~/Library/LaunchAgents/com.ayushdeolasee.plans.plist`. `RunAtLoad` +
  `KeepAlive` mean it starts at login and restarts on crash. Logs go to
  `~/Library/Logs/plans.log` (rotated on startup if it grows past ~10MB).
- **Linux** — installs a systemd **system** unit at
  `/etc/systemd/system/plans.service` by default (so it runs on a headless
  box with nobody logged in) — this needs root/sudo. Pass `-user` to install
  a user-level unit at `~/.config/systemd/user/plans.service` instead
  (`systemctl --user`), no root required. Logs go to the journal
  (`journalctl -u plans`).
- Pass `-print` (or set `PLANS_SERVICE_DRYRUN=1`) to any `service` subcommand
  to see exactly what would be written/run without touching the system.

### First-run flow

1. The server installer generates `~/.config/plans/config.json`, starts the
   service, and makes the `plans` CLI available on `PATH`.
   `http://<box-lan-ip>:8080` works immediately for LAN-only use.
2. If Tailscale is enabled (the default), the installer waits for and displays
   its one-time authentication URL. Open it and approve the new `plans` node,
   then return to the installer. Type `s` instead to skip this step.
3. Once authenticated, the installer reads the real tailnet FQDN and prints a
   complete laptop command such as
   `curl -fsSL …/install.sh | bash -s -- --client --tailscale-url https://plans.example.ts.net`.
   tsnet keeps its keys in the data directory and does not ask again.

## One-time tailnet ACL checklist (public sharing)

Public share links (`https://plans.<tailnet>.ts.net:8443/share/<token>`) use
Tailscale **Funnel**, which is off by default and only ever opens while at
least one plan is actively shared (see `plan.html` §3–4). Funnel has to be
permitted once, per node, in your tailnet's ACL policy before the first
share will work:

1. Open the [Tailscale admin console](https://login.tailscale.com/admin/acls).
2. Add (or confirm) a `"funnel"` node attribute for the `plans` node, e.g.:
   ```json
   "nodeAttrs": [
     { "target": ["plans.<tailnet>.ts.net"], "attr": ["funnel"] }
   ]
   ```
3. Save the policy.

Permitting funnel here **exposes nothing by itself** — nothing on the box is
reachable from the public internet until the app actually opens the funnel
listener, which only happens when you click **Share** on a plan from the
homepage. Revoking the last share closes the listener again.

## Config reference

`~/.config/plans/config.json` (same path on macOS and Linux; override with
`$PLANS_CONFIG`), generated with defaults on first run:

| Field | Type | Default | Meaning |
|---|---|---|---|
| `lan_listen` | string | `"0.0.0.0:8080"` | Address for the plain-TCP LAN listener. Use `"127.0.0.1:8080"` to restrict to localhost, or `"off"` to disable the LAN listener entirely (tailnet-only). |
| `tailscale_enabled` | bool | `true` | Whether the embedded tsnet node joins your tailnet at all. |
| `hostname` | string | `"plans"` | The tailnet node name — shapes both the tailnet URL (`https://<hostname>.<tailnet_domain>`) and share URLs. |
| `tailnet_domain` | string | `""` (empty until tsnet authenticates) | Your tailnet's `.ts.net` domain, filled in automatically once tsnet is authenticated. Absence here is why `urls.tailnet` in API responses is empty before first tailnet auth. |

You don't have to hand-edit this file: `lan_listen`, `tailscale_enabled`, and
`hostname` can all be managed from the web UI at `/#settings` — including
first-run Tailscale setup, since the page shows the auth URL as a clickable
"Authenticate this device" link the moment tsnet needs approval, and polls
its own connection state until it's live. Toggling Tailscale applies
immediately; changing the hostname or listen address is flagged as
requiring a service restart to take effect.

Data (plan files + `index.json` + tsnet state) lives at
`~/Library/Application Support/plans/` on macOS or `~/.local/share/plans/`
on Linux (respects `$XDG_DATA_HOME`); override either OS default with
`$PLANS_DATA_DIR`.

## API quick reference

No auth token — every endpoint below is open on the LAN/tailnet listeners.
The public funnel listener, when open, serves exactly one read-only route:
`GET /share/{token}`.

| Endpoint | What it does |
|---|---|
| `POST /api/plans` | Create (or new version if the slug exists). Body = raw HTML. Params: `?slug=&title=&agent=&repo=&branch=&note=`. |
| `PUT /api/plans/{slug}` | Full revision. `?base_version=N` for optimistic locking — mismatch returns `409` with `{"current": N}`. Omitting it is accepted but flagged `forced` in history. |
| `POST /api/plans/{slug}/append` | Conflict-free append of an HTML fragment (implementation notes, deviation logs). Never 409s. |
| `POST /api/plans/{slug}/status` | Conflict-free meta merge — phase status ticks etc. Body = partial JSON, e.g. `{"phases":[{"id":2,"status":"done"}]}`. |
| `GET /p/{slug}` | Latest version. `?version=N` serves history (with a banner). `?format=text` strips styling for cheap agent reads. Combinable. |
| `GET /api/plans` | List + filter: `?repo=&branch=&status=&q=`. |
| `GET /api/plans/{slug}` | One plan's metadata: latest version, parsed meta, tags, share state, history. |
| `GET /api/plans/{slug}/history` | Version list: number, timestamp, author, action, note. |
| `POST /api/plans/{slug}/restore?version=N` | Copy vN forward as the new latest version (restores are versions too). |
| `PATCH /api/plans/{slug}` | Rename title/slug (old slug 301-redirects). |
| `DELETE /api/plans/{slug}` | Archive to trash (recoverable). |
| `GET /api/trash` · `POST /api/trash/{slug}/restore` · `DELETE /api/trash/{slug}` | Trash view, restore, permanent purge (human-only by convention). |
| `POST /api/plans/{slug}/share` · `DELETE .../share` | Open a public share token (opens the funnel listener if needed) / revoke it (closes the listener if it was the last share). |
| `GET /healthz` | Liveness check; `plans service status` uses it. |

Every write response includes ready-to-open `urls.lan` / `urls.tailnet`.

## `push-plan`

`push-plan` is the one-command way to get an HTML file onto the server and
back a URL. It's a **global command**, installed once and run from whatever
repo you (or an agent) happen to be in — nobody clones this repo to use it.

### Install (no checkout needed)

It arrives with the client half of the installer, alongside both skills:

```bash
U=https://raw.githubusercontent.com/ayushdeolasee/hosted-html-plans/main/install.sh
curl -fsSL $U | bash -s -- --client
curl -fsSL $U | bash -s -- --client --url https://plans.<tailnet>.ts.net
```

The `push-plan` command is embedded in the installer. The two skills are
discovered from this repository by `npx skills`, which owns agent detection,
global placement, updates, and multi-agent selection. The push-plan skill also
bundles its own copy of the executable, so an agent can use it even where the
standalone command did not make it onto `PATH`.

### Generated artifacts — don't hand-edit

`install.sh` and `skill/push-plan/push-plan` are **generated** from
`scripts/push-plan` and `scripts/build-installer`. The `SKILL.md` files are
read directly from the repository by `npx skills`:

```bash
make installer         # regenerate both
make check-installer   # fails if they're stale (wired into `make test`)
```

`scripts/build-installer` refuses to emit an installer whose embedded
`push-plan` payload would collide with its heredoc delimiter, and
syntax-checks the result with `bash -n` before writing it.

### Developing on this repo

`scripts/install` (`make install`) is the wrapper for when you *do* have the
checkout: it regenerates `install.sh` and then runs it, so what you install is
exactly what ships — including uncommitted edits. `--cli-only` /
`--skills-only` do one half; `URL=…` is passed through
(`make install URL=https://plans.x.ts.net`). `make install-server` does the
same for the server half using your locally-built `./plans`.

### Usage

```bash
push-plan <file.html> [title] [-m "one-line note"]
```

- **Server URL resolution**: `$PLANS_URL` is an explicit override. Otherwise
  the CLI tries `tailscale_url`, then `lan_url`, then the legacy `push_url`
  from `~/.config/plans/config.json`. It uses localhost only when no address
  is configured.
- **Safe fallback**: push, pull, and GC advance to the next URL only when curl
  cannot connect. Any HTTP response is authoritative, so writes rejected with
  409/4xx/5xx are never replayed against a second server. If every address is
  unreachable, the error lists every attempted endpoint and its curl failure.
- **Repo/branch auto-tagging**: reads `git remote get-url origin`
  (normalized to `host/owner/name`, no `.git`/protocol) and `git branch
  --show-current` from wherever it's run; omits both gracefully outside a
  git repo or with no remote.
- **Agent identity**: `$PUSH_PLAN_AGENT` (defaults to `"cli"`).
- Prints the returned URLs and, on macOS, copies the tailnet URL to the
  clipboard (falls back to the LAN URL if tailnet isn't configured yet).
- Clear errors on a missing file, an unreachable server, or a non-2xx
  response (prints the response body).

## The two agent skills

Installed globally for whichever agents the user selects in the `npx skills`
picker:

- **`push-plan`** (`skill/push-plan/SKILL.md`) — for the *authoring* side.
  Teaches the model to embed the `plan-meta` JSON block in HTML
  deliverables, auto-fill `repo`/`branch` from git, check
  `GET /api/plans?repo=&branch=` first and revise an existing plan instead
  of creating a near-duplicate, push via the globally-installed `push-plan`
  command (falling back to the skill-local copy, then raw curl),
  always pass `?note=`, and hand back the tailnet URL. States explicitly
  that agents never use the share button — that's human-only.
- **`implement-plan`** (`skill/implement-plan/SKILL.md`) — for the
  *implementing* side. The 5-step protocol: fetch `?format=text` and note
  the base version; genuinely review the plan against repo reality (a gate,
  not a summary); verify `meta.repo`/`meta.branch` against the actual git
  remote/branch and stop on mismatch; implement phase by phase, checking
  each `accept` criterion; write back continuously via `status` ticks +
  `append` notes after each phase, reserving `PUT` + `base_version` +
  409-retry for structural revisions. Includes a soft gate on
  `status: draft` plans (warn + confirm) and repo/branch discovery for
  "implement the plan for this branch" with no URL pasted. Optional
  `--archive` behavior commits a final snapshot to `docs/plans/` in the
  target repo.

End users can also install them directly with:

```bash
npx skills add ayushdeolasee/hosted-html-plans --global
```

To test local edits from this checkout:

```bash
scripts/install --skills-only    # or `make install` to refresh the CLI too
```

## CLAUDE.md snippet

This service is **not** auto-wired into your global `~/.claude/CLAUDE.md` —
paste the following into your existing "Deliverables: HTML, not Markdown"
section yourself once the server is deployed and reachable, so every future
session knows to push instead of just leaving a local file:

```markdown
### Push HTML deliverables to plans
After writing a standalone HTML deliverable (plan, roadmap, report,
analysis), push it with `push-plan <file.html> [title]` — it's installed
globally, so run it from whatever repo you're in — and give me the returned
tailnet URL alongside, or instead of, the local file path.
Never use the plan service's share button or funnel — sharing is my
call to make from the homepage, not yours.
```
