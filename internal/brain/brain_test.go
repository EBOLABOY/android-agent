package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
			if c, ok := m["content"].(string); ok && strings.Contains(c, "[系统防卡死提示]") {
				sawLoopNudge.Store(true)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if sawLoopNudge.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"role": "assistant", "content": "已收到防卡死提示，切换策略汇报。"},
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
	if !strings.Contains(res.Reply, "防卡死") {
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
	// 1. In-flight compaction: keep only latest 1 JPEG & keep only latest 2 UI-tree blocks
	longTree1 := "Timing: 4ms | raw=80 | truncated=false\nApp: com.example.app (1080x2400, 20 nodes, ~200 tok)\n" + strings.Repeat("[1] Btn \"按钮1\" (click)\n", 10)
	longTree2 := "Timing: 5ms | raw=80 | truncated=false\nApp: com.example.app (1080x2400, 20 nodes, ~200 tok)\n[1] Btn \"按钮2\" (click)"
	longTree3 := "{\"ok\":true,\"id\":1}\n\n[操作后最新界面]\nApp: com.example.app (1080x2400, 5 nodes, ~60 tok)\n[1] Btn \"按钮3\" (click)"
	h := []ChatMessage{
		{Role: "user", Text: "start"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "phone_ui_tree"}}},
		{Role: "tool", ToolCallID: "1", ToolName: "phone_ui_tree", Text: longTree1},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "2", Name: "phone_screenshot"}}},
		{Role: "tool", ToolCallID: "2", ToolName: "phone_screenshot", Text: "img1", ImageJPEG: []byte{1, 2, 3}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "3", Name: "phone_ui_tree"}}},
		{Role: "tool", ToolCallID: "3", ToolName: "phone_ui_tree", Text: longTree2},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "4", Name: "phone_screenshot"}}},
		{Role: "tool", ToolCallID: "4", ToolName: "phone_screenshot", Text: "img2", ImageJPEG: []byte{4, 5, 6}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "5", Name: "phone_tap_node"}}},
		{Role: "tool", ToolCallID: "5", ToolName: "phone_tap_node", Text: longTree3},
	}
	compactInFlightHistory(h)
	if h[4].ImageJPEG != nil || len(h[8].ImageJPEG) == 0 {
		t.Fatalf("expected only latest ImageJPEG kept in-flight, got h[4]=%v h[8]=%v", h[4].ImageJPEG, h[8].ImageJPEG)
	}
	if h[2].Text != collapsedUITreeMarker {
		t.Fatalf("expected 3rd-newest phone_ui_tree to be collapsed to %q, got %q", collapsedUITreeMarker, h[2].Text)
	}
	if !strings.Contains(h[6].Text, "按钮2") || !strings.Contains(h[10].Text, "按钮3") {
		t.Fatalf("expected latest 2 UI-tree observations kept intact, got h[6]=%q h[10]=%q", h[6].Text, h[10].Text)
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

type richTraceExec struct{}

func (richTraceExec) ToolSpecs() []ToolSpec {
	return []ToolSpec{
		{Name: "phone_ui_tree", Description: "tree", Parameters: map[string]any{"type": "object"}},
		{Name: "phone_input_text", Description: "input", Parameters: map[string]any{"type": "object"}},
		{Name: "phone_screenshot", Description: "shot", Parameters: map[string]any{"type": "object"}},
	}
}

func (richTraceExec) ExecuteTool(_ context.Context, name string, _ map[string]any) ToolOutput {
	switch name {
	case "phone_ui_tree":
		return ToolOutput{Text: "pkg=com.jingdong.app.mall\n[1] Button \"签到\"\n[2] Text \"首页\""}
	case "phone_input_text":
		return ToolOutput{Text: "typed secret-passcode-889900"}
	case "phone_screenshot":
		return ToolOutput{Text: "captured", ImageJPEG: make([]byte, 4096)}
	default:
		return ToolOutput{Text: "ok"}
	}
}

func TestAgentSavedTurnsPersistenceHydrationRedactionAndClear(t *testing.T) {
	histFile := filepath.Join(t.TempDir(), "data", "chat_history.json")
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := round.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{"id": "t1", "type": "function", "function": map[string]any{"name": "phone_ui_tree", "arguments": `{}`}},
							{"id": "t2", "type": "function", "function": map[string]any{"name": "phone_input_text", "arguments": `{"text":"secret-passcode-889900"}`}},
							{"id": "t3", "type": "function", "function": map[string]any{"name": "phone_screenshot", "arguments": `{}`}},
						},
					},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role":    "assistant",
					"content": fmt.Sprintf("第 %d 轮已完成检查。", n),
				},
			}},
		})
	}))
	defer srv.Close()

	ag := NewAgent(config.LLMConfig{
		Provider: "openai",
		BaseURL:  srv.URL,
		APIKey:   "sk-test",
		Model:    "gpt-test",
		MaxSteps: 6,
	}, richTraceExec{}, nil)
	ag.SetHistoryFile(histFile)

	res, err := ag.Chat(context.Background(), "检查京东页面并输入口令")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(res.ToolTrace) != 3 {
		t.Fatalf("expected 3 tool steps, got %+v", res.ToolTrace)
	}
	if res.ToolTrace[0].Label != "读取页面结构" || res.ToolTrace[1].Label != "输入文本" || res.ToolTrace[2].Label != "屏幕快照" {
		t.Fatalf("unexpected crisp labels in ToolTrace: %+v", res.ToolTrace)
	}

	info, err := os.Stat(histFile)
	if err != nil {
		t.Fatalf("expected chat_history.json created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions on chat_history.json, got %o", info.Mode().Perm())
	}
	rawBytes, err := os.ReadFile(histFile)
	if err != nil {
		t.Fatalf("read chat_history.json: %v", err)
	}
	if strings.Contains(string(rawBytes), "889900") || strings.Contains(string(rawBytes), "secret-passcode") {
		t.Fatalf("sensitive input leaked into chat_history.json:\n%s", string(rawBytes))
	}

	// Push >40 turns to verify 40-turn cap and 12-exchange hydration cap on reload.
	for i := 0; i < 44; i++ {
		if _, err := ag.Chat(context.Background(), fmt.Sprintf("后续提问 #%d", i+1)); err != nil {
			t.Fatalf("Chat #%d failed: %v", i+1, err)
		}
	}
	if got := len(ag.SavedTurns()); got != 40 {
		t.Fatalf("expected SavedTurns capped at 40, got %d", got)
	}

	// Simulate daemon restart with a fresh Agent reading chat_history.json.
	agReload := NewAgent(config.LLMConfig{Provider: "openai", BaseURL: srv.URL, APIKey: "sk-test", Model: "gpt-test"}, richTraceExec{}, nil)
	agReload.SetHistoryFile(histFile)
	reloadedTurns := agReload.SavedTurns()
	if len(reloadedTurns) != 40 {
		t.Fatalf("expected 40 reloaded turns, got %d", len(reloadedTurns))
	}
	hydrated := agReload.History()
	if len(hydrated) != 24 { // 12 user+assistant exchanges
		t.Fatalf("expected 24 hydrated LLM messages (12 exchanges), got %d", len(hydrated))
	}
	if hydrated[len(hydrated)-2].Text != "后续提问 #44" {
		t.Fatalf("expected latest exchange hydrated, got %+v", hydrated[len(hydrated)-2])
	}

	// ClearHistory must wipe RAM history, SavedTurns, and chat_history.json on disk.
	agReload.ClearHistory()
	if len(agReload.History()) != 0 || len(agReload.SavedTurns()) != 0 {
		t.Fatalf("expected empty history & SavedTurns after ClearHistory")
	}
	agAfterClear := NewAgent(config.LLMConfig{}, richTraceExec{}, nil)
	agAfterClear.SetHistoryFile(histFile)
	if len(agAfterClear.SavedTurns()) != 0 || len(agAfterClear.History()) != 0 {
		t.Fatalf("expected persisted chat_history.json cleared on disk, got turns=%d", len(agAfterClear.SavedTurns()))
	}
}

