const state = {
  apiBase: "",
  atomToken: "",
  apiKey: "",
  domain: "",
  tenant: null,
  health: null,
  version: null,
  status: null,
  secrets: [],
  models: [],
  connections: [],
  repos: [],
  repoTotal: 0,
  repoPage: 1,
  repoPerPage: 30,
  repoQuery: "",
  repoSort: "name",
  repoDirection: "asc",
  repoBranches: new Map(),
  contexts: [],
  selectedRepos: new Set(),
  chatSearchContext: "",
  chatId: "",
  chatMessages: [],
  rightRepo: "",
  rightRef: "",
  rightPath: "",
  rightLanguage: "",
  codeRows: [],
  monaco: null,
  monacoReady: null,
  codeEditor: null,
  codeModel: null,
  hoverProviderDisposable: null,
  monacoClickDisposable: null,
  monacoMoveDisposable: null,
  monacoLeaveDisposable: null,
  monacoHoverIndex: new Map(),
  symbolHoverCache: new Map(),
  codeDecorations: [],
  activeHoverKey: "",
  activities: [],
};

const $ = (id) => document.getElementById(id);

function saveState() {
  localStorage.setItem("codeintelAdminState", JSON.stringify({
    apiBase: state.apiBase,
    atomToken: state.atomToken,
    apiKey: state.apiKey,
    domain: state.domain,
    tenant: state.tenant,
    selectedRepos: Array.from(state.selectedRepos),
    chatSearchContext: state.chatSearchContext,
    repoPage: state.repoPage,
    repoPerPage: state.repoPerPage,
    repoQuery: state.repoQuery,
    repoSort: state.repoSort,
    repoDirection: state.repoDirection,
  }));
  updateConnectionPill();
}

function loadState() {
  try {
    const raw = JSON.parse(localStorage.getItem("codeintelAdminState") || "{}");
    Object.assign(state, raw);
    state.selectedRepos = new Set(raw.selectedRepos || []);
    state.chatSearchContext = raw.chatSearchContext || "";
    state.repoPage = raw.repoPage || 1;
    state.repoPerPage = raw.repoPerPage || 30;
    state.repoQuery = raw.repoQuery || "";
    state.repoSort = raw.repoSort || "name";
    state.repoDirection = raw.repoDirection || "asc";
  } catch {
    state.selectedRepos = new Set();
  }
  $("apiBase").value = state.apiBase || "";
  $("atomToken").value = state.atomToken || "";
  $("apiKey").value = state.apiKey || "";
  $("domain").value = state.domain || "";
  $("repoQuery").value = state.repoQuery || "";
  $("repoPerPage").value = String(state.repoPerPage || 30);
  $("repoSort").value = state.repoSort || "name";
  $("repoDirection").value = state.repoDirection || "asc";
  updateConnectionPill();
}

function updateConnectionPill() {
  const domain = state.domain || "no workspace";
  $("connectionPill").textContent = state.apiKey ? `${domain} connected` : `${domain} missing API key`;
  $("connectionPill").className = `connection-pill ${state.apiKey ? "good" : "warn"}`;
}

function recordActivity(title, detail = "", level = "info") {
  state.activities.unshift({
    title,
    detail: typeof detail === "string" ? detail : JSON.stringify(detail),
    level,
    at: new Date().toLocaleTimeString(),
  });
  state.activities = state.activities.slice(0, 40);
  renderActivity();
}

function apiPath(path) {
  if (!state.apiBase) {
    return path;
  }
  return state.apiBase.replace(/\/+$/, "") + path;
}

