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

// pelicanRecord is one drawn page: its status is completed, failed or skipped.
type pelicanRecord struct {
	record
	Effort   string `json:"effort"`
	HTML     string `json:"html,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// summary drops the HTML, which previews load per record.
func (r pelicanRecord) summary() pelicanRecord {
	r.HTML = ""
	return r
}

type pelicanRunRequest struct {
	runRequest
	Effort string `json:"effort"`
}

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
	return startRuns("pelican", req.runRequest, 1,
		func(id string) {
			records.Pelicans.add(id, pelicanRecord{record: skippedRecord(req.Model), Effort: req.Effort})
		},
		func(ctx context.Context, auth credential, _ *progress) {
			r := evaluatePelican(ctx, auth, req.Model, req.Effort)
			mu.Lock()
			defer mu.Unlock()
			// Stopping ctx drops the run.
			if ctx.Err() == nil {
				records.Pelicans.add(auth.ID, r)
				_ = saveStateLocked()
			}
		})
}

func evaluatePelican(ctx context.Context, auth credential, model, effort string) pelicanRecord {
	started := time.Now()
	r := pelicanRecord{record: newRecord(model, started), Effort: effort}
	var page string
	out, attempts, err := askCodex(ctx, auth, model, effort, "", pelicanPrompt, answerTimeout, func(answer string) (err error) {
		page, err = pelicanPage(answer)
		return err
	})
	r.DurationMS, r.Attempts, r.usage = time.Since(started).Milliseconds(), attempts, out.usage
	if err != nil {
		r.Status, r.Error = "failed", truncate(err.Error(), 500)
		return r
	}
	r.Status, r.HTML = "completed", page
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
