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

	// Poll 2: background/cached game that is NOT frozen (oom_score_adj=900, another app in foreground)
	// MUST immediately trigger pause WITHOUT forking dumpsys!
	writeProc(200, "com.tencent.tmgp.sgame", "900", "ep_poll", "", "")
	setLastPID(200)
	fgResult.Store("com.tencent.mm")
	if got, err := det.DetectGuardedForeground(ctx, pkgs); err != nil || got != "com.tencent.tmgp.sgame" {
		t.Fatalf("alive unfrozen background game must trigger pause: got=%q err=%v", got, err)
	}
	if calls := dumpsysCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 dumpsys calls for alive unfrozen game, got %d", calls)
	}

	// Poll 3: frozen game (cgroup.events frozen=1) MUST resume / NOT trigger pause and NOT fork dumpsys!
	writeProc(200, "com.tencent.tmgp.sgame", "0", "do_freezer_trap", "/apps/uid_10200/pid_200", "populated 1\nfrozen 1\n")
	if got, err := det.DetectGuardedForeground(ctx, pkgs); err != nil || got != "" {
		t.Fatalf("frozen game should not trigger pause: got=%q err=%v", got, err)
	}
	if calls := dumpsysCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 dumpsys calls for frozen game, got %d", calls)
	}

	// Poll 4: game unfrozen again -> triggers pause immediately with 0 dumpsys calls!
	writeProc(200, "com.tencent.tmgp.sgame", "0", "ep_poll", "/apps/uid_10200/pid_200", "populated 1\nfrozen 0\n")
	fgResult.Store("")
	got, err := det.DetectGuardedForeground(ctx, pkgs)
	if err != nil || got != "com.tencent.tmgp.sgame" {
		t.Fatalf("expected active unfrozen game detection, got=%q err=%v", got, err)
	}
	if calls := dumpsysCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 dumpsys calls for unfrozen game, got %d", calls)
	}

	// Poll 5: remove pid 200; specialize volatile usap64 (pid 101) in-place WITHOUT changing loadavg lastPID!
	_ = os.RemoveAll(filepath.Join(fakeProc, "200"))
	writeProc(101, "com.tencent.tmgp.sgame", "0", "ep_poll", "", "")
	got, err = det.DetectGuardedForeground(ctx, pkgs)
	if err != nil || got != "com.tencent.tmgp.sgame" {
		t.Fatalf("expected usap64 in-place specialization to be detected even with unchanged lastPID, got=%q err=%v", got, err)
	}
}