async function request(label, method, path, { body, atom = false, raw = false } = {}) {
  const headers = { "content-type": "application/json" };
  if (atom) {
    headers.Authorization = `Bearer ${state.atomToken}`;
  } else if (state.apiKey) {
    headers["X-Api-Key"] = state.apiKey;
  }
  const res = await fetch(apiPath(path), {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let parsed = text;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = text;
  }
  if (!res.ok) {
    const message = `${label} failed with HTTP ${res.status}: ${typeof parsed === "string" ? parsed : JSON.stringify(parsed)}`;
    recordActivity(label, message, "error");
    throw new Error(message);
  }
  return raw ? { status: res.status, body: parsed } : parsed;
}

async function requestWithMeta(label, method, path, { body } = {}) {
  const headers = { "content-type": "application/json" };
  if (state.apiKey) {
    headers["X-Api-Key"] = state.apiKey;
  }
  const res = await fetch(apiPath(path), {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let parsed = text;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = text;
  }
  if (!res.ok) {
    const message = `${label} failed with HTTP ${res.status}: ${typeof parsed === "string" ? parsed : JSON.stringify(parsed)}`;
    recordActivity(label, message, "error");
    throw new Error(message);
  }
  return { status: res.status, body: parsed, headers: res.headers };
}

function log(target, value) {
  target.textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2);
}

function setBusy(button, busy, label) {
  if (!button) {
    return;
  }
  if (busy) {
    button.dataset.originalText = button.textContent;
    button.textContent = label || "Working";
    button.disabled = true;
  } else {
    button.textContent = button.dataset.originalText || button.textContent;
    button.disabled = false;
  }
}

function branchRef(branch) {
  const b = (branch || "").trim();
  if (!b) {
    return "";
  }
  return b.startsWith("refs/") ? b : `refs/heads/${b}`;
}

function indexRef(branch) {
  return branchRef(branch);
}

function splitBranches(value) {
  return (value || "").split(",").map((v) => v.trim()).filter(Boolean);
}

function splitCSV(value) {
  return (value || "").split(",").map((v) => v.trim()).filter(Boolean);
}

function statusBadge(status, color) {
  const cls = color === "green" ? "good" : color === "yellow" ? "warn" : color === "red" ? "bad" : "";
  return `<span class="badge ${cls}">${escapeHtml(status || "unknown")}</span>`;
}

function escapeHtml(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function activateView(name) {
  document.querySelectorAll(".nav").forEach((button) => button.classList.toggle("active", button.dataset.view === name));
  document.querySelectorAll(".view").forEach((view) => view.classList.toggle("active", view.id === `view-${name}`));
}

function handleNav(name) {
  activateView(name);
  if (!state.apiKey) {
    return;
  }
  if (name === "repos") {
    loadRepos().catch((error) => recordActivity("Repos refresh failed", error.message, "error"));
  } else if (name === "chat" || name === "mcp") {
    refreshWorkspaceState({ quiet: true }).catch((error) => recordActivity("Workspace refresh failed", error.message, "error"));
  } else if (name === "vcs") {
    loadConnections().catch((error) => recordActivity("Connections refresh failed", error.message, "error"));
  } else if (name === "llm") {
    loadLlmConfig().catch((error) => recordActivity("LLM refresh failed", error.message, "error"));
  }
}

function renderActivity() {
  const root = $("activityLog");
  if (!root) {
    return;
  }
  if (!state.activities.length) {
    root.innerHTML = `<div class="muted">No activity yet.</div>`;
    return;
  }
  root.innerHTML = state.activities.map((item) => `
    <div class="activity-item ${item.level === "error" ? "error" : ""}">
      <div class="activity-title">${escapeHtml(item.title)}</div>
      <div class="activity-meta">${escapeHtml(item.at)}${item.detail ? ` / ${escapeHtml(item.detail)}` : ""}</div>
    </div>
  `).join("");
}

function renderOverview() {
  const status = state.status || {};
  const repos = status.repos || {};
  const connections = status.connections || {};
  const zoekt = status.zoekt || {};
  const kpis = [
    { label: "API", value: state.health ? "OK" : "N/A", detail: state.version?.version || "version unknown" },
    { label: "Repos", value: repos.total ?? state.repos.length ?? 0, detail: `${repos.indexed ?? indexedRepoCount()} indexed / ${summarizeCount(repos.indexingJobs)} active jobs` },
    { label: "Connections", value: connections.total ?? state.connections.length ?? 0, detail: `${connections.synced ?? syncedConnectionCount()} synced` },
    { label: "Models", value: state.models.length, detail: state.models.map((m) => m.displayName || m.model).join(", ") || "none configured" },
    { label: "Zoekt", value: zoekt.mode || "unknown", detail: zoekt.orgIndex ? `org index ${summarizeStatusValue(zoekt.orgIndex)}` : "no shard status" },
  ];
  $("overviewKpis").innerHTML = kpis.map((kpi) => `
    <div class="kpi">
      <div class="kpi-label">${escapeHtml(kpi.label)}</div>
      <div class="kpi-value">${escapeHtml(kpi.value)}</div>
      <div class="kpi-detail">${escapeHtml(kpi.detail)}</div>
    </div>
  `).join("");
  $("workspaceSummary").innerHTML = [
    ["Domain", state.domain || "not set"],
    ["Tenant", state.tenant?.workspaceId || state.tenant?.id || "not loaded"],
    ["API base", state.apiBase || "admin proxy"],
    ["Secrets", `${state.secrets.length} configured`],
    ["Search contexts", `${state.contexts.length} configured`],
    ["Selected repos", selectedRepoNames().join(", ") || "none"],
  ].map(([key, value]) => `
    <div class="summary-line">
      <div class="summary-key">${escapeHtml(key)}</div>
      <div class="summary-value">${escapeHtml(value)}</div>
    </div>
  `).join("");
  renderActivity();
}

function summarizeCount(value) {
  if (typeof value === "number") {
    return value;
  }
  if (Array.isArray(value)) {
    return value.length;
  }
  if (value && typeof value === "object") {
    return value.count ?? value.total ?? Object.keys(value).length;
  }
  return 0;
}

function summarizeStatusValue(value) {
  if (value == null) {
    return "";
  }
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  if (Array.isArray(value)) {
    return `${value.length} entries`;
  }
  if (typeof value === "object") {
    return value.status || value.id || value.path || value.mode || `${Object.keys(value).length} fields`;
  }
  return String(value);
}

function indexedRepoCount() {
  return state.repos.filter((repo) => String(repo.indexStatus || "").toLowerCase().includes("indexed")).length;
}

function syncedConnectionCount() {
  return state.connections.filter((connection) => connection.syncedAt).length;
}

async function refreshWorkspaceState({ quiet = false } = {}) {
  if (!state.apiKey && !state.atomToken) {
    renderOverview();
    return;
  }
  const tasks = [
    ["health", () => request("health", "GET", "/api/health")],
    ["version", () => request("version", "GET", "/api/version")],
    ["status", () => request("status", "GET", "/api/status")],
    ["tenant", () => state.domain ? request("tenant metadata", "GET", `/api/tenants/${encodeURIComponent(state.domain)}/metadata`) : Promise.resolve(state.tenant)],
    ["secrets", () => request("secrets", "GET", "/api/secrets")],
    ["models", () => request("models", "GET", "/api/models")],
    ["connections", () => request("connections", "GET", "/api/connections")],
    ["repos", () => request("repos", "GET", "/api/repos?page=1&perPage=100")],
    ["contexts", () => request("search contexts", "GET", "/api/search-contexts")],
  ];
  const results = await Promise.allSettled(tasks.map(([, fn]) => fn()));
  results.forEach((result, index) => {
    const [key] = tasks[index];
    if (result.status === "fulfilled") {
      state[key] = result.value || (Array.isArray(state[key]) ? [] : null);
    }
  });
  renderOverview();
  renderSecrets();
  renderModels();
  renderConnections();
  renderProviderCards();
  renderRepos();
  renderRepoScope();
  renderContexts();
  renderChatSelectors();
  renderChatEvidenceStrip();
  if (!quiet) {
    recordActivity("Refreshed workspace state", state.domain || "current workspace");
  }
}

async function provisionWorkspace(event) {
  event.preventDefault();
  const button = event.submitter;
  setBusy(button, true, "Provisioning");
  try {
    state.atomToken = $("atomToken").value.trim();
    const body = {
      workspaceId: $("workspaceId").value.trim(),
      workspaceName: $("workspaceName").value.trim(),
      domain: $("workspaceDomain").value.trim(),
      createApiKey: $("createApiKey").checked,
      apiKeyName: "codeintel-admin-console",
    };
    const resp = await request("workspace provision", "POST", "/api/atom/workspaces", { body, atom: true });
    state.tenant = resp.tenant;
    state.domain = resp.tenant?.domain || body.domain;
    if (resp.apiKey) {
      state.apiKey = resp.apiKey;
    }
    $("apiKey").value = state.apiKey || "";
    $("domain").value = state.domain || "";
    log($("workspaceLog"), { request: body, response: { ...resp, apiKey: resp.apiKey ? "<redacted>" : undefined } });
    saveState();
    recordActivity("Workspace provisioned", `${state.domain} / ${body.workspaceId}`);
    await refreshWorkspaceState({ quiet: true });
  } catch (error) {
    log($("workspaceLog"), String(error.message || error));
  } finally {
    setBusy(button, false);
  }
}

async function loadConnections() {
  state.connections = await request("list connections", "GET", "/api/connections");
  renderConnections();
  renderProviderCards();
  renderOverview();
}

function workspaceId() {
  return state.tenant?.atomWorkspaceId || state.tenant?.atomWorkspaceID || state.tenant?.workspaceId || state.domain || "";
}

function providerConfig(provider, card) {
  const connectionId = card.querySelector('[data-role="atom-vcs-id"]').value.trim();
  const url = card.querySelector('[data-role="atom-vcs-url"]').value.trim();
  const repos = splitCSV(card.querySelector('[data-role="atom-vcs-repos"]').value);
  const groups = splitCSV(card.querySelector('[data-role="atom-vcs-groups"]').value);
  const branches = splitCSV(card.querySelector('[data-role="atom-vcs-branches"]').value);
  if (!connectionId) {
    throw new Error("Atom VCS connection id is required.");
  }
  const token = {
    atomVcsConnection: {
      workspaceId: workspaceId(),
      connectionId,
      provider,
      purpose: "repo-discovery",
    },
  };
  const base = {
    type: provider,
    token,
    url,
    revisions: { branches: branches.length ? branches : ["*"] },
    enforcePermissions: true,
    enforcePermissionsForPublicRepos: false,
  };
  if (provider === "github") {
    base.repos = repos;
    base.orgs = groups;
  } else if (provider === "gitlab") {
    base.projects = repos;
    base.groups = groups;
    if (!repos.length && !groups.length && !url.includes("gitlab.com")) {
      base.all = true;
    }
  } else if (provider === "bitbucket") {
    base.repos = repos;
    base.workspaces = groups;
    base.deploymentType = url.includes("bitbucket.org") ? "cloud" : "server";
    if (!repos.length && !groups.length && base.deploymentType === "server") {
      base.all = true;
    }
  }
  return { connectionId, config: base };
}

async function saveAtomProviderConnection(card) {
  const provider = card.dataset.provider;
  const { connectionId, config } = providerConfig(provider, card);
  const body = {
    name: `${provider}-atom-${connectionId}`,
    config,
    sync: true,
  };
  const resp = await request("save Atom VCS provider", "POST", "/api/connections", { body });
  recordActivity("Atom VCS connection saved", `${provider} / ${connectionId}`);
  await loadConnections();
  await loadRepos();
  return resp;
}

function renderProviderCards() {
  document.querySelectorAll(".provider-card").forEach((card) => {
    const provider = card.dataset.provider;
    const match = state.connections.find((connection) => {
      const cfg = connection.config || {};
      return cfg.type === provider && cfg.token?.atomVcsConnection;
    });
    card.classList.toggle("connected", Boolean(match));
    const existing = card.querySelector(".provider-state");
    if (existing) {
      existing.remove();
    }
    const stateLine = document.createElement("div");
    stateLine.className = "provider-state";
    stateLine.textContent = match
      ? `Connected via Atom id ${match.config.token.atomVcsConnection.connectionId || "redacted"}`
      : "Ready for Atom connection id.";
    card.appendChild(stateLine);
  });
}

function renderConnections() {
  const root = $("connectionList");
  if (!state.connections.length) {
    root.innerHTML = `<div class="row">No connections yet.</div>`;
    return;
  }
  root.innerHTML = state.connections.map((c) => `
    <div class="row">
      <div class="row-head">
        <div class="row-title">${escapeHtml(c.name)}</div>
        ${statusBadge(c.syncedAt ? "synced" : "not synced", c.syncedAt ? "green" : "gray")}
      </div>
      <div class="muted">${escapeHtml(c.connectionType)} / id ${c.id}</div>
      <div class="row-actions">
        <button data-action="sync-connection" data-id="${c.id}">Sync</button>
        <button data-action="connection-status" data-id="${c.id}">Status</button>
      </div>
      <pre class="log" data-connection-log="${escapeHtml(c.id)}"></pre>
    </div>
  `).join("");
}

async function saveConnection(event) {
  event.preventDefault();
  const button = event.submitter;
  setBusy(button, true, "Saving");
  try {
    const branches = splitBranches($("connectionBranches").value);
    const body = {
      name: $("connectionName").value.trim(),
      config: {
        type: "git",
        url: $("connectionUrl").value.trim(),
        revisions: { branches: branches.length ? branches : ["*"] },
      },
      sync: $("connectionSync").checked,
    };
    await request("save connection", "POST", "/api/connections", { body });
    recordActivity("Connection saved", body.name);
    await loadConnections();
    if (body.sync) {
      await loadRepos();
    }
  } finally {
    setBusy(button, false);
  }
}

async function syncConnection(id) {
  const resp = await request("sync connection", "POST", `/api/connections/${id}/sync`);
  recordActivity(`Connection ${id} sync queued`, resp.jobId || "job accepted");
  await loadConnectionStatus(id, resp.jobId);
  await loadRepos();
}

async function loadConnectionStatus(id, jobId) {
  const status = await request("connection status", "GET", `/api/connections/${id}/status`);
  recordActivity(`Connection ${id} status`, `latest=${status.latestJob?.status || "none"} job=${jobId || status.latestJob?.id || "none"} repos=${status.repoCount}`);
  const target = document.querySelector(`[data-connection-log="${CSS.escape(String(id))}"]`);
  if (target) {
    log(target, status);
  }
  return status;
}

async function loadRepos() {
  const query = new URLSearchParams({
    page: String(state.repoPage || 1),
    perPage: String(state.repoPerPage || 30),
    sort: state.repoSort || "name",
    direction: state.repoDirection || "asc",
  });
  if (state.repoQuery) {
    query.set("query", state.repoQuery);
  }
  const [repos, contexts] = await Promise.all([
    requestWithMeta("list repos", "GET", `/api/repos?${query.toString()}`),
    request("search contexts", "GET", "/api/search-contexts").catch(() => state.contexts),
  ]);
  state.repos = repos.body || [];
  state.repoTotal = parseTotalCount(repos.headers, state.repos.length);
  state.contexts = contexts || [];
  await loadVisibleBranchStatuses();
  renderRepos();
  renderRepoScope();
  renderContexts();
  renderChatSelectors();
  renderChatEvidenceStrip();
  renderOverview();
}

function parseTotalCount(headers, fallback) {
  const raw = headers?.get?.("x-total-count") || headers?.get?.("X-Total-Count") || "";
  const total = Number(raw);
  if (Number.isFinite(total) && total >= 0) {
    return total;
  }
  return fallback || 0;
}

async function loadVisibleBranchStatuses() {
  const entries = await Promise.allSettled(state.repos.map((repo) => request("repo branches", "GET", `/api/repos/${repo.repoId}/branches`)));
  entries.forEach((entry, index) => {
    if (entry.status === "fulfilled") {
      state.repoBranches.set(String(state.repos[index].repoId), entry.value);
    }
  });
}

async function loadLlmConfig() {
  const [secrets, models] = await Promise.all([
    request("secrets", "GET", "/api/secrets"),
    request("models", "GET", "/api/models"),
  ]);
  state.secrets = secrets || [];
  state.models = models || [];
  renderSecrets();
  renderModels();
  renderChatSelectors();
  renderOverview();
}

function renderSecrets() {
  const root = $("secretList");
  if (!root) {
    return;
  }
  if (!state.secrets.length) {
    root.innerHTML = `<div class="row compact"><div class="muted">No workspace secrets configured.</div></div>`;
    return;
  }
  root.innerHTML = state.secrets.map((secret) => `
    <div class="row compact">
      <div class="row-head">
        <div class="row-title">${escapeHtml(secret.key)}</div>
        <button data-action="delete-secret" data-key="${escapeHtml(secret.key)}">Delete</button>
      </div>
      <div class="muted">ref ${escapeHtml(secret.ref?.secretRef || secret.key)} / updated ${escapeHtml(secret.updatedAt || "")}</div>
    </div>
  `).join("");
}

function renderModels() {
  const root = $("modelList");
  if (!root) {
    return;
  }
  if (!state.models.length) {
    root.innerHTML = `<div class="row compact"><div class="muted">No workspace language models configured.</div></div>`;
    return;
  }
  root.innerHTML = state.models.map((model) => `
    <div class="row compact">
      <div class="row-head">
        <div class="row-title">${escapeHtml(model.displayName || model.model)}</div>
        ${statusBadge(model.provider, "green")}
      </div>
      <div class="muted">${escapeHtml(model.provider)} / ${escapeHtml(model.model)}</div>
    </div>
  `).join("");
}

async function saveSecret(event) {
  event.preventDefault();
  const button = event.submitter;
  setBusy(button, true, "Saving");
  const body = { key: $("secretKey").value.trim(), value: $("secretValue").value };
  try {
    const resp = await request("save secret", "PUT", "/api/secrets", { body });
    $("secretValue").value = "";
    log($("llmLog"), { request: { ...body, value: "<redacted>" }, response: resp });
    recordActivity("Secret saved", body.key);
    await loadLlmConfig();
  } catch (error) {
    log($("llmLog"), String(error.message || error));
  } finally {
    setBusy(button, false);
  }
}

async function deleteSecret(key) {
  const resp = await request("delete secret", "DELETE", `/api/secrets/${encodeURIComponent(key)}`);
  log($("llmLog"), { request: { key }, response: resp });
  recordActivity("Secret deleted", key);
  await loadLlmConfig();
}

async function saveModel(event) {
  event.preventDefault();
  const button = event.submitter;
  setBusy(button, true, "Saving");
  const model = {
    provider: $("modelProvider").value.trim(),
    model: $("modelName").value.trim(),
    displayName: $("modelDisplayName").value.trim() || undefined,
    baseUrl: $("modelBaseUrl").value.trim() || undefined,
    token: $("modelSecretRef").value.trim() ? { secretRef: $("modelSecretRef").value.trim() } : undefined,
  };
  try {
    const preserved = state.models
      .filter((existing) => !(existing.provider === model.provider && existing.model === model.model))
      .map((existing) => ({ provider: existing.provider, model: existing.model, displayName: existing.displayName || undefined }));
    const models = [...preserved, model];
    const resp = await request("save models", "PUT", "/api/models", { body: { models } });
    log($("llmLog"), { request: { models: models.map((m) => ({ ...m, token: m.token ? { secretRef: m.token.secretRef } : undefined })) }, response: resp });
    recordActivity("Language model configured", `${model.provider}/${model.model}`);
    state.models = resp || [];
    renderModels();
    renderChatSelectors();
    renderOverview();
  } catch (error) {
    log($("llmLog"), String(error.message || error));
  } finally {
    setBusy(button, false);
  }
}

function repoDefaultBranch(repo) {
  const revs = repo.indexedRevisions || [];
  if (revs.length) {
    return revs[0].replace(/^refs\/heads\//, "");
  }
  return repo.defaultBranch || "";
}

function renderRepos() {
  const root = $("repoList");
  renderRepoPageMeta();
  if (!state.repos.length) {
    root.innerHTML = `<div class="repo-card">No repos. Sync a connection first.</div>`;
    return;
  }
  root.innerHTML = state.repos.map((repo) => {
    const branch = repoDefaultBranch(repo);
    const branchInfo = state.repoBranches.get(String(repo.repoId));
    const normalizedStatus = String(repo.indexStatus || "").toLowerCase();
    const cardClass = normalizedStatus.includes("indexing") || normalizedStatus.includes("progress")
      ? "indexing"
      : normalizedStatus.includes("fail") || normalizedStatus.includes("error")
        ? "failed"
        : normalizedStatus.includes("indexed") || normalizedStatus.includes("ready")
          ? "indexed"
          : "";
    return `
      <div class="repo-card ${cardClass}" data-repo-id="${repo.repoId}">
        <div class="repo-head">
          <div>
            <div class="repo-name">${escapeHtml(repo.repoName)}</div>
            <div class="muted">repoId ${repo.repoId} / ${escapeHtml((repo.indexedRevisions || []).join(", ") || "no indexed branches")}</div>
          </div>
          ${statusBadge(repo.indexStatus, repo.indexStatusColor)}
        </div>
        <div class="repo-controls">
          <label>Branch or branch list <input data-role="branch" value="${escapeHtml(branch)}" placeholder="main or main,release"></label>
          <label>Branch policy <input data-role="policy" value="${escapeHtml((repo.indexedRevisions || []).map((v) => v.replace(/^refs\/heads\//, "")).join(", "))}" placeholder="main, release/* or *"></label>
        </div>
        <div class="repo-actions">
          <label class="scope-chip"><input type="checkbox" data-action="select-chat-repo" data-name="${escapeHtml(repo.repoName)}" ${state.selectedRepos.has(repo.repoName) ? "checked" : ""}> Chat scope</label>
          <button data-action="repo-status" data-id="${repo.repoId}">Status</button>
          <button data-action="repo-branches" data-id="${repo.repoId}">Branches</button>
          <button data-action="save-repo-branches" data-id="${repo.repoId}">Save Policy</button>
          <button data-action="index-repo" data-id="${repo.repoId}">Index/Reindex</button>
          <button data-action="remove-index" data-id="${repo.repoId}">Remove Index</button>
        </div>
        ${renderBranchStatusTable(branchInfo)}
        <pre class="log" data-role="repo-log"></pre>
      </div>
    `;
  }).join("");
}

function renderRepoPageMeta() {
  const total = state.repoTotal || 0;
  const perPage = state.repoPerPage || 30;
  const page = state.repoPage || 1;
  const pages = Math.max(1, Math.ceil(total / perPage));
  $("repoPageMeta").textContent = `Page ${page} of ${pages} / ${total} repos`;
  $("repoPrevPage").disabled = page <= 1;
  $("repoNextPage").disabled = page >= pages;
}

function branchStatusRows(branchInfo) {
  const rows = Array.isArray(branchInfo?.branchStatuses) ? [...branchInfo.branchStatuses] : [];
  if (!rows.length && branchInfo?.branchStatus) {
    rows.push(branchInfo.branchStatus);
  }
  return rows;
}

function renderBranchStatusTable(branchInfo) {
  const rows = branchStatusRows(branchInfo);
  if (!rows.length) {
    return `<div class="branch-table muted">No branch status rows loaded.</div>`;
  }
  return `
    <div class="branch-table">
      ${rows.slice(0, 8).map((branch) => `
        <div class="branch-row">
          <span>${escapeHtml(branch.branch || branch.ref || "default")}</span>
          ${statusBadge(branch.status || branch.indexStatus || "unknown", branch.color || branch.statusColor || branch.indexStatusColor)}
          <span class="muted">${escapeHtml(branch.commitHash || branch.revision || "")}</span>
        </div>
      `).join("")}
    </div>
  `;
}

async function loadRepoBranches(card) {
  const repoId = card.dataset.repoId;
  const branch = card.querySelector('[data-role="branch"]').value.trim();
  const path = `/api/repos/${repoId}/branches${branch ? `?branch=${encodeURIComponent(branch)}` : ""}`;
  const resp = await request("repo branches", "GET", path);
  state.repoBranches.set(String(repoId), resp);
  log(card.querySelector('[data-role="repo-log"]'), resp);
  renderRepos();
  return resp;
}

async function repoStatus(card) {
  const repoId = card.dataset.repoId;
  const branch = card.querySelector('[data-role="branch"]').value.trim();
  const status = await request("repo status", "GET", `/api/repos/${repoId}/status${branch ? `?branch=${encodeURIComponent(branch)}` : ""}`);
  log(card.querySelector('[data-role="repo-log"]'), status);
  return status;
}

async function saveRepoBranches(card) {
  const repoId = card.dataset.repoId;
  const branches = splitBranches(card.querySelector('[data-role="policy"]').value);
  const body = branches.includes("*") ? { mode: "all", sync: false } : { mode: "patterns", branches, sync: false };
  if (!branches.length) {
    body.mode = "default";
    delete body.branches;
  }
  const resp = await request("save repo branches", "PUT", `/api/repos/${repoId}/branches`, { body });
  log(card.querySelector('[data-role="repo-log"]'), resp);
  recordActivity(`Repo ${repoId} branch policy saved`, JSON.stringify(body));
  await loadRepos();
}

async function indexRepo(card) {
  const repoId = card.dataset.repoId;
  const branches = splitBranches(card.querySelector('[data-role="branch"]').value);
  const out = [];
  for (const branch of branches.length ? branches : [""]) {
    const resp = await request("index repo", "POST", `/api/repos/${repoId}/index`, { body: branch ? { ref: indexRef(branch) } : {} });
    out.push({ branch: branch || "default", jobId: resp.jobId });
  }
  log(card.querySelector('[data-role="repo-log"]'), out);
  recordActivity(`Repo ${repoId} index queued`, out.map((item) => `${item.branch}:${item.jobId}`).join(", "));
  pollRepoUntilQuiet(card);
}

async function removeIndex(card) {
  const repoId = card.dataset.repoId;
  const branches = splitBranches(card.querySelector('[data-role="branch"]').value);
  const out = [];
  for (const branch of branches.length ? branches : [""]) {
    const path = `/api/repos/${repoId}/index${branch ? `?ref=${encodeURIComponent(branchRef(branch))}` : ""}`;
    const resp = await request("remove index", "DELETE", path);
    out.push({ branch: branch || "all", jobId: resp.jobId });
  }
  log(card.querySelector('[data-role="repo-log"]'), out);
  recordActivity(`Repo ${repoId} remove-index queued`, out.map((item) => `${item.branch}:${item.jobId}`).join(", "));
  pollRepoUntilQuiet(card);
}

async function pollRepoUntilQuiet(card) {
  for (let i = 0; i < 60; i += 1) {
    const status = await repoStatus(card);
    if (!["PENDING", "IN_PROGRESS"].includes(status.latestJob?.status || "")) {
      await loadRepos();
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 3000));
  }
}

function renderRepoScope() {
  const root = $("chatRepoScope");
  if (!state.repos.length) {
    root.innerHTML = `<span class="muted">No repos loaded.</span>`;
    return;
  }
  const selected = selectedReposForChatStart();
  const preview = selected.slice(0, 4).map((repo) => repo.split("/").slice(-2).join("/"));
  const summary = selected.length
    ? `${selected.length} repos: ${preview.join(", ")}${selected.length > preview.length ? ` +${selected.length - preview.length}` : ""}`
    : "No repo scope selected";
  root.innerHTML = `
    <details class="scope-details">
      <summary>${escapeHtml(summary)}</summary>
      <div class="scope-chip-grid">
        ${state.repos.map((repo) => `
          <label class="scope-chip">
            <input type="checkbox" data-action="select-chat-repo" data-name="${escapeHtml(repo.repoName)}" ${state.selectedRepos.has(repo.repoName) ? "checked" : ""}>
            ${escapeHtml(repo.repoName.split("/").slice(-2).join("/"))}
          </label>
        `).join("")}
      </div>
    </details>
  `;
}

function renderContexts() {
  const root = $("contextList");
  if (!root) {
    return;
  }
  if (!state.contexts.length) {
    root.innerHTML = `<div class="row compact"><div class="muted">No search contexts configured.</div></div>`;
    return;
  }
  root.innerHTML = state.contexts.map((context) => `
    <div class="row compact">
      <div class="row-head">
        <div class="row-title">${escapeHtml(context.name)}</div>
        ${statusBadge(`${context.repoNames?.length || 0} repos`, context.repoNames?.length ? "green" : "gray")}
      </div>
      <div class="muted">${escapeHtml(context.description || "no description")}</div>
      <div class="muted">${escapeHtml((context.repoNames || []).join(", "))}</div>
    </div>
  `).join("");
}

async function loadSearchContexts() {
  state.contexts = await request("search contexts", "GET", "/api/search-contexts");
  renderContexts();
  renderChatSelectors();
  renderChatEvidenceStrip();
  renderOverview();
}

async function saveSelectedSearchContext(event) {
  event.preventDefault();
  const name = $("contextName").value.trim();
  const description = $("contextDescription").value.trim();
  const repos = selectedRepoNames();
  if (!name || !repos.length) {
    recordActivity("Search context not saved", "Provide a name and select at least one repo.", "error");
    return;
  }
  const existing = state.contexts.filter((context) => context.name !== name).map((context) => ({
    name: context.name,
    description: context.description || undefined,
    include: context.repoNames || [],
  }));
  const contexts = [...existing, { name, description, include: repos }];
  const resp = await request("save search contexts", "PUT", "/api/search-contexts", { body: { contexts } });
  recordActivity("Search context saved", `${name} (${repos.length} repos)`);
  await loadSearchContexts();
  return resp;
}

function selectedRepoNames() {
  return Array.from(state.selectedRepos);
}

function selectedRef() {
  const first = state.repos.find((repo) => state.selectedRepos.has(repo.repoName));
  return first ? repoDefaultBranch(first) : "";
}

function renderChatSelectors() {
  const modelSelect = $("chatConfiguredModel");
  const contextSelect = $("chatSearchContext");
  if (!modelSelect || !contextSelect) {
    return;
  }
  const currentContext = contextSelect.value || state.chatSearchContext || "";
  modelSelect.innerHTML = [
    `<option value="">Manual provider/model</option>`,
    ...state.models.map((model, index) => {
      const value = `${model.provider}::${model.model}`;
      const label = model.displayName || `${model.provider}/${model.model}`;
      return `<option value="${escapeHtml(value)}" ${index === 0 ? "selected" : ""}>${escapeHtml(label)}</option>`;
    }),
  ].join("");
  contextSelect.innerHTML = [
    `<option value="" ${currentContext ? "" : "selected"}>Selected repos</option>`,
    ...state.contexts.map((context) => `<option value="${escapeHtml(context.name)}" ${context.name === currentContext ? "selected" : ""}>${escapeHtml(context.name)} (${context.repoNames?.length || 0})</option>`),
  ].join("");
  const first = state.models[0];
  if (first && !$("chatProvider").value.trim() && !$("chatModel").value.trim()) {
    $("chatProvider").value = first.provider || "openai-compatible";
    $("chatModel").value = first.model || "";
  } else if (first) {
    $("chatProvider").value = first.provider || $("chatProvider").value;
    $("chatModel").value = first.model || $("chatModel").value;
  }
  $("chatIdDisplay").value = state.chatId || "";
  renderChatEvidenceStrip();
}

function applyConfiguredModel() {
  const value = $("chatConfiguredModel").value;
  if (!value) {
    return;
  }
  const [provider, model] = value.split("::");
  $("chatProvider").value = provider || "";
  $("chatModel").value = model || "";
}

function selectedSearchScopes() {
  const context = $("chatSearchContext")?.value || state.chatSearchContext || "";
  if (context) {
    return [{ type: "reposet", value: context }];
  }
  return selectedRepoNames().map((repo) => ({ type: "repo", value: repo }));
}

function selectedReposForChatStart() {
  const context = $("chatSearchContext")?.value || state.chatSearchContext || "";
  if (context) {
    const row = state.contexts.find((item) => item.name === context);
    return row?.repoNames || [];
  }
  return selectedRepoNames();
}

function renderChatEvidenceStrip() {
  const root = $("chatEvidenceStrip");
  if (!root) {
    return;
  }
  const selected = selectedReposForChatStart();
  const indexed = state.repos.filter((repo) => selected.includes(repo.repoName) && String(repo.indexStatus || "").toLowerCase().includes("indexed")).length;
  const context = $("chatSearchContext")?.value || state.chatSearchContext || "";
  const pills = [
    { label: context ? `${context} (${selected.length})` : `${selected.length} repo scopes`, good: selected.length > 0 },
    { label: `${indexed} indexed`, good: selected.length > 0 && indexed === selected.length },
    { label: `${state.contexts.length} contexts`, good: state.contexts.length > 0 },
    { label: `${state.models.length} models`, good: state.models.length > 0 },
    { label: state.chatId ? `chat ${state.chatId}` : "new chat", good: Boolean(state.chatId) },
  ];
  root.innerHTML = pills.map((pill) => `<span class="evidence-pill ${pill.good ? "good" : "warn"}">${escapeHtml(pill.label)}</span>`).join("");
}

async function sendChat(event) {
  event.preventDefault();
  const query = $("chatInput").value.trim();
  if (!query) {
    return;
  }
  $("chatInput").value = "";
  state.chatMessages.push({ role: "user", content: query });
  renderMessages();
  try {
    const languageModel = {
      provider: $("chatProvider").value.trim(),
      model: $("chatModel").value.trim(),
    };
    const repos = selectedReposForChatStart();
    const scopes = selectedSearchScopes();
    const response = state.chatId
      ? await request("chat follow-up", "POST", "/api/chat", {
          body: {
            id: state.chatId,
            query,
            selectedSearchScopes: scopes,
            languageModel,
            answerBudget: $("answerBudget").value,
          },
        })
      : await request("chat start", "POST", "/api/chat/blocking", {
          body: {
            query,
            repos,
            languageModel,
            visibility: "PRIVATE",
            answerBudget: $("answerBudget").value,
          },
        });
    const final = await resolveChat(response);
    state.chatId = final.chatId || state.chatId;
    $("chatIdDisplay").value = state.chatId || "";
    state.chatMessages.push({ role: "assistant", content: final.answer || final.error || JSON.stringify(final, null, 2), trace: final.toolTrace });
    renderChatEvidenceStrip();
    recordActivity("Chat response received", `${state.chatId || "new"} / ${String(final.status || "done")}`);
  } catch (error) {
    state.chatMessages.push({ role: "assistant", content: `Request failed: ${error.message || error}` });
  }
  renderMessages();
}

async function resolveChat(resp) {
  if (!["IN_PROGRESS", "PENDING", "QUEUED"].includes(resp.status || "")) {
    return resp;
  }
  for (let i = 0; i < 90; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 4000));
    const poll = await request("chat result", "GET", `/api/chat/${resp.chatId}/result?sessionId=${encodeURIComponent(resp.sessionId)}`);
    if (!["IN_PROGRESS", "PENDING", "QUEUED"].includes(poll.status || "")) {
      return poll;
    }
  }
  return { ...resp, error: "Timed out waiting for chat result." };
}

