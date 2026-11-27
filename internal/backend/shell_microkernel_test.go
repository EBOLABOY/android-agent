package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShellBackendScreenInfoTTLCache(t *testing.T) {
	be := NewShellBackend("", time.Second)
	var calls atomic.Int32
	be.screenQueryFn = func(context.Context) (ScreenInfo, error) {
		calls.Add(1)
		return ScreenInfo{Width: 1080, Height: 2400, Density: 440}, nil
	}

	s1, err := be.ScreenInfo(context.Background())
	if err != nil || s1.Width != 1080 || s1.Height != 2400 || s1.Density != 440 {
		t.Fatalf("unexpected first ScreenInfo: %+v err=%v", s1, err)
	}
	s2, err := be.ScreenInfo(context.Background())
	if err != nil || s2 != s1 {
		t.Fatalf("unexpected cached ScreenInfo: %+v err=%v", s2, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 query due to 60s TTL cache, got %d", got)
	}

	// Expire cache manually and verify refresh.
	be.screenMu.Lock()
	be.cachedScreenAt = time.Now().Add(-screenInfoCacheTTL - time.Second)
	be.screenMu.Unlock()

	if _, err := be.ScreenInfo(context.Background()); err != nil {
		t.Fatalf("refresh ScreenInfo failed: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2nd query after TTL expiry, got %d", got)
	}
}

func TestShellBackendFailClosedLockscreenGateAndInputText(t *testing.T) {
	be := NewShellBackend("", time.Second)
	be.backlightCheckFunc = func() bool { return true }

	// 1. Fail-closed when dumpsys fails
	be.policyQueryFunc = func(context.Context) (string, error) {
		return "", errors.New("dumpsys timeout")
	}
	if be.IsScreenUnlocked(context.Background()) {
		t.Fatal("expected IsScreenUnlocked=false when dumpsys fails (fail-closed)")
	}
	if _, err := be.InputText(context.Background(), "hello", "auto", false); err == nil {
		t.Fatal("expected InputText to fail-closed when dumpsys fails")
	}

	// 2. Locked keyguard -> IsScreenUnlocked=false and InputText blocked
	be.policyQueryFunc = func(context.Context) (string, error) {
		return "screenState=SCREEN_STATE_ON\nKeyguardServiceDelegate\n  showing=true\n", nil
	}
	if be.IsScreenUnlocked(context.Background()) {
		t.Fatal("expected IsScreenUnlocked=false when keyguard is showing")
	}
	if _, err := be.InputText(context.Background(), "hello", "auto", false); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected InputText to be blocked on lockscreen, got %v", err)
	}

	// 3. Display on and unlocked -> IsScreenUnlocked=true
	be.screenGateMu.Lock()
	be.lastGateAt = time.Time{}
	be.screenGateMu.Unlock()
	be.policyQueryFunc = func(context.Context) (string, error) {
		return "screenState=SCREEN_STATE_ON\nKeyguardServiceDelegate\n  showing=false\n", nil
	}
	if !be.IsScreenUnlocked(context.Background()) {
		t.Fatal("expected IsScreenUnlocked=true when screen is on and unlocked")
	}

	// 4. Backlight off -> IsScreenUnlocked=false even if cached gate was unlocked
	be.backlightCheckFunc = func() bool { return false }
	if be.IsScreenUnlocked(context.Background()) {
		t.Fatal("expected IsScreenUnlocked=false when display backlight is off")
	}
}

