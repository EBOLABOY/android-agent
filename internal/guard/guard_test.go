package guard

import (
	"context"
	"sync"
	"testing"
	"time"
)

type mockDetector struct {
	mu  sync.Mutex
	pkg string
}

func (m *mockDetector) set(pkg string) {
	m.mu.Lock()
	m.pkg = pkg
	m.mu.Unlock()
}

func (m *mockDetector) DetectGuardedForeground(_ context.Context, _ []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pkg, nil
}

type mockListener struct {
	mu         sync.Mutex
	listening  bool
	stopCount  int
	startCount int
}

func (m *mockListener) StopForGuard(_ string) {
	m.mu.Lock()
	m.listening = false
	m.stopCount++
	m.mu.Unlock()
}

func (m *mockListener) StartAfterGuard() {
	m.mu.Lock()
	m.listening = true
	m.startCount++
	m.mu.Unlock()
}

func (m *mockListener) IsListening() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listening
}

func TestGameGuardPauseAndResumeListener(t *testing.T) {
	det := &mockDetector{}
	lc := &mockListener{listening: true}

	g := New(Options{
		Packages:     []string{"com.tencent.tmgp.sgame"},
		PollInterval: 50 * time.Millisecond,
		ResumeGrace:  10 * time.Millisecond,
		Detector:     det,
		Listener:     lc,
	})

	ctx := context.Background()
	g.CheckOnce(ctx)
	if blocked, _ := g.IsActionBlocked(); blocked {
		t.Fatal("expected active state initially")
	}

	// Simulate Honor of Kings entering foreground
	det.set("com.tencent.tmgp.sgame")
	g.CheckOnce(ctx)

	st := g.Status()
	if st.State != StateGamePaused || !st.GamePaused || st.ActiveGamePkg != "com.tencent.tmgp.sgame" {
		t.Fatalf("expected game_paused, got %+v", st)
	}
	if lc.IsListening() {
		t.Fatal("expected listener to be stopped while sgame is in foreground")
	}
	if blocked, _ := g.IsActionBlocked(); !blocked {
		t.Fatal("expected actions to be blocked during game pause")
	}

	// Leave foreground -> first check records cleared timestamp
	det.set("")
	g.CheckOnce(ctx)
	time.Sleep(15 * time.Millisecond)
	g.CheckOnce(ctx)

	st = g.Status()
	if st.State != StateActive || st.GamePaused {
		t.Fatalf("expected active after grace period, got %+v", st)
	}
	if !lc.IsListening() {
		t.Fatal("expected listener to resume after sgame left foreground")
	}
}

func TestManualPauseResume(t *testing.T) {
	g := New(Options{
		Packages: []string{"com.tencent.tmgp.sgame"},
		Detector: &mockDetector{},
	})
	g.PauseManual("maintenance")
	if blocked, reason := g.IsActionBlocked(); !blocked || reason == "" {
		t.Fatalf("expected manual block, got blocked=%v reason=%q", blocked, reason)
	}
	g.ResumeManual()
	if blocked, _ := g.IsActionBlocked(); blocked {
		t.Fatal("expected unblocked after ResumeManual")
	}
}