function knownRepoNames() {
  return state.repos.map((repo) => repo.repoName).sort((a, b) => b.length - a.length);
}

function guessRepoForPath(path, currentRepo) {
  if (currentRepo) {
    return currentRepo;
  }
  const repos = knownRepoNames();
  if (path.startsWith("internal/instrumentation/") || path.startsWith("internal/webhook/") || path.startsWith("config/webhook/") || path === "main.go") {
    const operator = repos.find((repo) => repo.endsWith("/opentelemetry-operator"));
    if (operator) {
      return operator;
    }
  }
  return selectedRepoNames()[0] || repos[0] || "";
}

function fileHref({ repo, ref, path, line }) {
  return `#file:${encodeURIComponent(JSON.stringify({ repo, ref, path, line: Number(line || 1) }))}`;
}

function basename(path) {
  return String(path || "").split("/").pop();
}

function dirname(path) {
  const parts = String(path || "").split("/");
  parts.pop();
  return parts.join("/");
}

function resolveContextPath(path, currentFilePath) {
  const clean = String(path || "").trim();
  if (!clean || clean.includes("/") || !currentFilePath) {
    return clean;
  }
  if (basename(currentFilePath) === clean) {
    return currentFilePath;
  }
  const dir = dirname(currentFilePath);
  if (dir && clean !== "main.go") {
    return `${dir}/${clean}`;
  }
  return clean;
}

