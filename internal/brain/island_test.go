package brain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockLiveProgressClient struct {
	mu        sync.Mutex
	updates   []LiveProgressUpdate
	cancels   []string
	sendDelay time.Duration
	sendErr   error
}

func (m *mockLiveProgressClient) UpdateLiveProgress(ctx context.Context, u LiveProgressUpdate) error {
	m.mu.Lock()
	delay := m.sendDelay
	err := m.sendErr
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		return err
	}
	m.updates = append(m.updates, u)
	return nil
}

func (m *mockLiveProgressClient) CancelLiveProgress(_ context.Context, tag string, id int) error {
	m.mu.Lock()
	m.cancels = append(m.cancels, fmt.Sprintf("%s#%d", tag, id))
	m.mu.Unlock()
	return nil
}

func (m *mockLiveProgressClient) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.updates), len(m.cancels)
}

func (m *mockLiveProgressClient) lastUpdate() (LiveProgressUpdate, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.updates) == 0 {
		return LiveProgressUpdate{}, false
	}
	return m.updates[len(m.updates)-1], true
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(8 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestIslandControllerCoalescingTrailingEdgeConfirmAndGuardClear(t *testing.T) {
	client := &mockLiveProgressClient{}
	ctrl := NewIslandController(client)
	defer ctrl.Close()
	ctrl.minUpdateInterval = 90 * time.Millisecond
	ctrl.doneClearDelay = 70 * time.Millisecond
	ctrl.confirmClearDelay = 320 * time.Millisecond

	// Idle: 0 notifications.
	if ctrl.IsActive() {
		t.Fatal("expected idle controller not active")
	}
	up, can := client.counts()
	if up != 0 || can != 0 {
		t.Fatalf("expected 0 idle updates/cancels, got %d/%d", up, can)
	}

	// 1. Start run -> immediate forced update, emoji-free typography, ClickURI populated.
	ctrl.HandleEvent(RunEvent{RunID: "run-1", Type: "run_start", Summary: "🤖 查看当前屏幕", MaxSteps: 8})
	if !ctrl.IsActive() {
		t.Fatal("expected immediately active after run_start")
	}
	waitForCondition(t, 400*time.Millisecond, "run_start update", func() bool {
		u, _ := client.counts()
		return u == 1
	})
	first, _ := client.lastUpdate()
	if first.State != "starting" || strings.Contains(first.Title, "🤖") || first.ClickURI != DefaultIslandClickURI {
		t.Fatalf("unexpected run_start payload: %+v", first)
	}

	// 2. Burst of fast intermediate updates within 90ms window -> coalesced, AND trailing-edge delivers latest!
	ctrl.HandleEvent(RunEvent{RunID: "run-1", Type: "thinking", Step: 1, MaxSteps: 8})
	ctrl.HandleEvent(RunEvent{RunID: "run-1", Type: "tool_start", Step: 1, MaxSteps: 8, Tool: "phone_ui_tree", Label: "读取结构"})
	ctrl.HandleEvent(RunEvent{RunID: "run-1", Type: "thinking", Step: 2, MaxSteps: 8})
	ctrl.HandleEvent(RunEvent{RunID: "run-1", Type: "tool_start", Step: 2, MaxSteps: 8, Tool: "phone_tap_node", Label: "点击结算", Summary: "点击去结算按钮"})

	// Immediately during burst window: still 1 sent IPC.
	up, _ = client.counts()
	if up != 1 {
		t.Fatalf("expected burst within minUpdateInterval to coalesce (1 sent so far), got %d", up)
	}

	// Trailing-edge flush MUST deliver the latest update ("点击结算" step 2/8) automatically without another event!
	waitForCondition(t, 400*time.Millisecond, "trailing-edge coalesced flush", func() bool {
		u, _ := client.counts()
		return u == 2
	})
	flushed, _ := client.lastUpdate()
	if flushed.State != "acting" || flushed.ShortText != "点击结算" || flushed.Step != 2 || flushed.Progress != 25 || !flushed.Ongoing {
		t.Fatalf("expected trailing-edge flush to deliver latest step 2/8, got %+v", flushed)
	}

	// 3. Done -> immediate forced terminal update + auto clear after doneClearDelay.
	ctrl.HandleEvent(RunEvent{RunID: "run-1", Type: "done", Steps: 2, MaxSteps: 8, ElapsedSec: 0.6})
	waitForCondition(t, 400*time.Millisecond, "done terminal update", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "done"
	})
	doneUpd, _ := client.lastUpdate()
	if doneUpd.Ongoing || doneUpd.Title != "任务已完成" || doneUpd.ShortText != "已完成" || doneUpd.Progress != 100 {
		t.Fatalf("unexpected done payload: %+v", doneUpd)
	}

	waitForCondition(t, 400*time.Millisecond, "auto-clear after doneClearDelay", func() bool {
		_, c := client.counts()
		return c == 1 && !ctrl.IsActive()
	})

	// 4. Run with needs_confirmation -> retained past doneClearDelay, cleared synchronously by ClearImmediate,
	// and late events from the cleared RunID are suppressed.
	ctrl.HandleEvent(RunEvent{RunID: "run-2", Type: "run_start", Summary: "下单流程", MaxSteps: 5})
	ctrl.HandleEvent(RunEvent{RunID: "run-2", Type: "needs_confirmation", Step: 3, MaxSteps: 5, Summary: "到达确认订单页"})
	ctrl.HandleEvent(RunEvent{RunID: "run-2", Type: "done", NeedsConfirmation: true, Reply: "已停在付款确认页", Steps: 3, MaxSteps: 5})

	waitForCondition(t, 400*time.Millisecond, "confirm retention state", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "confirm" && !last.Ongoing
	})
	time.Sleep(110 * time.Millisecond)
	if !ctrl.IsActive() {
		t.Fatal("expected confirmation capsule to remain active past doneClearDelay")
	}
	if last, ok := client.lastUpdate(); !ok || last.Title != "需要人工核对" || last.ShortText != "待确认" {
		t.Fatalf("unexpected confirmation retention payload: %+v", last)
	}

	ctrl.ClearImmediate("game guard triggered")
	_, canAfterGuard := client.counts()
	if ctrl.IsActive() || canAfterGuard != 2 {
		t.Fatalf("expected synchronous ClearImmediate to cancel once cleanly (active=%v cancels=%d)", ctrl.IsActive(), canAfterGuard)
	}

	// Late cancelled event from already guard-cleared run-2 MUST NOT re-activate island.
	upBeforeLate, _ := client.counts()
	ctrl.HandleEvent(RunEvent{RunID: "run-2", Type: "cancelled", Error: "context canceled"})
	time.Sleep(50 * time.Millisecond)
	upAfterLate, canFinal := client.counts()
	if ctrl.IsActive() || upAfterLate != upBeforeLate {
		t.Fatalf("expected late event from suppressed RunID to be ignored (active=%v updates=%d->%d)", ctrl.IsActive(), upBeforeLate, upAfterLate)
	}

	// Wait past confirmClearDelay to ensure stale timer generation is ignored.
	time.Sleep(260 * time.Millisecond)
	_, canFinal = client.counts()
	if canFinal != 2 {
		t.Fatalf("expected no stale double-cancel after ClearImmediate, got %d", canFinal)
	}
}

