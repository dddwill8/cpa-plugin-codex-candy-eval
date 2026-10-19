package plugin

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fork-only model aliasing, health checks and one-click runs.
// Upstream handlers stay in their own files. init registers the extra routes
// and splices this page script into the embedded UI, so an upstream refresh
// keeps working as long as the app.js and ui.html seams stay put.

const healthPrompt = "say hi"

// healthProbeBudget is the whole health request, not one model attempt.
// The page aborts api() at 30s, so a stall must already be 不通过 before that.
var healthProbeBudget = 20 * time.Second

//go:embed web/extend.js
var extendScript string

//go:embed web/extend.css
var extendStyles string

func init() {
	uiHTML = bytes.Replace(uiHTML, []byte("/*EXTEND_SCRIPT*/"), []byte(extendScript), 1)
	uiHTML = bytes.Replace(uiHTML, []byte("/*EXTEND_STYLES*/"), []byte(extendStyles), 1)
	routes = append(routes,
		route{http.MethodPost, "/health/run", "Probe enabled credentials with say hi", withJSONBody(healthRunResponse)},
		route{http.MethodPost, "/candy/one-click", "Run candy once on each credential's first model", withJSONBody(func(body []byte) managementResponse { return oneClickResponse("candy", body) })},
		route{http.MethodPost, "/fingerprint/one-click", "Run fingerprint once on each credential's first model", withJSONBody(func(body []byte) managementResponse { return oneClickResponse("fingerprint", body) })},
		route{http.MethodPost, "/pelican/one-click", "Run pelican once on each credential's first model", withJSONBody(func(body []byte) managementResponse { return oneClickResponse("pelican", body) })},
	)
}

func withJSONBody(handle func([]byte) managementResponse) func(managementRequest) managementResponse {
	return func(req managementRequest) managementResponse { return handle(req.Body) }
}

func hiddenModelName(id string) bool {
	return strings.Contains(strings.ToLower(id), "image")
}

// listedModelIDs drops image names, then drops a bare id when another remaining
// id contains "/" and the text after that id's first "/" is exactly the bare id.
// Matching is case-sensitive. Prefixed ids are kept, including two prefixes of one bare id.
func listedModelIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok || hiddenModelName(id) {
			continue
		}
		seen[id] = struct{}{}
		kept = append(kept, id)
	}
	covered := make(map[string]struct{}, len(kept))
	for _, id := range kept {
		if slash := strings.IndexByte(id, '/'); slash >= 0 {
			covered[id[slash+1:]] = struct{}{}
		}
	}
	out := make([]string, 0, len(kept))
	for _, id := range kept {
		if !strings.Contains(id, "/") {
			if _, drop := covered[id]; drop {
				continue
			}
		}
		out = append(out, id)
	}
	return out
}

// firstModel is the first id the model selects would show for this credential.
// Fingerprint passes skipParentheses because that runner rejects "(" and ")".
func firstModel(ids []string, skipParentheses bool) string {
	filtered := listedModelIDs(ids)
	sort.SliceStable(filtered, func(i, j int) bool { return compareModels(filtered[i], filtered[j]) < 0 })
	for _, id := range filtered {
		if skipParentheses && strings.ContainsAny(id, "()") {
			continue
		}
		return id
	}
	return ""
}

func healthPassed(text string, err error) bool {
	return err == nil && strings.TrimSpace(text) != ""
}

var (
	gptHead = regexp.MustCompile(`^gpt-(\d+(?:\.\d+)*)`)
	gptTier = regexp.MustCompile(`^-(astra|sol|terra|luna)`)
)