func TestAliveUnfrozenGamePausesWithoutForegroundAndFrozenResumes(t *testing.T) {
	tmp := t.TempDir()
	fakeProc := filepath.Join(tmp, "proc")
	fakeCg := filepath.Join(tmp, "cgroup")
	if err := os.MkdirAll(fakeProc, 0o755); err != nil {
		t.Fatal(err)
	}

	writeProc := func(pid int, cmdline, cgPath, cgEvents string) {
		t.Helper()
		pdir := filepath.Join(fakeProc, strconv.Itoa(pid))
		_ = os.MkdirAll(pdir, 0o755)
		_ = os.WriteFile(filepath.Join(pdir, "cmdline"), []byte(cmdline+"\x00"), 0o644)
		_ = os.WriteFile(filepath.Join(pdir, "oom_score_adj"), []byte("700\n"), 0o644)
		if cgPath != "" {
			_ = os.WriteFile(filepath.Join(pdir, "cgroup"), []byte("0::"+cgPath+"\n"), 0o644)
			cgDir := filepath.Join(fakeCg, strings.TrimPrefix(cgPath, "/"))
			_ = os.MkdirAll(cgDir, 0o755)
			_ = os.WriteFile(filepath.Join(cgDir, "cgroup.events"), []byte(cgEvents), 0o644)
		}
	}

	_ = os.WriteFile(filepath.Join(fakeProc, "loadavg"), []byte("0.10 0.20 0.30 1/100 300\n"), 0o644)
	writeProc(300, "com.tencent.tmgp.sgame", "/apps/uid_10341/pid_300", "populated 1\nfrozen 0\n")

	det := &AndroidForegroundDetector{
		procRoot:   fakeProc,
		cgroupRoot: fakeCg,
		queryFgFunc: func(context.Context) string {
			t.Error("queryFgFunc must not be called when alive-and-unfrozen guard is active")
			return "com.tencent.mm"
		},
	}
	lc := &mockListener{listening: true}
	g := New(Options{
		Packages:     []string{"com.tencent.tmgp.sgame"},
		PollInterval: 50 * time.Millisecond,
		ResumeGrace:  10 * time.Millisecond,
		Detector:     det,
		Listener:     lc,
	})

	ctx := context.Background()
	// Alive and unfrozen in background (oom=700, WeChat in foreground) -> pauses immediately
	g.CheckOnce(ctx)
	if !g.Status().GamePaused || lc.IsListening() {
		t.Fatalf("expected game_paused and listener closed while sgame is alive & unfrozen, got status=%+v listening=%v", g.Status(), lc.IsListening())
	}

	// HyperOS freezes sgame in background -> resumes after resumeGrace
	writeProc(300, "com.tencent.tmgp.sgame", "/apps/uid_10341/pid_300", "populated 1\nfrozen 1\n")
	g.CheckOnce(ctx)
	time.Sleep(15 * time.Millisecond)
	g.CheckOnce(ctx)
	if g.Status().GamePaused || !lc.IsListening() {
		t.Fatalf("expected active and listener resumed once sgame is frozen, got status=%+v listening=%v", g.Status(), lc.IsListening())
	}
}

