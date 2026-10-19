package plugin

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Each run sends detectionTarget challenges, at most that many at once per credential and
// detectionGlobalConcurrency across credentials. A challenge holds its slot through its retries.
const (
	detectionTarget            = 3
	detectionGlobalConcurrency = 6
	detectionMaxText           = 64000
)

type detectionChallenge struct {
	Instructions  string `json:"instructions,omitempty"`
	Prompt        string `json:"prompt"`
	ExpectedCount int    `json:"expected_count"`
}

//go:embed data/lm-detector/enrollment-suite.json
var detectionSuiteJSON []byte

// detectionSuite holds the challenges the reference answers were collected with: 12 prompt environments that
// add a system prompt, a user prefix or nothing to their three challenges.
var detectionSuite = func() []detectionChallenge {
	var suite []detectionChallenge
	for _, c := range mustDecode[[]struct {
		System        string `json:"system"`
		UserPrefix    string `json:"user_prefix"`
		Prompt        string `json:"prompt"`
		ExpectedCount int    `json:"expected_count"`
	}](detectionSuiteJSON) {
		prompt := c.Prompt
		if c.UserPrefix != "" {
			prompt = c.UserPrefix + "\n\nFinal task:\n" + c.Prompt
		}
		suite = append(suite, detectionChallenge{c.System, prompt, c.ExpectedCount})
	}
	return suite
}()

func detectionChallenges() []detectionChallenge {
	challenges := make([]detectionChallenge, detectionTarget)
	for i, j := range rand.Perm(len(detectionSuite))[:detectionTarget] {
		challenges[i] = detectionSuite[j]
	}
	return challenges
}

// detectionAliases maps tested models to the candidate that stands for them: the bank enrolls gpt-6.1-sol
// answers under gpt-6-astra, and deepseek-flash serves the model enrolled as deepseek-v4.1-flash-1017.
// The page compares models with the same table.
var detectionAliases = map[string]string{"gpt-6.1-sol": "gpt-6-astra", "deepseek-flash": "deepseek-v4.1-flash-1017"}

// detectionModelID is the candidate a model name stands for, without a provider prefix such as "openai/".
func detectionModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = model[strings.LastIndex(model, "/")+1:]
	if alias, ok := detectionAliases[model]; ok {
		return alias
	}
	return model
}