// compareModels matches catalog.js. Go's regexp has no lookahead, so a tier is
// accepted only when the next character is "(", "-" or the end of the name.
func compareModels(a, b string) int {
	name := func(id string) string {
		id = strings.ToLower(id)
		if slash := strings.LastIndex(id, "/"); slash >= 0 {
			return id[slash+1:]
		}
		return id
	}
	group := func(id string) int {
		switch {
		case strings.HasPrefix(id, "gpt-"):
			return 0
		case strings.SplitN(id, "(", 2)[0] == "codex-auto-review":
			return 1
		case strings.HasPrefix(id, "claude-"):
			return 2
		default:
			return 3
		}
	}
	lexical := func(x, y string) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
	left, right := name(a), name(b)
	if family := group(left) - group(right); family != 0 {
		return family
	}
	if group(left) == 0 {
		lv, lt, lok := matchGeneration(left)
		rv, rt, rok := matchGeneration(right)
		if lok && rok {
			lver, rver := strings.Split(lv, "."), strings.Split(rv, ".")
			n := max(len(lver), len(rver))
			part := func(parts []string, i int) int {
				if i >= len(parts) {
					return 0
				}
				value := 0
				for _, c := range parts[i] {
					value = value*10 + int(c-'0')
				}
				return value
			}
			for i := range n {
				if version := part(rver, i) - part(lver, i); version != 0 {
					return version
				}
			}
			if tier := tierRank(lt) - tierRank(rt); tier != 0 {
				return tier
			}
		}
	}
	primary := lexical(left, right)
	if group(left) < 3 {
		primary = lexical(right, left)
	}
	if primary != 0 {
		return primary
	}
	return lexical(a, b)
}

func tierRank(tier string) int {
	switch tier {
	case "astra":
		return 0
	case "sol":
		return 1
	case "terra":
		return 2
	case "luna":
		return 3
	default:
		return 4
	}
}

func matchGeneration(name string) (version, tier string, ok bool) {
	head := gptHead.FindStringSubmatchIndex(name)
	if head == nil {
		return "", "", false
	}
	rest := name[head[1]:]
	version = name[head[2]:head[3]]
	if found := gptTier.FindStringSubmatchIndex(rest); found != nil {
		after := rest[found[1]:]
		if after == "" || after[0] == '(' || after[0] == '-' {
			return version, rest[found[2]:found[3]], true
		}
	}
	if rest == "" || rest[0] == '(' || rest[0] == '-' {
		return version, "", true
	}
	return "", "", false
}

type healthResult struct {
	CredentialID string `json:"credential_id"`
	Model        string `json:"model,omitempty"`
	Passed       bool   `json:"passed"`
	Text         string `json:"text,omitempty"`
	Error        string `json:"error,omitempty"`
}

