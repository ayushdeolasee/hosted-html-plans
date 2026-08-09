---
name: html-plans
description: Retrieve, author, publish, and revise private HTML plans on a hosted-html-plans server. Use when the user provides a hosted plan URL, asks for the plan associated with the current repository or branch, wants an existing hosted plan downloaded or revised, or wants an HTML plan or other HTML deliverable published to the plans server.
---

# HTML plans

This skill covers the transport layer and document contract for the private
hosted-html-plans service: finding plans, retrieving them, authoring their
HTML, publishing them, and revising them. It does not define an implementation
workflow. Once a plan is local, follow the user's request normally; an
implementation workflow may be provided by a separate skill.

Keep plan HTML in the private draft cache, never in the user's repository,
even when the request says to save a plan "here." The draft path is a working
copy, not the deliverable to return to the user.

Never make a plan public. Do not call `POST /api/plans/<slug>/share` and do not
offer a public link.

## Resolve the command and server

Use the `push-plan` CLI. 

The command resolves the server itself, in this order:

1. the server named in a full URL passed to `pull`
2. `PLANS_URL`
3. `tailscale_url` in `~/.config/plans/config.json`
4. `lan_url` in that file
5. the legacy `push_url`
6. `http://localhost:8080` when no server is configured

Connection failures advance to the next configured address. HTTP responses
do not: handle the response from that server instead of replaying the request
elsewhere. Do not guess unconfigured hosts, and do not set `PLANS_URL` unless
the user asked for a specific server that is not already in a URL they gave.

For direct HTTP examples below, `<server>` means the URL selected by this same
order. If the user supplied a full plan URL, use its origin and do not replace
it with a configured default.

## Author a plan

When authoring or revising a plan, write a complete HTML document to the path
printed by `push-plan draft`. Put exactly one machine-readable metadata block
inside `<body>`:

```html
<script type="application/json" id="plan-meta">
{
  "kind": "implementation-plan",
  "version": 1,
  "status": "draft",
  "repo": "github.com/owner/name",
  "branch": "main",
  "workdir_hint": "/absolute/path/to/repository",
  "phases": [
    {
      "id": 1,
      "title": "Core server",
      "status": "pending",
      "accept": "The server starts and a stored plan round-trips through the API"
    },
    {
      "id": 2,
      "title": "User interface",
      "status": "pending",
      "accept": "The required views and interactions work in the browser"
    }
  ],
  "constraints": [
    "No Docker",
    "Keep the service private"
  ]
}
</script>
```

Use this contract honestly:

- `kind` is `implementation-plan` for work to be implemented. Use `report`
  for a non-implementation HTML deliverable; reports may omit `phases` or use
  an empty array.
- `status` is `draft` unless the user explicitly approved the plan in the
  current conversation. Use `approved`, `in-progress`, or `done` only when
  that state is true for an existing plan.
- Give every phase a stable `id`, a clear `title`, a current status, and a
  concrete, checkable `accept` criterion. IDs are how status updates find the
  phase, so do not reuse them after splitting or reordering work.
- Put non-negotiable requirements in `constraints`, not only in prose.
- Fill `repo` and `branch` from the current Git context when available, using
  the command's normalized `host/owner/name` repository form. Omit them when
  there is no repository or remote. Set `workdir_hint` to the repository root
  when one exists.

The server parses this block into plan metadata, renders phase progress from
it, and merges later phase status updates by `id`. The block describes the
plan; it does not replace the implementation workflow.

## Retrieve a plan

Pass whatever the user gave — a full URL or a bare slug. Do not split a URL by
hand; the command parses it and pins the server it names, so a configured
default pointing at a different box cannot win.

```bash
push-plan pull https://plans.<tailnet>.ts.net/p/<slug>
push-plan pull <slug>                       # server from config
push-plan pull <slug|url> --version <n>     # requested version
```

A `?version=` already in the URL is honoured; an explicit `--version` wins
over it.