type detectionSample struct {
	detectionChallenge
	Attempts int    `json:"attempts,omitempty"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
	Parsed   int    `json:"parsed_numbers"`
	Valid    bool   `json:"accepted"`
}

type detectionResult struct {
	DetectorRevision string                `json:"detector_revision"`
	ID               string                `json:"id"`
	Time             time.Time             `json:"time"`
	Model            string                `json:"model"`
	Concurrency      int                   `json:"concurrency,omitempty"`
	Status           string                `json:"status"`
	Error            string                `json:"error,omitempty"`
	DurationMS       int64                 `json:"duration_ms"`
	InputTokens      int64                 `json:"input_tokens"`
	OutputTokens     int64                 `json:"output_tokens"`
	ReasoningTokens  int64                 `json:"reasoning_tokens"`
	Samples          []detectionSample     `json:"samples"`
	Attribution      *detectionAttribution `json:"attribution,omitempty"`
}

// summary drops the samples and rankings that only the detail dialog shows.
func (r detectionResult) summary() detectionResult {
	r.Samples = nil
	if r.Attribution != nil {
		attribution := *r.Attribution
		attribution.Results, attribution.Diagnostics = nil, nil
		r.Attribution = &attribution
	}
	return r
}

type detectionProgress struct {
	Model       string `json:"model"`
	Phase       string `json:"phase"`
	Done        int    `json:"done"`
	Concurrency int    `json:"concurrency"`
	cancel      context.CancelFunc
}

type detectionRunRequest struct {
	AuthIDs      []string            `json:"auth_ids"`
	All          bool                `json:"all"`
	Model        string              `json:"model"`
	Concurrency  int                 `json:"concurrency"`
	ModelCatalog map[string][]string `json:"model_catalog,omitempty"`
}

var (
	detectionResults = map[string][]detectionResult{}
	detectionRunning = map[string]*detectionProgress{}
	detectionSlots   = make(chan struct{}, detectionGlobalConcurrency)
)

func detectionRunResponse(body []byte) managementResponse {
	var req detectionRunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误")
	}
	req.Model = strings.TrimSpace(req.Model)
	if !validModelName(req.Model, false) {
		return jsonError(http.StatusBadRequest, "请选择不含推理强度后缀的模型")
	}
	if req.Concurrency == 0 {
		req.Concurrency = detectionTarget
	}
	if req.Concurrency < 1 || req.Concurrency > detectionTarget {
		return jsonError(http.StatusBadRequest, fmt.Sprintf("每凭证并发须为 1–%d", detectionTarget))
	}
	return startRuns(req.AuthIDs, req.All, req.ModelCatalog, req.Model,
		func(id string) {
			now := time.Now().UTC()
			appendDetectionResult(id, detectionResult{ID: fmt.Sprint(now.UnixNano()), Time: now, Model: req.Model, Status: "skipped", Error: unsupportedModelMessage})
		},
		func(auth credential) {
			ctx, cancel := context.WithCancel(runCtx)
			p := &detectionProgress{Model: req.Model, Phase: "collecting", Concurrency: req.Concurrency, cancel: cancel}
			detectionRunning[auth.ID] = p
			go runDetection(ctx, auth, req, p)
		})
}

func detectionCancelResponse(body []byte) managementResponse {
	var req struct {
		AuthIDs []string `json:"auth_ids"`
		All     bool     `json:"all"`
	}
	if json.Unmarshal(body, &req) != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误")
	}
	mu.Lock()
	defer mu.Unlock()
	count := 0
	for id, p := range detectionRunning {
		if req.All || slices.Contains(req.AuthIDs, id) {
			p.Phase = "cancelling"
			p.cancel()
			count++
		}
	}
	return jsonResponse(http.StatusOK, map[string]int{"cancelled": count})
}

func appendDetectionResult(id string, r detectionResult) {
	r.DetectorRevision = detectorRevision
	detectionResults[id] = appendRecent(detectionResults[id], r)
}

func runDetection(ctx context.Context, auth credential, req detectionRunRequest, p *detectionProgress) {
	defer tasks.Done()
	started := time.Now()
	done := 0
	r := detectionResult{ID: fmt.Sprint(started.UnixNano()), Time: started.UTC(), Model: req.Model, Concurrency: req.Concurrency, Status: "completed"}
	defer func() {
		if ctx.Err() != nil && done < detectionTarget {
			r.Status = "cancelled"
		}
		if recovered := recover(); recovered != nil {
			r.Status, r.Error = "failed", fmt.Sprint(recovered)
		}
		r.DurationMS = time.Since(started).Milliseconds()
		mu.Lock()
		defer mu.Unlock()
		p.cancel()
		appendDetectionResult(auth.ID, r)
		delete(detectionRunning, auth.ID)
		_ = saveStateLocked()
	}()
	challenges := detectionChallenges()
	type event struct {
		index  int
		sample detectionSample
		output modelResponse
		err    error
	}
	jobs, events := make(chan int, detectionTarget), make(chan event, detectionTarget)
	for i := range challenges {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range req.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				select {
				case detectionSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				sample, output, err := collectDetectionSample(ctx, auth, req.Model, challenges[i])
				<-detectionSlots
				if sample.Attempts > 0 {
					events <- event{i, sample, output, err}
				}
			}
		}()
	}
	go func() { workers.Wait(); close(events) }()
	samples := make([]*detectionSample, detectionTarget)
	for e := range events {
		samples[e.index] = &e.sample
		r.InputTokens += e.output.InputTokens
		r.OutputTokens += e.output.OutputTokens
		r.ReasoningTokens += e.output.ReasoningTokens
		if !errors.Is(e.err, context.Canceled) {
			done++
		}
		mu.Lock()
		p.Done = done
		mu.Unlock()
	}
	var outputs []detectionOutput
	// Preserve challenge order even when requests complete out of order.
	for _, sample := range samples {
		if sample == nil {
			continue
		}
		r.Samples = append(r.Samples, *sample)
		outputs = append(outputs, detectionOutput{sample.Text, sample.ExpectedCount})
	}
	attr, err := analyzeDetection(outputs)
	switch {
	case err == nil:
		r.Attribution = &attr
		if attr.Used < detectionTarget {
			r.Status = "partial"
		}
	case len(r.Samples) > 0 && r.Samples[len(r.Samples)-1].Error != "":
		// No answer was valid; the last request's error tells why.
		r.Status, r.Error = "failed", r.Samples[len(r.Samples)-1].Error
	default:
		r.Status, r.Error = "failed", err.Error()
	}
}

// collectDetectionSample asks challenge until an answer can be scored. The sample keeps the last answer.
func collectDetectionSample(ctx context.Context, auth credential, model string, challenge detectionChallenge) (sample detectionSample, out modelResponse, err error) {
	sample.detectionChallenge = challenge
	out, sample.Attempts, err = askCodex(ctx, auth, model, "", challenge.Instructions, challenge.Prompt, challengeTimeout, func(answer string) (err error) {
		sample.Text, sample.Parsed, err = detectionText(answer, challenge.ExpectedCount)
		return err
	})
	sample.Valid = err == nil
	if err != nil {
		sample.Error = truncate(err.Error(), 500)
	}
	return
}

// detectionText returns the text of answer that detection scores and the count of its numbers, or why the
// answer cannot be scored. Like upstream detection, only the requested count is scored when an answer runs
// longer.
func detectionText(answer string, expected int) (text string, parsed int, err error) {
	if len(answer) > detectionMaxText {
		return "", 0, errors.New("回答过长")
	}
	text, numbers := answer, parseNumbers(answer)
	if len(numbers) > expected {
		numbers = numbers[:expected]
		values := make([]string, len(numbers))
		for i, n := range numbers {
			values[i] = strconv.Itoa(n)
		}
		text = strings.Join(values, ", ")
	}
	if len(numbers) < minimumNumbers(expected) {
		err = errors.New("有效数字不足")
	}
	return text, len(numbers), err
}
