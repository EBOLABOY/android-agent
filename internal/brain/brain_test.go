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

func TestOpenAIStreamMidStreamErrorMalformedToolArgsAndFallbackDelta(t *testing.T) {
	t.Run("mid-stream EOF error after partial chunk propagates error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// Force chunked transfer then hijack & abruptly close TCP connection mid-chunked stream
			fl, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
			if fl != nil {
				fl.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
		}))
		defer srv.Close()

		_, err := callLLMStream(context.Background(), config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "m1",
		}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{})
		if err == nil {
			t.Fatal("expected error when SSE stream breaks mid-stream after partial text")
		}
	})

	t.Run("malformed tool call JSON fails instead of silent empty args", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_bad\",\"function\":{\"name\":\"phone_tap\",\"arguments\":\"{\\\"x\\\":12\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer srv.Close()

		_, err := callLLMStream(context.Background(), config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "m1",
		}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{})
		if err == nil || !strings.Contains(err.Error(), "arguments JSON") {
			t.Fatalf("expected invalid tool arguments JSON error, got %v", err)
		}
	})

	t.Run("non-stream fallback emits OnTextDelta", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Stream bool `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Stream {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"no stream"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"role": "assistant", "content": "回退流式输出"},
				}},
			})
		}))
		defer srv.Close()

		var deltas strings.Builder
		out, err := callLLMStream(context.Background(), config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "m1",
		}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{
			OnTextDelta: func(d string) { deltas.WriteString(d) },
		})
		if err != nil {
			t.Fatalf("fallback failed: %v", err)
		}
		if out.Text != "回退流式输出" || deltas.String() != "回退流式输出" {
			t.Fatalf("expected fallback to emit OnTextDelta, got text=%q deltas=%q", out.Text, deltas.String())
		}
	})
}

func TestGeminiStreamPreservesWhitespaceAndPropagatesError(t *testing.T) {
	t.Run("preserves spaces and markdown newlines across SSE chunks", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello \"}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"world\\n\\n\"}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"- item 1\"}]}}]}\n\n"))
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
			t.Fatalf("Gemini stream failed: %v", err)
		}
		want := "Hello world\n\n- item 1"
		if deltas.String() != want || resp.Text != want {
			t.Fatalf("expected whitespace/markdown preserved %q, got deltas=%q text=%q", want, deltas.String(), resp.Text)
		}
	})

	t.Run("propagates mid-stream Gemini error object", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"开始\"}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"quota exceeded mid-stream\"}}\n\n"))
		}))
		defer srv.Close()

		_, err := callLLMStream(context.Background(), config.LLMConfig{
			Provider: "gemini",
			BaseURL:  srv.URL,
			APIKey:   "gm-key",
			Model:    "gemini-2.5-flash",
		}, "sys", []ChatMessage{{Role: "user", Text: "hi"}}, nil, StreamCallbacks{})
		if err == nil || !strings.Contains(err.Error(), "quota exceeded mid-stream") {
			t.Fatalf("expected mid-stream Gemini error propagation, got %v", err)
		}
	})
}

func TestBuildOpenAIAndGeminiParallelToolOrdering(t *testing.T) {
	history := []ChatMessage{
		{Role: "user", Text: "截图并查状态"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "tc1", Name: "phone_screenshot", Args: map[string]any{}},
				{ID: "tc2", Name: "phone_status", Args: map[string]any{}},
			},
		},
		{Role: "tool", ToolCallID: "tc1", ToolName: "phone_screenshot", Text: "ok", ImageJPEG: []byte{0xff, 0xd8, 0xff}},
		{Role: "tool", ToolCallID: "tc2", ToolName: "phone_status", Text: `{"ok":true}`},
	}

	_, openBody, err := buildOpenAIRequest(context.Background(), config.LLMConfig{Model: "gpt-4o"}, "sys", history, nil, false)
	if err != nil {
		t.Fatalf("buildOpenAIRequest failed: %v", err)
	}
	var openReq struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(openBody, &openReq); err != nil {
		t.Fatalf("unmarshal openReq: %v", err)
	}
	// Expected order: system, user, assistant, tool(tc1), tool(tc2), user(image)
	wantRoles := []string{"system", "user", "assistant", "tool", "tool", "user"}
	if len(openReq.Messages) != len(wantRoles) {
		t.Fatalf("expected %d OpenAI messages, got %d: %+v", len(wantRoles), len(openReq.Messages), openReq.Messages)
	}
	for i, wr := range wantRoles {
		if openReq.Messages[i].Role != wr {
			t.Fatalf("OpenAI msg[%d] expected role %q, got %q", i, wr, openReq.Messages[i].Role)
		}
	}

	gemBody, err := buildGeminiPayload("sys", history, nil)
	if err != nil {
		t.Fatalf("buildGeminiPayload failed: %v", err)
	}
	var gemReq struct {
		Contents []struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(gemBody, &gemReq); err != nil {
		t.Fatalf("unmarshal gemReq: %v", err)
	}
	// Expected: user, model, single merged user turn containing 2 functionResponses + 1 inlineData
	if len(gemReq.Contents) != 3 {
		t.Fatalf("expected 3 Gemini content turns (merged tool responses), got %d", len(gemReq.Contents))
	}
	if gemReq.Contents[2].Role != "user" || len(gemReq.Contents[2].Parts) != 3 {
		t.Fatalf("expected merged user turn with 3 parts (2 functionResponse + 1 inlineData), got role=%q parts=%d", gemReq.Contents[2].Role, len(gemReq.Contents[2].Parts))
	}
}

func TestDeviceLeaseMutualExclusionAndSyncCancel(t *testing.T) {
	lease := NewDeviceLease()
	jm := NewJobManager()
	jm.SetDeviceLease(lease)

	jobStarted := make(chan struct{})
	jobRelease := make(chan struct{})
	_, err := jm.Start("daily_all", func(ctx context.Context, _ func(string)) (any, error) {
		close(jobStarted)
		<-jobRelease
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("JobManager.Start failed: %v", err)
	}
	<-jobStarted

	ag := NewAgent(config.LLMConfig{Provider: "openai", APIKey: "sk", Model: "m1"}, &mockExec{}, nil)
	ag.SetDeviceLease(lease)

	if _, err := ag.StartChatAsync("同时控机"); err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("expected device busy error while job holds lease, got %v", err)
	}

	close(jobRelease)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && lease.Owner() != "" {
		time.Sleep(5 * time.Millisecond)
	}
	if lease.Owner() != "" {
		t.Fatalf("expected lease released after job finished, still held by %q", lease.Owner())
	}

	// Verify StartChatAsync binds activeCancel synchronously BEFORE returning.
	blockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blockSrv.Close()
	ag.UpdateConfig(config.LLMConfig{Provider: "openai", BaseURL: blockSrv.URL, APIKey: "sk", Model: "m1", TimeoutSec: 5})

	runID, err := ag.StartChatAsync("立即取消测试")
	if err != nil || runID == "" {
		t.Fatalf("StartChatAsync failed: %v", err)
	}
	// Must be immediately cancellable without waiting for goroutine scheduling!
	if !ag.IsRunning() {
		t.Fatal("expected IsRunning() true immediately after StartChatAsync returns")
	}
	if !ag.CancelActive("immediate cancel") {
		t.Fatal("expected immediate CancelActive() to succeed without race")
	}
}

func TestRunReplayNotFoundAndAccumulatedText(t *testing.T) {
	store := newRunEventStore()
	missing := store.snapshot("run-unknown", 0)
	if !missing.NotFound || missing.RunID != "run-unknown" {
		t.Fatalf("expected NotFound=true for unknown run_id, got %+v", missing)
	}

	store.beginRun("run-1", "测试")
	store.emit(RunEvent{Type: "text_delta", Delta: "第一段"})
	store.emit(RunEvent{Type: "text_delta", Delta: "第二段"})
	snap := store.snapshot("run-1", 0)
	if snap.NotFound || snap.AccumulatedText != "第一段第二段" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	other := store.snapshot("run-other", 0)
	if !other.NotFound {
		t.Fatalf("expected NotFound=true for mismatched run_id, got %+v", other)
	}
	store.emit(RunEvent{Type: "tool_start", Tool: "phone_status"})
	snapAfterTool := store.snapshot("run-1", 0)
	if snapAfterTool.AccumulatedText != "" {
		t.Fatalf("expected AccumulatedText cleared on tool_start, got %q", snapAfterTool.AccumulatedText)
	}
}

func TestHistoryCompactionRedactionAndConfirmationNegation(t *testing.T) {
	// 1. In-flight compaction: keep only latest 1 JPEG & collapse phone_ui_tree older than 2 steps
	longTree := "pkg=com.example.app\n" + strings.Repeat("[1] btn 按钮\n", 40)
	h := []ChatMessage{
		{Role: "user", Text: "start"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "phone_ui_tree"}}},
		{Role: "tool", ToolCallID: "1", ToolName: "phone_ui_tree", Text: longTree},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "2", Name: "phone_screenshot"}}},
		{Role: "tool", ToolCallID: "2", ToolName: "phone_screenshot", Text: "img1", ImageJPEG: []byte{1, 2, 3}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "3", Name: "phone_screenshot"}}},
		{Role: "tool", ToolCallID: "3", ToolName: "phone_screenshot", Text: "img2", ImageJPEG: []byte{4, 5, 6}},
	}
	compactInFlightHistory(h)
	if h[4].ImageJPEG != nil || len(h[6].ImageJPEG) == 0 {
		t.Fatalf("expected only latest ImageJPEG kept in-flight, got h[4]=%v h[6]=%v", h[4].ImageJPEG, h[6].ImageJPEG)
	}
	if !strings.HasPrefix(h[2].Text, "[旧 UI 树已压缩:") {
		t.Fatalf("expected old phone_ui_tree to be collapsed, got %q", h[2].Text)
	}

	// 2. saveHistory strips ALL ImageJPEG and hard-trims even when last 24 messages have no user message
	ag := NewAgent(config.LLMConfig{}, &mockExec{}, nil)
	big := []ChatMessage{{Role: "user", Text: "初始指令"}}
	for i := 0; i < 16; i++ {
		big = append(big,
			ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "id", Name: "phone_screenshot"}}},
			ChatMessage{Role: "tool", ToolCallID: "id", ToolName: "phone_screenshot", Text: "ok", ImageJPEG: []byte{9, 9}},
		)
	}
	ag.saveHistory(big)
	saved := ag.History()
	if len(saved) > 24 {
		t.Fatalf("expected hard trim <= 24 messages, got %d", len(saved))
	}
	if len(saved) == 0 || saved[0].Role == "tool" {
		t.Fatalf("expected saved history not to start with orphan tool message, got %+v", saved[0])
	}
	for i, m := range saved {
		if len(m.ImageJPEG) > 0 {
			t.Fatalf("expected all ImageJPEG stripped on saveHistory, found non-empty at %d", i)
		}
	}

	// 3. Sensitive tool outcome redaction
	for _, tool := range []string{"phone_input_text", "phone_clipboard_set", "phone_clipboard_get"} {
		sum := summarizeToolOutcome(tool, ToolOutput{Text: "my-secret-otp-998877"})
		if strings.Contains(sum, "998877") || strings.Contains(sum, "secret") {
			t.Fatalf("expected sensitive output redacted for %s, got %q", tool, sum)
		}
	}

	// 4. Negation-aware replyNeedsConfirmation
	negatives := []string{
		"任务已完成，全程未发现验证码。",
		"所有签到已完成，无需确认。",
		"未遇到滑块，没有遇到验证弹窗。",
		"不需要人工确认，已安全结束。",
	}
	for _, s := range negatives {
		if replyNeedsConfirmation(s) {
			t.Fatalf("false positive in replyNeedsConfirmation(%q)", s)
		}
	}
	positives := []string{
		"已停在确认订单页，需要人工确认后付款。",
		"未发现滑块，但当前已到达确认订单页，请核对。",
		"页面弹出滑块验证码，请亲自完成。",
		"账号登录失效，请手动确认后继续。",
	}
	for _, s := range positives {
		if !replyNeedsConfirmation(s) {
			t.Fatalf("missed positive in replyNeedsConfirmation(%q)", s)
		}
	}
}
