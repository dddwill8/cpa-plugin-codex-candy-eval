package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// detectionAnswer is a gpt-6.1-sol reference answer from the golden cases, so it moves like a model's answer.
var detectionAnswer = func() string {
	raw, err := os.ReadFile("testdata/lm-detector-golden.json")
	if err != nil {
		panic(err)
	}
	return mustDecode[[]struct{ Outputs []detectionOutput }](raw)[0].Outputs[0].Text
}()

func TestDetectionUpstreamParity(t *testing.T) {
	// Golden results come from the pinned upstream analyzeSharedOutputs with allowPartial, as its page runs it:
	// two calibrated rankings, a partial ranking and answers that do not move like a model's.
	raw, err := os.ReadFile("testdata/lm-detector-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Outputs  []detectionOutput
		Expected detectionAttribution
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	close := func(a, b *float64) {
		t.Helper()
		if (a == nil) != (b == nil) || a != nil && !(math.Abs(*a-*b) <= 1e-9) {
			t.Fatalf("got %v, want %v", a, b)
		}
	}
	for _, tt := range cases {
		got, err := analyzeDetection(tt.Outputs)
		if err != nil {
			t.Fatal(err)
		}
		want := tt.Expected
		if got.Decision != want.Decision || got.Prediction != want.Prediction || got.PredictionName != want.PredictionName || got.FamilyPrediction != want.FamilyPrediction ||
			got.Used != want.Used || got.Sequence.ModelLike != want.Sequence.ModelLike || !reflect.DeepEqual(got.Diagnostics, want.Diagnostics) || len(got.Results) != len(want.Results) {
			t.Fatalf("attribution mismatch: %+v", got)
		}
		close(got.Probability, want.Probability)
		close(&got.Sequence.Autocorrelation, &want.Sequence.Autocorrelation)
		close(&got.Sequence.SmallStepShare, &want.Sequence.SmallStepShare)
		for i, r := range got.Results {
			w := want.Results[i]
			if r.Model != w.Model || r.DisplayName != w.DisplayName || r.Family != w.Family || r.FamilyName != w.FamilyName {
				t.Fatalf("rank %d: %+v, want %+v", i, r, w)
			}
			close(r.Probability, w.Probability)
			close(&r.Score, &w.Score)
		}
	}
}

func TestDetectionParseAndChallenges(t *testing.T) {
	for text, want := range map[string][]int{
		"生成 300 个：1, 22, 355, 0, 356, 99。完成 4": {1, 22, 355, 99},
		"1,2 words 3,4": {1, 2},
		"1,2 中文 3,4,5":  {3, 4, 5},
		"1,2 😀 3,4":     {1, 2, 3, 4},
	} {
		if got := parseNumbers(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("parse %q = %v, want %v", text, got, want)
		}
	}
	if _, err := analyzeDetection([]detectionOutput{{strings.Repeat("42,", 164), 300}}); err == nil {
		t.Fatal("short output was accepted")
	}
	if got, err := analyzeDetection([]detectionOutput{{strings.Repeat("42,", 165), 300}}); err != nil || got.Used != 1 || got.Decision != "not_model" || got.Results != nil {
		t.Fatalf("repeated numbers = %+v, %v", got, err)
	}
	instructed, prefixed := 0, 0
	for _, c := range detectionSuite {
		if c.Instructions != "" {
			instructed++
		}
		if strings.Contains(c.Prompt, "\n\nFinal task:\n") {
			prefixed++
		}
	}
	if len(detectionSuite) != 36 || instructed != 12 || prefixed != 9 {
		t.Fatalf("suite: %d challenges, %d with instructions, %d with a user prefix", len(detectionSuite), instructed, prefixed)
	}
	seen := map[string]bool{}
	for _, c := range detectionChallenges() {
		if seen[c.Prompt] || !slices.Contains(detectionSuite, c) {
			t.Fatalf("bad challenge: %+v", c)
		}
		seen[c.Prompt] = true
	}
	if detectionModelID("openai/GPT-6.1-sol") != "gpt-6-astra" || detectionModelID("deepseek/deepseek-flash") != "deepseek-v4.1-flash-1017" || detectionModelID("gpt-6-sol") != "gpt-6-sol" {
		t.Fatal("aliased models must compare as their candidates")
	}
}

func detectionTestHost(t *testing.T, answer func() (string, int)) {
	t.Helper()
	hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"id":"a","name":"a","provider":"codex"},{"id":"off","name":"off","provider":"codex","disabled":true}]}`), nil
		}
		if method != "host.model.execute_stream" {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		body, prompt := sentTurn(payload)
		if body["reasoning"].(map[string]any)["effort"] != nil || !strings.Contains(prompt, "355") {
			t.Error("unexpected challenge parameters")
		}
		text, status := answer()
		if status != http.StatusOK {
			return nil, &EnvelopeError{Code: "upstream", Message: text, HTTPStatus: status}
		}
		return mockModelResponse(text), nil
	})
}

func TestDetectionRunAndPersistence(t *testing.T) {
	setupTest(t)
	var calls atomic.Int32
	// The first challenge is refused on every attempt.
	detectionTestHost(t, func() (string, int) {
		if calls.Add(1) <= modelAttempts {
			return "无法回答", 200
		}
		return detectionAnswer, 200
	})
	candyResults["a"] = []candyResult{{Model: "preserved"}}
	pelicanResults["a"] = []pelicanResult{{Model: "preserved"}}
	response := detectionRunResponse([]byte(`{"all":true,"model":"test-model","concurrency":1}`))
	if response.StatusCode != 200 {
		t.Fatalf("run: %s", response.Body)
	}
	tasks.Wait()
	r := detectionResults["a"][0]
	if calls.Load() != 5 || r.Status != "partial" || r.Attribution.Decision != "partial" || r.Attribution.Prediction == "" || r.Attribution.Used != 2 || len(r.Samples) != 3 ||
		r.Samples[0].Attempts != modelAttempts || r.Samples[0].Error != "有效数字不足" || r.Samples[0].Text != "无法回答" ||
		r.Concurrency != 1 || r.InputTokens != 60 || r.OutputTokens != 15 || r.ReasoningTokens != 5 {
		t.Fatalf("result=%+v calls=%d", r, calls.Load())
	}
	if len(detectionResults["off"]) != 0 {
		t.Fatal("disabled credential ran")
	}
	detectionResults = map[string][]detectionResult{}
	loaded = false
	loadState()
	if len(detectionResults["a"]) != 1 || detectionResults["a"][0].Attribution.Prediction != r.Attribution.Prediction || detectionResults["a"][0].ReasoningTokens != 5 || detectionResults["a"][0].InputTokens != 60 || detectionResults["a"][0].OutputTokens != 15 {
		t.Fatal("history did not survive reload")
	}
	if summary := r.summary(); summary.Samples != nil || summary.Attribution.Results != nil || summary.Attribution.Prediction != r.Attribution.Prediction || len(r.Samples) != 3 {
		t.Fatal("summaries must drop details without touching stored records")
	}
	detail := handleManagement(managementRequest{Method: http.MethodGet, Path: managementBase + "/detection/record", Query: url.Values{"auth_id": {"a"}, "id": {r.ID}}})
	if detail.StatusCode != 200 || !bytes.Contains(detail.Body, []byte(`"samples":[`)) {
		t.Fatalf("record = %s", detail.Body)
	}
	if recordResponse("detection", url.Values{"auth_id": {"a"}, "id": {"missing"}}).StatusCode != http.StatusNotFound {
		t.Fatal("missing record was found")
	}
	if clearHistoryResponse("candy").StatusCode != 200 || len(detectionResults["a"]) != 1 {
		t.Fatal("candy clear removed detection history")
	}
	if clearHistoryResponse("detection").StatusCode != 200 || len(detectionResults) != 0 || len(pelicanResults["a"]) != 1 {
		t.Fatal("detection clear affected other history")
	}
}

// Refused answers are asked again; a rejected request is final.
func TestDetectionLimitsAndHardFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
		calls  int32
	}{{200, "有效数字不足", detectionTarget * modelAttempts}, {401, "拒答", detectionTarget}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			setupTest(t)
			var calls atomic.Int32
			detectionTestHost(t, func() (string, int) { calls.Add(1); return "拒答", tc.status })
			detectionRunResponse([]byte(`{"all":true,"model":"test-model"}`))
			tasks.Wait()
			if r := detectionResults["a"][0]; calls.Load() != tc.calls || r.Status != "failed" || !strings.Contains(r.Error, tc.reason) {
				t.Fatalf("calls=%d result=%+v", calls.Load(), r)
			}
		})
	}
}

func TestDetectionCancellationAndBusy(t *testing.T) {
	setupTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	detectionTestHost(t, func() (string, int) { calls.Add(1); close(entered); <-release; return strings.Repeat("42,", 320), 200 })
	detectionRunResponse([]byte(`{"all":true,"model":"test-model","concurrency":1}`))
	<-entered
	for _, run := range []func([]byte) managementResponse{detectionRunResponse, candyRunResponse} {
		r := run([]byte(`{"all":true,"model":"test-model"}`))
		var summary runSummary
		_ = json.Unmarshal(r.Body, &summary)
		if summary.Busy != 1 {
			close(release)
			t.Fatalf("busy check failed: %s", r.Body)
		}
	}
	detectionCancelResponse([]byte(`{"auth_ids":["a"]}`))
	close(release)
	tasks.Wait()
	if calls.Load() != 1 || detectionResults["a"][0].Status != "cancelled" || len(detectionRunning) != 0 || detectionResults["a"][0].InputTokens != 12 || detectionResults["a"][0].OutputTokens != 3 || detectionResults["a"][0].ReasoningTokens != 1 {
		t.Fatal("cancellation failed")
	}
}

func TestDetectionStopDuringLastRequest(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		status             int
		answered           int64
	}{
		{"completed", strings.Repeat("42,", 320), "completed", 200, 3},
		{"invalid_answer", "拒答", "cancelled", 200, 3},
		{"terminal_error", "", "partial", 401, 2},
		{"interrupted_retry", "", "cancelled", 503, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTest(t)
			var calls atomic.Int32
			detectionTestHost(t, func() (string, int) {
				if calls.Add(1) == detectionTarget {
					detectionCancelResponse([]byte(`{"auth_ids":["a"]}`))
					return tc.answer, tc.status
				}
				return strings.Repeat("42,", 320), 200
			})
			detectionRunResponse([]byte(`{"all":true,"model":"test-model","concurrency":1}`))
			tasks.Wait()
			r := detectionResults["a"][0]
			if calls.Load() != detectionTarget || r.Status != tc.want || len(r.Samples) != detectionTarget || r.InputTokens != tc.answered*12 || r.OutputTokens != tc.answered*3 || r.ReasoningTokens != tc.answered {
				t.Fatalf("calls=%d result=%+v", calls.Load(), r)
			}
		})
	}
}

func TestDetectionValidationAndHistoryProtection(t *testing.T) {
	setupTest(t)
	for _, body := range []string{`{`, `{"model":""}`, `{"model":"m(high)"}`, `{"model":"m","concurrency":4}`, `{"model":"m","concurrency":-1}`} {
		if r := detectionRunResponse([]byte(body)); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("accepted %s", body)
		}
	}
	detectionTestHost(t, func() (string, int) { t.Error("unsupported model requested"); return "", 500 })
	detectionRunResponse([]byte(`{"all":true,"model":"test-model","model_catalog":{"a":[]}}`))
	if detectionResults["a"][0].Status != "skipped" {
		t.Fatal("unsupported model not skipped")
	}
	for i := range historyLimit + 5 {
		appendDetectionResult("a", detectionResult{ID: fmt.Sprint(i)})
	}
	if len(detectionResults["a"]) != historyLimit || detectionResults["a"][0].ID != "5" {
		t.Fatal("history not bounded")
	}
	stateLoadError = fmt.Errorf("corrupt state")
	if r := clearHistoryResponse("detection"); r.StatusCode != 500 || len(detectionResults["a"]) != historyLimit {
		t.Fatal("failed clear lost history")
	}
	ctx, cancel := context.WithCancel(runCtx)
	detectionRunning["a"] = &detectionProgress{cancel: cancel}
	Quiesce()
	if ctx.Err() == nil || detectionRunning["a"].Phase != "cancelling" {
		t.Fatal("quiesce did not cancel the detection")
	}
}

func TestDetectionConcurrency(t *testing.T) {
	for _, concurrency := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			setupTest(t)
			want := concurrency
			if want == 0 {
				want = detectionTarget
			}
			started, release := make(chan struct{}, 3), make(chan struct{})
			var active, peak atomic.Int32
			detectionTestHost(t, func() (string, int) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				<-release
				return strings.Repeat("42,", 320), 200
			})
			detectionRunResponse([]byte(fmt.Sprintf(`{"all":true,"model":"test-model","concurrency":%d}`, concurrency)))
			for i := 0; i < want; i++ {
				select {
				case <-started:
				case <-time.After(time.Second):
					close(release)
					tasks.Wait()
					t.Fatal("configured challenges did not start concurrently")
				}
			}
			select {
			case <-started:
				close(release)
				tasks.Wait()
				t.Fatal("per-credential concurrency exceeded")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			tasks.Wait()
			r := detectionResults["a"][0]
			if peak.Load() != int32(want) || r.Concurrency != want || r.Status != "completed" || len(r.Samples) != 3 {
				t.Fatalf("peak=%d result=%+v", peak.Load(), r)
			}
		})
	}
}

func TestDetectionAssetsEmbedded(t *testing.T) {
	for _, content := range []string{`id="tab-detection"`, `id="dt-principle"`, `"requests":3`, `"gpt-6.1-sol":"gpt-6-astra"`, "https://github.com/Ikaleio/lm-detector", detectorLicense} {
		if !strings.Contains(string(uiHTML), content) {
			t.Fatalf("missing embedded content: %q", content)
		}
	}
	if strings.Contains(string(uiHTML), "/*DETECTION_") {
		t.Fatal("unresolved detection asset placeholder")
	}
}
