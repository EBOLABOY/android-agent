package guard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type State string

const (
	StateActive       State = "active"
	StateManualPaused State = "manual_paused"
	StateGamePaused   State = "game_paused"
)

type Status struct {
	State          State     `json:"state"`
	ManualPaused   bool      `json:"manual_paused"`
	ManualReason   string    `json:"manual_reason,omitempty"`
	GamePaused     bool      `json:"game_paused"`
	ActiveGamePkg  string    `json:"active_game_package,omitempty"`
	GuardPackages  []string  `json:"guard_packages"`
	ListenerActive bool      `json:"listener_active"`
	LastCheckAt    time.Time `json:"last_check_at"`
}

// ForegroundDetector abstracts how guarded packages are detected in the foreground.
type ForegroundDetector interface {
	// DetectGuardedForeground returns the package name if any guarded package is currently
	// in the foreground, or "" if none is in the foreground.
	DetectGuardedForeground(ctx context.Context, packages []string) (string, error)
}

// ListenerController is invoked by Guard when entering/leaving game-pause mode so the HTTP
// listener and all background activities can be completely stopped while playing a guarded game.
type ListenerController interface {
	StopForGuard(reason string)
	StartAfterGuard()
	IsListening() bool
}

type Guard struct {
	mu             sync.RWMutex
	packages       []string
	pollInterval   time.Duration
	resumeGrace    time.Duration
	pauseFlagPath  string
	detector       ForegroundDetector
	listener       ListenerController
	manualPaused   bool
	manualReason   string
	gamePaused     bool
	activeGamePkg  string
	gameClearedAt  time.Time
	lastCheckAt    time.Time
	onGamePause    func(pkg string)
	onGameResume   func()
	logger         func(format string, args ...any)
}

type Options struct {
	Packages      []string
	PollInterval  time.Duration
	ResumeGrace   time.Duration
	PauseFlagPath string
	Detector      ForegroundDetector
	Listener      ListenerController
	OnGamePause   func(pkg string)
	OnGameResume  func()
	Logger        func(format string, args ...any)
}

func New(opts Options) *Guard {
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	grace := opts.ResumeGrace
	if grace < 0 {
		grace = 3 * time.Second
	}
	det := opts.Detector
	if det == nil {
		det = &AndroidForegroundDetector{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = func(string, ...any) {}
	}
	pkgs := make([]string, 0, len(opts.Packages))
	for _, p := range opts.Packages {
		p = strings.TrimSpace(p)
		if p != "" {
			pkgs = append(pkgs, p)
		}
	}
	return &Guard{
		packages:      pkgs,
		pollInterval:  poll,
		resumeGrace:   grace,
		pauseFlagPath: opts.PauseFlagPath,
		detector:      det,
		listener:      opts.Listener,
		onGamePause:   opts.OnGamePause,
		onGameResume:  opts.OnGameResume,
		logger:        logger,
	}
}

func (g *Guard) SetListener(lc ListenerController) {
	g.mu.Lock()
	g.listener = lc
	g.mu.Unlock()
}

func (g *Guard) PauseManual(reason string) Status {
	g.mu.Lock()
	g.manualPaused = true
	if strings.TrimSpace(reason) == "" {
		reason = "paused via API"
	}
	g.manualReason = reason
	g.mu.Unlock()
	g.logger("guard: manual pause enabled (%s)", reason)
	return g.Status()
}

func (g *Guard) ResumeManual() Status {
	g.mu.Lock()
	g.manualPaused = false
	g.manualReason = ""
	flagPath := g.pauseFlagPath
	g.mu.Unlock()
	if flagPath != "" {
		_ = os.Remove(flagPath)
	}
	g.logger("guard: manual pause cleared")
	return g.Status()
}

func (g *Guard) IsActionBlocked() (bool, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.gamePaused {
		return true, fmt.Sprintf("agent auto-paused: guarded game %s is in foreground", g.activeGamePkg)
	}
	if g.manualPaused {
		return true, fmt.Sprintf("agent manually paused: %s", g.manualReason)
	}
	return false, ""
}

func (g *Guard) Status() Status {
	g.mu.RLock()
	defer g.mu.RUnlock()
	st := StateActive
	if g.gamePaused {
		st = StateGamePaused
	} else if g.manualPaused {
		st = StateManualPaused
	}
	listening := false
	if g.listener != nil {
		listening = g.listener.IsListening()
	}
	pkgs := append([]string(nil), g.packages...)
	return Status{
		State:          st,
		ManualPaused:   g.manualPaused,
		ManualReason:   g.manualReason,
		GamePaused:     g.gamePaused,
		ActiveGamePkg:  g.activeGamePkg,
		GuardPackages:  pkgs,
		ListenerActive: listening,
		LastCheckAt:    g.lastCheckAt,
	}
}

// CheckOnce runs one guard evaluation cycle (also useful for deterministic unit tests).
func (g *Guard) CheckOnce(ctx context.Context) {
	now := time.Now()

	// Check optional out-of-band pause flag file (e.g. /data/adb/android_agent/paused)
	if g.pauseFlagPath != "" {
		if _, err := os.Stat(g.pauseFlagPath); err == nil {
			g.mu.Lock()
			if !g.manualPaused {
				g.manualPaused = true
				g.manualReason = "pause flag file present"
				g.logger("guard: detected pause flag file %s", g.pauseFlagPath)
			}
			g.mu.Unlock()
		}
	}

	var detectedPkg string
	if len(g.packages) > 0 && g.detector != nil {
		pkg, err := g.detector.DetectGuardedForeground(ctx, g.packages)
		if err == nil {
			detectedPkg = pkg
		}
	}

	g.mu.Lock()
	g.lastCheckAt = now
	wasGamePaused := g.gamePaused
	listener := g.listener
	onPause := g.onGamePause
	onResume := g.onGameResume

	if detectedPkg != "" {
		g.gameClearedAt = time.Time{}
		g.activeGamePkg = detectedPkg
		if !wasGamePaused {
			g.gamePaused = true
			g.mu.Unlock()
			g.logger("guard: guarded game %s entered foreground -> pausing listener & background ops", detectedPkg)
			if onPause != nil {
				onPause(detectedPkg)
			}
			if listener != nil {
				listener.StopForGuard("guarded game in foreground: " + detectedPkg)
			}
			return
		}
		g.mu.Unlock()
		return
	}

	// No guarded game in foreground
	if wasGamePaused {
		if g.gameClearedAt.IsZero() {
			g.gameClearedAt = now
		}
		if now.Sub(g.gameClearedAt) >= g.resumeGrace {
			prevPkg := g.activeGamePkg
			g.gamePaused = false
			g.activeGamePkg = ""
			g.gameClearedAt = time.Time{}
			g.mu.Unlock()
			g.logger("guard: guarded game %s left foreground -> resuming listener", prevPkg)
			if listener != nil {
				listener.StartAfterGuard()
			}
			if onResume != nil {
				onResume()
			}
			return
		}
	}
	g.mu.Unlock()
}

// Run starts the background anti-risk monitoring loop until ctx is cancelled.
func (g *Guard) Run(ctx context.Context) {
	// Immediate check on startup before traffic arrives
	g.CheckOnce(ctx)
	ticker := time.NewTicker(g.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.CheckOnce(ctx)
		}
	}
}

