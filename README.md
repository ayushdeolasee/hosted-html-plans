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

### Build

```bash
make build          # compiles ./plans for your current machine
make cross           # cross-compiles darwin/linux × arm64/amd64 into dist/
make test            # go test ./...
```

`plans` is a single static binary (`CGO_ENABLED=0`) — copying it anywhere is
the entire deployment step.

### Deploy to a box

```bash
make cross
scp dist/plans-linux-amd64 <box>:~/plans      # or -darwin-arm64, etc.
ssh <box> 'chmod +x ~/plans'
```

(`make deploy BOX=<host>` is a ready-made example target for this — edit the
`BOX` var or pass it on the command line.)

### Install as a background service

On the box (or any Mac/Linux machine you want it running on):

```bash
./plans service install     # writes + enables launchd (macOS) / systemd (Linux), starts it
./plans service status      # running? which listeners? tailnet auth state? funnel on?
./plans service uninstall   # stop + remove the service registration
./plans run                 # foreground mode — development/debugging only
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

1. `plans service install` generates `~/.config/plans/config.json` with
   defaults on first run and starts the service. `http://<box-lan-ip>:8080`
   works immediately — no further setup needed for LAN-only use.
2. If Tailscale is enabled in config (`tailscale_enabled: true`, the
   default), the log and `plans service status` print a Tailscale auth URL
   the first time the embedded tsnet node needs to join your tailnet — open
   it, approve the new `plans` node, done. tsnet stores its keys under the
   data directory and doesn't ask again. Once authenticated, the service is
   reachable at `https://plans.<tailnet>.ts.net` with automatic HTTPS.

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

```bash
curl -fsSL <install-url>/install.sh | bash
curl -fsSL <install-url>/install.sh | bash -s -- --url https://plans.<tailnet>.ts.net
```

`install.sh` is **self-contained**: the `push-plan` script is embedded inline,
so the installer is a single file to host and needs no second fetch. It:

- writes `push-plan` to `~/.local/bin` (or `/usr/local/bin` if that's writable
  and `~/.local/bin` isn't on `PATH`; override with `--bin-dir DIR`),
- prints an explicit `export PATH=...` line for the right shell rc if the
  target dir isn't on `PATH`,
- with `--url`, records `push_url` in `~/.config/plans/config.json` (merging
  into any existing config) so you don't need `PLANS_URL` in every shell,
- supports `--uninstall`.

The Claude Code skill is distributed separately, the normal way:

```bash
npx skills add push-plan
```

The skill **bundles its own copy of the executable**, so an agent that has the
skill can always fall back to `~/.claude/skills/push-plan/push-plan` even on a
machine where the CLI was never installed on `PATH`.

### Generated artifacts — don't hand-edit

`scripts/push-plan` is the single source of truth. Both `install.sh` and
`skill/push-plan/push-plan` are **generated** from it:

```bash
make installer         # regenerate both after editing scripts/push-plan
make check-installer   # fails if they're stale (wired into `make test`)
```

### Developing on this repo

`scripts/install` (`make install`) is a convenience wrapper for when you *do*
have the checkout: it regenerates the artifacts, runs the real `install.sh`,
and copies both skills into `~/.claude/skills/` so you can test edits without
publishing. `--cli-only` / `--skills-only` do one half; `URL=…` is passed
through (`make install URL=https://plans.x.ts.net`).

### Usage

```bash
push-plan <file.html> [title] [-m "one-line note"]
```

- **Server URL resolution**, in order: `$PLANS_URL` env var → a `push_url`
  key in `~/.config/plans/config.json` (if you choose to set one there) →
  `http://localhost:8080`.
- **Repo/branch auto-tagging**: reads `git remote get-url origin`
  (normalized to `host/owner/name`, no `.git`/protocol) and `git branch
  --show-current` from wherever it's run; omits both gracefully outside a
  git repo or with no remote.
- **Agent identity**: `$PUSH_PLAN_AGENT` (defaults to `"cli"`).
- Prints the returned URLs and, on macOS, copies the tailnet URL to the
  clipboard (falls back to the LAN URL if tailnet isn't configured yet).
- Clear errors on a missing file, an unreachable server, or a non-2xx
  response (prints the response body).

## The two Claude Code skills

Installed as **personal skills** at `~/.claude/skills/` (available in every
session, any repo):

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

End users install these with `npx skills add push-plan` /
`npx skills add implement-plan`. To test local edits from this checkout:

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