func TestIslandControllerNonBlockingUnderSlowIPC(t *testing.T) {
	client := &mockLiveProgressClient{sendDelay: 180 * time.Millisecond}
	ctrl := NewIslandController(client)
	defer ctrl.Close()

	start := time.Now()
	ctrl.HandleEvent(RunEvent{RunID: "run-slow", Type: "run_start", Summary: "慢速IPC非阻塞测试", MaxSteps: 6})
	ctrl.HandleEvent(RunEvent{RunID: "run-slow", Type: "thinking", Step: 1, MaxSteps: 6})
	elapsed := time.Since(start)
	if elapsed > 30*time.Millisecond {
		t.Fatalf("HandleEvent must be non-blocking even when IPC takes 180ms, took %v", elapsed)
	}

	ctrl.ClearImmediate("cleanup")
}

func TestIslandDuplicateDonePreservesConfirmation(t *testing.T) {
	client := &mockLiveProgressClient{}
	ctrl := NewIslandController(client)
	defer ctrl.Close()
	ctrl.doneClearDelay = 40 * time.Millisecond
	ctrl.confirmClearDelay = 300 * time.Millisecond

	ctrl.HandleEvent(RunEvent{RunID: "run-confirm", Type: "run_start", Summary: "测试确认", MaxSteps: 3})
	ctrl.HandleEvent(RunEvent{RunID: "run-confirm", Type: "needs_confirmation", Step: 2, MaxSteps: 3})
	ctrl.HandleEvent(RunEvent{RunID: "run-confirm", Type: "done", NeedsConfirmation: true, Reply: "停在付款页", Steps: 2, MaxSteps: 3})

	waitForCondition(t, 400*time.Millisecond, "confirm done delivered", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "confirm" && !last.Ongoing
	})
	upBefore, _ := client.counts()

	// Duplicate done (even with NeedsConfirmation=false) must NOT overwrite confirmation state or shorten clear delay.
	ctrl.HandleEvent(RunEvent{RunID: "run-confirm", Type: "done", NeedsConfirmation: false, Reply: "重复done", Steps: 2, MaxSteps: 3})
	time.Sleep(80 * time.Millisecond)
	upAfter, _ := client.counts()
	if upAfter != upBefore {
		t.Fatalf("expected duplicate done after confirmation to be ignored, updates went %d -> %d", upBefore, upAfter)
	}
	if !ctrl.IsActive() {
		t.Fatal("expected confirmation state to survive past doneClearDelay after duplicate done")
	}
	if last, ok := client.lastUpdate(); !ok || last.Title != "需要人工核对" {
		t.Fatalf("expected confirmation title preserved, got %+v", last)
	}
	ctrl.ClearImmediate("cleanup")
}

