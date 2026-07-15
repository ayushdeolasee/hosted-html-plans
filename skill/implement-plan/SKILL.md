---
name: implement-plan
description: Review and implement a plan hosted on a hosted-html-plans server, writing progress back to the same URL as the work proceeds. Use when the user says "review this plan and implement it <url>", "implement the plan for this branch/repo", or points at a plans.*.ts.net URL and asks you to build it.
---

# implement-plan

You've been asked to implement a plan that lives on a hosted-html-plans
server rather than as a file in this repo. The plan is a **living
document**: every write to it creates a new immutable version, and your job
is both to build the thing and to keep the plan's URL an honest, live
reflection of progress as you go. 

## Step 0 — find the plan if no URL was given

If the user said something like "implement the plan for this branch" with
no URL pasted, discover it yourself:

```
GET <server>/api/plans?repo=<current-repo>&branch=<current-branch>
```

Get `<current-repo>` from `git remote get-url origin` (normalized to
`host/owner/name`) and `<current-branch>` from `git branch --show-current`.
If more than one plan matches, ask the user which one. If none match, say
so and stop rather than guessing.

## The protocol

### 1. Fetch and note the base version

```
GET <server>/p/<slug>?format=text
```

`?format=text` strips styling/boilerplate — read this, not the raw HTML, to
save tokens. Also fetch:

```
GET <server>/api/plans/<slug>
```

to get `latest` (the version number you just read — this is your
`base_version` for any later full revision) and the parsed `meta` block.

### 2. Review first — a gate, not a summary

Before writing any code, actually evaluate the plan against the real state
of the repo:

- Do the plan's assumptions still hold? Has the codebase moved since it was
  written?
- Are there contradictions, stale references, or missing pieces a careful
  read would catch?
- Is anything genuinely ambiguous enough that guessing wrong would waste
  work?

This is a real checkpoint, not a paraphrase of the plan back to the user.
If something is wrong or ambiguous, surface it and get it resolved (or
explicitly waved off by the user) before implementing. Don't rubber-stamp.

**Soft gate on `status`:** if `meta.status` is `"draft"` (not `"approved"`
or further along), warn the user explicitly — "this plan is still marked
draft, not approved — want me to go ahead anyway?" — and get a confirmation
before implementing. Don't refuse outright; it's a nudge, not a lock.

### 3. Verify targeting

Compare the plan's `meta.repo` / `meta.branch` against the actual repo and
branch you're sitting in (`git remote get-url origin`, normalized the same
way as above, and `git branch --show-current`). On a mismatch, stop and
report it clearly instead of implementing into the wrong place since the plan
may be meant for a different worktree or the branch may have been renamed.

### 4. Implement phase by phase

Work through `meta.phases` in order. For each phase, implement it, then
check it against its own `accept` criterion before moving to the next one. 
Don't mark or treat a phase as done until its accept criterion is actually
verified (run the command, load the page, whatever it specifies). Respect
everything in `meta.constraints` throughout.

### 5. Write back continuously

This is the part that makes the plan a living document instead of a
one-time read. A human may have the plan URL open while you work: the page
holds a live stream and re-renders itself the moment you write, with no
refresh. So the page only moves when you write to it — write at three
moments, not just at the end of a phase.

**When you start a phase**, mark it in-progress. Without this, a phase you
are actively working on looks exactly like one you haven't started:

```
POST <server>/api/plans/<slug>/status?agent=<you>&note=starting: <phase title>
Content-Type: application/json

{"phases":[{"id":<phase-id>,"status":"in-progress"}]}
```

**At checkpoints inside a phase**, append a one-line note as you clear each
meaningful chunk — a component built and its tests passing, a decision made,
a surprise hit. Aim for one every few minutes of work, so the reader sees
motion rather than a still page:

```
POST <server>/api/plans/<slug>/append?agent=<you>&note=<one-line note>
Content-Type: text/html

<p>Hub + latest-wins channel semantics done, tests green.</p>
```

**When you finish a phase** (and have actually verified its `accept`
criterion), mark it done, with any closing notes:

```
POST <server>/api/plans/<slug>/status?agent=<you>&note=<one-line note>
Content-Type: application/json

{"phases":[{"id":<phase-id>,"status":"done"}]}
```

Both `status` and `append` are conflict-free (§5 of `agent-loop.html`) —
they never need `base_version` and never 409. Use them liberally; they're
cheap.

Do keep each note substantive. Every write snapshots a full copy of the
document, so noise costs storage and buries the signal in the History panel:
narrate what changed and what you learned, not "still working."

Only use a full revision when the plan's *content* itself needs restructuring
(not just status/notes) — e.g. splitting a phase, rewriting a section that
turned out to be wrong. A full revision means editing the document on disk,
and **the document never lives in the repo you're implementing in**. Get its
one canonical local path from the CLI and work there:

```bash
push-plan pull <slug>            # overwrites <draftdir>/<slug>.html with the
                                 # server's latest, and prints the path
#   ...edit that file in place...
push-plan <that-path> --base-version <n> -m "<one-line note>"
```

`pull` always lands the plan on the same path (`<slug>.html` in the draft
cache), so revisions overwrite rather than accumulate, and pushing from that
path keeps the plan on its existing slug. If you'd rather drive the API by
hand, the raw shape is:

```
PUT <server>/api/plans/<slug>?base_version=<n>&agent=<you>&note=<one-line note>
Content-Type: text/html

<...full revised document...>
```

If this returns `409`, the response body has `{"current": N}` and so re-fetch
`?format=text` at the new version, re-apply your edit against the current
content, and retry with the new `base_version`. Don't force it by omitting
`base_version` — an omitted `base_version` is accepted but flagged
`forced` in history, which is a last resort, not a habit.

**Every write carries `?note=`** — treat it like a commit message. It's
what the plan's History panel (and the next agent who reads this plan)
relies on to understand what happened and why.

## Optional: `--archive` on completion

If the user asks for it (or passes `--archive`), once all phases are done,
commit a final snapshot of the plan into this repo at
`docs/plans/<slug>.html` (fetch the latest version, write the file, `git
add` + commit with a message referencing the plan). This is off by default,
the plans service is the working medium; git is the archive, per
`agent-loop.html` §11.

## What you must never do

- Never call `POST /api/plans/<slug>/share` or otherwise make a plan
  publicly reachable — sharing is a human-only action from the homepage.
- Never silently force a `PUT` past a `409` by dropping `base_version`.
- Never mark a phase `done` in `status` without having actually verified
  its `accept` criterion.
- Never write plan HTML into the repo's working tree — not as `plan.html`,
  not as a scratch copy you mean to delete later. The plan's only local file
  is the one `push-plan draft <slug>` / `push-plan pull <slug>` gives you, in
  the draft cache. (The one exception is the `--archive` snapshot above, which
  is an explicit, committed artifact.)
