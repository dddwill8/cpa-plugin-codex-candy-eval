package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// usage counts the tokens of one or more model responses.
type usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

func (u *usage) add(v usage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.ReasoningTokens += v.ReasoningTokens
}

type modelResponse struct {
	Answer string
	usage
}

func validModelName(model string, allowEffort bool) bool {
	return model != "" && len(model) <= 200 && strings.IndexFunc(model, unicode.IsControl) < 0 &&
		(allowEffort || !strings.ContainsAny(model, "()"))
}

// Time limits of an attempt, as a model stuck repeating itself streams until the upstream cuts it off.
// Fingerprint challenges normally answer within a minute.
const (
	challengeTimeout = 3 * time.Minute
	answerTimeout    = 10 * time.Minute
)

// A request is sent at most modelAttempts times. The first retry waits retryDelay, each later one twice as
// long.
const modelAttempts = 3

var retryDelay = 2 * time.Second

// retryableError marks a failure that sending the request again may avoid.
type retryableError struct{ error }

func (e retryableError) Unwrap() error { return e.error }

// askCodex sends prompt as a new Codex turn on auth and returns the first answer accept takes; a nil accept
// takes any answer. Effort "none" leaves the reasoning effort to CPA. Each attempt has limit to finish. The
// response sums the usage of all attempts and holds the last answer.
//
// CPA retries a request on its pinned credential until output starts (request-retry) and waits out short
// cooldowns (max-retry-interval). A rejection that still reaches the plugin is retried only when it is a
// timeout or server error; 429 means the credential is cooling down or out of quota. CPA cannot retry a
// stream it has started, so any later failure is retried: a broken or overlong stream, a response that
// failed or did not answer in text, and an answer accept rejects.
func askCodex(ctx context.Context, auth credential, model, effort, instructions, prompt string, limit time.Duration, accept func(answer string) error) (out modelResponse, attempts int, err error) {
	if effort == "none" {
		effort = ""
	}
	body, headers := codexTurn(auth.ID, model, effort, instructions, prompt)
	timeout := fmt.Errorf("模型 %d 分钟内未完成回答，已中断请求", int(limit.Minutes()))
	for delay := retryDelay; ; delay *= 2 {
		if err = ctx.Err(); err != nil {
			return
		}
		attempts++
		requestCtx, cancel := context.WithTimeoutCause(ctx, limit, timeout)
		var response modelResponse
		response, err = executeModel(requestCtx, auth, model, body, headers)
		cancel()
		out.Answer = response.Answer
		out.add(response.usage)
		if err == nil && accept != nil {
			if err = accept(response.Answer); err != nil {
				err = retryableError{err}
			}
		}
		if err == nil || attempts == modelAttempts || !errors.As(err, new(retryableError)) {
			return
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return out, attempts, ctx.Err()
		}
	}
}

// executeModel sends a streaming Responses request body with headers on auth's provider; CPA translates
// it to the provider's own protocol. When ctx ends first, the request is abandoned with ctx's cause. A
// failure another attempt may avoid is a retryableError, as askCodex explains.
func executeModel(ctx context.Context, auth credential, model string, body any, headers http.Header) (result modelResponse, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("模型请求异常：%v", recovered)
		}
	}()
	if strings.TrimSpace(auth.ID) == "" || strings.TrimSpace(auth.Provider) == "" {
		return result, fmt.Errorf("缺少凭证 ID 或提供商，无法指定凭证")
	}
	// Keep <, > and & literal, as Codex sends them.
	var request bytes.Buffer
	encoder := json.NewEncoder(&request)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return result, err
	}
	raw, err := hostCall("host.model.execute_stream", map[string]any{
		"entry_protocol": "openai-response", "exit_protocol": "openai-response",
		"model": model, "stream": true, "body": bytes.TrimSuffix(request.Bytes(), []byte("\n")), "headers": headers,
		"forced_provider": auth.Provider, "auth_id": auth.ID,
	})
	if err != nil {
		var rejected *EnvelopeError
		if errors.As(err, &rejected) && (rejected.HTTPStatus == http.StatusRequestTimeout || rejected.HTTPStatus >= http.StatusInternalServerError) {
			err = retryableError{err}
		}
		return result, err
	}
	var stream struct {
		ID string `json:"stream_id"`
	}
	if err := json.Unmarshal(raw, &stream); err != nil || stream.ID == "" {
		return result, fmt.Errorf("解析 CPA 响应失败：%s", raw)
	}
	// The host closes a stream by itself only once it is read to the end. Closing it early ends the pending
	// read and cancels the upstream request.
	closeStream := sync.OnceFunc(func() { hostCall("host.model.stream_close", map[string]any{"stream_id": stream.ID}) })
	defer closeStream()
	stop := context.AfterFunc(ctx, closeStream)
	defer stop()
	out, err := readResponseStream(stream.ID)
	switch {
	case err != nil && ctx.Err() != nil:
		err = context.Cause(ctx)
	case err == nil:
		result, err = answerOf(out)
	}
	if err != nil {
		err = retryableError{err}
	}
	return result, err
}