func healthRunResponse(body []byte) managementResponse {
	var req struct {
		CredentialIDs []string            `json:"credential_ids"`
		All           bool                `json:"all"`
		ModelCatalog  map[string][]string `json:"model_catalog"`
		Models        map[string]string   `json:"models"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误："+err.Error())
	}
	// Cooling-down credentials stay enabled. Health still probes them; one-click does not.
	auths, err := healthTargets(req.CredentialIDs, req.All)
	if err != nil {
		return jsonError(http.StatusBadGateway, err.Error())
	}
	if len(auths) == 0 {
		return jsonError(http.StatusBadRequest, "没有可检测的已启用凭证")
	}
	ctx, cancel := context.WithTimeout(runCtx, healthProbeBudget)
	defer cancel()
	results := make([]healthResult, len(auths))
	var wg sync.WaitGroup
	for i, auth := range auths {
		model := healthModel(req.ModelCatalog[auth.ID], req.Models[auth.ID])
		if model == "" {
			results[i] = healthResult{CredentialID: auth.ID, Error: "没有可用模型"}
			continue
		}
		wg.Add(1)
		go func(i int, auth credential, model string) {
			defer wg.Done()
			results[i] = probeHealth(ctx, auth, model)
		}(i, auth, model)
	}
	wg.Wait()
	return jsonResponse(http.StatusOK, map[string]any{"results": results})
}

// healthModel uses the caller's choice when it is still in the filtered catalog.
// An empty choice keeps the first filtered model.
func healthModel(catalog []string, chosen string) string {
	chosen = strings.TrimSpace(chosen)
	if chosen == "" {
		return firstModel(catalog, false)
	}
	for _, id := range listedModelIDs(catalog) {
		if id == chosen {
			return chosen
		}
	}
	return ""
}

// healthTargets are enabled credentials, including ones CPA marks unavailable.
// Disabled credentials are left out. selectedCredentials also drops unavailable ones, which one-click still uses.
func healthTargets(ids []string, all bool) ([]credential, error) {
	auths, err := credentials()
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	selected := make([]credential, 0, len(auths))
	for _, auth := range auths {
		if auth.Disabled || (!all && !wanted[auth.ID]) {
			continue
		}
		selected = append(selected, auth)
	}
	return selected, nil
}

// probeHealth sends one say-hi turn on auth. It does not grade candy, fingerprint or pelican,
// and it does not store a test record. A credential already running a test is left alone.
func probeHealth(parent context.Context, auth credential, model string) (result healthResult) {
	result = healthResult{CredentialID: auth.ID, Model: model}
	mu.Lock()
	if running[auth.ID] != nil {
		mu.Unlock()
		result.Model = ""
		result.Error = "正在测试"
		return result
	}
	if parent.Err() != nil || runCtx.Err() != nil {
		mu.Unlock()
		result.Error = "插件正在停止，请稍后重试"
		return result
	}
	ctx, cancel := context.WithCancel(parent)
	running[auth.ID] = &progress{Test: "health", Model: model, Total: 1, cancel: cancel}
	tasks.Add(1)
	mu.Unlock()
	defer func() {
		cancel()
		mu.Lock()
		if current := running[auth.ID]; current != nil && current.Test == "health" {
			delete(running, auth.ID)
		}
		mu.Unlock()
		tasks.Done()
	}()
	out, _, err := askCodex(ctx, auth, model, "none", "", healthPrompt, healthProbeBudget, nil)
	if healthPassed(out.Answer, err) {
		result.Passed = true
		result.Text = truncate(out.Answer, 500)
		return result
	}
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "未完成回答")) {
		result.Error = "检测超时，未收到文本"
		return result
	}
	if err != nil {
		result.Error = truncate(err.Error(), 500)
		return result
	}
	result.Error = "模型未返回文本"
	return result
}

func oneClickResponse(test string, body []byte) managementResponse {
	var req struct {
		CredentialIDs []string            `json:"credential_ids"`
		All           bool                `json:"all"`
		Effort        string              `json:"effort"`
		Concurrency   int                 `json:"concurrency"`
		Runs          int                 `json:"runs"`
		ModelCatalog  map[string][]string `json:"model_catalog"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误："+err.Error())
	}
	if test != "candy" && test != "fingerprint" && test != "pelican" {
		return jsonError(http.StatusBadRequest, "未知测试")
	}
	auths, err := selectedCredentials(req.CredentialIDs, req.All)
	if err != nil {
		return jsonError(http.StatusBadGateway, err.Error())
	}
	if len(auths) == 0 {
		return jsonError(http.StatusBadRequest, "没有可测试的已启用凭证")
	}
	groups := map[string][]string{}
	var order []string
	for _, auth := range auths {
		model := firstModel(req.ModelCatalog[auth.ID], test == "fingerprint")
		if model == "" {
			continue
		}
		if _, ok := groups[model]; !ok {
			order = append(order, model)
		}
		groups[model] = append(groups[model], auth.ID)
	}
	if len(order) == 0 {
		return jsonResponse(http.StatusOK, runSummary{})
	}
	summary := runSummary{}
	for _, model := range order {
		payload := map[string]any{
			"credential_ids": groups[model],
			"model":          model,
			"model_catalog":  req.ModelCatalog,
			"effort":         strings.TrimSpace(req.Effort),
		}
		if test == "candy" {
			payload["runs"] = 1
		}
		if test == "fingerprint" && req.Concurrency > 0 {
			payload["concurrency"] = req.Concurrency
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return jsonError(http.StatusInternalServerError, "编码响应失败："+err.Error())
		}
		var resp managementResponse
		switch test {
		case "candy":
			resp = candyRunResponse(raw)
		case "fingerprint":
			resp = fingerprintRunResponse(raw)
		default:
			resp = pelicanRunResponse(raw)
		}
		if resp.StatusCode != http.StatusOK {
			return resp
		}
		var part runSummary
		if err := json.Unmarshal(resp.Body, &part); err != nil {
			return jsonError(http.StatusInternalServerError, "编码响应失败："+err.Error())
		}
		summary.Started += part.Started
		summary.Busy += part.Busy
		summary.Skipped += part.Skipped
		summary.Unchecked += part.Unchecked
	}
	return jsonResponse(http.StatusOK, summary)
}
