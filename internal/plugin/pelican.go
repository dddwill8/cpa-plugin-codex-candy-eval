package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const pelicanMaxHTML = 256 << 10

// Simon Willison's pelican-on-a-bicycle test, framed so a Codex model answers in text instead of writing a file.
const pelicanPrompt = "不使用任何工具，也不要创建或修改文件，直接在回复中完成以下任务：\n\n" +
	"创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的 2D 动画，你不需要任何测试，不要使用外部资产。\n\n" +
	"在一个 ```html 代码块中给出完整的 HTML 文件。"

type pelicanResult struct {
	ID              string    `json:"id"`
	Time            time.Time `json:"time"`
	Model           string    `json:"model"`
	Effort          string    `json:"effort"`
	Skipped         bool      `json:"skipped,omitempty"`
	HTML            string    `json:"html,omitempty"`
	Error           string    `json:"error,omitempty"`
	Attempts        int       `json:"attempts,omitempty"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	DurationMS      int64     `json:"duration_ms"`
}

// summary drops the HTML, which previews load per record.
func (r pelicanResult) summary() pelicanResult {
	r.HTML = ""
	return r
}

type pelicanRunRequest struct {
	ModelCatalog map[string][]string `json:"model_catalog,omitempty"`
	AuthIDs      []string            `json:"auth_ids"`
	All          bool                `json:"all"`
	Model        string              `json:"model"`
	Effort       string              `json:"effort"`
}

var (
	pelicanResults = map[string][]pelicanResult{}
	pelicanRunning = map[string]bool{}
)

func pelicanRunResponse(body []byte) managementResponse {
	var req pelicanRunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误："+err.Error())
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Effort = strings.TrimSpace(req.Effort)
	if !validModelName(req.Model, true) {
		return jsonError(http.StatusBadRequest, "请选择模型")
	}
	return startRuns(req.AuthIDs, req.All, req.ModelCatalog, req.Model,
		func(id string) {
			now := time.Now().UTC()
			pelicanResults[id] = appendRecent(pelicanResults[id], pelicanResult{ID: fmt.Sprint(now.UnixNano()), Time: now, Model: req.Model, Effort: req.Effort, Skipped: true, Error: unsupportedModelMessage})
		},
		func(auth credential) {
			pelicanRunning[auth.ID] = true
			go runPelican(runCtx, auth, req.Model, req.Effort)
		})
}

// runPelican records the page of one answer. Stopping ctx drops the run.
func runPelican(ctx context.Context, auth credential, model, effort string) {
	defer tasks.Done()
	r := evaluatePelican(ctx, auth, model, effort)
	mu.Lock()
	defer mu.Unlock()
	delete(pelicanRunning, auth.ID)
	if ctx.Err() == nil {
		pelicanResults[auth.ID] = appendRecent(pelicanResults[auth.ID], r)
		_ = saveStateLocked()
	}
}

func evaluatePelican(ctx context.Context, auth credential, model, effort string) pelicanResult {
	started := time.Now()
	r := pelicanResult{ID: fmt.Sprint(started.UnixNano()), Time: started.UTC(), Model: model, Effort: effort}
	var page string
	out, attempts, err := askCodex(ctx, auth, model, effort, "", pelicanPrompt, answerTimeout, func(answer string) (err error) {
		page, err = pelicanPage(answer)
		return err
	})
	r.DurationMS, r.Attempts = time.Since(started).Milliseconds(), attempts
	r.InputTokens, r.OutputTokens, r.ReasoningTokens = out.InputTokens, out.OutputTokens, out.ReasoningTokens
	if err != nil {
		r.Error = truncate(err.Error(), 500)
		return r
	}
	r.HTML = page
	return r
}

// pelicanPage returns the HTML page of an answer, or why the answer has none to keep.
func pelicanPage(answer string) (string, error) {
	switch html := extractHTML(answer); {
	case html == "":
		return "", errors.New("回答中没有 HTML 代码")
	case len(html) > pelicanMaxHTML:
		return "", fmt.Errorf("HTML 超过 %d KB，未保存", pelicanMaxHTML>>10)
	default:
		return html, nil
	}
}

var codeBlock = regexp.MustCompile("(?s)```[ \\t]*([\\w-]*)[^\\n]*\\n(.*?)\\n[ \\t]*```")

// extractHTML returns the HTML document in a model answer: its html code block, else the first code block
// with markup, else the answer itself when it is bare markup.
func extractHTML(answer string) string {
	blocks := codeBlock.FindAllStringSubmatch(answer, -1)
	for _, block := range blocks {
		if strings.EqualFold(block[1], "html") {
			return strings.TrimSpace(block[2])
		}
	}
	for _, block := range blocks {
		if code := strings.TrimSpace(block[2]); strings.HasPrefix(code, "<") {
			return code
		}
	}
	if text := strings.TrimSpace(answer); len(blocks) == 0 && strings.HasPrefix(text, "<") {
		return text
	}
	return ""
}
