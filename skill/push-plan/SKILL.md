---
name: push-plan
description: Push an HTML deliverable (plan, roadmap, report, analysis) produced in this session to a hosted-html-plans server so the user gets a durable, viewable URL instead of a local file path. Use whenever a session produces a standalone HTML document meant to be read or revisited later, and whenever the user says things like "push this plan", "put this on plans", or "send me the URL for this".
---

# push-plan

You just wrote (or are about to write) an HTML deliverable, which could be a plan, 
roadmap,report, or analysis meant for the user to read, not source code. This skill
gets it onto the hosted-html-plans server and hands back a URL.

**Never share plans publicly.** The funnel/share button is human-only by
convention you push to the private LAN/tailnet listeners and stop there.
Do not call `POST /api/plans/{slug}/share` yourself, and do not suggest a
public link.

## Step 0 — write the HTML into the draft cache, never into the repo

Plan HTML is not source code and must never land in the git worktree you're
working in. Ask the CLI where to write instead:

```bash
push-plan draft            # brand-new plan → prints a temporary path
push-plan draft <slug>     # revising an existing plan → prints its path
```

It prints one absolute path (under
`${PLANS_DRAFT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/plans/drafts}`), creating
the directory if needed. Write your document *there*, and push *that* file.

There is exactly one local file per plan, forever: `<slug>.html`. Every
revision overwrites the same path, so drafts can never pile up as
`plan.html`, `plan-v2.html`, `plan-final.html`. On the first push the server
assigns the real slug and `push-plan` renames the temp file onto it for you —
so the next `push-plan draft <slug>` hands back the same path with your
content already in it.

Two more commands round this out:

```bash
push-plan pull <slug> [--version N]  # overwrite the draft with a server version
push-plan gc                         # delete drafts whose plan is gone
```

`pull` is the revert flow: it clobbers `<slug>.html` in place, so there is no
stale newer file left behind to clean up. Use it before revising a plan you
didn't write in this session, so you're editing the real current content
rather than reconstructing it.

Never `rm` plan HTML from a repo yourself and never write one there "just for
a moment" — use the draft path from the start.

## Step 1 — embed the plan-meta block

Every plan you author must contain exactly one machine-readable meta block,
placed anywhere in `<body>` (a `<script type="application/json">` tag is
invisible to human readers but lets the implementing side parse structure
instead of scraping prose):

```html
<script type="application/json" id="plan-meta">
{
  "kind": "implementation-plan",
  "version": 1,
  "status": "draft",
  "repo": "github.com/owner/name",
  "branch": "main",
  "workdir_hint": "~/Developer/name",
  "phases": [
    { "id": 1, "title": "Core server", "status": "pending",
      "accept": "curl push + GET round-trips a file locally" },
    { "id": 2, "title": "Homepage", "status": "pending",
      "accept": "list/open/delete/rename/search work in browser" }
  ],
  "constraints": [
    "no Docker", "funnel strictly on-demand", "minimal RAM/CPU"
  ]
}
</script>
```

Fill it in honestly for the document you actually wrote:
- `status` — `draft` unless the user has explicitly approved the plan in
  this conversation, in which case use `approved`. Use `in-progress` /
  `done` only when revising an existing plan whose implementation has
  started (see step 3).
- `repo` / `branch` — auto-fill from the git context you're running in:
  `git remote get-url origin` (normalized to `host/owner/name`, no `.git`,
  no protocol) and `git branch --show-current`. Omit both keys entirely
  (don't emit empty strings) if you're not inside a repo with a remote.
- `workdir_hint` — the absolute path to the repo root you're in, if any.
- `phases` — one entry per major unit of work, each with a concrete,
  checkable `accept` criterion. This is what turns "implement this" into a
  verifiable checklist for whoever implements it later (maybe you, in a
  future session).
- `constraints` — non-negotiables that must survive the handoff even if the
  reader skims the prose (stack choices, things explicitly ruled out, hard
  limits).

If the document isn't really a plan (e.g. a one-off research report with no
phases), you can still include the block with `"kind": "report"` and an
empty or omitted `phases` array. This is what makes the doc discoverable and
taggable by repo/branch even when there's nothing to implement.

## Step 2 — check for an existing plan before creating a near-duplicate

Before pushing, check whether a plan already exists for this repo + branch:

```
GET <server>/api/plans?repo=<repo>&branch=<branch>
```