func TestShellBackendEnsureTouchRetryWithBackoff(t *testing.T) {
	be := NewShellBackend("", time.Second)
	be.touchRetryBase = 15 * time.Millisecond

	var attempts atomic.Int32
	be.touchProbeFunc = func() (TouchDeviceInfo, error) {
		n := attempts.Add(1)
		if n == 1 {
			return TouchDeviceInfo{}, errors.New("evdev not ready during early boot")
		}
		return testTouchDevice(), nil
	}

	if ev := be.ensureTouch(); ev != nil {
		t.Fatalf("expected nil on first failed probe, got %+v", ev)
	}
	// Immediate retry within backoff must NOT re-probe.
	if ev := be.ensureTouch(); ev != nil {
		t.Fatalf("expected nil during backoff window, got %+v", ev)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 probe attempt during backoff, got %d", got)
	}

	// Wait past backoff -> retry succeeds and initializes EvdevTouch.
	time.Sleep(25 * time.Millisecond)
	ev := be.ensureTouch()
	if ev == nil || !ev.Available() {
		t.Fatalf("expected EvdevTouch after retry succeeded, got %+v", ev)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 probe attempts total, got %d", got)
	}
}

func TestShellBackendCancelLiveProgressSkipsColdStart(t *testing.T) {
	be := NewShellBackend("", time.Second)
	var spawns atomic.Int32
	be.kernel.cmdFactory = func(_ string, _ bool) *exec.Cmd {
		spawns.Add(1)
		return exec.Command("/bin/sh", "-c", `while IFS= read -r line; do printf '{"ok":true}\n'; done`)
	}

	if be.MicroKernelRunning() {
		t.Fatal("expected microkernel initially not running")
	}
	if err := be.CancelLiveProgress(context.Background(), "agent_live", 9900); err != nil {
		t.Fatalf("expected nil when skipping cold start on CancelLiveProgress, got %v", err)
	}
	if got := spawns.Load(); got != 0 {
		t.Fatalf("CancelLiveProgress must NOT cold-start JVM when not running, got %d spawns", got)
	}
}

func TestMicroKernelAppErrorDoesNotKillHealthyJVM(t *testing.T) {
	mk := NewMicroKernel(nil, 2*time.Second)
	defer mk.Stop("test cleanup")

	var spawns atomic.Int32
	mk.cmdFactory = func(_ string, _ bool) *exec.Cmd {
		spawns.Add(1)
		return exec.Command("/bin/sh", "-c", `
			while IFS= read -r line; do
				case "$line" in
					*bad_cmd*) printf '{"ok":false,"error":"invalid param"}\n' ;;
					*) printf '{"ok":true,"text":"pid:%d"}\n' "$$" ;;
				esac
			done
		`)
	}

	ctx := context.Background()
	r1, err := mk.Call(ctx, kernelRequest{Cmd: "ping"})
	if err != nil {
		t.Fatalf("first ping failed: %v", err)
	}

	_, err = mk.Call(ctx, kernelRequest{Cmd: "bad_cmd"})
	if err == nil || !strings.Contains(err.Error(), "invalid param") {
		t.Fatalf("expected app error 'invalid param', got %v", err)
	}
	if !mk.Running() {
		t.Fatal("healthy JVM must stay running after ok=false app error")
	}

	r2, err := mk.Call(ctx, kernelRequest{Cmd: "ping"})
	if err != nil {
		t.Fatalf("second ping failed: %v", err)
	}
	if r1.Text != r2.Text {
		t.Fatalf("expected same JVM PID across app error, got %q then %q", r1.Text, r2.Text)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("expected exactly 1 JVM spawn (no kill/respawn on ok=false), got %d", got)
	}
}

func TestMicroKernelGracefulEOFShutdownBeforeSIGKILL(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "graceful_eof")
	mk := NewMicroKernel(nil, 2*time.Second)
	mk.cmdFactory = func(_ string, _ bool) *exec.Cmd {
		script := fmt.Sprintf(`while IFS= read -r line; do printf '{"ok":true,"pong":true}\n'; done; touch %q`, marker)
		return exec.Command("/bin/sh", "-c", script)
	}

	if _, err := mk.Call(context.Background(), kernelRequest{Cmd: "ping"}); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	mk.Stop("graceful shutdown test")

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected child process to observe stdin EOF and exit gracefully before SIGKILL: %v", err)
	}
}
