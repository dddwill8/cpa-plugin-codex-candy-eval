package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestAliasAndHealthRule(t *testing.T) {
	got := listedModelIDs([]string{"grok-4.7", "xai/grok-4.7", "kept-bare", "openai/grok-4.7", "grok-4.7(high)", "vendor/IMAGE-preview", "Grok-4.7"})
	want := []string{"xai/grok-4.7", "kept-bare", "openai/grok-4.7", "grok-4.7(high)", "Grok-4.7"}
	if !slices.Equal(got, want) {
		t.Fatalf("listed models = %#v, want %#v", got, want)
	}
	if got := firstModel([]string{"grok-4.7", "xai/grok-4.7"}, false); got != "xai/grok-4.7" {
		t.Fatalf("prefixed alias wins: %s", got)
	}
	if got := firstModel([]string{"kept-bare"}, false); got != "kept-bare" {
		t.Fatalf("bare id stays: %s", got)
	}
	if got := firstModel([]string{"grok-4.7", "xai/grok-4.7", "openai/grok-4.7"}, false); got != "openai/grok-4.7" {
		t.Fatalf("both prefixes stay and sort: %s", got)
	}
	if got := firstModel([]string{"codex-auto-review(high)", "claude-sonnet"}, false); got != "codex-auto-review(high)" {
		t.Fatalf("candy keeps the parenthesis model: %s", got)
	}
	if got := firstModel([]string{"codex-auto-review(high)", "claude-sonnet"}, true); got != "claude-sonnet" {
		t.Fatalf("fingerprint skips parenthesis models: %s", got)
	}
	if got := firstModel([]string{"vendor/IMAGE-preview", "grok-4.7"}, false); got != "grok-4.7" {
		t.Fatalf("an image alias must not hide the bare id: %s", got)
	}
	if got := firstModel(nil, false); got != "" {
		t.Fatalf("empty catalog = %q", got)
	}
	if !healthPassed(" hi ", nil) || healthPassed(" \n\t", nil) || healthPassed("hi", errors.New("request failed")) || healthPassed("hi", context.DeadlineExceeded) {
		t.Fatal("health pass/fail rule mismatch")
	}
	sorted := []string{"gpt-6.1-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6.1-luna", "gpt-6-astra", "gpt-6-sol", "gpt-6-terra", "gpt-6-luna", "gpt-5.6-sol", "codex-auto-review(high)", "codex-auto-review", "claude-sonnet-4.6", "claude-opus-4.6", "claude-opus-4.5", "gemini-pro", "qwen"}
	input := append([]string(nil), sorted...)
	slices.Reverse(input)
	slices.SortStableFunc(input, compareModels)
	if !slices.Equal(input, sorted) {
		t.Fatalf("compareModels = %#v", input)
	}
	versions := []string{"gpt-6.9-sol", "gpt-6.10-sol", "gpt-7-sol"}
	slices.SortStableFunc(versions, compareModels)
	if !slices.Equal(versions, []string{"gpt-7-sol", "gpt-6.10-sol", "gpt-6.9-sol"}) {
		t.Fatalf("version order = %#v", versions)
	}
	prefixed := []string{"gpt-6-luna", "vendor/gpt-6.1-sol", "gpt-6-astra", "vendor/claude-sonnet", "other"}
	slices.SortStableFunc(prefixed, compareModels)
	if !slices.Equal(prefixed, []string{"vendor/gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna", "vendor/claude-sonnet", "other"}) {
		t.Fatalf("prefix order = %#v", prefixed)
	}
}

const extendAuthFiles = `{"files":[
	{"id":"alias","name":"alias","provider":"codex"},
	{"id":"both","name":"both","provider":"codex"},
	{"id":"bare","name":"bare","provider":"codex"},
	{"id":"empty","name":"empty","provider":"codex"},
	{"id":"image","name":"image","provider":"codex"},
	{"id":"bad","name":"bad","provider":"codex"},
	{"id":"blank","name":"blank","provider":"codex"},
	{"id":"off","name":"off","provider":"codex","disabled":true},
	{"id":"down","name":"down","provider":"codex","unavailable":true},
	{"id":"busy","name":"busy","provider":"codex"},
	{"id":"paren","name":"paren","provider":"codex"},
	{"id":"stall","name":"stall","provider":"codex"}
]}`

func extendCatalog() map[string][]string {
	return map[string][]string{
		"alias": {"grok-4.7", "xai/grok-4.7", "vendor/IMAGE-preview"},
		"both":  {"grok-4.7", "xai/grok-4.7", "openai/grok-4.7"},
		"bare":  {"kept-bare"},
		"empty": {},
		"image": {"vendor/IMAGE-preview"},
		"bad":   {"plain-model"},
		"blank": {"plain-model"},
		"off":   {"xai/grok-4.7"},
		"down":  {"xai/grok-4.7"},
		"busy":  {"xai/grok-4.7"},
		"paren": {"codex-auto-review(high)", "claude-sonnet"},
		"stall": {"xai/grok-4.7"},
	}
}

