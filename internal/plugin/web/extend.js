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

var healthResults = [];
var healthChoice = new Map();
const healthOpen = new Set();
let healthCatalogs = {};
let healthCatalogStamp = "";
var healthCatalogTask = Promise.resolve();
const HEALTH_STORE = "cpa-codex-candy-eval.health.v1";

function healthModelIDs(id) {
  return listedModelIDs(healthCatalogs?.[id] || []).sort(compareModels);
}

function forgetCredentialModels(ids) {
  for (const id of ids) {
    delete catalogCache.credentials[id];
    catalogFailures.delete(id);
    catalogRequests.delete(id);
  }
}

var healthHidden = new Set();
const healthAbort = new Map();
const healthChecking = new Set();
var healthRun = false;

function saveHealthResults() {
  const results = healthResults.map((result) => ({
    credential_id: result.credential_id,
    model: result.model || "",
    passed: !!result.passed,
    error: result.error || "",
  }));
  store(HEALTH_STORE, { owner: catalogCache.owner || "", results, hidden: [...healthHidden] });
}

function restoreHealthResults() {
  const saved = stored(HEALTH_STORE);
  if (!saved || saved.owner !== (catalogCache.owner || "")) return;
  if (Array.isArray(saved.results)) healthResults = saved.results.filter((result) => result && typeof result.credential_id === "string");
  healthHidden = new Set(Array.isArray(saved.hidden) ? saved.hidden.filter((id) => typeof id === "string") : []);
}

function mergeHealthResult(result) {
  healthResults = healthResults.filter((entry) => !(entry.credential_id === result.credential_id && (entry.model || "") === (result.model || "")));
  healthResults.push(result);
  saveHealthResults();
  renderHealth();
}

function loadHealthCatalogs(force) {
  const stamp = credentials.map((credential) => credential.id).join("\0");
  if (!force && stamp === healthCatalogStamp) return healthCatalogTask;
  healthCatalogStamp = stamp;
  const ids = credentials.map((credential) => credential.id);
  healthCatalogTask = (async () => {
    try {
      if (!ids.length) {
        healthCatalogs = {};
        return;
      }
      if (force) forgetCredentialModels(ids);
      const raw = await modelCatalog(ids);
      if (credentials.map((credential) => credential.id).join("\0") !== stamp) return;
      healthCatalogs = raw || {};
      renderHealth();
    } catch (err) {
      if (healthCatalogStamp === stamp) healthCatalogStamp = "";
      if (err instanceof AuthError) showLogin(err.message);
    }
  })();
  return healthCatalogTask;
}

function healthStatus(result) {
  if (!result) return `<span class="none">未检测</span>`;
  return result.passed ? statusPill("ok", "通过") : statusPill("bad", "不通过");
}

function healthSummary(credential, results) {
  if (!results.length && healthChecking.has(credential.id)) return `<span class="none">检测中</span>`;
  if (!results.length) return healthStatus(null);
  if (results.length === 1) return healthStatus(results[0]);
  const passed = results.filter((result) => result.passed).length;
  return statusPill(passed === results.length ? "ok" : "bad", `${passed === results.length ? "通过" : "不通过"} ${passed}/${results.length}`);
}

function healthDetail(results) {
  if (!results.length) return `<div class="health-error">尚未检测</div>`;
  if (results.length === 1) return `<div class="health-error">${esc(results[0].error || "无报错信息")}</div>`;
  return results.map((result) => `<div class="health-model-line"><span class="mono">${esc(result.model || "未选模型")}</span>${healthStatus(result)}${result.error ? `<div class="health-error">${esc(result.error)}</div>` : ""}</div>`).join("");
}

function healthRow(credential) {
  const models = healthModelIDs(credential.id);
  const previous = healthChoice.get(credential.id);
  const chosen = models.includes(previous) ? previous : (models[0] || "");
  if (chosen) healthChoice.set(credential.id, chosen);
  const results = healthResults.filter((entry) => entry.credential_id === credential.id);
  const options = models.map((id) => `<option value="${esc(id)}"${id === chosen ? " selected" : ""}>${esc(id)}</option>`).join("");
  const open = healthOpen.has(credential.id);
  const error = open ? `<div class="health-detail">${healthDetail(results)}</div>` : "";
  return `<div class="health-item${open ? " open" : ""}"><div class="health-row" data-health-toggle="${esc(credential.id)}"><button class="btn ghost health-hide" type="button" data-health-hide="${esc(credential.id)}" aria-label="取消检测并取消显示这个凭证" data-tip="取消检测，并取消显示这个凭证">取消并隐藏</button><span class="name">${esc(credential.name || credential.id)}</span><span class="native-select health-model"><select data-health-model="${esc(credential.id)}" aria-label="检测模型">${options}</select></span><span class="health-status">${healthSummary(credential, results)}</span></div>${error}</div>`;
}

function healthHiddenBox(hidden) {
  const rows = hidden.map((credential) => `<div class="health-row"><button class="btn ghost health-hide" type="button" data-health-restore="${esc(credential.id)}">恢复显示</button><span class="name">${esc(credential.name || credential.id)}</span></div>`).join("");
  return `<details class="health-hidden"><summary>已隐藏 ${hidden.length} 张凭证</summary>${rows}</details>`;
}

