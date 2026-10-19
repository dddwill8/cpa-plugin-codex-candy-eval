"use strict";

// Fork-only behavior. Upstream pages call globalThis.EXTEND and otherwise stay unchanged,
// so a later upstream drop-in replaces app.js, catalog.js and the three test pages
// without taking this file with it. See extend.go for the matching server paths.

const HEALTH_PROMPT = "say hi";

function hiddenModel(id) {
  return typeof HIDDEN_MODEL === "function" ? HIDDEN_MODEL(id) : String(id).toLowerCase().includes("image");
}

// Drop a bare id when a prefixed id's text after the first "/" is exactly that bare id.
// Image names are removed first, matching the catalog fetch, so an image alias cannot hide a bare id.
function listedModelIDs(ids) {
  const seen = new Set();
  const kept = [];
  for (const id of ids || []) {
    if (seen.has(id) || hiddenModel(id)) continue;
    seen.add(id);
    kept.push(id);
  }
  const covered = new Set();
  for (const id of kept) {
    const slash = id.indexOf("/");
    if (slash >= 0) covered.add(id.slice(slash + 1));
  }
  return kept.filter((id) => id.includes("/") || !covered.has(id));
}

function firstModel(ids, skipParentheses) {
  const models = listedModelIDs(ids).sort(compareModels);
  return models.find((id) => !skipParentheses || !/[()]/.test(id)) || "";
}

const enabledCredentials = () => credentials.filter((credential) => !credential.disabled);

function oneClickBody(test) {
  const ids = enabledCredentials().map((credential) => credential.id);
  const body = { credential_ids: ids, model_catalog: null };
  if (test !== "fingerprint") body.effort = $(test + "-effort").value;
  if (test === "fingerprint") body.concurrency = fingerprintConcurrency();
  return body;
}

function startOneClick(test) {
  if (!["candy", "fingerprint", "pelican"].includes(test)) return;
  return update("开始测试失败", async () => {
    await refreshCatalog();
    fillModels();
    const body = oneClickBody(test);
    body.model_catalog = await modelCatalog(body.credential_ids);
    const response = await api(`${BASE}/${test}/one-click`, { method: "POST", body });
    const messages = [`已为 ${response.started || 0} 个凭证开始测试`];
    if (response.skipped) messages.push(`${response.skipped} 个凭证不支持所选模型，已跳过`);
    if (response.unchecked) messages.push(`${response.unchecked} 个凭证无法读取模型列表，仍会尝试测试`);
    if (response.busy) messages.push(`${response.busy} 个凭证正在测试，已跳过`);
    setNotice("flash", messages.join("；"), response.unchecked ? "warning" : "info");
  });
}

let healthResults = [];
var healthChoice = new Map();
const healthOpen = new Set();
let healthCatalogs = {};
let healthCatalogStamp = "";
var healthCatalogTask = Promise.resolve();

function healthModelIDs(id) {
  return listedModelIDs(healthCatalogs?.[id] || []).sort(compareModels);
}

function ensureHealthCatalogs() {
  const stamp = credentials.map((credential) => credential.id).join("\0");
  if (stamp === healthCatalogStamp) return;
  healthCatalogStamp = stamp;
  const ids = credentials.map((credential) => credential.id);
  healthCatalogTask = (async () => {
    try {
      if (!ids.length) {
        healthCatalogs = {};
        return;
      }
      const raw = await modelCatalog(ids);
      if (credentials.map((credential) => credential.id).join("\0") !== stamp) return;
      healthCatalogs = raw || {};
      renderHealth();
    } catch (err) {
      if (healthCatalogStamp === stamp) healthCatalogStamp = "";
      if (err instanceof AuthError) showLogin(err.message);
    }
  })();
}

function healthStatus(result) {
  if (!result) return `<span class="none">未检测</span>`;
  return result.passed ? statusPill("ok", "通过") : statusPill("bad", "不通过");
}