type seenModel struct {
	ID, Model, Prompt, Effort string
}

func installExtendHost(t *testing.T, reply func(id string) (json.RawMessage, error)) *[]seenModel {
	t.Helper()
	var mu sync.Mutex
	calls := []seenModel{}
	hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(extendAuthFiles), nil
		}
		if method != "host.model.execute_stream" {
			t.Errorf("unexpected host call %s", method)
			return nil, fmt.Errorf("unexpected %s", method)
		}
		body, prompt := sentTurn(payload)
		effort, _ := body["reasoning"].(map[string]any)["effort"].(string)
		call := seenModel{
			ID:     payload.(map[string]any)["auth_id"].(string),
			Model:  payload.(map[string]any)["model"].(string),
			Prompt: prompt,
			Effort: effort,
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		return reply(call.ID)
	})
	return &calls
}

func postExtend(t *testing.T, path string, body any) managementResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(managementRequest{Method: http.MethodPost, Path: managementBase + path, Body: raw})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool               `json:"ok"`
		Result managementResponse `json:"result"`
		Error  *EnvelopeError     `json:"error"`
	}
	if err := json.Unmarshal(HandleMethod("management.handle", request), &env); err != nil || !env.OK {
		t.Fatalf("POST %s: ok=%v err=%v decode=%v", path, env.OK, env.Error, err)
	}
	return env.Result
}