func TestIslandWorkflowProgressAndPreviewDemo(t *testing.T) {
	client := &mockLiveProgressClient{}
	ctrl := NewIslandController(client)
	defer ctrl.Close()
	ctrl.minUpdateInterval = 60 * time.Millisecond
	ctrl.doneClearDelay = 60 * time.Millisecond

	// 1. Background workflow progress (ms_rewards -> 65% -> done)
	ctrl.HandleWorkflowProgress("ms_rewards", 13, 20, "[阶段 2/4] 执行必应搜索 (130/200)", false, "")
	waitForCondition(t, 400*time.Millisecond, "workflow progress 65%", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "workflow" && last.Progress == 65
	})
	wfUpd, _ := client.lastUpdate()
	if wfUpd.Title != "微软积分 · 65%" || wfUpd.ShortText != "积分 65%" || wfUpd.Text != "执行必应搜索 (130/200)" {
		t.Fatalf("unexpected workflow update payload: %+v", wfUpd)
	}

	ctrl.HandleWorkflowProgress("ms_rewards", 20, 20, "全部完成", true, "")
	waitForCondition(t, 400*time.Millisecond, "workflow done", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "done" && last.Title == "微软积分已完成"
	})
	waitForCondition(t, 400*time.Millisecond, "workflow auto-clear", func() bool {
		return !ctrl.IsActive()
	})

	// 2. PreviewDemo across all 5 /ui states + error propagation
	for _, st := range []string{"thinking", "acting", "workflow", "confirm", "done"} {
		if err := ctrl.PreviewDemo(context.Background(), st); err != nil {
			t.Fatalf("PreviewDemo(%q) failed: %v", st, err)
		}
		last, ok := client.lastUpdate()
		if !ok || last.State != st || last.ClickURI != DefaultIslandClickURI {
			t.Fatalf("PreviewDemo(%q) produced unexpected update: %+v", st, last)
		}
		if strings.ContainsAny(last.Title+last.ShortText, "🤖⚠✓■✕") {
			t.Fatalf("PreviewDemo(%q) must be emoji-free, got Title=%q ShortText=%q", st, last.Title, last.ShortText)
		}
	}

	client.mu.Lock()
	client.sendErr = errors.New("kernel unavailable")
	client.mu.Unlock()
	if err := ctrl.PreviewDemo(context.Background(), "thinking"); err == nil {
		t.Fatal("expected PreviewDemo to propagate client error")
	}
	client.mu.Lock()
	client.sendErr = nil
	client.mu.Unlock()
	ctrl.ClearImmediate("cleanup")
}