// answerOf returns the answer text and usage of a final response. Any final response reports usage,
// complete or not.
func answerOf(out responseBody) (result modelResponse, err error) {
	result.usage = usage{out.Usage.InputTokens, out.Usage.OutputTokens, out.Usage.OutputTokensDetails.ReasoningTokens}
	if (out.Status != "" && out.Status != "completed") || (len(out.Error) > 0 && string(out.Error) != "null") {
		return result, fmt.Errorf("模型响应未完成（%s）：%s", out.Status, out.Error)
	}
	var answer strings.Builder
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				answer.WriteString(part.Text)
			}
		}
	}
	if strings.TrimSpace(answer.String()) == "" {
		// Codex turns list tools, and items such as function_call or custom_tool_call request one.
		if slices.ContainsFunc(out.Output, func(item responseItem) bool { return strings.HasSuffix(item.Type, "_call") }) {
			return result, fmt.Errorf("模型调用了工具，没有直接回答")
		}
		return result, fmt.Errorf("模型未返回文本")
	}
	result.Answer = answer.String()
	return result, nil
}

type responseItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseBody struct {
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
	Output []responseItem  `json:"output"`
	Usage  struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		OutputTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// readResponseStream reads a Responses event stream to its end and returns the final response.
func readResponseStream(id string) (out responseBody, err error) {
	var events []byte
	for done := false; !done; {
		raw, err := hostCall("host.model.stream_read", map[string]any{"stream_id": id})
		if err != nil {
			return out, err
		}
		var chunk struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return out, fmt.Errorf("解析模型响应流失败：%w", err)
		}
		if chunk.Error != "" {
			return out, fmt.Errorf("模型响应流中断：%s", chunk.Error)
		}
		// A chunk may end mid-line, or start the next field without a line break.
		if len(events) > 0 && !bytes.HasSuffix(events, []byte("\n")) && startsSSEField(chunk.Payload) {
			events = append(events, '\n')
		}
		events = append(events, chunk.Payload...)
		done = chunk.Done
	}
	var items []responseItem
	final := false
	for _, line := range bytes.Split(events, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Item     json.RawMessage `json:"item"`
		}
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		switch event.Type {
		case "response.output_item.done":
			var item responseItem
			if json.Unmarshal(event.Item, &item) == nil {
				items = append(items, item)
			}
		case "response.completed", "response.incomplete", "response.failed":
			if err := json.Unmarshal(event.Response, &out); err != nil {
				return out, fmt.Errorf("解析模型响应失败：%w", err)
			}
			final = true
		case "error":
			return out, fmt.Errorf("模型返回错误：%s", bytes.TrimSpace(data))
		}
	}
	if !final {
		return out, fmt.Errorf("模型响应流在完成前中断")
	}
	// Codex streams the output items but may leave the final response's output empty.
	if len(out.Output) == 0 {
		out.Output = items
	}
	return out, nil
}

func startsSSEField(chunk []byte) bool {
	chunk = bytes.TrimLeft(chunk, " \t")
	for _, field := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(chunk, []byte(field)) {
			return true
		}
	}
	return false
}
