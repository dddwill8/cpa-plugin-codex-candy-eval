package plugin

import (
	"bytes"
	"encoding/json"
	"net/url"
	"testing"
)

func TestExtractHTML(t *testing.T) {
	page := "<!doctype html><svg></svg>"
	for answer, want := range map[string]string{
		"说明\n```css\nbody {}\n```\n```HTML\n" + page + "\n```": page,
		"```\n" + page + "\n```":                               page,
		"  " + page + "\n":                                     page,
		"```js\nlet svg = 1\n```\n" + page:                     "",
		"下面是页面：" + page:                                        "",
	} {
		if got := extractHTML(answer); got != want {
			t.Errorf("extractHTML(%q) = %q, want %q", answer, got, want)
		}
	}
}

func TestPelicanRun(t *testing.T) {
	setupTest(t)
	answers := map[string]json.RawMessage{
		"page": mockModelResponse("好的：\n```html\n<svg></svg>\n```"),
		"tool": json.RawMessage(`{"status":"completed","output":[{"type":"custom_tool_call"}],"usage":{}}`),
	}
	hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"id":"page","provider":"codex"},{"id":"tool","provider":"codex"}]}`), nil
		}
		// "none" leaves the effort to CPA.
		if body, prompt := sentTurn(payload); prompt != pelicanPrompt || body["reasoning"].(map[string]any)["effort"] != nil {
			t.Errorf("prompt = %q, reasoning = %v", prompt, body["reasoning"])
		}
		return answers[payload.(map[string]any)["auth_id"].(string)], nil
	})
	if response := pelicanRunResponse([]byte(`{"all":true,"model":"m","effort":"none"}`)); response.StatusCode != 200 {
		t.Fatalf("run: %s", response.Body)
	}
	tasks.Wait()
	page, tool := records.Pelicans["page"][0], records.Pelicans["tool"][0]
	// A turn that only calls a tool is asked again.
	if page.HTML != "<svg></svg>" || page.Error != "" || page.Attempts != 1 || page.OutputTokens != 3 || tool.HTML != "" || tool.Error != "模型调用了工具，没有直接回答" || tool.Attempts != modelAttempts {
		t.Fatalf("page = %+v, tool = %+v", page, tool)
	}
	// Lists carry summaries; the page loads with its record.
	if state := stateResponse(); bytes.Contains(state.Body, []byte("svg")) || !bytes.Contains(state.Body, []byte(page.ID)) {
		t.Fatalf("state = %s", state.Body)
	}
	var full pelicanRecord
	if err := json.Unmarshal(recordResponse("pelican", url.Values{"credential_id": {"page"}, "id": {page.ID}}).Body, &full); err != nil || full.HTML != page.HTML {
		t.Fatalf("record = %+v, %v", full, err)
	}
	records, loaded = emptyState(), false
	loadState()
	if len(records.Pelicans["page"]) != 1 || records.Pelicans["page"][0].HTML != page.HTML {
		t.Fatal("pelican history did not survive reload")
	}
}
