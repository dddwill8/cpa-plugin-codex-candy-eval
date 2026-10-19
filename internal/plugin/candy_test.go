package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestHasStandalone21(t *testing.T) {
	for text, want := range map[string]bool{
		"21": true, "最少取出**21**个": true, "答案：21。": true,
		"121": false, "210": false, "2 1": false,
	} {
		if got := hasStandalone21(text); got != want {
			t.Errorf("hasStandalone21(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestEvaluateCandy(t *testing.T) {
	setupTest(t)
	var body map[string]any
	var prompt string
	hostCall = streamHost(func(_ string, payload any) (json.RawMessage, error) {
		body, prompt = sentTurn(payload)
		return mockModelResponse("答案是 21"), nil
	})
	r := evaluateCandy(context.Background(), credential{ID: "a", Provider: "claude"}, "m", "xhigh")
	if r.Status != "correct" || r.Answer != "答案是 21" || r.InputTokens != 12 || r.OutputTokens != 3 || r.ReasoningTokens != 1 ||
		prompt != candyPrompt || body["reasoning"].(map[string]any)["effort"] != "xhigh" {
		t.Fatalf("result = %+v, body = %v", r, body)
	}
	// Unsupported efforts stay as sent and fail the request, which is neither retried nor graded.
	hostCall = func(string, any) (json.RawMessage, error) {
		return nil, &EnvelopeError{Code: "unsupported_effort", Message: "21", HTTPStatus: 400}
	}
	if r := evaluateCandy(context.Background(), credential{ID: "a", Provider: "claude"}, "m", "xhigh"); r.Status != "failed" || r.Error == "" || r.Attempts != 1 {
		t.Fatalf("failed request result = %+v", r)
	}
}

func TestCandyRunScope(t *testing.T) {
	setupTest(t)
	configuredCredentials["config"] = credential{ID: "config", Provider: "openai-compatible-demo", Source: "ai_providers"}
	hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"id":"skip","provider":"claude"},{"id":"unknown","provider":"gemini"},{"id":"off","provider":"codex","disabled":true}]}`), nil
		}
		if method != "host.model.execute_stream" {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		var req struct {
			ID       string `json:"auth_id"`
			Provider string `json:"forced_provider"`
		}
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &req)
		if (req.ID != "config" || req.Provider != "openai-compatible-demo") && (req.ID != "unknown" || req.Provider != "gemini") {
			t.Errorf("unpinned or skipped request: %+v", req)
		}
		if body, _ := sentTurn(payload); body["reasoning"].(map[string]any)["effort"] != nil {
			t.Error("none sent a reasoning effort")
		}
		return mockModelResponse("21"), nil
	})
	response := candyRunResponse([]byte(`{"all":true,"model":"alias","effort":"none","runs":99,"model_catalog":{"skip":[],"config":["alias"]}}`))
	if string(response.Body) != `{"started":2,"skipped":1,"unchecked":1}` {
		t.Fatalf("run: %s", response.Body)
	}
	tasks.Wait()
	if len(records.Candies["skip"]) != 1 || records.Candies["skip"][0].Status != "skipped" || records.Candies["skip"][0].DurationMS != 0 {
		t.Fatal("skip was executed or not recorded")
	}
	if len(records.Candies["config"]) != candyMaxRuns || len(records.Candies["unknown"]) != candyMaxRuns || len(records.Candies["off"]) != 0 {
		t.Fatal("incorrect run scope")
	}
	records, loaded = emptyState(), false
	loadState()
	if records.Candies["skip"][0].Status != "skipped" || records.Candies["config"][0].Status != "correct" {
		t.Fatal("new credential history was not restored")
	}
}
