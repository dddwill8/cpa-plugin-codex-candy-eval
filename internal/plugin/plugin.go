// Package plugin implements model evaluation and attribution tests for CPA credentials.
package plugin

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	pluginID       = "cpa-codex-candy-eval"
	pluginVersion  = "0.4.0"
	ABIVersion     = 1
	schemaVersion  = 6
	managementBase = "/v0/management/plugins/" + pluginID
	uiPath         = "/v0/resource/plugins/" + pluginID + "/ui"
)

var hostCall func(method string, payload any) (json.RawMessage, error)

func SetHostCall(call func(string, any) (json.RawMessage, error)) {
	hostCall = call
}

//go:embed web/ui.html
var uiTemplate string

//go:embed web/style.css
var uiStyles string

//go:embed web/theme.css
var uiThemes string

//go:embed web/vendor/markdown-it.min.js
var markdownScript string

//go:embed web/vendor/markdown-it-cjk-friendly.min.js
var cjkFriendlyScript string

//go:embed web/vendor/texmath.js
var texmathScript string

//go:embed web/vendor/temml.min.js
var temmlScript string

//go:embed web/vendor/temml.css
var temmlStyles string

//go:embed web/credentials.js
var credentialScript string

//go:embed web/app.js
var appScript string

//go:embed web/catalog.js
var catalogScript string

//go:embed web/components.js
var componentsScript string

//go:embed web/candy.js
var candyScript string

//go:embed web/detection.js
var detectionScript string

//go:embed web/pelican.js
var pelicanScript string

var uiHTML = func() []byte {
	config, _ := json.Marshal(map[string]any{"requests": detectionTarget, "aliases": detectionAliases})
	detection := strings.Replace(detectionScript, `/*DETECTION_CONFIG*/{}`, string(config), 1)
	return []byte(strings.NewReplacer(
		"/*APP_STYLES*/", temmlStyles+"\n"+uiThemes+"\n"+uiStyles,
		"<!--PLUGIN_VERSION-->", pluginVersion,
		"<!--CANDY_PROMPT-->", html.EscapeString(candyPrompt),
		"<!--PELICAN_PROMPT-->", html.EscapeString(pelicanPrompt),
		"<!--DETECTOR_LICENSE-->", "<!-- LM Fingerpoint Detector\n"+detectorLicense+"-->",
		"/*MARKDOWN_SCRIPT*/", markdownScript,
		"/*CJK_FRIENDLY_SCRIPT*/", cjkFriendlyScript,
		"/*TEXMATH_SCRIPT*/", texmathScript,
		"/*TEMML_SCRIPT*/", temmlScript,
		"/*CREDENTIALS_SCRIPT*/", credentialScript,
		"/*CATALOG_SCRIPT*/", catalogScript,
		"/*COMPONENTS_SCRIPT*/", componentsScript,
		"/*CANDY_SCRIPT*/", candyScript,
		"/*DETECTION_SCRIPT*/", detection,
		"/*PELICAN_SCRIPT*/", pelicanScript,
		"/*APP_SCRIPT*/", appScript,
	).Replace(uiTemplate))
}()

// Lucide "candy" icon. Hosts render it in an img element, so the stroke color is fixed.
const logoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#72787c" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><style>@media (prefers-color-scheme: dark) { :root { stroke: #9c9d9b; } }</style><path d="M10 7v10.9"/><path d="M14 6.1V17"/><path d="M16 7V3a1 1 0 0 1 1.707-.707 2.5 2.5 0 0 0 2.152.717 1 1 0 0 1 1.131 1.131 2.5 2.5 0 0 0 .717 2.152A1 1 0 0 1 21 8h-4"/><path d="M16.536 7.465a5 5 0 0 0-7.072 0l-2 2a5 5 0 0 0 0 7.07 5 5 0 0 0 7.072 0l2-2a5 5 0 0 0 0-7.07"/><path d="M8 17v4a1 1 0 0 1-1.707.707 2.5 2.5 0 0 0-2.152-.717 1 1 0 0 1-1.131-1.131 2.5 2.5 0 0 0-.717-2.152A1 1 0 0 1 3 16h4"/></svg>`

