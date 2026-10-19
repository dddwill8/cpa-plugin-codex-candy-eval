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

function healthVerdict(result) {
  const verdict = result.passed ? statusPill("ok", "通过") : statusPill("bad", "不通过");
  const detail = result.passed ? result.model : (result.error || result.model || "");
  const credential = credentials.find((entry) => entry.id === result.credential_id);
  return `<div class="health-row"><span class="name">${esc(credential?.name || result.credential_id)}</span>${verdict}<span class="meta mono">${esc(detail)}</span></div>`;
}

function renderHealth() {
  const root = $("health-results");
  if (!root) return;
  root.innerHTML = healthResults.length
    ? healthResults.map(healthVerdict).join("")
    : `<div class="empty">对每张已启用凭证，用它的第一个模型发送 ${HEALTH_PROMPT}。</div>`;
  const button = $("health-run");
  if (button) button.disabled = pending || !enabledCredentials().length;
}

function startHealth() {
  return update("健康检测失败", async () => {
    const ids = enabledCredentials().map((credential) => credential.id);
    const response = await api(`${BASE}/health/run`, {
      method: "POST",
      body: { credential_ids: ids, model_catalog: await modelCatalog(ids) },
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
    return upstreamShowLogin(message);
  };
  for (const test of ["candy", "fingerprint", "pelican"]) {
    $(test + "-one-click").addEventListener("click", () => startOneClick(test));
  }
  $("health-run").addEventListener("click", () => startHealth());
  renderOneClick();
}

var EXTEND = {
  tabs: ["health"],
  listedModels: listedModelIDs,
  firstModel,
  wire: wireExtend,
};
globalThis.EXTEND = EXTEND;