function fullPathFromLine(line) {
  const match = line.match(/\b(?:File|file):\**\s*`?((?:[A-Za-z0-9_.-]+\/)+[A-Za-z0-9_.-]+\.(?:go|ts|tsx|js|jsx|py|java|rs|cs|fs|vb|proto|yaml|yml|json|md|xml|gradle|toml|sln|csproj|fsproj|vbproj|rb|dart|cpp|cc|c|h|hpp))`?/);
  if (match) {
    return match[1];
  }
  const anyPath = line.match(/(^|[\s│`])((?:[A-Za-z0-9_.-]+\/)+[A-Za-z0-9_.-]+\.(?:go|ts|tsx|js|jsx|py|java|rs|cs|fs|vb|proto|yaml|yml|json|md|xml|gradle|toml|sln|csproj|fsproj|vbproj|rb|dart|cpp|cc|c|h|hpp))(?=[\s│`),.:]|$)/);
  if (anyPath) {
    return anyPath[2];
  }
  return "";
}

function rewriteExistingFileLinks(line, currentRepo, currentFilePath) {
  return line.replace(/#file:([A-Za-z0-9%._~!$&'()*+,;=:@/-]+)/g, (match, encoded) => {
    try {
      const ref = JSON.parse(decodeURIComponent(encoded));
      const path = resolveContextPath(ref.path, currentFilePath);
      const repo = guessRepoForPath(path, ref.repo || currentRepo);
      const revision = ref.ref || repoDefaultBranch(state.repos.find((r) => r.repoName === repo) || {}) || "main";
      return fileHref({ repo, ref: revision, path, line: ref.line });
    } catch {
      return match;
    }
  });
}