func TestJobManagerProgressObserverRecentLogsAndCancel(t *testing.T) {
	jm := NewJobManager()
	var obsMu sync.Mutex
	var events []WorkflowProgressEvent
	jm.SetProgressObserver(func(ev WorkflowProgressEvent) {
		obsMu.Lock()
		events = append(events, ev)
		obsMu.Unlock()
	})

	doneCh := make(chan struct{})
	_, err := jm.Start("ms_rewards", func(ctx context.Context, logFn func(string)) (any, error) {
		defer close(doneCh)
		logFn("[阶段 1/4] 检查 Edge Daily Set")
		for i := 1; i <= 85; i++ {
			logFn(fmt.Sprintf("detail line #%d", i))
		}
		logFn("[阶段 2/4] 执行必应搜索 (120/200)")
		jm.AppendLog("[阶段 3/4] Bing 每日签入")
		logFn("[阶段 4/4] 阅读新闻 (18/30)")
		jm.UpdateProgress(4, 4, 95, "阅读新闻收尾")
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("jm.Start failed: %v", err)
	}
	<-doneCh
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && jm.Status().State == "running" {
		time.Sleep(5 * time.Millisecond)
	}

	st := jm.Status()
	if st.State != "completed" || st.ProgressPct != 100 || st.Step != 4 || st.TotalSteps != 4 {
		t.Fatalf("unexpected completed JobStatus: %+v", st)
	}
	if len(st.RecentLogs) != 80 {
		t.Fatalf("expected RecentLogs ring buffer capped at 80 lines, got %d", len(st.RecentLogs))
	}
	if !strings.Contains(st.RecentLogs[len(st.RecentLogs)-1], "completed") {
		t.Fatalf("expected newest completion log at tail of RecentLogs, got %q", st.RecentLogs[len(st.RecentLogs)-1])
	}

	obsMu.Lock()
	evs := append([]WorkflowProgressEvent(nil), events...)
	obsMu.Unlock()
	var sawSearch40, sawUpdate95, sawDone100 bool
	for _, ev := range evs {
		if ev.Step == 2 && ev.Total == 4 && ev.Pct == 40 && strings.Contains(ev.Stage, "120/200") {
			sawSearch40 = true
		}
		if ev.Pct == 95 && ev.Stage == "阅读新闻收尾" {
			sawUpdate95 = true
		}
		if ev.Done && ev.Pct == 100 && ev.Err == "" {
			sawDone100 = true
		}
	}
	if !sawSearch40 || !sawUpdate95 || !sawDone100 {
		t.Fatalf("missing expected progress events: search40=%v update95=%v done100=%v events=%+v", sawSearch40, sawUpdate95, sawDone100, evs)
	}

	// Test immediate Cancel() marking status "cancelled" and cancelling ctx immediately.
	started := make(chan struct{})
	ctxDoneObserved := make(chan error, 1)
	_, err = jm.Start("jd_daily", func(ctx context.Context, logFn func(string)) (any, error) {
		logFn("[JD 1/3] 领京豆签到")
		close(started)
		<-ctx.Done()
		ctxDoneObserved <- ctx.Err()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("second jm.Start failed: %v", err)
	}
	<-started
	if !jm.Cancel() {
		t.Fatal("expected jm.Cancel() to return true for running job")
	}
	// Status MUST be "cancelled" immediately after Cancel() returns!
	cancelSnap := jm.Status()
	if cancelSnap.State != "cancelled" || cancelSnap.CurrentStage != "已取消" {
		t.Fatalf("expected immediate State=cancelled on Cancel(), got %+v", cancelSnap)
	}
	select {
	case cerr := <-ctxDoneObserved:
		if cerr != context.Canceled {
			t.Fatalf("expected context.Canceled inside job, got %v", cerr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workflow context cancellation")
	}
}

type v041Exec struct {
	mu      sync.Mutex
	calls   []string
	sameUI  bool
	stepNum int
}

func (e *v041Exec) ToolSpecs() []ToolSpec {
	return []ToolSpec{
		{Name: "phone_tap_node", Description: "tap node", Parameters: map[string]any{"type": "object"}},
		{Name: "phone_scroll_node", Description: "scroll node", Parameters: map[string]any{"type": "object"}},
		{Name: "phone_status", Description: "status", Parameters: map[string]any{"type": "object"}},
	}
}

func (e *v041Exec) ExecuteTool(_ context.Context, name string, args map[string]any) ToolOutput {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stepNum++
	b, _ := json.Marshal(args)
	e.calls = append(e.calls, name+":"+string(b))

	if name == "phone_status" {
		return ToolOutput{Text: `{"guard":{"state":"active"}}`}
	}
	nodeLine := fmt.Sprintf("[1] Btn \"动态按钮_%d\" (click)", e.stepNum)
	if e.sameUI {
		nodeLine = `[1] Btn "被弹窗遮挡的死按钮" (click)`
	}
	return ToolOutput{
		Text: fmt.Sprintf(`{"ok":true,"step":%d}`+"\n\n[操作后最新界面]\nApp: com.sankuai.meituan (1080x2400, 1 nodes, ~25 tok)\n%s", e.stepNum, nodeLine),
	}
}

func TestV041PeriodCyclesUnchangedFingerprintAndEarlyStallBreak(t *testing.T) {
	// 1. Direct unit checks on detectToolCallCycle (period 1, 2, 3) & extractUITreeFingerprint
	if got := detectToolCallCycle([]string{"A", "A", "A"}); got != 1 {
		t.Fatalf("expected period-1 cycle, got %d", got)
	}
	if got := detectToolCallCycle([]string{"A", "B", "A", "B"}); got != 2 {
		t.Fatalf("expected period-2 cycle, got %d", got)
	}
	if got := detectToolCallCycle([]string{"A", "B", "C", "A", "B", "C"}); got != 3 {
		t.Fatalf("expected period-3 cycle, got %d", got)
	}
	if got := detectToolCallCycle([]string{"A", "B", "C", "D"}); got != 0 {
		t.Fatalf("expected no cycle, got %d", got)
	}

	// 2. Period-2 cycle (A -> B -> A -> B) triggers [系统防卡死提示]
	t.Run("period-2 A->B->A->B cycle triggers anti-stall prompt", func(t *testing.T) {
		var round atomic.Int32
		var sawPeriod2Hint atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Messages []map[string]any `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			for _, m := range req.Messages {
				if c, ok := m["content"].(string); ok && strings.Contains(c, "[系统防卡死提示]") && strings.Contains(c, "周期-2") {
					sawPeriod2Hint.Store(true)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if sawPeriod2Hint.Load() {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{"role": "assistant", "content": "已识别到 A->B->A->B 周期循环，立即结束并汇报。"},
					}},
				})
				return
			}
			n := round.Add(1)
			var tcName, tcArgs string
			if n%2 == 1 {
				tcName, tcArgs = "phone_tap_node", `{"id":1}`
			} else {
				tcName, tcArgs = "phone_scroll_node", `{"direction":"down","id":3}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       fmt.Sprintf("tc_%d", n),
							"type":     "function",
							"function": map[string]any{"name": tcName, "arguments": tcArgs},
						}},
					},
				}},
			})
		}))
		defer srv.Close()

		exec := &v041Exec{sameUI: false}
		ag := NewAgent(config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "gpt-test",
			MaxSteps: 12,
		}, exec, nil)

		res, err := ag.Chat(context.Background(), "测试 A->B->A->B 检测")
		if err != nil {
			t.Fatalf("Chat failed: %v", err)
		}
		if !sawPeriod2Hint.Load() {
			t.Fatal("expected period-2 cycle [系统防卡死提示] after A->B->A->B")
		}
		if res.Steps != 5 {
			t.Fatalf("expected completion on step 5 right after period-2 hint on step 4, got steps=%d", res.Steps)
		}
	})

	// 3. 3x unchanged UI fingerprint (distinct mutating calls!) triggers [系统防卡死提示],
	//    and 6x unchanged breaks early out of an 80-step ceiling into tool-free summary!
	t.Run("3x unchanged UI fingerprint warns and 6x breaks early before MaxSteps=80", func(t *testing.T) {
		var round atomic.Int32
		var sawUnchangedAtStep4 atomic.Bool
		var finalCallHadNoTools atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := round.Add(1)
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			msgs, _ := req["messages"].([]any)
			for _, rawM := range msgs {
				m, _ := rawM.(map[string]any)
				if c, ok := m["content"].(string); ok && strings.Contains(c, "[系统防卡死提示]") && strings.Contains(c, "UI 指纹") {
					if n == 4 {
						sawUnchangedAtStep4.Store(true)
					}
				}
			}

			w.Header().Set("Content-Type", "application/json")
			if _, hasTools := req["tools"]; !hasTools {
				finalCallHadNoTools.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{
							"role":    "assistant",
							"content": "1) 已尝试点击多个入口；2) 当前被弹窗遮挡无变化；3) 请手动关闭弹窗后点击继续执行。",
						},
					}},
				})
				return
			}

			// Every step calls a DIFFERENT node id so it is NOT a tool-signature cycle, ONLY unchanged UI fingerprint!
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       fmt.Sprintf("tc_u_%d", n),
							"type":     "function",
							"function": map[string]any{"name": "phone_tap_node", "arguments": fmt.Sprintf(`{"id":%d}`, n)},
						}},
					},
				}},
			})
		}))
		defer srv.Close()

		exec := &v041Exec{sameUI: true}
		ag := NewAgent(config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "gpt-test",
			MaxSteps: 80,
		}, exec, nil)

		res, err := ag.Chat(context.Background(), "测试连续无变化提前熔断")
		if err != nil {
			t.Fatalf("Chat failed: %v", err)
		}
		if !sawUnchangedAtStep4.Load() {
			t.Fatal("expected 3x unchanged UI fingerprint [系统防卡死提示] to be visible on LLM call #4")
		}
		if res.Steps != 6 {
			t.Fatalf("expected early stall break at 6 unchanged steps (not 80), got steps=%d", res.Steps)
		}
		if !finalCallHadNoTools.Load() || !res.CheckpointPaused {
			t.Fatalf("expected tool-free summary and CheckpointPaused=true, got noTools=%v res=%+v", finalCallHadNoTools.Load(), res)
		}
	})
}