function renderHealth() {
  const root = $("health-results");
  if (!root) return;
  loadHealthCatalogs(false);
  const visible = credentials.filter((credential) => !healthHidden.has(credential.id));
  const hidden = credentials.filter((credential) => healthHidden.has(credential.id));
  const html = `${visible.length ? visible.map(healthRow).join("") : `<div class="empty">${credentials.length ? "凭证都已隐藏。" : "没有凭证。"}</div>`}${healthHiddenBox(hidden)}`;
  if (root.healthHTML !== html) {
    root.healthHTML = html;
    root.innerHTML = html;
  }
  for (const id of ["health-run", "health-run-all"]) {
    const button = $(id);
    if (button) button.disabled = healthRun || pending || !enabledCredentials().some((credential) => !healthHidden.has(credential.id));
  }
}

function hideHealthCredential(id) {
  healthAbort.get(id)?.abort();
  healthHidden.add(id);
  healthChecking.delete(id);
  saveHealthResults();
  renderHealth();
}

function restoreHealthCredential(id) {
  healthHidden.delete(id);
  saveHealthResults();
  renderHealth();
}

function toggleHealth(id) {
  healthOpen.has(id) ? healthOpen.delete(id) : healthOpen.add(id);
  renderHealth();
}

function setHealthModel(id, model) {
  healthChoice.set(id, model);
  renderHealth();
}

function healthProbeJobs(ids, allModels) {
  const groups = [];
  for (const id of ids) {
    const available = healthModelIDs(id);
    const chosen = available.includes(healthChoice.get(id)) ? healthChoice.get(id) : (available[0] || "");
    const models = allModels ? available : (chosen ? [chosen] : []);
    groups.push({ id, models });
  }
  return groups;
}

async function probeHealthCredential(group) {
  if (healthHidden.has(group.id)) return;
  if (!group.models.length) {
    mergeHealthResult({ credential_id: group.id, passed: false, error: "没有可用模型" });
    return;
  }
  const controller = new AbortController();
  healthAbort.set(group.id, controller);
  healthChecking.add(group.id);
  renderHealth();
  try {
    for (const model of group.models) {
      if (controller.signal.aborted || healthHidden.has(group.id)) return;
      const response = await api(`${BASE}/health/run`, {
        method: "POST",
        timeout: 30000,
        signal: controller.signal,
        body: {
          credential_ids: [group.id],
          model_catalog: { [group.id]: healthCatalogs[group.id] || [] },
          models: { [group.id]: model },
          all_models: false,
        },
      });
      if (controller.signal.aborted || healthHidden.has(group.id)) return;
      const result = (response.results || []).find((entry) => entry.credential_id === group.id) || { credential_id: group.id, model, passed: false, error: "没有结果" };
      mergeHealthResult(result);
    }
  } catch (err) {
    if (controller.signal.aborted || err?.name === "AbortError" || healthHidden.has(group.id)) return;
    mergeHealthResult({ credential_id: group.id, passed: false, error: err.message || "检测失败" });
  } finally {
    healthChecking.delete(group.id);
    if (healthAbort.get(group.id) === controller) healthAbort.delete(group.id);
    renderHealth();
  }
}

function startHealth(allModels) {
  if (healthRun) return;
  healthRun = true;
  renderHealth();
  return (async () => {
    try {
      const ids = enabledCredentials().map((credential) => credential.id).filter((id) => !healthHidden.has(id));
      await loadHealthCatalogs(true);
      healthResults = healthResults.filter((result) => !ids.includes(result.credential_id));
      saveHealthResults();
      renderHealth();
      const groups = healthProbeJobs(ids, allModels);
      let cursor = 0;
      const worker = async () => {
        while (cursor < groups.length) {
          const group = groups[cursor++];
          await probeHealthCredential(group);
        }
      };
      await Promise.all(Array.from({ length: Math.min(4, groups.length) }, worker));
      const passed = healthResults.filter((result) => ids.includes(result.credential_id) && result.passed).length;
      const total = healthResults.filter((result) => ids.includes(result.credential_id)).length;
      setNotice("flash", `检测完成：${passed} 个通过，${total - passed} 个不通过`, passed === total ? "info" : "warning");
    } catch (err) {
      if (err instanceof AuthError) return showLogin(err.message);
      setNotice("flash", "健康检测失败：" + err.message);
    } finally {
      healthRun = false;
      renderHealth();
    }
  })();
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
    healthHidden = new Set();
    healthChoice.clear();
    healthOpen.clear();
    healthCatalogs = {};
    healthCatalogStamp = "";
    return upstreamShowLogin(message);
  };
  const upstreamStart = start;
  start = async function startWithHealth() {
    await upstreamStart();
    restoreHealthResults();
    renderHealth();
  };
  const upstreamSwitchTab = switchTab;
  switchTab = function switchTabWithHealth(name) {
    upstreamSwitchTab(name);
    if (name === "health") loadHealthCatalogs(true);
  };
  for (const test of ["candy", "fingerprint", "pelican"]) {
    $(test + "-one-click").addEventListener("click", () => startOneClick(test));
  }
  $("health-run").addEventListener("click", () => startHealth(false));
  $("health-run-all").addEventListener("click", () => startHealth(true));
  $("health-results").addEventListener("click", (e) => {
    const hide = e.target.closest?.("[data-health-hide]");
    if (hide) return hideHealthCredential(hide.getAttribute("data-health-hide"));
    const restore = e.target.closest?.("[data-health-restore]");
    if (restore) return restoreHealthCredential(restore.getAttribute("data-health-restore"));
    if (e.target.closest?.("select, summary")) return;
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