func TestHealthProbeUsesFirstModel(t *testing.T) {
	setupTest(t)
	calls := installExtendHost(t, func(id string) (json.RawMessage, error) {
		switch id {
		case "bad":
			return nil, &EnvelopeError{Code: "bad_request", Message: "nope", HTTPStatus: http.StatusBadRequest}
		case "blank":
			return mockModelResponse(" \n\t"), nil
		default:
			return mockModelResponse("hi there"), nil
		}
	})
	mu.Lock()
	running["busy"] = &progress{Test: "candy", Model: "held", cancel: func() {}}
	mu.Unlock()
	response := postExtend(t, "/health/run", map[string]any{
		"credential_ids": []string{"alias", "both", "bare", "empty", "image", "bad", "blank", "off", "down", "busy"},
		"model_catalog":  extendCatalog(),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status %d: %s", response.StatusCode, response.Body)
	}
	var payload struct {
		Results []healthResult `json:"results"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	byID := map[string]healthResult{}
	for _, result := range payload.Results {
		byID[result.CredentialID] = result
	}
	if _, ok := byID["off"]; ok {
		t.Error("disabled credential was probed or shown")
	}
	if !byID["down"].Passed || byID["down"].Model != "xai/grok-4.7" {
		t.Errorf("unavailable credential result = %+v", byID["down"])
	}
	if !byID["alias"].Passed || byID["alias"].Model != "xai/grok-4.7" || byID["alias"].Text != "hi there" {
		t.Errorf("alias result = %+v", byID["alias"])
	}
	if !byID["both"].Passed || byID["both"].Model != "openai/grok-4.7" {
		t.Errorf("both prefixes result = %+v", byID["both"])
	}
	if !byID["bare"].Passed || byID["bare"].Model != "kept-bare" {
		t.Errorf("bare result = %+v", byID["bare"])
	}
	for _, id := range []string{"empty", "image", "bad", "blank", "busy"} {
		if byID[id].Passed {
			t.Errorf("%s shown as passed: %+v", id, byID[id])
		}
	}
	if byID["empty"].Error == "" || byID["image"].Error == "" || byID["busy"].Error != "正在测试" {
		t.Errorf("skipped probes = empty:%+v image:%+v busy:%+v", byID["empty"], byID["image"], byID["busy"])
	}
	probed := map[string]bool{}
	for _, call := range *calls {
		probed[call.ID] = true
		if call.Prompt != healthPrompt {
			t.Errorf("%s prompt = %q", call.ID, call.Prompt)
		}
		if call.Prompt == candyPrompt || call.Prompt == pelicanPrompt {
			t.Errorf("health graded %s", call.ID)
		}
	}
	for _, id := range []string{"alias", "both", "bare", "bad", "blank", "down"} {
		if !probed[id] {
			t.Errorf("%s was not probed", id)
		}
	}
	for _, id := range []string{"empty", "image", "off", "busy"} {
		if probed[id] {
			t.Errorf("%s was sent a request", id)
		}
	}
	if len(records.Candies) != 0 || len(records.Fingerprints) != 0 || len(records.Pelicans) != 0 {
		t.Fatal("health stored a graded test record")
	}
	mu.Lock()
	busy := running["busy"]
	mu.Unlock()
	if busy == nil || busy.Test != "candy" || busy.Model != "held" {
		t.Fatalf("busy credential was restarted: %+v", busy)
	}
}

func TestHealthUsesChosenModel(t *testing.T) {
	setupTest(t)
	calls := installExtendHost(t, func(string) (json.RawMessage, error) {
		return mockModelResponse("hi there"), nil
	})
	response := postExtend(t, "/health/run", map[string]any{
		"credential_ids": []string{"both", "alias"},
		"model_catalog":  extendCatalog(),
		"models":         map[string]string{"both": "xai/grok-4.7", "alias": "grok-4.7"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status %d: %s", response.StatusCode, response.Body)
	}
	var payload struct {
		Results []healthResult `json:"results"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	byID := map[string]healthResult{}
	for _, result := range payload.Results {
		byID[result.CredentialID] = result
	}
	if !byID["both"].Passed || byID["both"].Model != "xai/grok-4.7" {
		t.Errorf("chosen model result = %+v", byID["both"])
	}
	if byID["alias"].Passed || byID["alias"].Error == "" {
		t.Errorf("filtered bare id was accepted: %+v", byID["alias"])
	}
	for _, call := range *calls {
		if call.ID == "alias" || call.Model != "xai/grok-4.7" || call.Prompt != healthPrompt {
			t.Errorf("unexpected call %+v", call)
		}
	}
	if len(*calls) == 0 {
		t.Fatal("chosen model was not called")
	}
}

func TestHealthAllModelsProbesEachFilteredModel(t *testing.T) {
	setupTest(t)
	calls := installExtendHost(t, func(string) (json.RawMessage, error) {
		return mockModelResponse("hi there"), nil
	})
	response := postExtend(t, "/health/run", map[string]any{
		"credential_ids": []string{"both", "image", "off"},
		"model_catalog":  extendCatalog(),
		"all_models":     true,
		"models":         map[string]string{"both": "xai/grok-4.7"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status %d: %s", response.StatusCode, response.Body)
	}
	var payload struct {
		Results []healthResult `json:"results"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, call := range *calls {
		if call.Prompt != healthPrompt {
			t.Errorf("prompt = %q", call.Prompt)
		}
		got[call.ID+" "+call.Model] = true
	}
	if !got["both openai/grok-4.7"] || !got["both xai/grok-4.7"] || got["both grok-4.7"] || len(got) != 2 {
		t.Fatalf("calls = %#v", got)
	}
	var image healthResult
	for _, result := range payload.Results {
		if result.CredentialID == "image" {
			image = result
		}
		if result.CredentialID == "off" {
			t.Fatal("disabled credential was probed")
		}
	}
	if image.Passed || image.Error == "" {
		t.Fatalf("image-only credential = %+v", image)
	}
	if len(records.Candies) != 0 || len(records.Fingerprints) != 0 || len(records.Pelicans) != 0 {
		t.Fatal("all-model health stored a graded test")
	}
}

func TestHealthStalledProbeFailsCredential(t *testing.T) {
	if healthProbeBudget <= 0 || healthProbeBudget >= 30*time.Second {
		t.Fatalf("health budget %s must finish before the page's 30s request timeout", healthProbeBudget)
	}
	setupTest(t)
	healthProbeBudget = time.Second
	installExtendHost(t, func(id string) (json.RawMessage, error) {
		if id == "stall" {
			return nil, nil
		}
		return mockModelResponse("hi there"), nil
	})
	started := time.Now()
	response := postExtend(t, "/health/run", map[string]any{
		"credential_ids": []string{"alias", "stall"},
		"model_catalog":  extendCatalog(),
	})
	if time.Since(started) > 5*time.Second {
		t.Fatal("a stalled say-hi blocked the health request")
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status %d: %s", response.StatusCode, response.Body)
	}
	var payload struct {
		Results []healthResult `json:"results"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	byID := map[string]healthResult{}
	for _, result := range payload.Results {
		byID[result.CredentialID] = result
	}
	if !byID["alias"].Passed || byID["alias"].Text != "hi there" {
		t.Errorf("fast credential = %+v", byID["alias"])
	}
	if byID["stall"].Passed || byID["stall"].Error != "检测超时，未收到文本" {
		t.Errorf("stalled credential = %+v", byID["stall"])
	}
	mu.Lock()
	held := running["stall"]
	mu.Unlock()
	if held != nil {
		t.Fatalf("stalled probe still holds the credential: %+v", held)
	}
	if len(records.Candies) != 0 || len(records.Fingerprints) != 0 || len(records.Pelicans) != 0 {
		t.Fatal("stalled health stored a graded test record")
	}
}

func TestOneClickUsesEachCredentialFirstModel(t *testing.T) {
	setupTest(t)
	calls := installExtendHost(t, func(string) (json.RawMessage, error) {
		return mockModelResponse("答案是 21"), nil
	})
	mu.Lock()
	running["busy"] = &progress{Test: "fingerprint", Model: "held", cancel: func() {}}
	mu.Unlock()
	response := postExtend(t, "/candy/one-click", map[string]any{
		"credential_ids": []string{"alias", "bare", "empty", "paren", "off", "down", "busy"},
		"effort":         "high",
		"runs":           9,
		"model_catalog":  extendCatalog(),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("candy one-click %d: %s", response.StatusCode, response.Body)
	}
	tasks.Wait()
	got := map[string]string{}
	for _, call := range *calls {
		got[call.ID] = call.Model
		if call.Prompt != candyPrompt || call.Effort != "high" {
			t.Errorf("candy call %+v", call)
		}
	}
	if got["alias"] != "xai/grok-4.7" || got["bare"] != "kept-bare" || got["paren"] != "codex-auto-review(high)" {
		t.Fatalf("models = %#v", got)
	}
	for _, id := range []string{"empty", "off", "down", "busy"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s was started", id)
		}
	}
	for _, id := range []string{"alias", "bare", "paren"} {
		if len(records.Candies[id]) != 1 || records.Candies[id][0].Status != "correct" || records.Candies[id][0].Model != got[id] || records.Candies[id][0].Effort != "high" {
			t.Errorf("candy record %s = %+v", id, records.Candies[id])
		}
	}
	if len(records.Candies["busy"]) != 0 || len(records.Candies["empty"]) != 0 || len(records.Fingerprints) != 0 || len(records.Pelicans) != 0 {
		t.Fatal("candy one-click started another test or reported a skipped credential as done")
	}
	mu.Lock()
	busy := running["busy"]
	mu.Unlock()
	if busy == nil || busy.Test != "fingerprint" || busy.Model != "held" {
		t.Fatalf("busy credential was double-started: %+v", busy)
	}

	*calls = nil
	fingerprint := postExtend(t, "/fingerprint/one-click", map[string]any{
		"credential_ids": []string{"paren"},
		"concurrency":    1,
		"model_catalog":  extendCatalog(),
	})
	if fingerprint.StatusCode != http.StatusOK {
		t.Fatalf("fingerprint one-click %d: %s", fingerprint.StatusCode, fingerprint.Body)
	}
	tasks.Wait()
	if len(*calls) == 0 || (*calls)[0].Model != "claude-sonnet" || (*calls)[0].Prompt == candyPrompt || (*calls)[0].Prompt == healthPrompt {
		t.Fatalf("fingerprint calls = %+v", *calls)
	}
	if len(records.Fingerprints["paren"]) != 1 || records.Fingerprints["paren"][0].Model != "claude-sonnet" || len(records.Pelicans) != 0 {
		t.Fatalf("fingerprint records = %+v pelicans = %+v", records.Fingerprints["paren"], records.Pelicans)
	}
	before := len(records.Candies["paren"])

	*calls = nil
	hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(extendAuthFiles), nil
		}
		body, prompt := sentTurn(payload)
		effort, _ := body["reasoning"].(map[string]any)["effort"].(string)
		*calls = append(*calls, seenModel{
			ID:     payload.(map[string]any)["auth_id"].(string),
			Model:  payload.(map[string]any)["model"].(string),
			Prompt: prompt,
			Effort: effort,
		})
		return mockModelResponse("好的：\n```html\n<svg></svg>\n```"), nil
	})
	pelican := postExtend(t, "/pelican/one-click", map[string]any{
		"credential_ids": []string{"alias"},
		"effort":         "medium",
		"model_catalog":  extendCatalog(),
	})
	if pelican.StatusCode != http.StatusOK {
		t.Fatalf("pelican one-click %d: %s", pelican.StatusCode, pelican.Body)
	}
	tasks.Wait()
	if len(*calls) != 1 || (*calls)[0].Model != "xai/grok-4.7" || (*calls)[0].Prompt != pelicanPrompt || (*calls)[0].Effort != "medium" {
		t.Fatalf("pelican calls = %+v", *calls)
	}
	if len(records.Pelicans["alias"]) != 1 || records.Pelicans["alias"][0].Status != "completed" || len(records.Candies["paren"]) != before {
		t.Fatal("pelican one-click changed another test")
	}
}
