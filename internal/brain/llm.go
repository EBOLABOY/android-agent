package brain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"android-agent/internal/config"
)

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolOutput struct {
	Text      string `json:"text"`
	ImageJPEG []byte `json:"-"`
	IsError   bool   `json:"is_error,omitempty"`
}

type ToolExecutor interface {
	ToolSpecs() []ToolSpec
	ExecuteTool(ctx context.Context, name string, args map[string]any) ToolOutput
}

type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type ChatMessage struct {
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Text       string     `json:"text,omitempty"`
	ImageJPEG  []byte     `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
}

type llmResponse struct {
	Text      string
	ToolCalls []ToolCall
}

type StreamCallbacks struct {
	OnTextDelta func(delta string)
}

var llmRetryBaseDelay = time.Second

func isRetryableHTTPStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func doStreamHTTPWithRetry(ctx context.Context, buildReq func() (*http.Request, error)) (*http.Response, error) {
	const maxAttempts = 3
	var lastErr error
	var lastResp *http.Response

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
		} else if !isRetryableHTTPStatus(resp.StatusCode) {
			return resp, nil
		} else {
			if lastResp != nil {
				lastResp.Body.Close()
			}
			lastResp = resp
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}

		if attempt+1 < maxAttempts {
			delay := llmRetryBaseDelay * time.Duration(1<<attempt)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				if lastResp != nil {
					lastResp.Body.Close()
				}
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

func doHTTPWithRetry(ctx context.Context, buildReq func() (*http.Request, error)) (int, []byte, error) {
	const maxAttempts = 3
	var lastErr error
	var lastCode int
	var lastRaw []byte

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		req, err := buildReq()
		if err != nil {
			return 0, nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return 0, nil, err
			}
		} else {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
			resp.Body.Close()
			if rerr != nil {
				lastErr = rerr
				if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
					return 0, nil, rerr
				}
			} else if !isRetryableHTTPStatus(resp.StatusCode) {
				return resp.StatusCode, raw, nil
			} else {
				lastCode = resp.StatusCode
				lastRaw = raw
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}

		if attempt+1 < maxAttempts {
			delay := llmRetryBaseDelay * time.Duration(1<<attempt)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 0, nil, ctx.Err()
			case <-timer.C:
			}
		}
	}

	if lastCode > 0 {
		return lastCode, lastRaw, nil
	}
	return 0, nil, lastErr
}

func llmTimeout(cfg config.LLMConfig) time.Duration {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return timeout
}

func callLLM(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec) (*llmResponse, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("LLM api_key is not configured; set it in Settings (/ui) or config.json")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("LLM model is not configured; set it in Settings (/ui) or config.json")
	}
	cctx, cancel := context.WithTimeout(ctx, llmTimeout(cfg))
	defer cancel()

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "gemini" {
		return callGemini(cctx, cfg, systemPrompt, history, tools)
	}
	return callOpenAI(cctx, cfg, systemPrompt, history, tools)
}

func callLLMStream(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec, cb StreamCallbacks) (*llmResponse, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("LLM api_key is not configured; set it in Settings (/ui) or config.json")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("LLM model is not configured; set it in Settings (/ui) or config.json")
	}

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "gemini" {
		return callGeminiStream(ctx, cfg, systemPrompt, history, tools, cb)
	}
	return callOpenAIStream(ctx, cfg, systemPrompt, history, tools, cb)
}

func fallbackCallOpenAI(parentCtx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec, cb StreamCallbacks) (*llmResponse, error) {
	if err := parentCtx.Err(); err != nil {
		return nil, err
	}
	fbCtx, cancel := context.WithTimeout(parentCtx, llmTimeout(cfg))
	defer cancel()
	out, err := callOpenAI(fbCtx, cfg, systemPrompt, history, tools)
	if err != nil {
		return nil, err
	}
	if out != nil && out.Text != "" && cb.OnTextDelta != nil {
		cb.OnTextDelta(out.Text)
	}
	return out, nil
}

func fallbackCallGemini(parentCtx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec, cb StreamCallbacks) (*llmResponse, error) {
	if err := parentCtx.Err(); err != nil {
		return nil, err
	}
	fbCtx, cancel := context.WithTimeout(parentCtx, llmTimeout(cfg))
	defer cancel()
	out, err := callGemini(fbCtx, cfg, systemPrompt, history, tools)
	if err != nil {
		return nil, err
	}
	if out != nil && out.Text != "" && cb.OnTextDelta != nil {
		cb.OnTextDelta(out.Text)
	}
	return out, nil
}

func buildOpenAIRequest(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec, stream bool) (string, []byte, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	endpoint := base + "/chat/completions"
	if strings.HasSuffix(base, "/chat/completions") {
		endpoint = base
	}

	var msgs []map[string]any
	if systemPrompt != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": systemPrompt})
	}
	var pendingImages []ChatMessage
	flushPendingImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		content := make([]map[string]any, 0, len(pendingImages)*2)
		for _, img := range pendingImages {
			content = append(content,
				map[string]any{"type": "text", "text": fmt.Sprintf("[Screenshot from %s]", img.ToolName)},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img.ImageJPEG)}},
			)
		}
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": content,
		})
		pendingImages = nil
	}

	for _, m := range history {
		if m.Role != "tool" {
			flushPendingImages()
		}
		switch m.Role {
		case "assistant":
			msg := map[string]any{"role": "assistant"}
			if m.Text != "" {
				msg["content"] = m.Text
			}
			if len(m.ToolCalls) > 0 {
				var tcs []map[string]any
				for _, tc := range m.ToolCalls {
					argBytes, _ := json.Marshal(tc.Args)
					if len(argBytes) == 0 {
						argBytes = []byte("{}")
					}
					tcs = append(tcs, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": string(argBytes),
						},
					})
				}
				msg["tool_calls"] = tcs
			}
			msgs = append(msgs, msg)

		case "tool":
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      m.Text,
			})
			if len(m.ImageJPEG) > 0 {
				pendingImages = append(pendingImages, m)
			}

		default:
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Text})
		}
	}
	flushPendingImages()

	var toolDefs []map[string]any
	for _, t := range tools {
		toolDefs = append(toolDefs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}

	payload := map[string]any{
		"model":    cfg.Model,
		"messages": msgs,
	}
	if stream {
		payload["stream"] = true
	}
	if len(toolDefs) > 0 {
		payload["tools"] = toolDefs
		payload["tool_choice"] = "auto"
	}

	bodyBytes, err := json.Marshal(payload)
	return endpoint, bodyBytes, err
}

func decodeOpenAIResponse(raw []byte) (*llmResponse, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode LLM response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("LLM error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("LLM returned empty choices")
	}

	msg := parsed.Choices[0].Message
	out := &llmResponse{Text: strings.TrimSpace(msg.Content)}
	for i, tc := range msg.ToolCalls {
		args := map[string]any{}
		if rawArgs := strings.TrimSpace(tc.Function.Arguments); rawArgs != "" {
			if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
				return nil, fmt.Errorf("invalid tool call %q arguments JSON: %w", tc.Function.Name, err)
			}
			if args == nil {
				args = map[string]any{}
			}
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:   id,
			Name: tc.Function.Name,
			Args: args,
		})
	}
	return out, nil
}

func callOpenAIStream(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec, cb StreamCallbacks) (*llmResponse, error) {
	streamCtx, cancel := context.WithTimeout(ctx, llmTimeout(cfg))
	defer cancel()

	endpoint, bodyBytes, err := buildOpenAIRequest(streamCtx, cfg, systemPrompt, history, tools, true)
	if err != nil {
		return nil, err
	}
	resp, err := doStreamHTTPWithRetry(streamCtx, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(streamCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		return req, nil
	})
	if err != nil {
		if ctx.Err() != nil || streamCtx.Err() != nil {
			return nil, err
		}
		return fallbackCallOpenAI(ctx, cfg, systemPrompt, history, tools, cb)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		// Graceful fallback to non-streaming if upstream rejects stream:true (e.g. 400/404/422/501)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound ||
			resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusUnprocessableEntity ||
			resp.StatusCode == http.StatusNotImplemented {
			return fallbackCallOpenAI(ctx, cfg, systemPrompt, history, tools, cb)
		}
		return nil, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		if err != nil {
			return nil, err
		}
		out, err := decodeOpenAIResponse(raw)
		if err != nil {
			return nil, err
		}
		if out.Text != "" && cb.OnTextDelta != nil {
			cb.OnTextDelta(out.Text)
		}
		return out, nil
	}

	type partialTC struct {
		id   string
		name string
		args strings.Builder
	}
	tcMap := make(map[int]*partialTC)
	var textBuf strings.Builder
	emitted := false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			if !emitted && len(tcMap) == 0 && ctx.Err() == nil {
				return fallbackCallOpenAI(ctx, cfg, systemPrompt, history, tools, cb)
			}
			return nil, fmt.Errorf("LLM stream error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			emitted = true
			textBuf.WriteString(delta.Content)
			if cb.OnTextDelta != nil {
				cb.OnTextDelta(delta.Content)
			}
		}
		for _, tc := range delta.ToolCalls {
			p, ok := tcMap[tc.Index]
			if !ok {
				p = &partialTC{}
				tcMap[tc.Index] = p
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Function.Name != "" {
				p.name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				p.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !emitted && len(tcMap) == 0 && streamCtx.Err() == nil {
			return fallbackCallOpenAI(ctx, cfg, systemPrompt, history, tools, cb)
		}
		return nil, fmt.Errorf("LLM stream interrupted: %w", err)
	}

	if !emitted && len(tcMap) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if streamCtx.Err() != nil {
			return nil, streamCtx.Err()
		}
		return fallbackCallOpenAI(ctx, cfg, systemPrompt, history, tools, cb)
	}

	out := &llmResponse{Text: strings.TrimSpace(textBuf.String())}
	if len(tcMap) > 0 {
		idxs := make([]int, 0, len(tcMap))
		for idx := range tcMap {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for pos, idx := range idxs {
			p := tcMap[idx]
			args := map[string]any{}
			if rawArgs := strings.TrimSpace(p.args.String()); rawArgs != "" {
				if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
					return nil, fmt.Errorf("invalid streamed tool call %q arguments JSON: %w", p.name, err)
				}
				if args == nil {
					args = map[string]any{}
				}
			}
			id := p.id
			if id == "" {
				id = fmt.Sprintf("call_%d", pos)
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   id,
				Name: p.name,
				Args: args,
			})
		}
	}
	return out, nil
}

func callOpenAI(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec) (*llmResponse, error) {
	endpoint, bodyBytes, err := buildOpenAIRequest(ctx, cfg, systemPrompt, history, tools, false)
	if err != nil {
		return nil, err
	}
	statusCode, raw, err := doHTTPWithRetry(ctx, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("openai request failed: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("LLM HTTP %d: %s", statusCode, strings.TrimSpace(string(raw)))
	}
	return decodeOpenAIResponse(raw)
}

func buildGeminiPayload(systemPrompt string, history []ChatMessage, tools []ToolSpec) ([]byte, error) {
	var contents []map[string]any
	var pendingToolParts []map[string]any
	flushToolParts := func() {
		if len(pendingToolParts) == 0 {
			return
		}
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": pendingToolParts,
		})
		pendingToolParts = nil
	}

	for _, m := range history {
		if m.Role != "tool" {
			flushToolParts()
		}
		switch m.Role {
		case "assistant":
			var parts []map[string]any
			if m.Text != "" {
				parts = append(parts, map[string]any{"text": m.Text})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": tc.Name,
						"args": tc.Args,
					},
				})
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})

		case "tool":
			pendingToolParts = append(pendingToolParts, map[string]any{
				"functionResponse": map[string]any{
					"name":     m.ToolName,
					"response": map[string]any{"output": m.Text},
				},
			})
			if len(m.ImageJPEG) > 0 {
				pendingToolParts = append(pendingToolParts, map[string]any{
					"inlineData": map[string]any{
						"mimeType": "image/jpeg",
						"data":     base64.StdEncoding.EncodeToString(m.ImageJPEG),
					},
				})
			}

		default:
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": m.Text}},
			})
		}
	}
	flushToolParts()

	payload := map[string]any{
		"contents": contents,
	}
	if systemPrompt != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": systemPrompt}},
		}
	}
	if len(tools) > 0 {
		var decls []map[string]any
		for _, t := range tools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			})
		}
		payload["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}

	return json.Marshal(payload)
}

type geminiAPIError struct {
	msg string
}

func (e *geminiAPIError) Error() string {
	return "Gemini error: " + e.msg
}

func decodeGeminiPayload(raw []byte, streaming bool) (*llmResponse, error) {
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode Gemini response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, &geminiAPIError{msg: parsed.Error.Message}
	}
	if len(parsed.Candidates) == 0 {
		if streaming {
			return &llmResponse{}, nil
		}
		return nil, errors.New("Gemini returned empty candidates")
	}

	out := &llmResponse{}
	var textBuf strings.Builder
	var textParts []string
	for i, part := range parsed.Candidates[0].Content.Parts {
		if part.Text != "" {
			if streaming {
				textBuf.WriteString(part.Text)
			} else {
				textParts = append(textParts, part.Text)
			}
		}
		if part.FunctionCall != nil {
			args := part.FunctionCall.Args
			if args == nil {
				args = map[string]any{}
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   fmt.Sprintf("gem_%d", i),
				Name: part.FunctionCall.Name,
				Args: args,
			})
		}
	}
	if streaming {
		out.Text = textBuf.String()
	} else {
		out.Text = strings.TrimSpace(strings.Join(textParts, "\n"))
	}
	return out, nil
}

func decodeGeminiResponse(raw []byte) (*llmResponse, error) {
	return decodeGeminiPayload(raw, false)
}

func decodeGeminiStreamChunk(raw []byte) (*llmResponse, error) {
	return decodeGeminiPayload(raw, true)
}

func callGemini(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec) (*llmResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	model := strings.TrimPrefix(strings.TrimSpace(cfg.Model), "models/")
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", base, model)

	bodyBytes, err := buildGeminiPayload(systemPrompt, history, tools)
	if err != nil {
		return nil, err
	}
	statusCode, raw, err := doHTTPWithRetry(ctx, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", cfg.APIKey)
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("Gemini HTTP %d: %s", statusCode, strings.TrimSpace(string(raw)))
	}
	return decodeGeminiResponse(raw)
}

func callGeminiStream(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec, cb StreamCallbacks) (*llmResponse, error) {
	streamCtx, cancel := context.WithTimeout(ctx, llmTimeout(cfg))
	defer cancel()

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	model := strings.TrimPrefix(strings.TrimSpace(cfg.Model), "models/")
	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", base, model)

	bodyBytes, err := buildGeminiPayload(systemPrompt, history, tools)
	if err != nil {
		return nil, err
	}
	resp, err := doStreamHTTPWithRetry(streamCtx, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(streamCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("x-goog-api-key", cfg.APIKey)
		return req, nil
	})
	if err != nil {
		if ctx.Err() != nil || streamCtx.Err() != nil {
			return nil, err
		}
		return fallbackCallGemini(ctx, cfg, systemPrompt, history, tools, cb)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound ||
			resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
			return fallbackCallGemini(ctx, cfg, systemPrompt, history, tools, cb)
		}
		return nil, fmt.Errorf("Gemini HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		if err != nil {
			return nil, err
		}
		out, err := decodeGeminiResponse(raw)
		if err != nil {
			return nil, err
		}
		if out.Text != "" && cb.OnTextDelta != nil {
			cb.OnTextDelta(out.Text)
		}
		return out, nil
	}

	var textBuf strings.Builder
	var toolCalls []ToolCall
	emitted := false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		chunkOut, derr := decodeGeminiStreamChunk([]byte(data))
		if derr != nil {
			var apiErr *geminiAPIError
			if errors.As(derr, &apiErr) {
				return nil, fmt.Errorf("Gemini stream error: %s", apiErr.msg)
			}
			continue
		}
		if chunkOut.Text != "" {
			emitted = true
			textBuf.WriteString(chunkOut.Text)
			if cb.OnTextDelta != nil {
				cb.OnTextDelta(chunkOut.Text)
			}
		}
		for _, tc := range chunkOut.ToolCalls {
			tc.ID = fmt.Sprintf("gem_%d", len(toolCalls))
			toolCalls = append(toolCalls, tc)
		}
	}

	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !emitted && len(toolCalls) == 0 && streamCtx.Err() == nil {
			return fallbackCallGemini(ctx, cfg, systemPrompt, history, tools, cb)
		}
		return nil, fmt.Errorf("Gemini stream interrupted: %w", err)
	}
	if !emitted && len(toolCalls) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if streamCtx.Err() != nil {
			return nil, streamCtx.Err()
		}
		return fallbackCallGemini(ctx, cfg, systemPrompt, history, tools, cb)
	}
	return &llmResponse{
		Text:      strings.TrimSpace(textBuf.String()),
		ToolCalls: toolCalls,
	}, nil
}
