package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"android-agent/internal/config"
	"android-agent/internal/guard"
)

type mockExec struct {
	called atomic.Int32
}

func (m *mockExec) ToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "phone_status",
			Description: "Get device status",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func (m *mockExec) ExecuteTool(_ context.Context, name string, _ map[string]any) ToolOutput {
	m.called.Add(1)
	if name == "phone_status" {
		return ToolOutput{Text: `{"guard":{"state":"active"}}`}
	}
	return ToolOutput{Text: "unknown", IsError: true}
}

func TestOpenAIAgentTwoStepToolCall(t *testing.T) {
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test-key" {
			t.Fatalf("missing bearer key header: %q", r.Header.Get("Authorization"))
		}
		n := round.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{
								{
									"id":   "tc_1",
									"type": "function",
									"function": map[string]any{
										"name":      "phone_status",
										"arguments": "{}",
									},
								},
							},
						},
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": "手机守护状态正常（active）。",
					},
				},
			},
		})
	}))
	defer srv.Close()

	exec := &mockExec{}
	ag := NewAgent(config.LLMConfig{
		Provider: "openai",
		BaseURL:  srv.URL,
		APIKey:   "sk-test-key",
		Model:    "gpt-test",
		MaxSteps: 5,
	}, exec, nil)

	res, err := ag.Chat(context.Background(), "检查手机状态")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if exec.called.Load() != 1 {
		t.Fatalf("expected 1 tool call, got %d", exec.called.Load())
	}
	if !strings.Contains(res.Reply, "active") {
		t.Fatalf("unexpected reply: %q", res.Reply)
	}
}

func TestGeminiAgentTwoStepToolCall(t *testing.T) {
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "gem-secret" {
			t.Fatalf("missing gemini key header")
		}
		if !strings.HasSuffix(r.URL.Path, "/models/gemini-2.5-flash:generateContent") {
			t.Fatalf("unexpected gemini path: %s", r.URL.Path)
		}
		n := round.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"parts": []map[string]any{
								{"functionCall": map[string]any{"name": "phone_status", "args": map[string]any{}}},
							},
						},
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{{"text": "Gemini检查完毕：守护运行中。"}},
					},
				},
			},
		})
	}))
	defer srv.Close()

	exec := &mockExec{}
	ag := NewAgent(config.LLMConfig{
		Provider: "gemini",
		BaseURL:  srv.URL,
		APIKey:   "gem-secret",
		Model:    "gemini-2.5-flash",
		MaxSteps: 5,
	}, exec, nil)

	res, err := ag.Chat(context.Background(), "查看状态")
	if err != nil {
		t.Fatalf("Gemini Chat failed: %v", err)
	}
	if exec.called.Load() != 1 || !strings.Contains(res.Reply, "Gemini") {
		t.Fatalf("unexpected gemini result: %+v", res)
	}
}

type blockedDet struct{}

func (blockedDet) DetectGuardedForeground(_ context.Context, _ []string) (string, error) {
	return "com.tencent.tmgp.sgame", nil
}

func TestGuardBlocksAgentChat(t *testing.T) {
	g := guard.New(guard.Options{
		Packages: []string{"com.tencent.tmgp.sgame"},
		Detector: blockedDet{},
	})
	g.CheckOnce(context.Background())

	ag := NewAgent(config.LLMConfig{Provider: "openai", APIKey: "k", Model: "m"}, &mockExec{}, g)
	if _, err := ag.Chat(context.Background(), "hello"); err == nil {
		t.Fatal("expected guard-blocked error")
	}
}