func TestV041MaxStepsGracefulToolFreeSummaryAndFallback(t *testing.T) {
	t.Run("streams tool-free final LLM summary and sets CheckpointPaused", func(t *testing.T) {
		histFile := filepath.Join(t.TempDir(), "chat_history.json")
		var calls atomic.Int32
		var sawSummaryPrompt, summaryOmittedTools atomic.Bool

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			rawBody, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(rawBody, &req)

			if n <= 3 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{{
								"id":       fmt.Sprintf("step_%d", n),
								"type":     "function",
								"function": map[string]any{"name": "phone_tap_node", "arguments": fmt.Sprintf(`{"id":%d}`, n)},
							}},
						},
					}},
				})
				return
			}

			// 4th call MUST be the tool-free checkpoint summary!
			if _, hasTools := req["tools"]; !hasTools {
				summaryOmittedTools.Store(true)
			}
			if strings.Contains(string(rawBody), "已到达当前阶段步数检查点") {
				sawSummaryPrompt.Store(true)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"已完成前 3 步点击，\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"如需继续请点击下方「继续执行」。\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer srv.Close()

		exec := &v041Exec{sameUI: false}
		ag := NewAgent(config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "gpt-test",
			MaxSteps: 3,
		}, exec, nil)
		ag.SetHistoryFile(histFile)

		var streamedDeltas strings.Builder
		var doneEvent RunEvent
		res, err := ag.ChatStream(context.Background(), "执行长链路多步任务", func(ev RunEvent) {
			if ev.Type == "text_delta" {
				streamedDeltas.WriteString(ev.Delta)
			}
			if ev.Type == "done" {
				doneEvent = ev
			}
		})
		if err != nil {
			t.Fatalf("ChatStream failed: %v", err)
		}
		if !summaryOmittedTools.Load() || !sawSummaryPrompt.Load() {
			t.Fatalf("expected tool-free summary call with checkpoint prompt: noTools=%v sawPrompt=%v", summaryOmittedTools.Load(), sawSummaryPrompt.Load())
		}
		if !res.CheckpointPaused || !doneEvent.CheckpointPaused {
			t.Fatalf("expected CheckpointPaused=true on result & done event: res=%+v ev=%+v", res, doneEvent)
		}
		if !strings.Contains(streamedDeltas.String(), "已完成前 3 步点击") || res.Reply != streamedDeltas.String() {
			t.Fatalf("unexpected streamed checkpoint summary: deltas=%q reply=%q", streamedDeltas.String(), res.Reply)
		}
		turns := ag.SavedTurns()
		if len(turns) != 1 || !turns[0].CheckpointPaused {
			t.Fatalf("expected SavedTurn with CheckpointPaused=true, got %+v", turns)
		}
	})

	t.Run("falls back cleanly to trace summary if final LLM call fails", func(t *testing.T) {
		oldDelay := llmRetryBaseDelay
		llmRetryBaseDelay = time.Millisecond
		defer func() { llmRetryBaseDelay = oldDelay }()

		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n <= 2 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{{
								"id":       fmt.Sprintf("s_%d", n),
								"type":     "function",
								"function": map[string]any{"name": "phone_tap_node", "arguments": fmt.Sprintf(`{"id":%d}`, n)},
							}},
						},
					}},
				})
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream error"))
		}))
		defer srv.Close()

		exec := &v041Exec{sameUI: false}
		ag := NewAgent(config.LLMConfig{
			Provider: "openai",
			BaseURL:  srv.URL,
			APIKey:   "sk-test",
			Model:    "gpt-test",
			MaxSteps: 2,
		}, exec, nil)

		res, err := ag.Chat(context.Background(), "测试汇总失败兜底")
		if err != nil {
			t.Fatalf("expected graceful fallback summary instead of error, got %v", err)
		}
		if !res.CheckpointPaused || !strings.Contains(res.Reply, "继续执行剩余步骤") {
			t.Fatalf("unexpected fallback summary result: %+v", res)
		}
	})
}

