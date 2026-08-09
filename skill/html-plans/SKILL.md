---
name: html-plans
description: Retrieve and publish private HTML plans on a hosted-html-plans server. Use when the user provides a hosted plan URL, asks for the plan associated with the current repository or branch, wants an existing hosted plan downloaded or revised, or wants an HTML plan or other HTML deliverable published to the plans server.
---

# HTML plans

Use this skill only for accessing the hosted-html-plans service. Once a plan
has been retrieved, treat it like any local HTML plan and follow the user's
request normally. 

Never make a plan public. Do not call `POST /api/plans/<slug>/share` and do
not offer a public link. 

## Resolve the command and server

Prefer the global `push-plan` command. If it is unavailable, run the
`push-plan` executable bundled beside this file. Do not search the user's
current repository for the command.

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

The command downloads the raw HTML and prints its canonical local path under
the private draft cache. Read or edit that file directly. It is deliberately
outside the current repository.

If only readable text is needed, fetch the compact representation instead:

```text
GET <server>/p/<slug>?format=text
```

Use `GET <server>/api/plans/<slug>` only when metadata such as the latest
version is needed.

When the user asks for the plan for the current repository or branch without
providing a URL:

1. Read `git remote get-url origin` and normalize it to `host/owner/name`
   without a protocol or `.git` suffix.
2. Read `git branch --show-current`.
3. Query `GET <server>/api/plans?repo=<repo>&branch=<branch>`.
4. Use the sole match. If there are multiple plausible matches, ask the user
   which one; if there are none, report that no hosted plan was found.

## Publish or revise a plan

Keep plan HTML out of the repository. Ask the command for its canonical draft
path before writing:

```bash
push-plan draft            # new plan
push-plan draft <slug>     # existing plan already available locally
push-plan pull <slug>      # retrieve the current server copy before revising
```

Publish the file from that path:

```bash
push-plan <file.html> [title] -m "<one-line version note>"
```

The command automatically tags the request with the current Git repository
and branch when available. A file named `<slug>.html` in the draft cache is
sent back to that same plan. For an explicitly version-locked revision, add
`--base-version <n>`; if it returns HTTP 409, retrieve the latest version,
reapply the edit, and retry with the new base version.

Always include a short `-m` note. Return the response's tailnet URL to the
user, falling back to its LAN URL only when no tailnet URL is present. Never
return the draft-cache path as the deliverable.

If neither the global nor bundled command is available, use the equivalent
HTTP API request for the immediate task. Do not install software without the
user's permission.
