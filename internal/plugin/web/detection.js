"use strict";
const DT_CONFIG = /*DETECTION_CONFIG*/{};
const dtSelected = new Set();
const dtExpanded = new Set();
const dtPercent = (value) => Number.isFinite(value) && value >= 0 && value <= 1 ? `${(value * 100).toFixed(1)}%` : "—";
const dtConcurrency = () => boundedInput("dt-concurrency", DT_CONFIG.requests, DT_CONFIG.requests);

// The candidate a model name stands for, as detectionModelID compares them on the server.
function dtModelID(model) {
  const id = String(model || "").trim().toLowerCase().split("/").pop();
  return Object.hasOwn(DT_CONFIG.aliases, id) ? DT_CONFIG.aliases[id] : id;
}

function dtComparison(r) {
  const a = r?.attribution;
  if (!a || !["completed", "partial"].includes(r.status)) return { tone: "neutral", symbol: "chart", label: "" };
  if (a.decision === "not_model") return { tone: "warn", symbol: "circle-alert", label: "不像语言模型回答" };
  return dtModelID(r.model) === dtModelID(a.prediction)
    ? { tone: "ok", symbol: "circle-check", label: "与测试模型一致" }
    : { tone: "warn", symbol: "shuffle", label: "与测试模型不一致" };
}

const DT_STATES = { failed: ["bad", "circle-x", "测试失败", "查看详情了解原因"], cancelled: ["neutral", "circle-pause", "测试已停止", ""], skipped: ["neutral", "ban", "已跳过", "此凭证不支持所选模型"] };

function dtOutcome(r) {
  const a = r?.attribution, state = DT_STATES[r?.status];
  if (state || !a) {
    const [tone, symbol, title, detail] = state || ["idle", "fingerprint", "等待测试", ""];
    return resultOutcome({ tone, symbol, titleHTML: title, detailHTML: detail });
  }
  const { tone, symbol, label } = dtComparison(r);
  if (a.decision === "not_model") return resultOutcome({ tone, symbol, titleHTML: label, detailHTML: "相邻数字的变化方式与语言模型不同，未生成排名" });
  const probability = a.probability != null ? `<span class="dt-probability mono">${dtPercent(a.probability)}</span>` : "";
  return resultOutcome({
    tone, symbol,
    titleHTML: `<span class="dt-prediction"><span>${esc(a.prediction_name)}</span>${probability}</span>`,
    detailHTML: `${label}${r.status === "partial" ? " · 部分结果" : ""}`,
  });
}

function renderDetectionRow(a) {
  const history = a.detections || [], last = history.at(-1), p = a.detection_running;
  const latest = p ? collectionOutcome(p, DT_CONFIG.requests) : dtOutcome(last);
  return credentialRow({
    type: "dt", credential: a, selected: dtSelected.has(a.id), open: dtExpanded.has(a.id), selectable: runnable("dt-", a), button: collectionButton("dt", a, p, "测试"),
    result: latest, tested: !!(last || p),
    meta: `<div class="dt-test-model mono">${esc(p?.model || last?.model || "—")}</div>${listTime(!p && last?.time)}`,
    history: () => [...history].reverse().map((r) => historyEntry("dt", a.id, r, dtOutcome(r))).join(""),
  });
}

function renderDetection() {
  renderSelection("dt-", dtSelected, "测试");
  const selected = selectedCredentials(dtSelected).filter(availableCredential);
  $("dt-cost").textContent = `每个凭证 ${DT_CONFIG.requests} 次请求（失败或数字不足时重试）${selected.length ? ` · 已选 ${selected.length} 个，共 ${DT_CONFIG.requests * selected.length} 次` : ""}`;
  $("dt-clear").disabled = pending || !credentials.some((a) => a.detections?.length);
  renderList("dt-", renderDetectionRow);
}

function runDetection(body) {
  if (!$("dt-toolbar").reportValidity()) return;
  return update("/detection/run", { method: "POST", body: { ...body, model: $("dt-model").value, concurrency: dtConcurrency() } }, "开始测试失败");
}