function healthRow(credential) {
  const models = healthModelIDs(credential.id);
  const previous = healthChoice.get(credential.id);
  const chosen = models.includes(previous) ? previous : (models[0] || "");
  if (chosen) healthChoice.set(credential.id, chosen);
  const result = healthResults.find((entry) => entry.credential_id === credential.id);
  const options = models.map((id) => `<option value="${esc(id)}"${id === chosen ? " selected" : ""}>${esc(id)}</option>`).join("");
  const open = healthOpen.has(credential.id);
  const error = open ? `<div class="health-error">${esc(result?.error || (result ? "无报错信息" : "尚未检测"))}</div>` : "";
  return `<div class="health-item${open ? " open" : ""}"><div class="health-row" data-health-toggle="${esc(credential.id)}"><span class="name">${esc(credential.name || credential.id)}</span><span class="native-select health-model"><select data-health-model="${esc(credential.id)}" aria-label="检测模型">${options}</select></span><span class="health-status">${healthStatus(result)}</span></div>${error}</div>`;
}

function renderHealth() {
  const root = $("health-results");
  if (!root) return;
  ensureHealthCatalogs();
  const html = credentials.length ? credentials.map(healthRow).join("") : `<div class="empty">没有凭证。</div>`;
  if (root.healthHTML !== html) {
    root.healthHTML = html;
    root.innerHTML = html;
  }
  const button = $("health-run");
  if (button) button.disabled = pending || !enabledCredentials().length;
}

function toggleHealth(id) {
  healthOpen.has(id) ? healthOpen.delete(id) : healthOpen.add(id);
  renderHealth();
}

function setHealthModel(id, model) {
  healthChoice.set(id, model);
  renderHealth();
}

function startHealth() {
  return update("健康检测失败", async () => {
    const ids = enabledCredentials().map((credential) => credential.id);
    const catalog = await modelCatalog(ids);
    const models = {};
    for (const id of ids) {
      const choice = healthChoice.get(id) || firstModel(catalog?.[id], false);
      if (choice) models[id] = choice;
    }
    const response = await api(`${BASE}/health/run`, {
      method: "POST",
      body: { credential_ids: ids, model_catalog: catalog, models },
    });
    healthResults = Array.isArray(response.results) ? response.results : [];
    const passed = healthResults.filter((result) => result.passed).length;
    setNotice("flash", `检测完成：${passed} 个通过，${healthResults.length - passed} 个不通过`, passed === healthResults.length ? "info" : "warning");
    renderHealth();
  });
}

function renderOneClick() {
  for (const test of ["candy", "fingerprint", "pelican"]) {
    const button = $(test + "-one-click");
    if (!button) continue;
    const count = enabledCredentials().length;
    button.disabled = pending || !count;
    button.dataset.tip = count ? "" : "没有已启用的凭证";
  }
  renderHealth();
}

function wireExtend() {
  TEST_LABELS.health = "健康检测";
  const upstreamRender = render;
  render = function renderWithExtend() {
    upstreamRender();
    renderOneClick();
  };
  const upstreamShowLogin = showLogin;
  showLogin = function showLoginWithExtend(message) {
    healthResults = [];
    healthChoice.clear();
    healthOpen.clear();
    healthCatalogs = {};
    healthCatalogStamp = "";
    return upstreamShowLogin(message);
  };
  for (const test of ["candy", "fingerprint", "pelican"]) {
    $(test + "-one-click").addEventListener("click", () => startOneClick(test));
  }
  $("health-run").addEventListener("click", () => startHealth());
  $("health-results").addEventListener("click", (e) => {
    if (e.target.closest?.("select")) return;
    const id = e.target.closest?.("[data-health-toggle]")?.getAttribute("data-health-toggle");
    if (id) toggleHealth(id);
  });
  $("health-results").addEventListener("change", (e) => {
    const id = e.target.getAttribute?.("data-health-model");
    if (id) healthChoice.set(id, e.target.value);
  });
  renderOneClick();
}

var EXTEND = {
  tabs: ["health"],
  listedModels: listedModelIDs,
  firstModel,
  wire: wireExtend,
};
globalThis.EXTEND = EXTEND;