function preprocessFileRefs(text) {
  const repos = knownRepoNames();
  let currentRepo = "";
  let currentFilePath = "";
  return (text || "").split(/\r?\n/).map((line) => {
    for (const repo of repos) {
      if (line.includes(repo)) {
        currentRepo = repo;
        break;
      }
    }
    const nextFullPath = fullPathFromLine(line);
    if (nextFullPath) {
      currentFilePath = nextFullPath;
    }
    const rewritten = rewriteExistingFileLinks(line, currentRepo, currentFilePath);
    return rewritten.replace(/(^|[\s(`])((?:[A-Za-z0-9_.-]+\/)*[A-Za-z0-9_.-]+\.(?:go|ts|tsx|js|jsx|py|java|rs|cs|fs|vb|proto|yaml|yml|json|md|xml|gradle|toml|sln|csproj|fsproj|vbproj|rb|dart|cpp|cc|c|h|hpp)):(\d+)(?:-\d+)?(?=[:)\]`\s,.]|$)/g, (m, prefix, path, lineNo) => {
      const resolvedPath = resolveContextPath(path, currentFilePath);
      const repo = guessRepoForPath(resolvedPath, currentRepo);
      const ref = repoDefaultBranch(state.repos.find((r) => r.repoName === repo) || {}) || "main";
      return `${prefix}[${path}:${lineNo}](${fileHref({ repo, ref, path: resolvedPath, line: lineNo })})`;
    });
  }).join("\n");
}

function renderMarkdown(text) {
  const input = preprocessFileRefs(text || "");
  if (window.markdownit) {
    return window.markdownit({ html: false, linkify: true, breaks: true }).render(input);
  }
  return `<pre>${escapeHtml(input)}</pre>`;
}

function renderMessages() {
  const root = $("messages");
  root.innerHTML = state.chatMessages.map((msg) => `
    <div class="message ${msg.role}">
      <div class="role">${escapeHtml(msg.role)}</div>
      <div class="md">${renderMarkdown(msg.content)}</div>
    </div>
  `).join("");
  root.querySelectorAll('a[href^="#file:"]').forEach((link) => {
    link.classList.add("file-ref");
    link.addEventListener("click", (event) => {
      event.preventDefault();
      const raw = link.getAttribute("href").replace(/^#file:/, "");
      try {
        const ref = JSON.parse(decodeURIComponent(raw));
        openFileReference(ref.path, Number(ref.line), ref.repo, ref.ref);
      } catch {
        const [, encodedPath, line] = link.getAttribute("href").match(/^#file:([^:]+):(\d+)/) || [];
        if (encodedPath) {
          openFileReference(decodeURIComponent(encodedPath), Number(line));
        }
      }
    });
  });
  renderMermaid(root);
  root.scrollTop = root.scrollHeight;
}

function renderMermaid(root) {
  if (!window.mermaid) {
    return;
  }
  window.mermaid.initialize({ startOnLoad: false, securityLevel: "strict" });
  root.querySelectorAll("pre code.language-mermaid").forEach((code, index) => {
    const div = document.createElement("div");
    div.className = "mermaid";
    div.textContent = code.textContent;
    div.id = `mermaid-${Date.now()}-${index}`;
    code.closest("pre").replaceWith(div);
  });
  window.mermaid.run({ nodes: root.querySelectorAll(".mermaid") }).catch(() => {});
}

async function openFileReference(path, line, repoOverride = "", refOverride = "") {
  const repo = repoOverride || selectedRepoNames()[0] || state.rightRepo || state.repos[0]?.repoName;
  const ref = refOverride || selectedRef() || state.rightRef || repoDefaultBranch(state.repos.find((r) => r.repoName === repo) || {});
  if (!repo) {
    $("codeMeta").textContent = "Select a repo before opening file references.";
    return;
  }
  const text = await readFullFile(repo, path, ref);
  state.rightRepo = repo;
  state.rightRef = ref;
  state.rightPath = path;
  $("codeMeta").textContent = `${repo} / ${ref || "default"} / ${path}:${line}`;
  await renderCode(text, { repo, ref, path, highlightLine: line });
}

async function readFullFile(repo, path, ref) {
  const chunks = [];
  let offset = 1;
  let lastLine = 0;
  for (let i = 0; i < 32; i += 1) {
    const chunk = await mcpTool("read_file", { repo, path, ref, offset, limit: 800 });
    const normalized = normalizeReadFileText(chunk, path);
    if (!normalized.rows.length) {
      chunks.push(chunk);
      break;
    }
    chunks.push(chunk);
    const maxLine = Math.max(...normalized.rows.map((row) => row.number));
    if (normalized.rows.length < 800 || maxLine <= lastLine) {
      break;
    }
    lastLine = maxLine;
    offset = maxLine + 1;
  }
  return chunks.join("\n");
}

function extractToolText(body) {
  const values = Array.isArray(body) ? body : [body];
  const texts = [];
  const visit = (value) => {
    if (Array.isArray(value)) {
      value.forEach(visit);
      return;
    }
    if (!value || typeof value !== "object") {
      return;
    }
    const content = value.result?.content;
    if (Array.isArray(content)) {
      for (const item of content) {
        if (item.text) {
          texts.push(item.text);
        }
      }
    }
  };
  values.forEach(visit);
  return texts.join("\n") || JSON.stringify(body, null, 2);
}

async function mcpTool(name, args) {
  const id = `admin-${Date.now()}`;
  const body = { jsonrpc: "2.0", id, method: "tools/call", params: { name, arguments: args } };
  const resp = await request("mcp tool", "POST", `/api/${state.domain}/mcp`, { body, raw: true });
  return extractToolText(resp.body);
}

function detectLanguage(path, content = "") {
  const lower = String(path || "").toLowerCase();
  const ext = lower.split(".").pop();
  const map = {
    go: "go", ts: "typescript", tsx: "tsx", js: "javascript", jsx: "jsx", py: "python",
    java: "java", rs: "rust", cs: "csharp", fs: "fsharp", vb: "vb", proto: "proto",
    yaml: "yaml", yml: "yaml", json: "json", xml: "xml", gradle: "gradle", toml: "toml",
    rb: "ruby", dart: "dart", cpp: "cpp", cc: "cpp", c: "c", h: "c", hpp: "cpp",
    md: "markdown", sql: "sql", sh: "shell", bash: "shell", Dockerfile: "dockerfile",
  };
  if (lower.endsWith("dockerfile") || lower.includes("/dockerfile")) {
    return "dockerfile";
  }
  if (map[ext]) {
    return map[ext];
  }
  const trimmed = content.trim();
  if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
    return "json";
  }
  if (trimmed.startsWith("<")) {
    return "xml";
  }
  return "text";
}

function monacoLanguage(language, path = "") {
  const lower = String(path || "").toLowerCase();
  if (lower.endsWith("dockerfile") || lower.includes("/dockerfile")) {
    return "dockerfile";
  }
  const map = {
    go: "go",
    typescript: "typescript",
    tsx: "typescript",
    javascript: "javascript",
    jsx: "javascript",
    python: "python",
    java: "java",
    rust: "rust",
    csharp: "csharp",
    fsharp: "fsharp",
    vb: "vb",
    proto: "proto",
    yaml: "yaml",
    json: "json",
    xml: "xml",
    gradle: "java",
    toml: "ini",
    ruby: "ruby",
    dart: "dart",
    cpp: "cpp",
    c: "c",
    markdown: "markdown",
    sql: "sql",
    shell: "shell",
    dockerfile: "dockerfile",
  };
  return map[language] || "plaintext";
}

function normalizeReadFileText(text, fallbackPath) {
  const raw = String(text || "").replace(/<\/?content>/g, "");
  const pathMatch = raw.match(/<path>([^<]+)<\/path>/);
  const path = pathMatch?.[1] || fallbackPath || "";
  const rows = [];
  let sawNumbered = false;
  for (const rawLine of raw.split(/\r?\n/)) {
    const line = rawLine.replace(/<\/?repo[^>]*>/g, "");
    if (!line.trim() || line.trim() === state.rightRepo || /^<path>/.test(line.trim())) {
      continue;
    }
    const numbered = line.match(/^\s*(\d+):\s?(.*)$/);
    if (numbered) {
      sawNumbered = true;
      rows.push({ number: Number(numbered[1]), text: numbered[2] });
      continue;
    }
    if (!sawNumbered && !/^github\.com\//.test(line.trim())) {
      rows.push({ number: rows.length + 1, text: line });
    }
  }
  return { path, rows };
}

function lineTextAt(lineNumber) {
  const row = state.codeRows.find((item) => item.number === Number(lineNumber));
  return row?.text || "";
}

function classifySymbolAt(lineText, word) {
  const escaped = word.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  if (new RegExp(`\\b(func|function|def|class|type|struct|interface)\\b[^\\n]*\\b${escaped}\\b`).test(lineText)) {
    return "declaration";
  }
  if (new RegExp(`\\b${escaped}\\s*\\(`).test(lineText)) {
    return "function call";
  }
  if (/^\s*import\b|^\s*from\b|^\s*use\b|^\s*package\b/.test(lineText)) {
    return "import/package reference";
  }
  if (new RegExp(`\\.${escaped}\\b`).test(lineText)) {
    return "field or method reference";
  }
  if (new RegExp(`\\b(const|let|var)\\s+${escaped}\\b|\\b${escaped}\\s*:?=`).test(lineText)) {
    return "variable assignment";
  }
  return "identifier reference";
}

function buildHoverIndex(rows) {
  const index = new Map();
  for (const row of rows) {
    const words = row.text.match(/\b[A-Za-z_$][A-Za-z0-9_$]{1,127}\b/g) || [];
    for (const word of words) {
      const key = `${row.number}:${word}`;
      if (!index.has(key)) {
        index.set(key, {
          symbol: word,
          line: row.number,
          kind: classifySymbolAt(row.text, word),
          snippet: row.text.trim(),
        });
      }
    }
  }
  return index;
}

async function semanticHoverDetails(symbol) {
  const repo = state.rightRepo || selectedRepoNames()[0] || state.repos[0]?.repoName || "";
  const ref = state.rightRef || selectedRef() || "";
  if (!repo || !symbol) {
    return "";
  }
  const key = `${repo}:${ref}:${symbol}`;
  if (state.symbolHoverCache.has(key)) {
    return state.symbolHoverCache.get(key);
  }
  const pending = Promise.allSettled([
    mcpTool("find_symbol_definitions", { symbol, repo, revision: ref, limit: 3 }),
    mcpTool("find_symbol_references", { symbol, repo, revision: ref, limit: 5 }),
  ]).then((results) => {
    const definition = results[0].status === "fulfilled" ? results[0].value : "";
    const references = results[1].status === "fulfilled" ? results[1].value : "";
    const sections = [];
    if (definition && !/No definition|not found/i.test(definition)) {
      sections.push(`**MCP definitions**\n\n${definition.slice(0, 900)}`);
    }
    if (references && !/No reference|not found/i.test(references)) {
      sections.push(`**MCP references**\n\n${references.slice(0, 900)}`);
    }
    return sections.join("\n\n");
  }).catch(() => "");
  state.symbolHoverCache.set(key, pending);
  const resolved = await pending;
  state.symbolHoverCache.set(key, resolved);
  return resolved;
}

function monacoUriFor(repo, ref, path) {
  const clean = [repo, ref || "default", path].map((value) => encodeURIComponent(String(value || "").replaceAll("/", "__"))).join("/");
  return `inmemory://codeintel/${clean}`;
}

function loadMonaco() {
  if (state.monacoReady) {
    return state.monacoReady;
  }
  state.monacoReady = new Promise((resolve, reject) => {
    if (window.monaco?.editor) {
      state.monaco = window.monaco;
      resolve(window.monaco);
      return;
    }
    const start = () => {
      if (!window.require) {
        reject(new Error("Monaco loader unavailable"));
        return;
      }
      window.require.config({ paths: { vs: "https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs" } });
      window.MonacoEnvironment = {
        getWorkerUrl() {
          return "data:text/javascript;charset=utf-8," + encodeURIComponent(`
            self.MonacoEnvironment = { baseUrl: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/' };
            importScripts('https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs/base/worker/workerMain.js');
          `);
        },
      };
      window.require(["vs/editor/editor.main"], () => {
        state.monaco = window.monaco;
        resolve(window.monaco);
      }, reject);
    };
    if (window.require) {
      start();
      return;
    }
    setTimeout(() => {
      if (window.require) {
        start();
      } else {
        reject(new Error("Monaco loader did not initialize"));
      }
    }, 1500);
  }).catch((error) => {
    recordActivity("Monaco unavailable", error.message || String(error), "error");
    state.monacoReady = null;
    throw error;
  });
  return state.monacoReady;
}

function registerMonacoHoverProvider(monaco, language) {
  if (state.hoverProviderDisposable) {
    state.hoverProviderDisposable.dispose();
    state.hoverProviderDisposable = null;
  }
  state.hoverProviderDisposable = monaco.languages.registerHoverProvider(language, {
    async provideHover(model, position) {
      if (model !== state.codeModel) {
        return null;
      }
      const word = model.getWordAtPosition(position);
      if (!word) {
        return null;
      }
      const actualLine = state.codeRows[position.lineNumber - 1]?.number || position.lineNumber;
      const row = state.monacoHoverIndex.get(`${actualLine}:${word.word}`);
      if (!row) {
        return null;
      }
      const semantic = await semanticHoverDetails(row.symbol);
      const contents = [
        { value: `**${row.symbol}**` },
        { value: `Kind: ${row.kind}` },
        { value: `Location: ${state.rightPath}:${row.line}` },
        { value: `Repo: ${state.rightRepo}` },
        { value: `Ref: ${state.rightRef || "default"} / Language: ${state.rightLanguage || "text"}` },
        { value: `\`\`\`${state.rightLanguage || ""}\n${row.snippet}\n\`\`\`` },
      ];
      if (semantic) {
        contents.push({ value: semantic });
      } else {
        contents.push({ value: `No precise MCP definition/reference was returned for this hover yet; click to navigate with local fallback.` });
      }
      return {
        range: new monaco.Range(position.lineNumber, word.startColumn, position.lineNumber, word.endColumn),
        contents,
      };
    },
  });
}

function codeHoverElement() {
  let el = document.getElementById("codeHover");
  if (!el) {
    el = document.createElement("div");
    el.id = "codeHover";
    el.className = "code-hover-card hidden";
    document.body.appendChild(el);
  }
  return el;
}

function hideCodeHover() {
  const el = document.getElementById("codeHover");
  if (el) {
    el.classList.add("hidden");
  }
  state.activeHoverKey = "";
}

function hoverHtml(row, semantic = "") {
  const snippet = escapeHtml(row.snippet || "");
  const localRefs = localSymbolReferences(row.symbol, row.line);
  return `
    <div class="hover-title">${escapeHtml(row.symbol)}</div>
    <div class="hover-grid">
      <span>Kind</span><strong>${escapeHtml(row.kind)}</strong>
      <span>File</span><strong>${escapeHtml(`${state.rightPath}:${row.line}`)}</strong>
      <span>Repo</span><strong>${escapeHtml(state.rightRepo || "")}</strong>
      <span>Ref</span><strong>${escapeHtml(state.rightRef || "default")}</strong>
      <span>Language</span><strong>${escapeHtml(state.rightLanguage || "text")}</strong>
    </div>
    <pre>${snippet}</pre>
    ${localRefs ? `<div class="hover-local">${localRefs}</div>` : ""}
    ${semantic ? `<div class="hover-semantic">${renderMarkdown(semantic)}</div>` : `<div class="hover-note">Semantic details loading or unavailable. Click to navigate.</div>`}
  `;
}

function localSymbolReferences(symbol, currentLine) {
  const escaped = symbol.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const pattern = new RegExp(`\\b${escaped}\\b`);
  const rows = state.codeRows.filter((row) => pattern.test(row.text || ""));
  if (!rows.length) {
    return "";
  }
  const preview = rows.slice(0, 8).map((row) => {
    const marker = row.number === currentLine ? "current" : classifySymbolAt(row.text, symbol);
    return `<li><strong>${row.number}</strong> <span>${escapeHtml(marker)}</span> <code>${escapeHtml(row.text.trim()).slice(0, 180)}</code></li>`;
  }).join("");
  return `
    <div class="hover-local-title">Local references in this file: ${rows.length}</div>
    <ul>${preview}</ul>
  `;
}

function showCodeHover(row, clientX, clientY) {
  const el = codeHoverElement();
  const key = `${state.rightRepo}:${state.rightRef}:${state.rightPath}:${row.line}:${row.symbol}`;
  state.activeHoverKey = key;
  el.innerHTML = hoverHtml(row);
  el.classList.remove("hidden");
  const left = Math.min(clientX + 14, Math.max(16, window.innerWidth - 440));
  const top = Math.min(clientY + 16, Math.max(16, window.innerHeight - 320));
  el.style.left = `${left}px`;
  el.style.top = `${top}px`;
  semanticHoverDetails(row.symbol).then((semantic) => {
    if (state.activeHoverKey === key) {
      el.innerHTML = hoverHtml(row, semantic);
    }
  });
}

async function renderMonacoCode({ repo, ref, path, rows, language, highlightLine }) {
  const monaco = await loadMonaco();
  const codeView = $("codeView");
  codeView.classList.add("monaco-host");
  const value = rows.map((row) => row.text).join("\n");
  const actualLines = rows.map((row) => row.number);
  const editorLanguage = monacoLanguage(language, path);
  const uri = monaco.Uri.parse(monacoUriFor(repo, ref, path));
  const oldModel = state.codeModel;
  state.codeModel = monaco.editor.createModel(value, editorLanguage, uri);
  if (oldModel) {
    oldModel.dispose();
  }
  if (!state.codeEditor) {
    state.codeEditor = monaco.editor.create(codeView, {
      model: state.codeModel,
      theme: "vs-dark",
      readOnly: true,
      automaticLayout: true,
      minimap: { enabled: true },
      fontSize: 13,
      lineHeight: 21,
      fontLigatures: true,
      scrollBeyondLastLine: false,
      renderWhitespace: "selection",
      roundedSelection: false,
      contextmenu: true,
      scrollbar: { verticalScrollbarSize: 11, horizontalScrollbarSize: 11 },
      lineNumbers: (lineNumber) => String(actualLines[lineNumber - 1] || lineNumber),
      glyphMargin: true,
      folding: true,
      wordWrap: "off",
    });
  } else {
    state.codeEditor.setModel(state.codeModel);
    state.codeEditor.updateOptions({
      lineNumbers: (lineNumber) => String(actualLines[lineNumber - 1] || lineNumber),
    });
  }
  registerMonacoHoverProvider(monaco, editorLanguage);
  if (state.monacoClickDisposable) {
    state.monacoClickDisposable.dispose();
  }
  if (state.monacoMoveDisposable) {
    state.monacoMoveDisposable.dispose();
  }
  if (state.monacoLeaveDisposable) {
    state.monacoLeaveDisposable.dispose();
  }
  state.monacoClickDisposable = state.codeEditor.onMouseDown((event) => {
    if (!event.target?.position) {
      return;
    }
    const word = state.codeModel.getWordAtPosition(event.target.position);
    if (word) {
      navigateSymbol(word.word);
    }
  });
  state.monacoMoveDisposable = state.codeEditor.onMouseMove((event) => {
    if (!event.target?.position) {
      hideCodeHover();
      return;
    }
    const word = state.codeModel.getWordAtPosition(event.target.position);
    if (!word) {
      hideCodeHover();
      return;
    }
    const actualLine = state.codeRows[event.target.position.lineNumber - 1]?.number || event.target.position.lineNumber;
    const row = state.monacoHoverIndex.get(`${actualLine}:${word.word}`);
    if (!row) {
      hideCodeHover();
      return;
    }
    const mouse = event.event?.browserEvent || {};
    showCodeHover(row, mouse.clientX ?? event.event?.posx ?? 32, mouse.clientY ?? event.event?.posy ?? 32);
  });
  state.monacoLeaveDisposable = state.codeEditor.onMouseLeave(() => hideCodeHover());
  state.codeEditor.setPosition({ lineNumber: Math.max(1, rows.findIndex((row) => row.number === Number(highlightLine)) + 1), column: 1 });
  state.codeEditor.revealLineInCenter(Math.max(1, rows.findIndex((row) => row.number === Number(highlightLine)) + 1));
  const modelLine = rows.findIndex((row) => row.number === Number(highlightLine)) + 1;
  state.codeDecorations = state.codeEditor.deltaDecorations(state.codeDecorations || [], modelLine > 0 ? [{
    range: new monaco.Range(modelLine, 1, modelLine, 1),
    options: { isWholeLine: true, className: "monaco-active-line", glyphMarginClassName: "monaco-active-glyph" },
  }] : []);
}

function languageKeywords(language) {
  const common = ["return", "if", "else", "for", "while", "switch", "case", "default", "break", "continue", "try", "catch", "finally", "throw", "new", "class", "interface", "type", "struct", "enum", "const", "let", "var", "function", "async", "await", "import", "export", "from", "package", "namespace", "public", "private", "protected", "static", "final", "nil", "null", "true", "false"];
  const extra = {
    go: ["func", "defer", "go", "chan", "select", "range", "map", "make", "interface", "type", "package"],
    rust: ["fn", "let", "mut", "impl", "trait", "pub", "crate", "use", "match", "Some", "None", "Result", "Ok", "Err"],
    python: ["def", "self", "lambda", "with", "as", "yield", "None", "True", "False", "elif", "except"],
    java: ["implements", "extends", "throws", "void", "boolean", "int", "long", "String"],
    csharp: ["using", "var", "async", "await", "record", "sealed", "partial"],
  };
  return new Set([...(extra[language] || []), ...common]);
}

function highlightCodeLine(line, language) {
  const placeholders = [];
  const stash = (html) => {
    const id = `\uE000${String.fromCharCode(0xE100 + placeholders.length)}\uE001`;
    placeholders.push(html);
    return id;
  };
  let value = escapeHtml(line || "");
  value = value.replace(/(&quot;[^&]*(?:\\.[^&]*)?&quot;|'[^']*'|`[^`]*`)/g, (m) => stash(`<span class="tok-string">${m}</span>`));
  value = value.replace(/(\/\/.*$|#.*$|--.*$|\/\*.*?\*\/)/g, (m) => stash(`<span class="tok-comment">${m}</span>`));
  const keywords = languageKeywords(language);
  value = value.replace(/\b([A-Za-z_$][A-Za-z0-9_$]*)\b(?=\s*\()/g, (m, sym) => stash(`<button class="code-token tok-call" data-symbol="${escapeHtml(sym)}">${escapeHtml(sym)}</button>`));
  value = value.replace(/\b([A-Za-z_$][A-Za-z0-9_$]*)\b/g, (m, word) => keywords.has(word) ? `<span class="tok-keyword">${word}</span>` : word);
  placeholders.forEach((html, index) => {
    value = value.replaceAll(`\uE000${String.fromCharCode(0xE100 + index)}\uE001`, html);
  });
  return value || " ";
}

async function renderCode(text, { repo, ref, path, highlightLine }) {
  const normalized = normalizeReadFileText(text, path);
  const content = normalized.rows.map((row) => row.text).join("\n");
  const language = detectLanguage(normalized.path || path, content);
  state.rightLanguage = language;
  state.rightPath = normalized.path || path || "";
  state.codeRows = normalized.rows;
  state.monacoHoverIndex = buildHoverIndex(normalized.rows);
  state.symbolHoverCache = new Map();
  $("codeToolbar").innerHTML = `
    <span class="code-badge">${escapeHtml(language)}</span>
    <span>${escapeHtml(repo || "")}</span>
    <span>${escapeHtml(ref || "default")}</span>
    <span>${escapeHtml(normalized.path || path || "")}</span>
  `;
  try {
    await renderMonacoCode({ repo, ref, path: normalized.path || path, rows: normalized.rows, language, highlightLine });
  } catch {
    renderFallbackCode({ rows: normalized.rows, language, highlightLine });
  }
}

function renderFallbackCode({ rows, language, highlightLine }) {
  $("codeView").dataset.language = language;
  $("codeView").classList.remove("monaco-host");
  $("codeView").innerHTML = rows.map((row) => {
    const highlighted = row.number === Number(highlightLine);
    return `<div class="code-line ${highlighted ? "active" : ""}" data-line="${row.number}"><span class="line-no">${row.number}</span><code class="line-code">${highlightCodeLine(row.text, language)}</code></div>`;
  }).join("");
  const active = $("codeView").querySelector(".code-line.active");
  if (active) {
    active.scrollIntoView({ block: "center" });
  }
}

async function navigateSelectedSymbol() {
  const symbol = String(window.getSelection()?.toString() || "").trim();
  if (!/^[A-Za-z_$][A-Za-z0-9_$]{1,127}$/.test(symbol)) {
    return;
  }
  const repo = state.rightRepo || selectedRepoNames()[0] || state.repos[0]?.repoName;
  const ref = state.rightRef || selectedRef();
  if (!repo) {
    return;
  }
  $("codeMeta").textContent = `Finding ${symbol} in ${repo}`;
  try {
    const text = await mcpTool("find_symbol_definitions", { symbol, repo, revision: ref, limit: 10 });
    const match = text.match(/((?:[A-Za-z0-9_.-]+\/)+[A-Za-z0-9_.-]+):(\d+)/);
    if (match) {
      await openFileReference(match[1], Number(match[2]));
      return;
    }
    $("codeMeta").textContent = `No definition file reference returned for ${symbol}.`;
  } catch (error) {
    $("codeMeta").textContent = `Symbol navigation failed: ${error.message || error}`;
  }
}

async function navigateSymbol(symbol) {
  const clean = String(symbol || "").trim();
  if (!/^[A-Za-z_$][A-Za-z0-9_$]{1,127}$/.test(clean)) {
    return;
  }
  const repo = state.rightRepo || selectedRepoNames()[0] || state.repos[0]?.repoName;
  const ref = state.rightRef || selectedRef();
  if (!repo) {
    return;
  }
  $("codeMeta").textContent = `Finding ${clean} in ${repo}`;
  try {
    const text = await mcpTool("find_symbol_definitions", { symbol: clean, repo, revision: ref, limit: 10 });
    const match = text.match(/((?:[A-Za-z0-9_.-]+\/)+[A-Za-z0-9_.-]+):(\d+)/) || text.match(/\b([A-Za-z0-9_.-]+\.(?:go|ts|tsx|js|jsx|py|java|rs|cs|fs|vb|proto|yaml|yml|json|xml|rb|dart|cpp|cc|c|h|hpp)):(\d+)/);
    if (match) {
      await openFileReference(resolveContextPath(match[1], state.rightPath), Number(match[2]), repo, ref);
      return;
    }
    if (jumpToLocalSymbol(clean)) {
      $("codeMeta").textContent = `${repo} / ${ref || "default"} / ${state.rightPath} / local symbol ${clean}`;
      return;
    }
    $("codeMeta").textContent = `No definition file reference returned for ${clean}.`;
  } catch (error) {
    if (jumpToLocalSymbol(clean)) {
      $("codeMeta").textContent = `${repo} / ${ref || "default"} / ${state.rightPath} / local symbol ${clean}`;
      return;
    }
    $("codeMeta").textContent = `Symbol navigation failed: ${error.message || error}`;
  }
}

function jumpToLocalSymbol(symbol) {
  if (state.codeEditor && state.codeModel && state.codeRows.length) {
    return jumpToLocalSymbolInMonaco(symbol);
  }
  const rows = Array.from($("codeView").querySelectorAll(".code-line"));
  const escaped = symbol.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const declaration = new RegExp(`\\b(func|function|def|class|type|struct|interface|const|let|var)\\b[^\\n]*\\b${escaped}\\b|\\b${escaped}\\s*[:=]\\s*(func|function|async|\\()`);
  const call = new RegExp(`\\b${escaped}\\s*\\(`);
  const target = rows.find((row) => declaration.test(row.innerText || "")) || rows.find((row) => call.test(row.innerText || ""));
  if (!target) {
    return false;
  }
  rows.forEach((row) => row.classList.remove("active"));
  target.classList.add("active");
  target.scrollIntoView({ block: "center" });
  return true;
}

function jumpToLocalSymbolInMonaco(symbol) {
  const escaped = symbol.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const declaration = new RegExp(`\\b(func|function|def|class|type|struct|interface)\\b[^\\n]*\\b${escaped}\\b|\\b${escaped}\\s*[:=]\\s*(func|function|async|\\()`);
  const call = new RegExp(`\\b${escaped}\\s*\\(`);
  const row = state.codeRows.find((item) => declaration.test(item.text || "")) || state.codeRows.find((item) => call.test(item.text || ""));
  if (!row) {
    return false;
  }
  const modelLine = Math.max(1, state.codeRows.findIndex((item) => item.number === row.number) + 1);
  state.codeEditor.setPosition({ lineNumber: modelLine, column: 1 });
  state.codeEditor.revealLineInCenter(modelLine);
  const monaco = state.monaco;
  if (monaco) {
    state.codeDecorations = state.codeEditor.deltaDecorations(state.codeDecorations || [], [{
      range: new monaco.Range(modelLine, 1, modelLine, 1),
      options: { isWholeLine: true, className: "monaco-active-line", glyphMarginClassName: "monaco-active-glyph" },
    }]);
  }
  return true;
}

async function sendMcp() {
  let args;
  try {
    args = JSON.parse($("mcpArgs").value || "{}");
  } catch (error) {
    log($("mcpLog"), `Invalid JSON: ${error.message}`);
    return;
  }
  const tool = $("mcpTool").value;
  const id = `admin-${Date.now()}`;
  const body = { jsonrpc: "2.0", id, method: "tools/call", params: { name: tool, arguments: args } };
  try {
    const resp = await request("mcp", "POST", `/api/${state.domain}/mcp`, { body, raw: true });
    log($("mcpLog"), { request: body, response: resp.body });
    const text = extractToolText(resp.body);
    state.chatMessages.push({ role: "assistant", content: `MCP ${tool}\n\n${text}` });
    renderMessages();
    recordActivity("MCP tool completed", tool);
  } catch (error) {
    log($("mcpLog"), String(error.message || error));
  }
}

async function loadLatestTestArtifact() {
  try {
    const res = await fetch("/admin-artifacts/latest", { cache: "no-store" });
    const artifact = await res.json();
    if (!res.ok) {
      throw new Error(artifact.detail || artifact.error || `HTTP ${res.status}`);
    }
    const lines = [
      `# Real UI Test Artifact`,
      ``,
      `workspace: ${artifact.workspace || "unknown"}`,
      `repos: ${artifact.repoCount ?? artifact.repos?.length ?? "unknown"}`,
      `chatId: ${artifact.chatId || "n/a"}`,
      `durationMs: ${artifact.durationMs ?? "n/a"}`,
      ``,
      artifact.answer || artifact.response || JSON.stringify(artifact, null, 2),
    ];
    if (Array.isArray(artifact.repos) && artifact.repos.length) {
      state.selectedRepos = new Set(artifact.repos);
      const matchingContext = state.contexts.find((context) => {
        const names = context.repoNames || [];
        return names.length === artifact.repos.length && artifact.repos.every((repo) => names.includes(repo));
      });
      state.chatSearchContext = matchingContext?.name || "";
      if ($("chatSearchContext")) {
        $("chatSearchContext").value = state.chatSearchContext;
      }
      saveState();
    }
    state.chatId = artifact.chatId || state.chatId;
    state.chatMessages.push({ role: "user", content: artifact.question || "Loaded latest real test artifact." });
    state.chatMessages.push({ role: "assistant", content: lines.join("\n") });
    if (artifact.search) {
      state.chatMessages.push({ role: "assistant", content: `# Direct Search API Evidence\n\n\`\`\`json\n${JSON.stringify(artifact.search, null, 2)}\n\`\`\`` });
    }
    if (artifact.mcp) {
      state.chatMessages.push({ role: "assistant", content: `# MCP Evidence\n\n\`\`\`json\n${JSON.stringify(artifact.mcp, null, 2)}\n\`\`\`` });
    }
    renderMessages();
    renderChatSelectors();
    recordActivity("Loaded test artifact", `${artifact.workspace || "workspace"} / ${artifact.chatId || "no chat id"}`);
  } catch (error) {
    state.chatMessages.push({ role: "assistant", content: `Failed to load test artifact: ${error.message || error}` });
    renderMessages();
    recordActivity("Load artifact failed", error.message || String(error), "error");
  }
}

function mcpPresetArgs(tool) {
  const repo = selectedRepoNames()[0] || state.repos[0]?.repoName || "";
  const ref = selectedRef();
  if (tool === "list_repos" || tool === "graph_status") {
    return {};
  }
  if (tool === "read_file") {
    return { repo, ref, path: "", offset: 1, limit: 120 };
  }
  if (tool === "grep") {
    return { query: "", repo, revision: ref, limit: 50 };
  }
  if (tool === "codegraph_context") {
    return { query: "", repos: selectedRepoNames(), ref, compact: false, limit: 40 };
  }
  if (tool === "compare_branches") {
    return { repo, baseRef: "refs/heads/main", headRef: ref || "refs/heads/main", query: "" };
  }
  return { symbol: "", repo, revision: ref, limit: 20 };
}

function applyMcpPreset() {
  $("mcpArgs").value = JSON.stringify(mcpPresetArgs($("mcpTool").value), null, 2);
}

async function sendSearch() {
  const query = $("searchQuery").value.trim();
  if (!query) {
    log($("searchLog"), "Enter a query first.");
    return;
  }
  const repos = selectedRepoNames();
  const ref = $("searchRef").value.trim();
  const body = {
    query,
    repos,
    contextLines: 3,
    count: 50,
    ...(ref ? { ref: indexRef(ref) } : {}),
  };
  try {
    const resp = await request("search", "POST", "/api/search", { body, raw: true });
    log($("searchLog"), { request: body, response: resp.body });
    recordActivity("Search completed", `${query} / ${repos.length || "all"} repos`);
  } catch (error) {
    log($("searchLog"), String(error.message || error));
  }
}

function newChat() {
  state.chatId = "";
  state.chatMessages = [];
  $("chatIdDisplay").value = "";
  renderChatEvidenceStrip();
  renderMessages();
}

function handleDelegatedClick(event) {
  const target = event.target.closest("[data-action]");
  if (!target) {
    return;
  }
  const action = target.dataset.action;
  const card = target.closest(".repo-card");
  if (action === "sync-connection") {
    syncConnection(target.dataset.id).catch((error) => recordActivity("Connection sync failed", error.message, "error"));
  } else if (action === "save-atom-provider") {
    saveAtomProviderConnection(target.closest(".provider-card")).catch((error) => recordActivity("Atom VCS connection failed", error.message, "error"));
  } else if (action === "connection-status") {
    loadConnectionStatus(target.dataset.id).catch((error) => recordActivity("Connection status failed", error.message, "error"));
  } else if (action === "repo-status") {
    repoStatus(card).catch((error) => recordActivity("Repo status failed", error.message, "error"));
  } else if (action === "repo-branches") {
    loadRepoBranches(card).catch((error) => recordActivity("Repo branches failed", error.message, "error"));
  } else if (action === "save-repo-branches") {
    saveRepoBranches(card).catch((error) => recordActivity("Save branch policy failed", error.message, "error"));
  } else if (action === "index-repo") {
    indexRepo(card).catch((error) => recordActivity("Index failed", error.message, "error"));
  } else if (action === "remove-index") {
    removeIndex(card).catch((error) => recordActivity("Remove index failed", error.message, "error"));
  } else if (action === "delete-secret") {
    deleteSecret(target.dataset.key).catch((error) => recordActivity("Delete secret failed", error.message, "error"));
  }
}

function applyRepoFilters() {
  state.repoPage = 1;
  state.repoQuery = $("repoQuery").value.trim();
  state.repoPerPage = Number($("repoPerPage").value || 30);
  state.repoSort = $("repoSort").value;
  state.repoDirection = $("repoDirection").value;
  saveState();
  loadRepos().catch((error) => recordActivity("Repos refresh failed", error.message, "error"));
}

function handleChange(event) {
  const target = event.target;
  if (target.dataset.action === "select-chat-repo") {
    if (target.checked) {
      state.selectedRepos.add(target.dataset.name);
    } else {
      state.selectedRepos.delete(target.dataset.name);
    }
    saveState();
    renderRepoScope();
    renderRepos();
    renderChatEvidenceStrip();
    applyMcpPreset();
  }
}

function boot() {
  loadState();
  document.querySelectorAll(".nav").forEach((button) => button.addEventListener("click", () => handleNav(button.dataset.view)));
  $("saveRuntime").addEventListener("click", () => {
    state.apiBase = $("apiBase").value.trim();
    state.atomToken = $("atomToken").value.trim();
    state.apiKey = $("apiKey").value.trim();
    state.domain = $("domain").value.trim();
    saveState();
    recordActivity("Runtime saved", state.domain || "no domain");
    refreshWorkspaceState().catch((error) => recordActivity("Refresh failed", error.message, "error"));
  });
  $("refreshAll").addEventListener("click", () => refreshWorkspaceState().catch((error) => recordActivity("Refresh failed", error.message, "error")));
  $("workspaceForm").addEventListener("submit", provisionWorkspace);
  $("secretForm").addEventListener("submit", saveSecret);
  $("modelForm").addEventListener("submit", saveModel);
  $("connectionForm").addEventListener("submit", saveConnection);
  $("refreshLlm").addEventListener("click", () => loadLlmConfig().catch((error) => recordActivity("LLM refresh failed", error.message, "error")));
  $("refreshConnections").addEventListener("click", () => loadConnections().catch((error) => recordActivity("Connections refresh failed", error.message, "error")));
  $("refreshRepos").addEventListener("click", () => loadRepos().catch((error) => recordActivity("Repos refresh failed", error.message, "error")));
  $("repoQuery").addEventListener("input", () => {
    clearTimeout(state.repoQueryTimer);
    state.repoQueryTimer = setTimeout(applyRepoFilters, 350);
  });
  $("repoPerPage").addEventListener("change", applyRepoFilters);
  $("repoSort").addEventListener("change", applyRepoFilters);
  $("repoDirection").addEventListener("change", applyRepoFilters);
  $("repoPrevPage").addEventListener("click", () => {
    state.repoPage = Math.max(1, (state.repoPage || 1) - 1);
    saveState();
    loadRepos().catch((error) => recordActivity("Repos refresh failed", error.message, "error"));
  });
  $("repoNextPage").addEventListener("click", () => {
    state.repoPage = (state.repoPage || 1) + 1;
    saveState();
    loadRepos().catch((error) => recordActivity("Repos refresh failed", error.message, "error"));
  });
  $("saveSearchContext").addEventListener("click", (event) => saveSelectedSearchContext(event).catch((error) => recordActivity("Search context save failed", error.message, "error")));
  $("chatForm").addEventListener("submit", sendChat);
  $("refreshChatState").addEventListener("click", () => refreshWorkspaceState().catch((error) => recordActivity("Chat state refresh failed", error.message, "error")));
  $("loadTestArtifact").addEventListener("click", loadLatestTestArtifact);
  $("chatConfiguredModel").addEventListener("change", applyConfiguredModel);
  $("chatSearchContext").addEventListener("change", () => {
    state.chatSearchContext = $("chatSearchContext").value;
    saveState();
    renderChatEvidenceStrip();
  });
  $("newChat").addEventListener("click", newChat);
  $("clearCode").addEventListener("click", () => {
    $("codeMeta").textContent = "Click a file reference in chat or MCP output.";
    $("codeToolbar").textContent = "";
    state.codeRows = [];
    state.monacoHoverIndex = new Map();
    state.symbolHoverCache = new Map();
    if (state.codeEditor) {
      state.codeEditor.setModel(null);
    }
    if (state.codeModel) {
      state.codeModel.dispose();
      state.codeModel = null;
    }
    $("codeView").innerHTML = "";
  });
  $("codeView").addEventListener("click", (event) => {
    const token = event.target.closest(".code-token[data-symbol]");
    if (token) {
      navigateSymbol(token.dataset.symbol);
    }
  });
  $("codeView").addEventListener("dblclick", () => {
    navigateSelectedSymbol();
  });
  $("sendMcp").addEventListener("click", sendMcp);
  $("mcpTool").addEventListener("change", applyMcpPreset);
  $("sendSearch").addEventListener("click", sendSearch);
  document.body.addEventListener("click", handleDelegatedClick);
  document.body.addEventListener("change", handleChange);
  if (state.apiKey) {
    refreshWorkspaceState({ quiet: true }).catch((error) => recordActivity("Startup refresh failed", error.message, "error"));
  } else {
    renderOverview();
    renderSecrets();
    renderModels();
    renderContexts();
    renderChatSelectors();
    renderChatEvidenceStrip();
  }
  applyMcpPreset();
}

document.addEventListener("DOMContentLoaded", boot);