var (
	mu    sync.Mutex
	tasks sync.WaitGroup
	// runCtx carries every test run. Quiescing the plugin ends it, which stops the runs, and resuming renews
	// it.
	runCtx, stopRuns = context.WithCancel(context.Background())
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *EnvelopeError) Error() string { return e.Code + ": " + e.Message }

type managementRequest struct {
	Method string     `json:"Method"`
	Path   string     `json:"Path"`
	Query  url.Values `json:"Query"`
	Body   []byte     `json:"Body"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func HandleMethod(method string, request []byte) (response []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = errorEnvelope("plugin_panic", fmt.Sprint(recovered), http.StatusInternalServerError)
		}
	}()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		loadState()
		mu.Lock()
		if runCtx.Err() != nil {
			runCtx, stopRuns = context.WithCancel(context.Background())
		}
		mu.Unlock()
		return okEnvelope(map[string]any{
			"schema_version": schemaVersion,
			"metadata": map[string]any{
				"Name":             pluginID,
				"Version":          pluginVersion,
				"Author":           "haowang02",
				"GitHubRepository": "https://github.com/haowang02/cpa-plugin-codex-candy-eval",
				"Logo":             "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(logoSVG)),
				"ConfigFields":     []any{},
			},
			"capabilities": map[string]bool{"management_api": true},
		})
	case "management.register":
		return okEnvelope(map[string]any{
			"routes": []map[string]string{
				{"Method": http.MethodPost, "Path": managementBase + "/credentials/sync", "Description": "Sync configured credential identities"},
				{"Method": http.MethodGet, "Path": managementBase + "/state", "Description": "View credential test results"},
				{"Method": http.MethodPost, "Path": managementBase + "/run", "Description": "Run the candy test on credentials"},
				{"Method": http.MethodDelete, "Path": managementBase + "/results", "Description": "Clear candy test results"},
				{"Method": http.MethodPost, "Path": managementBase + "/detection/run", "Description": "Run fingerprint detection on credentials"},
				{"Method": http.MethodPost, "Path": managementBase + "/detection/cancel", "Description": "Stop fingerprint detection"},
				{"Method": http.MethodGet, "Path": managementBase + "/detection/record", "Description": "View a fingerprint detection record"},
				{"Method": http.MethodDelete, "Path": managementBase + "/detection/results", "Description": "Clear fingerprint detection history"},
				{"Method": http.MethodPost, "Path": managementBase + "/pelican/run", "Description": "Run the pelican test on credentials"},
				{"Method": http.MethodGet, "Path": managementBase + "/pelican/record", "Description": "View a pelican animation"},
				{"Method": http.MethodDelete, "Path": managementBase + "/pelican/results", "Description": "Clear pelican history"},
				{"Method": http.MethodDelete, "Path": managementBase + "/all/results", "Description": "Clear all test history"},
				{"Method": http.MethodPost, "Path": managementBase + "/inspection", "Description": "Inspect credentials with the candy test or fingerprint detection"},
				{"Method": http.MethodGet, "Path": managementBase + "/inspection", "Description": "View the latest inspection of a test"},
			},
			"resources": []map[string]string{
				{"Path": uiPath, "Menu": "Codex 降智测试", "Description": "通过糖果题、指纹检测和画鹈鹕检测 CPA 凭证"},
			},
		})
	case "management.handle":
		var req managementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest)
		}
		return okEnvelope(handleManagement(req))
	case "plugin.quiesce":
		Quiesce()
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "Unsupported plugin method: "+method, http.StatusNotFound)
	}
}

func handleManagement(req managementRequest) managementResponse {
	path := strings.TrimRight(req.Path, "/")
	switch {
	case req.Method == http.MethodGet && path == uiPath:
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-store"},
				"X-Content-Type-Options":  {"nosniff"},
				"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; font-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
			},
			Body: uiHTML,
		}
	case req.Method == http.MethodPost && path == managementBase+"/credentials/sync":
		return syncCredentialsResponse(req.Body)
	case req.Method == http.MethodGet && path == managementBase+"/state":
		return stateResponse()
	case req.Method == http.MethodPost && path == managementBase+"/run":
		return candyRunResponse(req.Body)
	case req.Method == http.MethodDelete && path == managementBase+"/results":
		return clearHistoryResponse("candy")
	case req.Method == http.MethodPost && path == managementBase+"/detection/run":
		return detectionRunResponse(req.Body)
	case req.Method == http.MethodPost && path == managementBase+"/detection/cancel":
		return detectionCancelResponse(req.Body)
	case req.Method == http.MethodGet && path == managementBase+"/detection/record":
		return recordResponse("detection", req.Query)
	case req.Method == http.MethodDelete && path == managementBase+"/detection/results":
		return clearHistoryResponse("detection")
	case req.Method == http.MethodPost && path == managementBase+"/pelican/run":
		return pelicanRunResponse(req.Body)
	case req.Method == http.MethodGet && path == managementBase+"/pelican/record":
		return recordResponse("pelican", req.Query)
	case req.Method == http.MethodDelete && path == managementBase+"/pelican/results":
		return clearHistoryResponse("pelican")
	case req.Method == http.MethodDelete && path == managementBase+"/all/results":
		return clearHistoryResponse("all")
	case req.Method == http.MethodPost && path == managementBase+"/inspection":
		return inspectionRunResponse(req.Body)
	case req.Method == http.MethodGet && path == managementBase+"/inspection":
		return inspectionResponse(req.Query.Get("test"))
	default:
		return jsonError(http.StatusNotFound, "Route not found: "+req.Method+" "+req.Path)
	}
}

func stateResponse() managementResponse {
	auths, err := credentials()
	if err != nil {
		return jsonError(http.StatusBadGateway, err.Error())
	}
	// Host calls must stay outside the results lock.
	for i := range auths {
		if auths[i].Source == credentialSourceFile && auths[i].Provider == "codex" {
			auths[i].PlanType = authPlanType(auths[i])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	views := make([]credentialView, 0, len(auths))
	for _, auth := range auths {
		view := credentialView{credential: auth, Results: candyResults[auth.ID]}
		if view.Results == nil {
			view.Results = []candyResult{}
		}
		view.Running = candyRunning[auth.ID]
		view.DetectionRunning = detectionRunning[auth.ID]
		view.Detections = summaries(detectionResults[auth.ID])
		view.PelicanRunning = pelicanRunning[auth.ID]
		view.Pelicans = summaries(pelicanResults[auth.ID])
		views = append(views, view)
	}
	return jsonResponse(http.StatusOK, map[string]any{"auths": views, "storage_error": storageError})
}

func summaries[T interface{ summary() T }](records []T) []T {
	out := make([]T, len(records))
	for i, record := range records {
		out[i] = record.summary()
	}
	return out
}

func Quiesce() {
	mu.Lock()
	stopRuns()
	for _, p := range detectionRunning {
		p.Phase = "cancelling"
	}
	mu.Unlock()
	tasks.Wait()
}

func mustDecode[T any](data []byte) T {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		panic(err)
	}
	return value
}

func truncate(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

func okEnvelope(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return errorEnvelope("encode_failed", err.Error(), http.StatusInternalServerError)
	}
	data, _ := json.Marshal(Envelope{OK: true, Result: raw})
	return data
}

func errorEnvelope(code, message string, status int) []byte {
	data, _ := json.Marshal(Envelope{Error: &EnvelopeError{Code: code, Message: message, HTTPStatus: status}})
	return data
}

func jsonResponse(status int, v any) managementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "编码响应失败："+err.Error())
	}
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, Body: body}
}

func jsonError(status int, message string) managementResponse {
	return jsonResponse(status, map[string]any{"error": map[string]string{"message": message}})
}
