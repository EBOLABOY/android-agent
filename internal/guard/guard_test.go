package guard

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

type countingDetector struct {
	calls atomic.Int32
}

func (c *countingDetector) DetectGuardedForeground(_ context.Context, _ []string) (string, error) {
	c.calls.Add(1)
	return "", nil
}

func TestGuardScreenOffThrottling(t *testing.T) {
	var screenOn atomic.Bool
	det := &countingDetector{}
	g := New(Options{
		Packages:              []string{"com.tencent.tmgp.sgame"},
		PollInterval:          12 * time.Millisecond,
		ScreenOffPollInterval: 90 * time.Millisecond,
		Detector:              det,
		ScreenChecker: func() bool {
			return screenOn.Load()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()

	time.Sleep(60 * time.Millisecond)
	if calls := det.calls.Load(); calls != 1 {
		t.Fatalf("expected only 1 startup check while screen off during 60ms (<90ms), got %d", calls)
	}

	screenOn.Store(true)
	time.Sleep(110 * time.Millisecond)
	cancel()
	<-done

	if calls := det.calls.Load(); calls < 3 {
		t.Fatalf("expected rapid 12ms checks once screen turned on, got %d calls", calls)
	}
}

func TestCheckScreenSysfsDRMDpmsAndOLEDAOD(t *testing.T) {
	tmp := t.TempDir()
	blDir := filepath.Join(tmp, "backlight", "panel0-backlight")
	drmDir := filepath.Join(tmp, "drm", "card0-DSI-1")
	if err := os.MkdirAll(blDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(drmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blPath := filepath.Join(blDir, "brightness")
	dpmsPath := filepath.Join(drmDir, "dpms")
	blPats := []string{filepath.Join(tmp, "backlight", "*", "brightness")}
	dpmsPats := []string{filepath.Join(tmp, "drm", "card*-DSI-*", "dpms")}

	// OLED AOD: backlight brightness > 0 while DRM dpms is Off -> MUST report screen off (false)
	_ = os.WriteFile(blPath, []byte("50\n"), 0o644)
	_ = os.WriteFile(dpmsPath, []byte("Off\n"), 0o644)
	if checkScreenSysfs(blPats, dpmsPats) {
		t.Fatal("expected false during OLED AOD (brightness>0, dpms=Off)")
	}

	// Screen interactively on: dpms=On and brightness > 0 -> true
	_ = os.WriteFile(blPath, []byte("450\n"), 0o644)
	_ = os.WriteFile(dpmsPath, []byte("On\n"), 0o644)
	if !checkScreenSysfs(blPats, dpmsPats) {
		t.Fatal("expected true when dpms=On and brightness=450")
	}

	// Backlight off even if dpms=On -> false
	_ = os.WriteFile(blPath, []byte("0\n"), 0o644)
	if checkScreenSysfs(blPats, dpmsPats) {
		t.Fatal("expected false when brightness=0")
	}

	// Non-Android host with no sysfs entries -> true
	if !checkScreenSysfs([]string{filepath.Join(tmp, "missing", "*")}, []string{filepath.Join(tmp, "missing2", "*")}) {
		t.Fatal("expected true when no sysfs entries exist")
	}
}

func TestAndroidForegroundDetectorDeltaCacheAndOomPreFilter(t *testing.T) {
	tmp := t.TempDir()
	fakeProc := filepath.Join(tmp, "proc")
	fakeCg := filepath.Join(tmp, "cgroup")
	if err := os.MkdirAll(fakeProc, 0o755); err != nil {
		t.Fatal(err)
	}

	writeProc := func(pid int, cmdline, oomAdj, wchan, cgPath, cgEvents string) {
		t.Helper()
		pdir := filepath.Join(fakeProc, strconv.Itoa(pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(pdir, "cmdline"), []byte(cmdline+"\x00"), 0o644)
		if oomAdj != "" {
			_ = os.WriteFile(filepath.Join(pdir, "oom_score_adj"), []byte(oomAdj+"\n"), 0o644)
		}
		if wchan != "" {
			_ = os.WriteFile(filepath.Join(pdir, "wchan"), []byte(wchan+"\n"), 0o644)
		}
		if cgPath != "" {
			_ = os.WriteFile(filepath.Join(pdir, "cgroup"), []byte("0::"+cgPath+"\n"), 0o644)
			cgDir := filepath.Join(fakeCg, strings.TrimPrefix(cgPath, "/"))
			_ = os.MkdirAll(cgDir, 0o755)
			if cgEvents != "" {
				_ = os.WriteFile(filepath.Join(cgDir, "cgroup.events"), []byte(cgEvents), 0o644)
			}
		}
	}
	setLastPID := func(pid int) {
		t.Helper()
		_ = os.WriteFile(filepath.Join(fakeProc, "loadavg"), []byte("0.10 0.20 0.30 1/100 "+strconv.Itoa(pid)+"\n"), 0o644)
	}

	// Initial idle processes (no game, plus one pre-forked usap64)
	writeProc(100, "com.android.systemui", "-800", "ep_poll", "", "")
	writeProc(101, "usap64", "0", "ep_poll", "", "")
	setLastPID(101)

	var dumpsysCalls atomic.Int32
	var fgResult atomic.Value
	fgResult.Store("")

	det := &AndroidForegroundDetector{
		procRoot:   fakeProc,
		cgroupRoot: fakeCg,
		queryFgFunc: func(context.Context) string {
			dumpsysCalls.Add(1)
			return fgResult.Load().(string)
		},
	}

	pkgs := []string{"com.tencent.tmgp.sgame"}
	ctx := context.Background()

	// Poll 1: populates knownSafePIDs (100) and volatilePIDs (101)
	if got, err := det.DetectGuardedForeground(ctx, pkgs); err != nil || got != "" {
		t.Fatalf("poll 1 unexpected: got=%q err=%v", got, err)
	}
	if _, ok := det.knownSafePIDs[100]; !ok {
		t.Fatal("expected pid 100 to be cached in knownSafePIDs")
	}
	if _, ok := det.volatilePIDs[101]; !ok {
		t.Fatal("expected usap64 pid 101 to stay in volatilePIDs")
	}

	// Poll 2: cached/background game (oom_score_adj=900) MUST NOT fork dumpsys!
	writeProc(200, "com.tencent.tmgp.sgame", "900", "ep_poll", "", "")
	setLastPID(200)
	if got, err := det.DetectGuardedForeground(ctx, pkgs); err != nil || got != "" {
		t.Fatalf("cached game should not trigger foreground: got=%q err=%v", got, err)
	}
	if calls := dumpsysCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 dumpsys calls for cached game (oom=900), got %d", calls)
	}

	// Poll 3: frozen game (oom=0 but frozen in cgroup.events) MUST NOT fork dumpsys!
	writeProc(200, "com.tencent.tmgp.sgame", "0", "do_freezer_trap", "/apps/uid_10200/pid_200", "populated 1\nfrozen 1\n")
	if got, err := det.DetectGuardedForeground(ctx, pkgs); err != nil || got != "" {
		t.Fatalf("frozen game should not trigger foreground: got=%q err=%v", got, err)
	}
	if calls := dumpsysCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 dumpsys calls for frozen game, got %d", calls)
	}

	// Poll 4: game unfrozen and active (oom=0) -> forks dumpsys and detects foreground!
	writeProc(200, "com.tencent.tmgp.sgame", "0", "ep_poll", "/apps/uid_10200/pid_200", "populated 1\nfrozen 0\n")
	fgResult.Store("com.tencent.tmgp.sgame")
	got, err := det.DetectGuardedForeground(ctx, pkgs)
	if err != nil || got != "com.tencent.tmgp.sgame" {
		t.Fatalf("expected active foreground game detection, got=%q err=%v", got, err)
	}
	if calls := dumpsysCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 dumpsys call for active game, got %d", calls)
	}

	// Poll 5: remove pid 200; specialize volatile usap64 (pid 101) in-place WITHOUT changing loadavg lastPID!
	_ = os.RemoveAll(filepath.Join(fakeProc, "200"))
	writeProc(101, "com.tencent.tmgp.sgame", "0", "ep_poll", "", "")
	got, err = det.DetectGuardedForeground(ctx, pkgs)
	if err != nil || got != "com.tencent.tmgp.sgame" {
		t.Fatalf("expected usap64 in-place specialization to be detected even with unchanged lastPID, got=%q err=%v", got, err)
	}
}
