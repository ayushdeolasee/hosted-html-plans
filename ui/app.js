// Plans homepage — vanilla JS, no build step. Talks to the JSON API in
// server/http.go and renders the repo/branch index, the per-plan version
// spine, the version log, trash, and the funnel (public share) controls
// described in plan.html §9 and agent-loop.html §7/§12.
"use strict";

(function () {
  const STATUSES = ["draft", "approved", "in-progress", "done"];

  const state = {
    plans: [],
    trash: [],
    trashLoaded: false,
    loading: true,
    loadError: null,
    view: "plans",          // "plans" | "trash" | "settings"
    status: "",             // "" = any status
    repo: null,             // null = any repo; "" = the untagged group
    q: "",
    openDetails: new Set(), // preserve disclosures when filters or data refresh
    openLogs: new Set(),    // slugs whose version log is expanded
    rename: null,          // active title input; defer list replacement while editing
    armed: new Map(),       // "action:slug" -> timeout id, for two-click confirms

    // settings view
    settings: null,          // last GET/PUT /api/settings response
    settingsLoaded: false,
    update: null,
    updateCurrent: "—",
    updateBusy: false,
    updateConfirm: false,
    updateError: "",
    updateMessage: "",
    tsToggleBusy: false,     // tailscale_enabled PUT in flight (disables the switch)
    tailnetPollTimer: null,  // only set while polling GET /api/tailnet/status
  };

  const el = (id) => document.getElementById(id);
  const groupsEl = el("groups");
  const plansPlaceholderEl = el("plans-placeholder");
  const trashRowsEl = el("trash-rows");
  const trashPlaceholderEl = el("trash-placeholder");
  const statusFacetsEl = el("status-facets");
  const repoFacetsEl = el("repo-facets");
  const searchEl = el("search");
  const funnelEl = el("funnel");
  const funnelTextEl = el("funnel-text");
  const toastsEl = el("toasts");

  const settingsViewEl = el("settings-view");
  const restartBannerEl = el("restart-banner");
  const tsEnabledEl = el("ts-enabled");
  const tsStatusBadgeEl = el("ts-status-badge");
  const tsStatusDetailEl = el("ts-status-detail");
  const tsAuthBlockEl = el("ts-auth-block");
  const tsAuthLinkEl = el("ts-auth-link");
  const tsAuthCopyEl = el("ts-auth-copy");
  const hostnameInputEl = el("hostname-input");
  const lanListenInputEl = el("lan-listen-input");
  const settingsErrorEl = el("settings-error");
  const settingsSaveEl = el("settings-save");

  // ---- api ----

  let plansLoadRequest = 0;
  let trashLoadRequest = 0;

  async function api(method, path, body) {
    const opts = { method };
    if (body !== undefined) {
      opts.body = typeof body === "string" ? body : JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    let data = null;
    try { data = await res.json(); } catch (_) {
      if (res.ok && method === "GET") {
        throw new Error("The service returned an unreadable response.");
      }
      // Successful mutations may legitimately return an empty body.
    }
    if (!res.ok) {
      const msg = (data && (data.error || data.message)) || res.statusText;
      throw new Error(msg || `request failed (${res.status})`);
    }
    return data;
  }

  async function loadPlans() {
    const request = ++plansLoadRequest;
    try {
      const plans = await api("GET", "/api/plans");
      if (request !== plansLoadRequest) return;
      if (plans !== null && !Array.isArray(plans)) {
        throw new Error("The service returned an invalid plan list.");
      }
      state.plans = plans || [];
      state.loading = false;
      state.loadError = null;
      render();
    } catch (e) {
      if (request !== plansLoadRequest) return;
      if (state.loading || state.loadError !== null) {
        state.loading = false;
        state.loadError = e.message;
        render();
      }
      throw e;
    }
  }

  async function loadTrash() {
    const request = ++trashLoadRequest;
    try {
      const trash = await api("GET", "/api/trash");
      if (request !== trashLoadRequest) return;
      if (trash !== null && !Array.isArray(trash)) {
        throw new Error("The service returned an invalid trash list.");
      }
      state.trash = trash || [];
      state.trashLoaded = true;
      renderTrash();
    } catch (e) {
      if (request !== trashLoadRequest) return;
      throw e;
    }
  }

  // Wraps an action so a failure surfaces as a toast rather than an alert().
  async function attempt(failureMessage, fn) {
    try {
      await fn();
    } catch (e) {
      toast(`${failureMessage} ${e.message}`, true);
    }
  }

  // ---- formatting ----

  function esc(s) {
    return String(s ?? "").replace(/[&<>"']/g, (c) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    })[c]);
  }

  function relative(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "—";
    const mins = Math.round((Date.now() - d.getTime()) / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;
    if (mins < 60 * 24) return `${Math.round(mins / 60)}h ago`;
    if (mins < 60 * 24 * 7) return `${Math.round(mins / (60 * 24))}d ago`;
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }

  function absolute(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    return d.toLocaleString(undefined, {
      month: "short", day: "numeric", hour: "numeric", minute: "2-digit",
    });
  }

  function statusOf(plan) {
    return plan.status || "draft";
  }

  function statusLabel(status) {
    if (status === "in-progress") return "In progress";
    return status ? status[0].toUpperCase() + status.slice(1) : "Draft";
  }

  function phaseCounts(plan) {
    const phases = plan.meta && Array.isArray(plan.meta.phases) ? plan.meta.phases : null;
    if (!phases || phases.length === 0) return null;
    return {
      total: phases.length,
      done: phases.filter((p) => p && p.status === "done").length,
      active: phases.filter((p) => p && p.status === "in-progress").length,
    };
  }

  // A repo is stored as host/owner/name. The host is true but rarely the part
  // you're scanning for, so it stays but recedes.
  function repoParts(repo) {
    if (!repo) return null;
    const bits = repo.split("/").filter(Boolean);
    if (bits.length < 2) return { host: "", name: repo };
    return { host: bits[0] + "/", name: bits.slice(1).join("/") };
  }

  function repoLabel(repo) {
    const p = repoParts(repo);
    return p ? p.name : "Untagged";
  }

  function planUrl(slug) {
    return `${location.origin}/p/${encodeURIComponent(slug)}`;
  }

  // ---- filtering / grouping ----

  function matches(plan) {
    if (state.status && statusOf(plan) !== state.status) return false;
    if (state.repo !== null && (plan.repo || "") !== state.repo) return false;
    if (state.q) {
      const hay = [plan.title, plan.slug, plan.branch, plan.repo]
        .filter(Boolean).join(" ").toLowerCase();
      if (!hay.includes(state.q.toLowerCase())) return false;
    }
    return true;
  }

  function byUpdatedDesc(a, b) {
    return new Date(b.updated) - new Date(a.updated);
  }

  // Untagged plans sink below the repo-tagged ones; likewise unnamed branches.
  function keySort(a, b, label = (x) => x) {
    if (a === "") return 1;
    if (b === "") return -1;
    return label(a).localeCompare(label(b));
  }

  function group(plans) {
    const repos = new Map(); // repoKey -> Map(branchKey -> plans[])
    for (const p of [...plans].sort(byUpdatedDesc)) {
      const rk = p.repo || "";
      if (!repos.has(rk)) repos.set(rk, new Map());
      const branches = repos.get(rk);
      const bk = p.branch || "";
      if (!branches.has(bk)) branches.set(bk, []);
      branches.get(bk).push(p);
    }
    return repos;
  }

  // Replacing a rendered control must not send keyboard focus back to the
  // document. Data attributes or a plan link identify the same control in the new DOM.
  function preserveControlFocus(container, renderContent) {
    const focused = document.activeElement;
    if (!container.contains(focused) || !focused.matches("button, .plan-title a")) {
      renderContent();
      return;
    }
    const controls = [...container.querySelectorAll("button, .plan-title a")]
      .filter((control) => !control.disabled && !control.closest("details:not([open])") && control.getClientRects().length);
    const index = controls.indexOf(focused);
    const candidates = index < 0 ? [] : [focused,
      ...controls.slice(index + 1), ...controls.slice(0, index).reverse()];
    renderContent();
    if (candidates.length === 0 || focused.isConnected) return;
    const replacements = [...container.querySelectorAll("button, .plan-title a")]
      .filter((control) => !control.disabled && !control.closest("details:not([open])") && control.getClientRects().length);
    for (const candidate of candidates) {
      const keys = Object.keys(candidate.dataset);
      const href = candidate.matches(".plan-title a") ? candidate.getAttribute("href") : null;
      if (keys.length === 0 && !href) continue;
      const replacement = replacements.find((control) => href
        ? control.matches(".plan-title a") && control.getAttribute("href") === href
        : keys.every((key) => control.dataset[key] === candidate.dataset[key]));
      if (replacement) {
        replacement.focus({ preventScroll: true });
        return;
      }
    }
    const fallback = container.querySelector(".plan-title a")
      || container.closest("section")?.querySelector("h2, .note-line") || el("library-title");
    if (!fallback.matches("a[href]")) fallback.tabIndex = -1;
    fallback.focus({ preventScroll: true });
  }

  // ---- rail ----

  function facet({ label, count, active, key, mono }) {
    return `<button class="facet" data-key="${esc(key)}" aria-pressed="${active}">
      <span class="label"${mono ? ` title="${esc(label)}"` : ""}>${esc(label)}</span>
      <span class="n">${count}</span>
    </button>`;
  }

  function renderRail() {
    preserveControlFocus(statusFacetsEl.closest(".rail"), renderRailContent);
  }

  function renderRailContent() {
    const byStatus = (s) => state.plans.filter((p) => statusOf(p) === s).length;
    statusFacetsEl.innerHTML = [
      facet({ label: "Any", count: state.plans.length, active: state.status === "", key: "" }),
      ...STATUSES.map((s) => facet({
        label: s === "in-progress" ? "In progress" : s[0].toUpperCase() + s.slice(1),
        count: byStatus(s),
        active: state.status === s,
        key: s,
      })),
    ].join("");

    const counts = new Map();
    for (const p of state.plans) {
      const k = p.repo || "";
      counts.set(k, (counts.get(k) || 0) + 1);
    }
    const keys = [...counts.keys()].sort((a, b) => keySort(a, b, repoLabel));
    repoFacetsEl.innerHTML = [
      facet({ label: "All", count: state.plans.length, active: state.repo === null, key: "all-projects" }),
      ...keys.map((k) => facet({
        label: repoLabel(k),
        count: counts.get(k),
        active: state.repo === k,
        key: `repo:${k}`,
        mono: true,
      })),
    ].join("");
  }

  // ---- the version spine ----

  // One tick per version, newest at top. The tick's width says how much of the
  // document that write touched: a create or a full revision is a slab, an
  // append is a partial bar, a status merge is a stub. Read top-to-bottom, the
  // spine is the plan's whole provenance at a glance.
  function renderSpine(plan) {
    const history = [...(plan.history || [])].sort((a, b) => b.version - a.version);
    if (history.length === 0) return `<div class="spine"></div>`;
    const ticks = history.map((h, i) => {
      const action = String(h.action || "revise");
      const title = `v${h.version} · ${action}${h.forced ? " (forced)" : ""} · ${absolute(h.timestamp)}`
        + (h.note ? ` · ${h.note}` : "");
      return `<button class="tick act-${esc(action)}${h.forced ? " forced" : ""}"
        style="animation-delay:${Math.min(i * 25, 300)}ms"
        data-action="open-version" data-slug="${esc(plan.slug)}" data-version="${h.version}"
        title="${esc(title)}" aria-label="Open version ${h.version} (${esc(action)})"></button>`;
    }).join("");
    return `<div class="spine">${ticks}</div>`;
  }

  function renderPhases(plan) {
    const c = phaseCounts(plan);
    if (!c) return "";
    return `<div class="phases">
      <progress class="phase-progress" value="${c.done}" max="${c.total}"
        aria-label="Plan progress" aria-valuetext="${c.done} of ${c.total} phases complete"
        title="${c.done} of ${c.total} phases complete">${c.done} of ${c.total} phases complete</progress>
    </div>`;
  }

  // Deleting history is a hard delete — the blob is unlinked, there is no
  // trash tier for it — so the control arms first, like every other
  // destructive button here. The latest version is not deletable at all: it
  // *is* the plan, and Delete (which trashes the whole thing) covers that.
  function renderLog(plan) {
    if (!state.openLogs.has(plan.slug)) return "";
    const rows = [...(plan.history || [])].sort((a, b) => b.version - a.version);
    if (rows.length === 0) return `<div class="log"><span class="log-row">No versions recorded.</span></div>`;

    const del = (h) => {
      if (h.version === plan.latest) {
        return `<button disabled title="The latest version can't be deleted — it is the plan. Delete the plan instead.">Delete</button>`;
      }
      const armed = state.armed.has(`delete-version:${plan.slug}:${h.version}`);
      return `<button class="${armed ? "armed" : ""}" data-action="delete-version"
        data-slug="${esc(plan.slug)}" data-version="${h.version}"
        title="Delete v${h.version} permanently">${armed ? "Delete for real?" : "Delete"}</button>`;
    };

    const clearArmed = state.armed.has(`clear-history:${plan.slug}`);
    const foot = rows.length > 1
      ? `<div class="log-foot">
           <span class="log-hint">History is kept forever until you delete it. Deletion is permanent.</span>
           <button class="${clearArmed ? "armed" : ""}" data-action="clear-history" data-slug="${esc(plan.slug)}"
             title="Permanently delete every version except v${plan.latest}">${clearArmed ? "Delete all but v" + plan.latest + "?" : "Clear history"}</button>
         </div>`
      : "";

    return `<div class="log">${rows.map((h) => `
      <div class="log-row">
        <span class="v">v${h.version}</span>
        <span class="act ${esc(h.action)}${h.forced ? " forced" : ""}">${esc(h.action)}${h.forced ? "!" : ""}</span>
        <span class="when">${esc(absolute(h.timestamp))}${h.agent ? " · " + esc(h.agent) : ""}</span>
        <span class="note">${esc(h.note || "")}</span>
        <span class="log-actions">
          <button data-action="open-version" data-slug="${esc(plan.slug)}" data-version="${h.version}">Open</button>
          <button data-action="restore-version" data-slug="${esc(plan.slug)}" data-version="${h.version}">Restore</button>
          ${del(h)}
        </span>
      </div>`).join("")}${foot}</div>`;
  }

  function renderRow(plan, index) {
    const status = statusOf(plan);
    const shared = !!plan.share_token;
    const versions = (plan.history || []).length || plan.latest || 0;
    const armedDelete = state.armed.has(`delete:${plan.slug}`);

    return `
    <article class="row" data-slug="${esc(plan.slug)}" style="animation-delay:${Math.min(index * 35, 250)}ms">
      <div class="row-body">
        <div class="row-head">
          <div class="row-head-main">
            <h3 class="plan-title" data-role="title">
              <a href="/p/${encodeURIComponent(plan.slug)}" target="_blank" rel="noopener">${esc(plan.title || plan.slug)}</a>
            </h3>
            <span class="tag status status-${esc(status)}">${esc(statusLabel(status))}</span>
            ${shared ? `<span class="tag status status-shared">public</span>` : ""}
          </div>
          <div class="row-summary"><time title="${esc(absolute(plan.updated))}">${esc(relative(plan.updated))}</time></div>
        </div>

        <div class="facts">
          <span>${esc(plan.agent || "unknown")}</span>
          <span class="dot">·</span>
          <span>/p/${esc(plan.slug)}</span>
          <span class="dot">·</span>
          <span>${versions} version${versions === 1 ? "" : "s"}</span>
        </div>

        ${renderPhases(plan)}

        <details class="plan-details"${state.openDetails.has(plan.slug) || state.openLogs.has(plan.slug) ? " open" : ""}>
          <summary>Details &amp; actions</summary>
          <div class="details-content">
            <div class="actions">
              <button data-action="copy-link" data-slug="${esc(plan.slug)}">Copy link</button>
              <button data-action="rename" data-slug="${esc(plan.slug)}">Rename</button>
              ${status === "draft" ? `<button data-action="approve" data-slug="${esc(plan.slug)}">Approve</button>` : ""}
              <button data-action="share" data-slug="${esc(plan.slug)}">${shared ? "Stop sharing" : "Share publicly"}</button>
              <button class="${armedDelete ? "armed" : ""}" data-action="delete" data-slug="${esc(plan.slug)}">${armedDelete ? "Delete for real?" : "Delete"}</button>
              <button class="versions" data-action="toggle-log" data-slug="${esc(plan.slug)}"
                      aria-expanded="${state.openLogs.has(plan.slug)}">View history</button>
            </div>
            ${renderLog(plan)}
          </div>
        </details>
      </div>
    </article>`;
  }

  // ---- plans view ----

  function placeholder(node, html) {
    node.classList.remove("hidden");
    node.innerHTML = html;
  }

  function renderGroups() {
    if (state.rename) return;
    // Native toggle events are queued; capture the current disclosure before
    // a keyboard action replaces its DOM, even if that event has not fired yet.
    for (const details of groupsEl.querySelectorAll(".plan-details")) {
      const slug = details.closest("[data-slug]").dataset.slug;
      if (details.open) state.openDetails.add(slug);
      else state.openDetails.delete(slug);
    }
    preserveControlFocus(groupsEl, renderGroupsContent);
  }

  function renderGroupsContent() {
    const filtered = state.plans.filter(matches);
    const hasFilters = !!(state.q || state.status || state.repo !== null);
    const projects = new Set(filtered.map((p) => p.repo || ""));
    el("library-title").textContent = state.repo === null ? "All plans" : repoLabel(state.repo);
    el("library-count").textContent = state.loading ? "Loading your library…"
      : state.loadError ? "Library unavailable"
      : `${filtered.length} ${hasFilters ? "matching " : ""}plan${filtered.length === 1 ? "" : "s"} · ${projects.size} project${projects.size === 1 ? "" : "s"}`;
    el("clear-filters").classList.toggle("hidden", !hasFilters);
    if (state.loading) {
      groupsEl.innerHTML = "";
      placeholder(plansPlaceholderEl, `<p>Loading plans…</p>`);
      return;
    }
    if (state.loadError) {
      groupsEl.innerHTML = "";
      plansPlaceholderEl.classList.add("failed");
      placeholder(plansPlaceholderEl,
        `<p>Couldn't reach the plans service.</p><p><code>${esc(state.loadError)}</code></p>
         <button data-action="retry">Try again</button>`);
      return;
    }
    plansPlaceholderEl.classList.remove("failed");

    if (filtered.length === 0) {
      groupsEl.innerHTML = "";
      if (state.plans.length === 0) {
        placeholder(plansPlaceholderEl,
          `<p>No plans yet.</p><p>Agents push them here with <code>push-plan &lt;file.html&gt;</code>.</p>`);
      } else {
        placeholder(plansPlaceholderEl,
          `<p>No plans match this filter.</p><button data-action="clear-filters">Clear filters</button>`);
      }
      return;
    }
    plansPlaceholderEl.classList.add("hidden");

    const repos = group(filtered);
    const repoKeys = [...repos.keys()].sort((a, b) => keySort(a, b, repoLabel));

    let n = 0;
    let html = "";
    for (const rk of repoKeys) {
      const head = rk ? esc(rk.split("/").filter(Boolean).pop() || rk) : "Untagged";
      const count = [...repos.get(rk).values()].reduce((sum, plans) => sum + plans.length, 0);
      html += `<section class="repo-group"><h2 class="repo-head"><span class="repo-name" title="${esc(rk || "Untagged")}" aria-label="${esc(rk || "Untagged")}">${head}</span><span class="repo-count">${count} plan${count === 1 ? "" : "s"}</span></h2>`;

      const branches = repos.get(rk);
      const branchKeys = [...branches.keys()].sort((a, b) => keySort(a, b));
      for (const bk of branchKeys) {
        const plans = branches.get(bk);
        // An untagged plan has no branch to name — skip the empty band.
        const showBand = rk !== "" || bk !== "";
        html += showBand
          ? `<div class="branch-band">
               <span class="tag">${esc(bk || "no branch")}</span>
               <span class="rule"></span>
               <span class="n">${plans.length}</span>
             </div>`
          : `<div style="height:14px"></div>`;
        html += `<div class="rows">${plans.map((p) => renderRow(p, n++)).join("")}</div>`;
      }
      html += `</section>`;
    }
    groupsEl.innerHTML = html;
  }

  function renderFunnel() {
    const shared = state.plans.filter((p) => !!p.share_token);
    funnelEl.classList.toggle("hidden", shared.length === 0);
    if (shared.length === 0) return;
    funnelTextEl.textContent = shared.length === 1
      ? `1 plan is reachable from the public internet.`
      : `${shared.length} plans are reachable from the public internet.`;
  }

  function render() {
    renderRail();
    renderGroups();
    renderFunnel();
  }

  // ---- trash view ----

  function renderTrashRow(plan, index) {
    const armed = state.armed.has(`purge:${plan.slug}`);
    return `
    <article class="row" data-slug="${esc(plan.slug)}" style="animation-delay:${Math.min(index * 35, 250)}ms">
      ${renderSpine(plan)}
      <div class="row-body">
        <div class="row-head">
          <h3 class="plan-title">${esc(plan.title || plan.slug)}</h3>
          <span class="tag status status-draft">deleted</span>
        </div>
        <div class="facts">
          <span>/p/${esc(plan.slug)}</span>
          <span class="dot">·</span>
          <span>v${plan.latest}</span>
          <span class="dot">·</span>
          <span title="${esc(absolute(plan.trashed_at))}">deleted ${esc(relative(plan.trashed_at))}</span>
        </div>
        <div class="actions">
          <button data-action="untrash" data-slug="${esc(plan.slug)}">Restore</button>
          <button class="${armed ? "armed" : ""}" data-action="purge" data-slug="${esc(plan.slug)}">${armed ? "Purge for real?" : "Purge"}</button>
        </div>
      </div>
    </article>`;
  }

  function renderTrash() {
    preserveControlFocus(trashRowsEl, renderTrashContent);
  }

  function renderTrashContent() {
    if (state.trash.length === 0) {
      trashRowsEl.innerHTML = "";
      placeholder(trashPlaceholderEl, `<p>Trash is empty.</p>`);
      return;
    }
    trashPlaceholderEl.classList.add("hidden");
    trashRowsEl.innerHTML = state.trash.map(renderTrashRow).join("");
  }

  // ---- settings view ----

  async function loadSettings() {
    state.settings = await api("GET", "/api/settings");
    state.settingsLoaded = true;
    renderSettings({ fillInputs: true });
    scheduleTailnetPoll();
  }

  // ---- manual software updates ----

  function renderUpdate() {
    const u = state.update;
    el("update-current").textContent = u ? u.current_version : state.updateCurrent;
    el("update-latest").textContent = u && u.latest_version ? u.latest_version : u ? "Not available" : "Not checked";
    el("update-status").textContent = state.updateMessage || (u
      ? !u.supported ? u.reason || "This installation must be updated from the command line."
        : u.update_available ? "A new stable release is ready to install."
        : u.latest_version ? "You're up to date." : "No stable release has been published yet."
      : "Check for a new release when you're ready.");
    el("update-error").textContent = state.updateError;
    el("update-error").classList.toggle("hidden", !state.updateError);
    el("update-check").disabled = state.updateBusy;
    el("update-install").disabled = state.updateBusy;
    el("update-install").classList.toggle("hidden", !(u && u.supported && u.update_available) || state.updateConfirm);
    el("update-confirm").classList.toggle("hidden", !state.updateConfirm);
    el("update-confirm-install").disabled = state.updateBusy;
    el("update-cancel").disabled = state.updateBusy;
    const notes = el("update-notes");
    // Build the link on the official repository, never from an arbitrary URL.
    const releaseTag = u && /^v\d+\.\d+\.\d+$/.test(u.latest_version) ? u.latest_version : "";
    notes.classList.toggle("hidden", !releaseTag);
    if (releaseTag) notes.href = `https://github.com/ayushdeolasee/hosted-html-plans/releases/tag/${encodeURIComponent(releaseTag)}`;
    else notes.removeAttribute("href");
  }

  async function checkUpdates() {
    if (state.updateBusy) return;
    state.updateBusy = true;
    state.updateConfirm = false;
    state.updateError = "";
    state.updateMessage = "Checking GitHub for a stable release…";
    renderUpdate();
    try {
      const response = await fetch("/api/updates", { cache: "no-store", signal: AbortSignal.timeout(30000) });
      const result = await response.json();
      if (!response.ok || result.status === "error") throw new Error(result.error || result.reason || "GitHub could not be reached.");
      state.update = result;
      state.updateCurrent = result.current_version;
      if (result.status === "available" && result.reason) state.updateError = result.reason;
    } catch (e) {
      state.update = null;
      state.updateError = `Couldn't check for updates. ${e.message}`;
    } finally {
      state.updateBusy = false;
      state.updateMessage = "";
      renderUpdate();
    }
  }

  async function installUpdate() {
    if (state.updateBusy || !state.updateConfirm || !state.update || !state.update.supported || !state.update.update_available) return;
    let target = state.update.latest_version;
    state.updateBusy = true;
    state.updateError = "";
    state.updateMessage = "Downloading and verifying the update. Keep this page open…";
    renderUpdate();
    try {
      const response = await fetch("/api/updates", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
        signal: AbortSignal.timeout(180000),
      });
      const result = await response.json();
      if (!response.ok || result.status === "error") throw new Error(result.error || result.reason || "The update could not be installed.");
      target = result.latest_version || target;
      state.updateConfirm = false;
      state.updateMessage = `Restarting the service. Waiting for ${target}…`;
      renderUpdate();
      // Health checks are local: reconnecting must not repeatedly query GitHub.
      const deadline = Date.now() + 90000;
      while (Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        try {
          const health = await fetch("/healthz", { cache: "no-store", signal: AbortSignal.timeout(4000) });
          if (!health.ok) continue;
          const data = await health.json();
          if (data.version === target) {
            location.reload();
            return;
          }
        } catch (_) { /* The service is expected to go offline briefly. */ }
      }
      throw new Error("Couldn't confirm the new version after restarting. Check the service on the host, then reload this page.");
    } catch (e) {
      state.updateError = `${e.message} Your plans and settings have not been removed.`;
    } finally {
      state.updateBusy = false;
      state.updateConfirm = false;
      state.updateMessage = "";
      renderUpdate();
    }
  }

  function tsStateLabel(tnState) {
    switch (tnState) {
      case "starting": return "Starting…";
      case "waiting-for-auth": return "Waiting for authentication";
      case "authenticated": return "Connected";
      default: return "Disabled";
    }
  }

  function renderSettings(opts = {}) {
    if (!state.settings) return;
    const s = state.settings;
    const tn = s.tailnet || { state: "disabled" };

    tsEnabledEl.checked = !!s.tailscale_enabled;
    tsEnabledEl.disabled = state.tsToggleBusy;

    tsStatusBadgeEl.className = `tag ts-badge ts-state-${esc(tn.state || "disabled")}`;
    tsStatusBadgeEl.textContent = tsStateLabel(tn.state);

    if (tn.state === "authenticated" && tn.fqdn) {
      tsStatusDetailEl.innerHTML =
        `Connected as <a href="https://${esc(tn.fqdn)}" target="_blank" rel="noopener">${esc(tn.fqdn)}</a>`;
    } else if (tn.state === "waiting-for-auth") {
      tsStatusDetailEl.textContent = "Approve the new node to continue.";
    } else if (tn.state === "starting") {
      tsStatusDetailEl.textContent = "Connecting to your tailnet…";
    } else {
      tsStatusDetailEl.textContent = "";
    }

    const waiting = tn.state === "waiting-for-auth" && !!tn.auth_url;
    tsAuthBlockEl.classList.toggle("hidden", !waiting);
    if (waiting) tsAuthLinkEl.href = tn.auth_url;

    if (opts.fillInputs) {
      hostnameInputEl.value = s.hostname || "";
      lanListenInputEl.value = s.lan_listen || "";
    }

    restartBannerEl.classList.toggle("hidden", !s.restart_required);
  }

  // Polls GET /api/tailnet/status every 3s while the tailnet is mid-flight
  // (starting or waiting for the human to approve it in the admin console)
  // and the settings view is on screen. Stops the moment either condition
  // stops holding — see clearTailnetPoll, called from setView and here.
  function scheduleTailnetPoll() {
    clearTailnetPoll();
    if (state.view !== "settings" || !state.settings) return;
    const st = state.settings.tailnet && state.settings.tailnet.state;
    if (st !== "starting" && st !== "waiting-for-auth") return;
    state.tailnetPollTimer = setTimeout(async () => {
      try {
        const tn = await api("GET", "/api/tailnet/status");
        if (state.settings) {
          state.settings.tailnet = tn;
          renderSettings();
        }
      } catch (_) {
        // Transient network hiccup — just try again on the next tick.
      }
      scheduleTailnetPoll();
    }, 3000);
  }

  function clearTailnetPoll() {
    if (state.tailnetPollTimer) {
      clearTimeout(state.tailnetPollTimer);
      state.tailnetPollTimer = null;
    }
  }

  async function toggleTailscale() {
    const next = tsEnabledEl.checked;
    state.tsToggleBusy = true;
    renderSettings();
    await attempt("Couldn't update Tailscale:", async () => {
      state.settings = await api("PUT", "/api/settings", { tailscale_enabled: next });
    });
    state.tsToggleBusy = false;
    renderSettings();
    scheduleTailnetPoll();
  }

  // Only hostname/lan_listen are diffed here — tailscale_enabled has its own
  // immediate PUT via toggleTailscale, so Save never re-sends it.
  function changedServerFields() {
    const s = state.settings;
    const patch = {};
    const hostname = hostnameInputEl.value.trim();
    const lanListen = lanListenInputEl.value.trim();
    if (hostname !== (s.hostname || "")) patch.hostname = hostname;
    if (lanListen !== (s.lan_listen || "")) patch.lan_listen = lanListen;
    return patch;
  }

  async function saveServerSettings() {
    settingsErrorEl.classList.add("hidden");
    settingsErrorEl.textContent = "";
    const patch = changedServerFields();
    if (Object.keys(patch).length === 0) {
      toast("Nothing to save.");
      return;
    }
    settingsSaveEl.disabled = true;
    try {
      state.settings = await api("PUT", "/api/settings", patch);
      renderSettings({ fillInputs: true });
      scheduleTailnetPoll();
      toast("Settings saved.");
    } catch (e) {
      settingsErrorEl.textContent = e.message;
      settingsErrorEl.classList.remove("hidden");
    } finally {
      settingsSaveEl.disabled = false;
    }
  }

  function setView(view) {
    state.view = view;
    el("plans-view").classList.toggle("hidden", view !== "plans");
    el("trash-view").classList.toggle("hidden", view !== "trash");
    settingsViewEl.classList.toggle("hidden", view !== "settings");
    el("frame").classList.toggle("on-trash", view === "trash");
    el("frame").classList.toggle("on-settings", view === "settings");
    for (const [id, name] of [["view-plans", "plans"], ["view-trash", "trash"], ["view-settings", "settings"]]) {
      const b = el(id);
      b.classList.toggle("solid", view === name);
      b.setAttribute("aria-pressed", String(view === name));
    }
    if (view === "trash" && !state.trashLoaded) {
      attempt("Couldn't load the trash:", loadTrash);
    }
    if (view === "settings") {
      if (location.hash !== "#settings") history.replaceState(null, "", "#settings");
      if (state.updateCurrent === "—") {
        api("GET", "/healthz").then((health) => {
          state.updateCurrent = health.version || "Unknown";
          renderUpdate();
        }).catch(() => { /* Checking for updates can retry if the host is offline. */ });
      }
      if (!state.settingsLoaded) {
        attempt("Couldn't load settings:", loadSettings);
      } else {
        scheduleTailnetPoll();
      }
    } else {
      clearTailnetPoll();
      if (location.hash === "#settings") {
        history.replaceState(null, "", location.pathname + location.search);
      }
    }
  }

  // ---- toasts ----

  function toast(message, bad) {
    const node = document.createElement("div");
    node.className = `toast${bad ? " bad" : ""}`;
    node.textContent = message;
    toastsEl.appendChild(node);
    setTimeout(() => node.remove(), bad ? 6000 : 2600);
  }

  // Destructive buttons arm on first click and fire on the second.
  function arm(key, fire, rerender) {
    if (state.armed.has(key)) {
      clearTimeout(state.armed.get(key));
      state.armed.delete(key);
      fire();
      return;
    }
    state.armed.set(key, setTimeout(() => {
      state.armed.delete(key);
      rerender();
    }, 4000));
    rerender();
  }

  // ---- actions ----

  function doRename(slug, titleEl) {
    if (state.rename) {
      state.rename.focus();
      return;
    }
    const current = titleEl.textContent.trim();
    const input = document.createElement("input");
    input.type = "text";
    input.className = "title-edit";
    input.value = current;
    input.setAttribute("aria-label", "Plan title");
    state.rename = input;
    titleEl.replaceWith(input);
    input.focus();
    input.select();

    const restoreFocus = () => {
      if (document.activeElement !== input && document.activeElement !== document.body) return;
      const target = [...groupsEl.querySelectorAll('[data-action="rename"]')]
        .find((button) => button.dataset.slug === slug)
        || groupsEl.querySelector(".plan-title a") || el("library-title");
      if (!target.matches("button, a[href]")) target.tabIndex = -1;
      target.focus({ preventScroll: true });
    };
    let settled = false;
    const commit = async (returnFocus) => {
      if (settled) return;
      settled = true;
      input.readOnly = true;
      const next = input.value.trim();
      if (next && next !== current) {
        try {
          await api("PATCH", `/api/plans/${encodeURIComponent(slug)}`, { title: next });
          const plan = state.plans.find((plan) => plan.slug === slug);
          if (plan) plan.title = next;
          toast(`Renamed to “${next}”.`);
        } catch (e) {
          settled = false;
          input.readOnly = false;
          toast(`Couldn't rename the plan: ${e.message}`, true);
          if (returnFocus && (document.activeElement === input || document.activeElement === document.body)) input.focus();
          return;
        }
      }
      state.rename = null;
      render();
      if (returnFocus) restoreFocus();
      attempt("Couldn't refresh plans:", loadPlans);
    };
    // Let Tab/click navigation settle before replacing the row on blur.
    input.addEventListener("blur", () => queueMicrotask(() => commit(false)));
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        commit(true);
      }
      if (e.key === "Escape" && !settled) {
        e.preventDefault();
        settled = true;
        state.rename = null;
        render();
        restoreFocus();
      }
    });
  }

  function doShare(slug, shared) {
    return attempt("Couldn't change sharing:", async () => {
      if (shared) {
        await api("DELETE", `/api/plans/${encodeURIComponent(slug)}/share`);
        toast("Stopped sharing. The public link no longer resolves.");
      } else {
        const res = await api("POST", `/api/plans/${encodeURIComponent(slug)}/share`, "");
        await copy((res && res.url) || planUrl(slug));
        toast("Shared publicly. Link copied to the clipboard.");
      }
      await loadPlans();
    });
  }

  function doRevokeAll() {
    return attempt("Couldn't revoke every link:", async () => {
      const shared = state.plans.filter((p) => !!p.share_token);
      await Promise.all(shared.map((p) =>
        api("DELETE", `/api/plans/${encodeURIComponent(p.slug)}/share`)));
      await loadPlans();
      toast("Revoked every public link.");
    });
  }

  function doApprove(slug) {
    return attempt("Couldn't approve the plan:", async () => {
      await api("POST", `/api/plans/${encodeURIComponent(slug)}/status`, { status: "approved" });
      await loadPlans();
      toast("Approved.");
    });
  }

  function doDelete(slug) {
    return attempt("Couldn't delete the plan:", async () => {
      await api("DELETE", `/api/plans/${encodeURIComponent(slug)}`);
      await loadPlans();
      if (state.trashLoaded) await loadTrash();
      toast("Deleted. It's in the trash with every version intact.");
    });
  }

  function doUntrash(slug) {
    return attempt("Couldn't restore the plan:", async () => {
      await api("POST", `/api/trash/${encodeURIComponent(slug)}/restore`);
      await loadTrash();
      await loadPlans();
      toast("Restored to the plans list.");
    });
  }

  function doPurge(slug) {
    return attempt("Couldn't purge the plan:", async () => {
      await api("DELETE", `/api/trash/${encodeURIComponent(slug)}`);
      await loadTrash();
      toast("Purged for good.");
    });
  }

  function doRestoreVersion(slug, version) {
    return attempt("Couldn't restore that version:", async () => {
      await api("POST", `/api/plans/${encodeURIComponent(slug)}/restore?version=${version}`);
      await loadPlans();
      toast(`Copied v${version} forward as the latest version.`);
    });
  }

  function doDeleteVersion(slug, version) {
    return attempt("Couldn't delete that version:", async () => {
      await api("DELETE", `/api/plans/${encodeURIComponent(slug)}/history/${version}`);
      await loadPlans();
      toast(`Deleted v${version}. That version is gone for good.`);
    });
  }

  function doClearHistory(slug) {
    return attempt("Couldn't clear the history:", async () => {
      const res = await api("DELETE", `/api/plans/${encodeURIComponent(slug)}/history`);
      await loadPlans();
      const n = (res && res.pruned) || 0;
      toast(n === 1
        ? `Deleted 1 old version. Only the latest remains.`
        : `Deleted ${n} old versions. Only the latest remains.`);
    });
  }

  async function copy(text) {
    try {
      await navigator.clipboard.writeText(text);
    } catch (_) {
      // Clipboard API needs a secure context; the LAN listener is plain HTTP.
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.cssText = "position:fixed;opacity:0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch (_) { /* nothing left to try */ }
      ta.remove();
    }
  }

  function clearFilters() {
    state.status = "";
    state.repo = null;
    state.q = "";
    searchEl.value = "";
    render();
  }

  // ---- events (delegated: whole containers get re-rendered) ----

  document.body.addEventListener("click", (e) => {
    const btn = e.target.closest("button[data-action]");
    if (!btn) return;
    const { action, slug, version } = btn.dataset;

    switch (action) {
      case "open-version":
        window.open(`/p/${encodeURIComponent(slug)}?version=${version}`, "_blank", "noopener");
        break;
      case "toggle-log":
        if (state.openLogs.has(slug)) state.openLogs.delete(slug);
        else state.openLogs.add(slug);
        renderGroups();
        break;
      case "copy-link":
        copy(planUrl(slug));
        btn.textContent = "Copied";
        setTimeout(() => { btn.textContent = "Copy link"; }, 1400);
        break;
      case "rename": {
        const titleEl = btn.closest(".row").querySelector('[data-role="title"]');
        if (titleEl) doRename(slug, titleEl);
        break;
      }
      case "approve":
        doApprove(slug);
        break;
      case "share": {
        const plan = state.plans.find((p) => p.slug === slug);
        doShare(slug, !!(plan && plan.share_token));
        break;
      }
      case "restore-version":
        doRestoreVersion(slug, version);
        break;
      case "delete-version":
        arm(`delete-version:${slug}:${version}`, () => doDeleteVersion(slug, version), renderGroups);
        break;
      case "clear-history":
        arm(`clear-history:${slug}`, () => doClearHistory(slug), renderGroups);
        break;
      case "delete":
        arm(`delete:${slug}`, () => doDelete(slug), renderGroups);
        break;
      case "untrash":
        doUntrash(slug);
        break;
      case "purge":
        arm(`purge:${slug}`, () => doPurge(slug), renderTrash);
        break;
      case "clear-filters":
        clearFilters();
        break;
      case "retry":
        state.loading = true;
        renderGroups();
        boot();
        break;
    }
  });

  statusFacetsEl.addEventListener("click", (e) => {
    const f = e.target.closest(".facet");
    if (!f) return;
    state.status = f.dataset.key;
    render();
  });

  repoFacetsEl.addEventListener("click", (e) => {
    const f = e.target.closest(".facet");
    if (!f) return;
    state.repo = f.dataset.key === "all-projects" ? null : f.dataset.key.slice(5);
    render();
  });

  groupsEl.addEventListener("toggle", (e) => {
    if (!e.target.matches(".plan-details") || !groupsEl.contains(e.target)) return;
    const slug = e.target.closest("[data-slug]").dataset.slug;
    if (e.target.open) state.openDetails.add(slug);
    else state.openDetails.delete(slug);
  }, true);

  searchEl.addEventListener("input", () => {
    state.q = searchEl.value.trim();
    renderGroups();
  });

  el("view-plans").addEventListener("click", () => setView("plans"));
  el("view-trash").addEventListener("click", () => setView("trash"));
  el("view-settings").addEventListener("click", () => setView("settings"));
  el("revoke-all").addEventListener("click", doRevokeAll);

  el("update-check").addEventListener("click", checkUpdates);
  el("update-install").addEventListener("click", () => {
    state.updateConfirm = true;
    renderUpdate();
    el("update-confirm-install").focus();
  });
  el("update-cancel").addEventListener("click", () => {
    state.updateConfirm = false;
    renderUpdate();
    el("update-install").focus();
  });
  el("update-confirm-install").addEventListener("click", installUpdate);
  tsEnabledEl.addEventListener("change", toggleTailscale);
  settingsSaveEl.addEventListener("click", saveServerSettings);
  tsAuthCopyEl.addEventListener("click", () => {
    const url = state.settings && state.settings.tailnet && state.settings.tailnet.auth_url;
    if (!url) return;
    copy(url);
    tsAuthCopyEl.textContent = "Copied";
    setTimeout(() => { tsAuthCopyEl.textContent = "Copy link"; }, 1400);
  });

  document.addEventListener("keydown", (e) => {
    const typing = /^(INPUT|TEXTAREA)$/.test(document.activeElement.tagName);
    if (e.key === "/" && !typing) {
      e.preventDefault();
      searchEl.focus();
      searchEl.select();
    } else if (e.key === "Escape" && document.activeElement === searchEl) {
      searchEl.value = "";
      state.q = "";
      searchEl.blur();
      renderGroups();
    }
  });

  // ---- boot ----

  function boot() {
    loadPlans()
      .catch(() => { /* Initial failures are rendered by loadPlans. */ })
      .finally(() => {
        // The entrance animation is a one-time moment. Once it has played,
        // drop the class: filtering, expanding a log and ticking a status all
        // re-render these same containers, and replaying the entrance on every
        // one of those would flash the whole list on each keystroke.
        setTimeout(() => document.body.classList.remove("booting"), 800);
      });
  }

  renderGroups(); // paint the loading state before the first fetch settles
  boot();
  if (location.hash === "#settings") setView("settings");
})();