func TestV041SlidingWindowAutoObservedCompaction(t *testing.T) {
	mkObs := func(id int, label string) string {
		return fmt.Sprintf(`{"ok":true,"id":%d,"label":%q}`+"\n\n[操作后最新界面]\nApp: com.sankuai.meituan (1080x2400, 12 nodes, ~180 tok)\n[1] Btn %q (click)", id, label, label)
	}
	h := []ChatMessage{
		{Role: "user", Text: "点选商品"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "phone_tap_node"}}},
		{Role: "tool", ToolCallID: "c1", ToolName: "phone_tap_node", Text: mkObs(1, "第一页外卖")},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "phone_tap_node"}}},
		{Role: "tool", ToolCallID: "c2", ToolName: "phone_tap_node", Text: mkObs(2, "第二页奶茶")},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c3", Name: "phone_tap_node"}}},
		{Role: "tool", ToolCallID: "c3", ToolName: "phone_tap_node", Text: mkObs(3, "第三页选规格")},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c4", Name: "phone_tap_node"}}},
		{Role: "tool", ToolCallID: "c4", ToolName: "phone_tap_node", Text: mkObs(4, "第四页加购")},
	}

	compactInFlightHistory(h)

	// Older 2 tool messages (h[2], h[4]) MUST preserve action JSON header and collapse [操作后最新界面]
	for _, idx := range []int{2, 4} {
		if !strings.HasPrefix(h[idx].Text, `{"ok":true,`) {
			t.Fatalf("h[%d] lost JSON action header: %q", idx, h[idx].Text)
		}
		if strings.Contains(h[idx].Text, "[操作后最新界面]") {
			t.Fatalf("h[%d] should have collapsed [操作后最新界面]: %q", idx, h[idx].Text)
		}
		if !strings.Contains(h[idx].Text, collapsedUITreeMarker) {
			t.Fatalf("h[%d] missing collapsed marker %q: %q", idx, collapsedUITreeMarker, h[idx].Text)
		}
	}
	// Latest 2 tool messages (h[6], h[8]) MUST keep full [操作后最新界面]
	if !strings.Contains(h[6].Text, "[操作后最新界面]") || !strings.Contains(h[6].Text, "第三页选规格") {
		t.Fatalf("expected 2nd latest UI tree kept intact in h[6], got %q", h[6].Text)
	}
	if !strings.Contains(h[8].Text, "[操作后最新界面]") || !strings.Contains(h[8].Text, "第四页加购") {
		t.Fatalf("expected latest UI tree kept intact in h[8], got %q", h[8].Text)
	}
}
