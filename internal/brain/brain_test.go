package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		if strings.HasSuffix(r.URL.Path, "/models/gemini-2.5-flash:streamGenerateContent") {
			w.WriteHeader(http.StatusNotFound)
			return
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

func TestAgentConcurrencyBusyAndCancelActive(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ag := NewAgent(config.LLMConfig{
		Provider:   "openai",
		BaseURL:    srv.URL,
		APIKey:     "sk-test",
		Model:      "test-model",
		TimeoutSec: 5,
	}, &mockExec{}, nil)

	firstErr := make(chan error, 1)
	go func() {
		_, err := ag.Chat(context.Background(), "慢请求")
		firstErr <- err
	}()

	<-started
	if _, err := ag.Chat(context.Background(), "并发双击"); err != ErrChatBusy {
		t.Fatalf("expected ErrChatBusy while chat in flight, got %v", err)
	}
	if !ag.CancelActive("com.tencent.tmgp.sgame") {
		t.Fatal("expected CancelActive to report true while chat in flight")
	}
	err := <-firstErr
	if err == nil {
		t.Fatal("expected cancelled error after CancelActive")
	}
}

func TestAgentLLMRetryAndLoopDetection(t *testing.T) {
	oldDelay := llmRetryBaseDelay
	llmRetryBaseDelay = 2 * time.Millisecond
	defer func() { llmRetryBaseDelay = oldDelay }()

	var attempts atomic.Int32
	var sawLoopNudge atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		// Fail first 2 HTTP attempts (429 then 503) to exercise retry backoff
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if n == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if c, ok := m["content"].(string); ok && strings.Contains(c, "[系统防死循环提示]") {
				sawLoopNudge.Store(true)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if sawLoopNudge.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"role": "assistant", "content": "已收到防死循环提示，切换策略汇报。"},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":       "tc_loop",
						"type":     "function",
						"function": map[string]any{"name": "phone_status", "arguments": "{}"},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	ag := NewAgent(config.LLMConfig{
		Provider: "openai",
		BaseURL:  srv.URL,
		APIKey:   "sk-test",
		Model:    "test-model",
		MaxSteps: 8,
	}, &mockExec{}, nil)

	res, err := ag.Chat(context.Background(), "测试重试与死循环检测")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if !sawLoopNudge.Load() {
		t.Fatal("expected loop detection prompt to be injected after 3 identical tool calls")
	}
	if !strings.Contains(res.Reply, "防死循环") {
		t.Fatalf("unexpected reply: %q", res.Reply)
	}
}


func TestOpenAIStreamDeltasAndToolCalls(t *testing.T) {
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Fatalf("expected stream=true on streaming test request")
		}
		n := round.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_stream_1\",\"type\":\"function\",\"function\":{\"name\":\"phone_\",\"arguments\":\"{\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"status\",\"arguments\":\"}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"当前\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"设备正常\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	exec := &mockExec{}
	ag := NewAgent(config.LLMConfig{
		Provider: "openai",
		BaseURL:  srv.URL,
		APIKey:   "sk-stream",
		Model:    "stream-model",
		MaxSteps: 4,
	}, exec, nil)

	var got []RunEvent
	res, err := ag.ChatStream(context.Background(), "流式检查", func(ev RunEvent) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	if res.Reply != "当前设备正常" || exec.called.Load() != 1 {
		t.Fatalf("unexpected stream result=%+v called=%d", res, exec.called.Load())
	}

	var deltas strings.Builder
	var sawToolStart, sawToolEnd, sawDone bool
	for _, ev := range got {
		switch ev.Type {
		case "text_delta":
			deltas.WriteString(ev.Delta)
		case "tool_start":
			if ev.Tool == "phone_status" {
				sawToolStart = true
			}
		case "tool_end":
			if ev.Tool == "phone_status" {
				sawToolEnd = true
			}
		case "done":
			sawDone = true
		}
	}
	if deltas.String() != "当前设备正常" || !sawToolStart || !sawToolEnd || !sawDone {
		t.Fatalf("missing expected streamed events: deltas=%q toolStart=%v toolEnd=%v done=%v", deltas.String(), sawToolStart, sawToolEnd, sawDone)
	}
}

func TestOpenAIStreamNonSSEAndErrorFallback(t *testing.T) {
	t.Run("non-sse json response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"role": "assistant", "content": "非流式JSON兼容"},
				}},
			})
		}))
		defer srv.Close()

		var deltas strings.Builder
		resp, err := callLLMStream(context.Background(), config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "m1",
		}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{
			OnTextDelta: func(d string) { deltas.WriteString(d) },
		})
		if err != nil {
			t.Fatalf("callLLMStream failed: %v", err)
		}
		if resp.Text != "非流式JSON兼容" || deltas.String() != "非流式JSON兼容" {
			t.Fatalf("unexpected non-SSE fallback: resp=%+v deltas=%q", resp, deltas.String())
		}
	})

	t.Run("stream http error falls back to non-stream", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Stream bool `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			attempts.Add(1)
			if req.Stream {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"stream unsupported"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"role": "assistant", "content": "回退成功"},
				}},
			})
		}))
		defer srv.Close()

		resp, err := callLLMStream(context.Background(), config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "m1",
		}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{})
		if err != nil {
			t.Fatalf("expected graceful fallback, got err=%v", err)
		}
		if resp.Text != "回退成功" || attempts.Load() != 2 {
			t.Fatalf("unexpected fallback result=%+v attempts=%d", resp, attempts.Load())
		}
	})
}

func TestGeminiSSEStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") || r.URL.Query().Get("alt") != "sse" {
			t.Fatalf("expected streamGenerateContent?alt=sse, got %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Gemini\"}]}}]}\n\n"))
	}))
	defer srv.Close()

	var deltas strings.Builder
	resp, err := callLLMStream(context.Background(), config.LLMConfig{
		Provider: "gemini",
		BaseURL:  srv.URL,
		APIKey:   "gm-key",
		Model:    "gemini-2.5-flash",
	}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{
		OnTextDelta: func(d string) { deltas.WriteString(d) },
	})
	if err != nil {
		t.Fatalf("Gemini SSE failed: %v", err)
	}
	if resp.Text != "你好Gemini" || deltas.String() != "你好Gemini" {
		t.Fatalf("unexpected Gemini SSE output: resp=%+v deltas=%q", resp, deltas.String())
	}
}

func TestAgentStartChatAsyncReplayAndCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ag := NewAgent(config.LLMConfig{
		Provider:   "openai",
		BaseURL:    srv.URL,
		APIKey:     "sk-test",
		Model:      "m1",
		TimeoutSec: 5,
	}, &mockExec{}, nil)

	runID, err := ag.StartChatAsync("长耗时任务")
	if err != nil || runID == "" {
		t.Fatalf("StartChatAsync failed: runID=%q err=%v", runID, err)
	}
	snap, ch, unsub, ok := ag.SubscribeRun(runID, 0)
	if !ok {
		t.Fatalf("expected SubscribeRun to find %s", runID)
	}
	defer unsub()
	if snap.RunID != runID {
		t.Fatalf("unexpected runID in snapshot: %+v", snap)
	}

	<-started
	if !ag.IsRunning() {
		t.Fatal("expected IsRunning() true while LLM call is blocked")
	}
	if !ag.CancelActive("user stop") {
		t.Fatal("expected CancelActive to cancel active async run")
	}

	deadline := time.After(2 * time.Second)
	sawCancelled := false
	for _, ev := range snap.Events {
		if ev.Type == "cancelled" {
			sawCancelled = true
		}
	}
	for !sawCancelled {
		select {
		case ev, more := <-ch:
			if !more {
				goto checkReplay
			}
			if ev.Type == "cancelled" {
				sawCancelled = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for cancelled event from SubscribeRun")
		}
	}

checkReplay:
	if !sawCancelled {
		t.Fatal("expected cancelled event from subscriber")
	}
	replay := ag.ReplayEvents(runID, 0)
	if !replay.Finished || replay.Active {
		t.Fatalf("expected finished replay snapshot after cancel, got %+v", replay)
	}
	foundReplayCancel := false
	for _, ev := range replay.Events {
		if ev.Type == "cancelled" {
			foundReplayCancel = true
		}
	}
	if !foundReplayCancel {
		t.Fatalf("expected cancelled event in ReplayEvents: %+v", replay.Events)
	}
}