func TestGuardScreenWakeEliminates30sRace(t *testing.T) {
	var screenOn atomic.Bool
	var screenChecks atomic.Int32
	var checkCalls atomic.Int32
	wokenCh := make(chan time.Time, 8)

	det := &callbackDetector{
		fn: func() (string, error) {
			n := checkCalls.Add(1)
			if n >= 2 {
				select {
				case wokenCh <- time.Now():
				default:
				}
			}
			return "", nil
		},
	}

	g := New(Options{
		Packages:              []string{"com.tencent.tmgp.sgame"},
		PollInterval:          40 * time.Millisecond,
		ScreenOffPollInterval: 30 * time.Second, // real 30s screen-off full poll interval
		Detector:              det,
		ScreenChecker: func() bool {
			screenChecks.Add(1)
			return screenOn.Load()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Wait briefly while screen is off: only startup CheckOnce should have run
	time.Sleep(150 * time.Millisecond)
	if got := checkCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 startup CheckOnce while screen off, got %d", got)
	}

	// Wake screen -> must detect transition and run CheckOnce promptly (<1.5s, NOT 30s)
	wakeAt := time.Now()
	screenOn.Store(true)

	select {
	case firedAt := <-wokenCh:
		if elapsed := firedAt.Sub(wakeAt); elapsed > 1500*time.Millisecond {
			t.Fatalf("screen-wake CheckOnce took too long: %v (want <1.5s)", elapsed)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("timed out waiting for screen-wake CheckOnce within 1.5s (screenChecks=%d, checkCalls=%d)", screenChecks.Load(), checkCalls.Load())
	}

	// Verify active pollInterval (40ms) resumes right after wake
	time.Sleep(120 * time.Millisecond)
	if got := checkCalls.Load(); got < 3 {
		t.Fatalf("expected active 40ms polling after screen wake, got %d total calls", got)
	}
}

type callbackDetector struct {
	fn func() (string, error)
}

func (c *callbackDetector) DetectGuardedForeground(_ context.Context, _ []string) (string, error) {
	return c.fn()
}

func TestFindRunningPackagesEmptyCmdlineSystemUIDStaysVolatile(t *testing.T) {
	tmp := t.TempDir()
	fakeProc := filepath.Join(tmp, "proc")
	if err := os.MkdirAll(fakeProc, 0o755); err != nil {
		t.Fatal(err)
	}

	writeProcFile := func(pid int, name string, raw []byte) {
		t.Helper()
		pdir := filepath.Join(fakeProc, strconv.Itoa(pid))
		_ = os.MkdirAll(pdir, 0o755)
		_ = os.WriteFile(filepath.Join(pdir, name), raw, 0o644)
	}

	var uidMu sync.Mutex
	pidUIDs := map[int]uint32{
		2:   0, // kthreadd (pid == 2, empty cmdline) -> knownSafePIDs
		12:  0, // kworker kernel thread (PPid: 2, empty cmdline) -> knownSafePIDs
		10:  0, // steady system daemon with non-empty cmdline -> knownSafePIDs
		500: 0, // freshly forked zygote/usap child (PPid: 50, UID<10000, empty cmdline) -> volatilePIDs
		501: 0, // UID<10000 empty cmdline with missing status (fail-safe) -> volatilePIDs
	}
	writeProcFile(2, "cmdline", []byte{})
	writeProcFile(12, "cmdline", []byte{})
	writeProcFile(12, "status", []byte("Name:\tkworker/u16:0\nPPid:\t2\n"))
	writeProcFile(10, "cmdline", []byte("/system/bin/servicemanager\x00"))
	writeProcFile(10, "status", []byte("Name:\tservicemanager\nPPid:\t1\n"))
	writeProcFile(500, "cmdline", []byte{})
	writeProcFile(500, "status", []byte("Name:\tusap64\nPPid:\t50\n"))
	writeProcFile(501, "cmdline", []byte{})
	_ = os.WriteFile(filepath.Join(fakeProc, "loadavg"), []byte("0.10 0.20 0.30 1/100 501\n"), 0o644)

	det := &AndroidForegroundDetector{
		procRoot: fakeProc,
		uidFunc: func(pid int, _ os.DirEntry) (uint32, bool) {
			uidMu.Lock()
			defer uidMu.Unlock()
			u, ok := pidUIDs[pid]
			return u, ok
		},
	}

	pkgs := []string{"com.tencent.tmgp.sgame"}
	ctx := context.Background()

	// Poll 1: kernel threads (PID 2, PID 12 with PPid: 2) -> knownSafePIDs;
	// non-kthread empty-cmdline UID<10000 (PID 500 with PPid: 50, PID 501 missing status) -> volatilePIDs.
	if got, err := det.DetectGuardedForeground(ctx, pkgs); err != nil || got != "" {
		t.Fatalf("poll 1 unexpected: got=%q err=%v", got, err)
	}
	for _, safePID := range []int{2, 10, 12} {
		if _, safe := det.knownSafePIDs[safePID]; !safe {
			t.Fatalf("expected PID %d to be cached in knownSafePIDs", safePID)
		}
		if _, vol := det.volatilePIDs[safePID]; vol {
			t.Fatalf("PID %d must NOT be in volatilePIDs", safePID)
		}
	}
	for _, volPID := range []int{500, 501} {
		if _, safe := det.knownSafePIDs[volPID]; safe {
			t.Fatalf("empty-cmdline non-kthread UID<10000 PID %d must NOT be cached in knownSafePIDs", volPID)
		}
		if _, vol := det.volatilePIDs[volPID]; !vol {
			t.Fatalf("expected empty-cmdline non-kthread UID<10000 PID %d to be tracked in volatilePIDs", volPID)
		}
	}

	// Poll 2 (fast path, unchanged lastPID): PID 500 specializes into com.tencent.tmgp.sgame
	uidMu.Lock()
	pidUIDs[500] = 10341
	uidMu.Unlock()
	writeProcFile(500, "cmdline", []byte("com.tencent.tmgp.sgame\x00"))

	got, err := det.DetectGuardedForeground(ctx, pkgs)
	if err != nil || got != "com.tencent.tmgp.sgame" {
		t.Fatalf("expected PID 500 to be detected once cmdline populated, got=%q err=%v", got, err)
	}
}