The command fetches the byte-exact stored HTML using `?format=raw`, writes it
to its canonical path in the private draft cache, and prints that path. Read
or edit that file directly. `pull` overwrites `<slug>.html` in place, so copy
or publish any uncommitted local edits before pulling again.

Do not use the ordinary `/p/<slug>` response as the source for a revision. The
latest ordinary view injects a live-reload client, and a historical ordinary
view injects a version banner. Pushing either response back would accidentally
store the injected UI in the plan.

For direct reads, choose the representation deliberately:

```text
GET <server>/p/<slug>?format=raw                 # exact stored HTML, no UI
GET <server>/p/<slug>?format=raw&version=<n>     # exact historical HTML
GET <server>/p/<slug>?format=text                # readable text, no HTML UI
```

`format=text` preserves the metadata as a readable `[plan-meta]` block, so it
is useful for understanding a plan without spending tokens on styling. Use
`format=raw` or `push-plan pull` whenever the file may later be edited and
published. An agent-only view is a representation of the same plan, not a
separate UI-free copy to publish.

Use `GET <server>/api/plans/<slug>` when metadata such as the latest version,
parsed `plan-meta`, tags, share state, or history is needed. Do not use the
rendered page to infer those values.

When the user asks for the plan for the current repository or branch without
providing a URL:

1. Read `git remote get-url origin` and `git branch --show-current`.
2. Normalize the remote using the same rules as `push-plan`: produce
   `host/owner/name` without a protocol, credentials, or trailing `.git`.
   Do not guess a repository name or manually split a full plan URL.
3. Query `GET <server>/api/plans?repo=<repo>&branch=<branch>`.
4. Use the sole plausible match. If there are multiple plausible matches,
   ask the user which one; if there are none, report that no hosted plan was
   found. Use the `status` filter when archived or otherwise non-active plans
   should be excluded.

## Publish or revise a plan

Keep plan HTML out of the repository. Ask the command for its canonical draft
path before writing:

```bash
push-plan draft            # new plan; prints a private draft-cache path
push-plan draft <slug>     # path for a chosen/new slug; it need not exist yet
push-plan pull <slug>      # retrieve the current server copy before revising
```

Publish the file from that path:

```bash
push-plan <file.html> [title] -m "<one-line version note>"
```

Use `--slug <slug>` when publishing a file that is not named
`<draft-cache>/<slug>.html`. The command automatically tags the request with
the current Git repository and branch when available. Set `PUSH_PLAN_AGENT` to
the current agent/model name when useful; otherwise history records the
command's default `cli` identity.

Use separate write paths for separate kinds of changes:

- **Progress notes:** `POST /api/plans/<slug>/append` with an HTML fragment.
  Use this for implementation notes and deviation logs.
- **Phase ticks:** `POST /api/plans/<slug>/status` with partial JSON, for
  example `{"phases":[{"id":2,"status":"done"}]}`. Include only the
  fields that changed. These append/status writes are conflict-free and do
  not need a base version.
- **Content rewrites:** edit the raw draft HTML and publish with
  `--base-version <n>`. If the server returns HTTP 409, pull the latest
  version, reapply the edit, and retry with the new base version.

Always include a short `-m` note for a full publish. Return the response's
private tailnet URL, falling back to its LAN URL only when no tailnet URL is
present. Never return the draft-cache path as the deliverable. Run
`push-plan gc` to remove cached drafts whose plans no longer exist on the
server.

If neither the global nor bundled command is available, use the equivalent
HTTP API request for the immediate task. Do not install software without the
user's permission. The write fallbacks are:

```text
POST <server>/api/plans?slug=<slug>&title=<title>&agent=<agent>&repo=<repo>&branch=<branch>&note=<note>
Content-Type: text/html
<body of the new plan as the request body>

PUT <server>/api/plans/<slug>?base_version=<n>&agent=<agent>&note=<note>
Content-Type: text/html
<body of the revised plan as the request body>
```

Use the append and status endpoints above for notes and phase ticks instead of
full rewrites whenever possible.