- If a plan already exists that this document is clearly a revision of
  (same topic, same repo/branch), revise it instead of creating a new
  slug: fetch it, note its `latest` version as `base_version`, and push
  with `PUT /api/plans/{slug}?base_version=<n>` rather than
  `POST /api/plans`. This keeps one URL as the living document instead of
  scattering `feature-roadmap`, `feature-roadmap-2`, `feature-roadmap-v2`...
  I review the implement-plan skill which outlines in detail the process 
  to revise a plan.
- If nothing matches, create fresh with `POST /api/plans`.
- When genuinely unsure whether this is a revision or a new topic, ask the
  user — don't guess silently.

## Step 3 — push

Use the `push-plan` command. It handles server-URL resolution, repo/branch
auto-tagging, and clipboard copy for you — you should not normally have to
hand-write a curl:

```
push-plan <file.html> [title] [-m "one-line version note"]
```

`<file.html>` is the path `push-plan draft` gave you (step 0) — not a path in
the repo. Because the file is named after the plan, the CLI tells the server
which plan it belongs to, so a reworded title can't fork a second plan. For a
revision with optimistic locking, add `--base-version <n>` (that sends the
`PUT` described below, and 409s instead of clobbering someone else's write).

You are almost certainly running inside some *unrelated* repo — whatever
project you're working on. That's expected: `push-plan` is a globally
installed command, not something you run out of a checkout. Never look for
it at a repo-relative path like `scripts/push-plan`.

Resolve it in this order, using the first that works:

1. **`push-plan` on `PATH`** — the normal case. Check: `command -v push-plan`.
2. **The copy bundled with this skill** — this skill ships the executable
   next to this file, so if you can read `SKILL.md`, the script is beside
   it. Use it by absolute path:
   ```bash
   "$(dirname <path-to-this-SKILL.md>)/push-plan" plan.html "Title" -m "note"
   # typically: ~/.claude/skills/push-plan/push-plan
   ```
3. **Raw curl** against the API (below) — last resort.

### If push-plan isn't installed

If steps 1 and 2 both fail, the CLI isn't on this machine. Use the curl
fallback for *this* push so the user isn't blocked, and then tell them they
can install the command once with:

```bash
curl -fsSL https://raw.githubusercontent.com/ayushdeolasee/hosted-html-plans/main/install.sh | bash -s -- --url <their-server-url>
```

Don't try to install it yourself without asking — it writes to their
`PATH` directory and their config.

### Server URL

The script resolves it in this order:

1. `$PLANS_URL` as an explicit single-server override.
2. `tailscale_url` from `~/.config/plans/config.json`.
3. `lan_url` from that config.
4. The legacy `push_url` key.
5. `http://localhost:8080` only when nothing is configured.

For push, pull, and GC, connection failure automatically advances to the next
configured URL. HTTP responses do not: a 409, 404, 500, or other response came
from a real server and must be handled rather than replayed elsewhere. If every
configured server is unreachable, the CLI reports each attempted endpoint.
Do not guess another address; ask the user to rerun the installer or use
`PLANS_URL=<url> push-plan …` as a temporary explicit override.

Set `$PUSH_PLAN_AGENT` to identify yourself (e.g. `claude-code`); it
defaults to `cli`.

### curl fallback

Only when the command is genuinely unavailable. Same shape the script uses:

```bash
curl -sS -X POST "$PLANS_URL/api/plans?title=<title>&agent=claude&repo=<repo>&branch=<branch>&note=<note>" \
  -H "Content-Type: text/html" \
  --data-binary @plan.html
```

For a revision of an existing plan, use `PUT` with `base_version`:

```bash
curl -sS -X PUT "$PLANS_URL/api/plans/<slug>?base_version=<n>&note=<note>" \
  -H "Content-Type: text/html" \
  --data-binary @plan.html
```

If the server returns `409` (someone else wrote a newer version between
your fetch and your push), re-fetch the current version, re-apply your
edit on top, and retry — don't force it.

**Always pass `?note=`** — a one-line summary of what changed, the
equivalent of a commit message. It's what makes the plan's History panel
useful.

## Step 4 — hand back the URL

Give the user the **tailnet URL** from the response (`urls.tailnet`), not
the local file path and not the LAN URL — it's the one that works from
their phone and any of their machines. Fall back to `urls.lan` only if
`urls.tailnet` is empty (tsnet not yet configured on the server). Do not
mention or offer a public share link.
