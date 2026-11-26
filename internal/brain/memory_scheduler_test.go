package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"android-agent/internal/config"
	"android-agent/internal/guard"
)

func TestMemoryStoreDefaultsAppendAndPromptInjection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	ms := NewMemoryStore(dir)
	if err := ms.EnsureDefaults(); err != nil {
		t.Fatalf("EnsureDefaults failed: %v", err)
	}

	files := ms.List()
	if len(files) != 3 {
		t.Fatalf("expected 3 memory files, got %d", len(files))
	}

	// SOUL.md must forbid AI tool appends, while allowing user Write()
	if err := ms.Append(MemorySoulFile, "overwrite soul"); err == nil {
		t.Fatal("expected Append to SOUL.md to be rejected")
	}
	if err := ms.Append("../etc/passwd", "x"); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}

	if err := ms.Append(MemoryUserFile, "美团点奶茶默认少冰三分糖"); err != nil {
		t.Fatalf("Append MEMORY.md failed: %v", err)
	}
	if err := ms.Append(MemoryLearningsFile, "美团自绘弹窗用 phone_screenshot 兜底"); err != nil {
		t.Fatalf("Append LEARNINGS.md failed: %v", err)
	}

	sec := ms.BuildPromptSection()
	if !strings.Contains(sec, "少冰三分糖") || !strings.Contains(sec, "phone_screenshot") || !strings.Contains(sec, "需要人工确认") {
		t.Fatalf("missing expected memory entries in prompt section:\n%s", sec)
	}
}

func TestAgentInjectsMemoryRecordsTraceAndFlagsConfirmation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	ms := NewMemoryStore(dir)
	_ = ms.EnsureDefaults()
	_ = ms.Append(MemoryUserFile, "咖啡要热美式不加糖")

	var seenSystem string
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
			if sys, ok := msgs[0].(map[string]any); ok {
				seenSystem, _ = sys["content"].(string)
			}
		}
		n := round.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "tc_1",
							"type":     "function",
							"function": map[string]any{"name": "phone_status", "arguments": "{}"},
						}},
					},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role":    "assistant",
					"content": "已加入购物车并停在确认订单页，请核对金额，需要人工确认后付款。",
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
		MaxSteps: 5,
	}, &mockExec{}, nil)
	ag.SetMemoryStore(ms)

	res, err := ag.Chat(context.Background(), "帮我看看手机并准备下单")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if !strings.Contains(seenSystem, "热美式不加糖") {
		t.Fatalf("expected MEMORY.md injected into system prompt, got:\n%s", seenSystem)
	}
	if len(res.ToolTrace) != 1 || res.ToolTrace[0].Tool != "phone_status" || res.ToolTrace[0].Category != "system" {
		t.Fatalf("unexpected ToolTrace: %+v", res.ToolTrace)
	}
	if !res.NeedsConfirmation {
		t.Fatal("expected NeedsConfirmation=true when reply halts at checkout page")
	}
}

type toggleGameDetector struct {
	blocked atomic.Bool
}

func (d *toggleGameDetector) DetectGuardedForeground(_ context.Context, _ []string) (string, error) {
	if d.blocked.Load() {
		return "com.tencent.tmgp.sgame", nil
	}
	return "", nil
}

func TestSchedulerRandomWindowDeferOnGameAndRun(t *testing.T) {
	det := &toggleGameDetector{}
	det.blocked.Store(true)
	g := guard.New(guard.Options{
		Packages: []string{"com.tencent.tmgp.sgame"},
		Detector: det,
	})
	g.CheckOnce(context.Background())

	jm := NewJobManager()
	var ran atomic.Int32
	sched := NewScheduler(config.ScheduleConfig{
		Enabled: true,
		Entries: []config.ScheduleEntry{
			{
				ID:          "daily-all",
				Job:         "daily_all",
				Enabled:     true,
				WindowStart: "08:00",
				WindowEnd:   "09:00",
				DeferOnGame: true,
			},
		},
	}, g, jm, filepath.Join(t.TempDir(), "schedule_history.json"), func(_ context.Context, _ string, _ func(string)) (any, error) {
		ran.Add(1)
		return map[string]any{"ok": true}, nil
	}, nil)

	// Force deterministic midpoint trigger for test
	sched.mu.Lock()
	rt := sched.runtimes["daily-all"]
	baseDay := time.Date(2026, 11, 26, 8, 25, 0, 0, time.Local)
	rt.plannedDate = baseDay.Format("2006-01-02")
	rt.nextRunAt = baseDay
	rt.windowEndTime = time.Date(2026, 11, 26, 9, 0, 0, 0, time.Local)
	sched.mu.Unlock()

	// 1. At 08:26 while sgame is active -> MUST defer (not run!)
	sched.Tick(context.Background(), baseDay.Add(1*time.Minute))
	if ran.Load() != 0 {
		t.Fatal("scheduler must NOT run while guarded game is active")
	}
	st := sched.Status()
	if len(st.Entries) != 1 || st.Entries[0].LastState != "deferred" {
		t.Fatalf("expected deferred state while game active, got %+v", st.Entries)
	}

	// 2. Game exits; guard resumes after grace -> tick after deferred time executes cleanly!
	det.blocked.Store(false)
	g.CheckOnce(context.Background())
	sched.Tick(context.Background(), baseDay.Add(20*time.Minute))

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && ran.Load() == 0 {
		time.Sleep(15 * time.Millisecond)
	}
	if ran.Load() != 1 {
		t.Fatalf("expected scheduler to run once after game exited, got ran=%d", ran.Load())
	}
}