func TestIslandHighMaxStepsCleanFormatting(t *testing.T) {
	client := &mockLiveProgressClient{}
	ctrl := NewIslandController(client)
	defer ctrl.Close()
	ctrl.minUpdateInterval = 20 * time.Millisecond
	ctrl.doneClearDelay = 80 * time.Millisecond

	ctrl.HandleEvent(RunEvent{RunID: "run-80", Type: "run_start", Summary: "长任务", MaxSteps: 80})
	waitForCondition(t, 300*time.Millisecond, "run_start", func() bool {
		u, _ := client.counts()
		return u >= 1
	})

	time.Sleep(30 * time.Millisecond)
	ctrl.HandleEvent(RunEvent{RunID: "run-80", Type: "thinking", Step: 3, MaxSteps: 80})
	waitForCondition(t, 300*time.Millisecond, "thinking step 3", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "thinking" && last.Step == 3
	})
	thinkUpd, _ := client.lastUpdate()
	if thinkUpd.Text != "第 3 步 · 推理" || thinkUpd.ShortText != "第 3 步" || strings.Contains(thinkUpd.Text+thinkUpd.ShortText, "/80") {
		t.Fatalf("expected clean '第 3 步' formatting when MaxSteps>=40, got %+v", thinkUpd)
	}
	if thinkUpd.Progress < 20 || thinkUpd.Progress > 92 {
		t.Fatalf("expected sleek soft progress in 20..92, got %d", thinkUpd.Progress)
	}

	time.Sleep(30 * time.Millisecond)
	ctrl.HandleEvent(RunEvent{RunID: "run-80", Type: "tool_start", Step: 3, MaxSteps: 80, Tool: "phone_tap_node", Label: "点击控件", Summary: "点击节点 #2"})
	waitForCondition(t, 300*time.Millisecond, "tool_start step 3", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "acting"
	})
	actUpd, _ := client.lastUpdate()
	if !strings.HasPrefix(actUpd.Text, "第 3 步 · ") || strings.Contains(actUpd.Text+actUpd.ShortText, "/80") {
		t.Fatalf("expected clean '第 3 步 · ...' tool_start formatting when MaxSteps>=40, got %+v", actUpd)
	}

	ctrl.HandleEvent(RunEvent{RunID: "run-80", Type: "done", Steps: 3, MaxSteps: 80, CheckpointPaused: true, ElapsedSec: 1.2})
	waitForCondition(t, 300*time.Millisecond, "checkpoint done", func() bool {
		last, ok := client.lastUpdate()
		return ok && last.State == "done"
	})
	doneUpd, _ := client.lastUpdate()
	if doneUpd.Title != "阶段检查点" || doneUpd.ShortText != "待继续" {
		t.Fatalf("expected checkpoint done title/shortText, got %+v", doneUpd)
	}
	ctrl.ClearImmediate("cleanup")
}
