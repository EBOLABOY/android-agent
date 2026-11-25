package brain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func callLLM(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec) (*llmResponse, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("LLM api_key is not configured; set it in Settings (/ui) or config.json")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("LLM model is not configured; set it in Settings (/ui) or config.json")
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "gemini" {
		return callGemini(cctx, cfg, systemPrompt, history, tools)
	}
	return callOpenAI(cctx, cfg, systemPrompt, history, tools)
}

func callOpenAI(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec) (*llmResponse, error) {
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
	for _, m := range history {
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
				msgs = append(msgs, map[string]any{
					"role": "user",
					"content": []map[string]any{
						{"type": "text", "text": fmt.Sprintf("[Screenshot from %s]", m.ToolName)},
						{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(m.ImageJPEG)}},
					},
				})
			}

		default: // user
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Text})
		}
	}

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
	if len(toolDefs) > 0 {
		payload["tools"] = toolDefs
		payload["tool_choice"] = "auto"
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

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
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
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

func callGemini(ctx context.Context, cfg config.LLMConfig, systemPrompt string, history []ChatMessage, tools []ToolSpec) (*llmResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	model := strings.TrimPrefix(strings.TrimSpace(cfg.Model), "models/")
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", base, model)

	var contents []map[string]any
	for _, m := range history {
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
			parts := []map[string]any{
				{
					"functionResponse": map[string]any{
						"name":     m.ToolName,
						"response": map[string]any{"output": m.Text},
					},
				},
			}
			if len(m.ImageJPEG) > 0 {
				parts = append(parts, map[string]any{
					"inlineData": map[string]any{
						"mimeType": "image/jpeg",
						"data":     base64.StdEncoding.EncodeToString(m.ImageJPEG),
					},
				})
			}
			contents = append(contents, map[string]any{"role": "user", "parts": parts})

		default:
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": m.Text}},
			})
		}
	}

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

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Gemini HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

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
		return nil, fmt.Errorf("Gemini error: %s", parsed.Error.Message)
	}
	if len(parsed.Candidates) == 0 {
		return nil, errors.New("Gemini returned empty candidates")
	}

	out := &llmResponse{}
	var textParts []string
	for i, part := range parsed.Candidates[0].Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, strings.TrimSpace(part.Text))
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
	out.Text = strings.TrimSpace(strings.Join(textParts, "\n"))
	return out, nil
}