function dtResultHTML(r) {
  const a = r.attribution, comparison = dtComparison(r), scored = a.probability != null;
  if (a.decision === "not_model") return `${dtOutcome(r)}<p class="detail-note">相邻数字的变化方式与参考库中的语言模型回答不同，常见于程序生成、排序或打乱顺序的数字，因此未生成排名。</p>`;
  return `<dl class="dt-summary ${comparison.tone}"><div><dt>${comparison.label ? `${icon(comparison.symbol)}${comparison.label}` : "最接近的模型"}</dt><dd class="mono">${esc(a.prediction_name)}${scored ? `<small>${dtPercent(a.probability)}</small>` : ""}</dd></div><div><dt>模型家族</dt><dd>${esc(a.family_prediction_name)}</dd></div><div><dt>有效回答</dt><dd class="mono">${a.used_outputs} / ${DT_CONFIG.requests}</dd></div></dl>
    <div class="dt-result-heading"><h3>候选模型排行</h3><span class="meta">${scored ? "归因概率" : "有效回答不足三条，不计算归因概率"}</span></div>
    <div class="dt-ranking">${(a.results || []).map((c, i) => `<div class="dt-rank"><span class="meta mono">${String(i + 1).padStart(2, "0")}</span><span class="mono dt-rank-name">${esc(c.display_name)}</span><div class="dt-bar" aria-hidden="true"><span style="width:${Math.max(0, Math.min(100, (c.probability || 0) * 100))}%"></span></div><span class="mono">${dtPercent(c.probability)}</span></div>`).join("")}</div>`;
}

const dtSamplePart = (label, text, mono = false) => `<div class="dt-sample-part"><div class="meta">${label}</div><pre${mono ? ' class="mono"' : ""}>${esc(text)}</pre></div>`;

function showDetectionDetail(r) {
  const samples = r.samples || [];
  const sampleEntries = samples.map((s, i) => `<details class="dt-sample">
    <summary>${icon("chevron-right", "chev")}挑战 ${i + 1}<span class="meta">${s.parsed_numbers} / ${s.expected_count} 个数字${s.attempts > 1 ? ` · 重试 ${s.attempts - 1} 次` : ""}</span><span class="verdict ${s.accepted ? "ok" : "err"}">${s.accepted ? "有效" : "未计入"}</span></summary>
    ${s.instructions ? dtSamplePart("开发者指令", s.instructions) : ""}${dtSamplePart("提示词", s.prompt)}${s.text ? dtSamplePart("回答", s.text, true) : ""}${s.error ? `<p class="detail-warning">${esc(s.error)}</p>` : ""}
  </details>`).join("");
  $("dt-detail-body").innerHTML = `<p class="result-dialog-meta"><span class="mono">${esc(r.model)}</span> · ${esc(fmtTime(r.time))} · ${fmtSec(r.duration_ms)}${r.concurrency ? ` · 并发 ${esc(r.concurrency)}` : ""}</p>
    ${r.attribution && r.status !== "completed" ? `<p class="detail-note">${{ partial: "部分结果", cancelled: "测试已停止" }[r.status]}</p>` : ""}
    ${r.attribution ? dtResultHTML(r) : dtOutcome(r)}
    ${r.error ? `<p class="detail-note detail-warning">${esc(r.error)}</p>` : ""}
    ${samples.length ? `<div class="dt-result-heading"><h3>挑战记录</h3><span class="meta">${samples.reduce((n, s) => n + s.attempts, 0)} 次请求</span></div>${sampleEntries}` : ""}`;
}

function initializeDetection() {
  $("dt-concurrency").max = DT_CONFIG.requests;
  $("dt-concurrency").value = preference("dt-concurrency", DT_CONFIG.requests);
  $("dt-concurrency").value = dtConcurrency();
  $("dt-model").addEventListener("change", renderDetection);
  $("dt-concurrency").addEventListener("change", () => { $("dt-concurrency").value = dtConcurrency(); });
  bindCollectionActions({ type: "dt", scope: "detection", expanded: dtExpanded, renderRows: renderDetection, run: runDetection, showDetail: showDetectionDetail });
}