// AndroidForegroundDetector performs a zero-subprocess /proc scan first; only when a guarded
// game process actually exists does it check foreground focus via oom_score_adj & dumpsys.
type AndroidForegroundDetector struct{}

var pkgActivityRe = regexp.MustCompile(`u\d+\s+([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)`)

func (d *AndroidForegroundDetector) DetectGuardedForeground(ctx context.Context, packages []string) (string, error) {
	if len(packages) == 0 {
		return "", nil
	}
	// Step 1: Zero-exec /proc scan — returns immediately if none of the guarded games are running.
	running := findRunningPackages(packages)
	if len(running) == 0 {
		return "", nil
	}

	// Step 2: Check current focused window / resumed activity via lightweight dumpsys.
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if fgPkg := queryForegroundPackage(checkCtx); fgPkg != "" {
		for _, target := range packages {
			if fgPkg == target {
				return target, nil
			}
		}
		return "", nil
	}

	// Fallback if dumpsys was temporarily unavailable: check oom_score_adj == 0 (foreground app)
	for pkg, pids := range running {
		for _, pid := range pids {
			if isForegroundOom(pid) {
				return pkg, nil
			}
		}
	}
	return "", nil
}

func findRunningPackages(packages []string) map[string][]int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	targets := make(map[string]struct{}, len(packages))
	for _, p := range packages {
		targets[p] = struct{}{}
	}
	found := make(map[string][]int)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		if idx := bytes.IndexByte(raw, 0); idx >= 0 {
			raw = raw[:idx]
		}
		procName := string(raw)
		if _, ok := targets[procName]; ok {
			found[procName] = append(found[procName], pid)
			continue
		}
		// Handle sub-processes like com.tencent.tmgp.sgame:xg_vip_service
		if colon := strings.IndexByte(procName, ':'); colon > 0 {
			base := procName[:colon]
			if _, ok := targets[base]; ok {
				found[base] = append(found[base], pid)
			}
		}
	}
	return found
}

func isForegroundOom(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	if err != nil {
		return false
	}
	adj, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return false
	}
	return adj <= 0
}

func queryForegroundPackage(ctx context.Context) string {
	// `dumpsys window displays` is fast and exposes mCurrentFocus / mFocusedApp on Android 16 / HyperOS 3.
	out, err := exec.CommandContext(ctx, "/system/bin/dumpsys", "window", "displays").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "mCurrentFocus=") || strings.Contains(line, "mFocusedApp=") {
				if m := pkgActivityRe.FindStringSubmatch(line); len(m) >= 2 {
					return m[1]
				}
			}
		}
	}
	out, err = exec.CommandContext(ctx, "/system/bin/dumpsys", "activity", "activities").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "topResumedActivity=") || strings.Contains(line, "ResumedActivity:") {
				if m := pkgActivityRe.FindStringSubmatch(line); len(m) >= 2 {
					return m[1]
				}
			}
		}
	}
	return ""
}
