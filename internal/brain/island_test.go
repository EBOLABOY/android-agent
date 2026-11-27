package brain

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type mockLiveProgressClient struct {
	mu      sync.Mutex
	updates []LiveProgressUpdate
	cancels []string
}

func (m *mockLiveProgressClient) UpdateLiveProgress(_ context.Context, u LiveProgressUpdate) error {
	m.mu.Lock()
	m.updates = append(m.updates, u)
	m.mu.Unlock()
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

func TestIslandControllerLifecycleThrottleConfirmAndGuardClear(t *testing.T) {
	client := &mockLiveProgressClient{}
	ctrl := NewIslandController(client)
	ctrl.minUpdateInterval = 80 * time.Millisecond
	ctrl.doneClearDelay = 60 * time.Millisecond
	ctrl.confirmClearDelay = 300 * time.Millisecond

	// Idle: 0 notifications.
	if ctrl.IsActive() {
		t.Fatal("expected idle controller not active")
	}
	up, can := client.counts()
	if up != 0 || can != 0 {
		t.Fatalf("expected 0 idle updates/cancels, got %d/%d", up, can)
	}

	// Start run -> immediate forced update.
	ctrl.HandleEvent(RunEvent{Type: "run_start", Summary: "查看屏幕", MaxSteps: 5})
	if !ctrl.IsActive() {
		t.Fatal("expected active after run_start")
	}
	up, _ = client.counts()
	if up != 1 {
		t.Fatalf("expected 1 update after run_start, got %d", up)
	}

	// Burst of intermediate updates within minUpdateInterval -> throttled.
	ctrl.HandleEvent(RunEvent{Type: "thinking", Step: 1, MaxSteps: 5})
	ctrl.HandleEvent(RunEvent{Type: "tool_start", Step: 1, MaxSteps: 5, Tool: "phone_ui_tree", Label: "UI树"})
	ctrl.HandleEvent(RunEvent{Type: "thinking", Step: 2, MaxSteps: 5})
	up, _ = client.counts()
	if up != 1 {
		t.Fatalf("expected intermediate updates within 80ms to be throttled, got %d updates", up)
	}

	// After minUpdateInterval -> accepted.
	time.Sleep(95 * time.Millisecond)
	ctrl.HandleEvent(RunEvent{Type: "tool_start", Step: 2, MaxSteps: 5, Tool: "phone_tap_node", Label: "点击节点", Summary: "点击确认"})
	up, _ = client.counts()
	if up != 2 {
		t.Fatalf("expected update after throttle window, got %d updates", up)
	}
	if last, ok := client.lastUpdate(); !ok || last.ShortText == "" || !last.Focus || !last.Ongoing {
		t.Fatalf("unexpected intermediate live progress payload: %+v", last)
	}

	// Done -> forced terminal update + auto clear after doneClearDelay.
	ctrl.HandleEvent(RunEvent{Type: "done", Steps: 2, MaxSteps: 5, ElapsedSec: 0.4})
	if last, ok := client.lastUpdate(); !ok || last.Ongoing || last.Title != "✓ Agent 任务完成" {
		t.Fatalf("unexpected done live progress payload: %+v", last)
	}
	time.Sleep(110 * time.Millisecond)
	_, can = client.counts()
	if can != 1 || ctrl.IsActive() {
		t.Fatalf("expected auto-clear after done delay (cancels=%d active=%v)", can, ctrl.IsActive())
	}

	// New run with needs_confirmation -> retained past doneClearDelay, cleared synchronously by ClearImmediate.
	ctrl.HandleEvent(RunEvent{Type: "run_start", Summary: "下单流程", MaxSteps: 5})
	ctrl.HandleEvent(RunEvent{Type: "needs_confirmation", Step: 3, MaxSteps: 5})
	ctrl.HandleEvent(RunEvent{Type: "done", NeedsConfirmation: true, Reply: "已停在付款确认页", Steps: 3, MaxSteps: 5})
	time.Sleep(100 * time.Millisecond)
	if !ctrl.IsActive() {
		t.Fatal("expected confirmation capsule to remain active past doneClearDelay")
	}
	if last, ok := client.lastUpdate(); !ok || last.Title != "⚠ 待人工确认订单/验证" {
		t.Fatalf("unexpected confirmation retention title: %+v", last)
	}

	ctrl.ClearImmediate("game guard triggered")
	_, canAfterGuard := client.counts()
	if ctrl.IsActive() || canAfterGuard != 2 {
		t.Fatalf("expected synchronous ClearImmediate to cancel once cleanly (active=%v cancels=%d)", ctrl.IsActive(), canAfterGuard)
	}

	// Wait past confirmClearDelay to ensure stale timer generation is ignored.
	time.Sleep(330 * time.Millisecond)
	_, canFinal := client.counts()
	if canFinal != 2 {
		t.Fatalf("expected no stale double-cancel after ClearImmediate, got %d", canFinal)
	}
}

func TestIslandDuplicateDonePreservesConfirmation(t *testing.T) {
	client := &mockLiveProgressClient{}
	ctrl := NewIslandController(client)
	ctrl.doneClearDelay = 40 * time.Millisecond
	ctrl.confirmClearDelay = 300 * time.Millisecond

	ctrl.HandleEvent(RunEvent{Type: "run_start", Summary: "测试确认", MaxSteps: 3})
	ctrl.HandleEvent(RunEvent{Type: "needs_confirmation", Step: 2, MaxSteps: 3})
	ctrl.HandleEvent(RunEvent{Type: "done", NeedsConfirmation: true, Reply: "停在付款页", Steps: 2, MaxSteps: 3})
	upBefore, _ := client.counts()

	// Duplicate done (even with NeedsConfirmation=false) must NOT overwrite confirmation state or shorten clear delay.
	ctrl.HandleEvent(RunEvent{Type: "done", NeedsConfirmation: false, Reply: "重复done", Steps: 2, MaxSteps: 3})
	upAfter, _ := client.counts()
	if upAfter != upBefore {
		t.Fatalf("expected duplicate done after confirmation to be ignored, updates went %d -> %d", upBefore, upAfter)
	}
	time.Sleep(80 * time.Millisecond)
	if !ctrl.IsActive() {
		t.Fatal("expected confirmation state to survive past doneClearDelay after duplicate done")
	}
	if last, ok := client.lastUpdate(); !ok || last.Title != "⚠ 待人工确认订单/验证" {
		t.Fatalf("expected confirmation title preserved, got %+v", last)
	}
	ctrl.ClearImmediate("cleanup")
}
