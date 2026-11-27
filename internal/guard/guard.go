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
	"syscall"
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
	mu                    sync.RWMutex
	packages              []string
	pollInterval          time.Duration
	screenOffPollInterval time.Duration
	resumeGrace           time.Duration
	pauseFlagPath         string
	detector              ForegroundDetector
	screenChecker         func() bool
	listener              ListenerController
	manualPaused          bool
	manualReason          string
	gamePaused            bool
	activeGamePkg         string
	gameClearedAt         time.Time
	lastCheckAt           time.Time
	onGamePause           func(pkg string)
	onGameResume          func()
	logger                func(format string, args ...any)
}

type Options struct {
	Packages              []string
	PollInterval          time.Duration
	ScreenOffPollInterval time.Duration
	ResumeGrace           time.Duration
	PauseFlagPath         string
	Detector              ForegroundDetector
	ScreenChecker         func() bool
	Listener              ListenerController
	OnGamePause           func(pkg string)
	OnGameResume          func()
	Logger                func(format string, args ...any)
}

func New(opts Options) *Guard {
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	screenOffPoll := opts.ScreenOffPollInterval
	if screenOffPoll <= 0 {
		screenOffPoll = 30 * time.Second
	}
	grace := opts.ResumeGrace
	if grace < 0 {
		grace = 3 * time.Second
	}
	det := opts.Detector
	if det == nil {
		det = &AndroidForegroundDetector{}
	}
	screenChecker := opts.ScreenChecker
	if screenChecker == nil {
		screenChecker = DefaultScreenChecker
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
		packages:              pkgs,
		pollInterval:          poll,
		screenOffPollInterval: screenOffPoll,
		resumeGrace:           grace,
		pauseFlagPath:         opts.PauseFlagPath,
		detector:              det,
		screenChecker:         screenChecker,
		listener:              opts.Listener,
		onGamePause:           opts.OnGamePause,
		onGameResume:          opts.OnGameResume,
		logger:                logger,
	}
}

// DefaultScreenChecker performs a zero-fork sysfs display check.
// When DRM dpms status is exposed, it must report "On" alongside backlight brightness > 0
// so OLED AOD (brightness > 0 while dpms != On) does not falsely appear screen-on.
// If no display sysfs entries exist (e.g. non-Android test hosts), it returns true.
func DefaultScreenChecker() bool {
	return checkScreenSysfs(
		[]string{"/sys/class/backlight/*/brightness", "/sys/class/leds/*backlight*/brightness"},
		[]string{"/sys/class/drm/card*-DSI-*/dpms"},
	)
}

func checkScreenSysfs(backlightPatterns, dpmsPatterns []string) bool {
	dpmsChecked := false
	dpmsOn := false
	for _, pat := range dpmsPatterns {
		matches, _ := filepath.Glob(pat)
		for _, p := range matches {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			dpmsChecked = true
			if strings.EqualFold(strings.TrimSpace(string(raw)), "On") {
				dpmsOn = true
			}
		}
	}
	if dpmsChecked && !dpmsOn {
		return false
	}

	blChecked := false
	blOn := false
	for _, pat := range backlightPatterns {
		matches, _ := filepath.Glob(pat)
		for _, p := range matches {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			blChecked = true
			if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && v > 0 {
				blOn = true
			}
		}
	}
	if blChecked {
		return blOn
	}
	if dpmsChecked {
		return dpmsOn
	}
	return true
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

func (g *Guard) isGamePausedFast() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.gamePaused
}

// Run starts the background anti-risk monitoring loop until ctx is cancelled.
// When the display is off and no game pause is active, it throttles polling to save standby power.
func (g *Guard) Run(ctx context.Context) {
	g.CheckOnce(ctx)
	for {
		interval := g.pollInterval
		if g.screenChecker != nil && !g.isGamePausedFast() && !g.screenChecker() {
			interval = g.screenOffPollInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if g.isGamePausedFast() || g.screenChecker == nil || g.screenChecker() {
				g.CheckOnce(ctx)
			}
		}
	}
}

// AndroidForegroundDetector performs a zero-subprocess delta /proc scan first; only when a
// non-cached, non-frozen guarded game process actually exists does it check foreground focus
// via dumpsys.
type AndroidForegroundDetector struct {
	mu            sync.Mutex
	lastSeenPID   int
	knownSafePIDs map[int]struct{}
	volatilePIDs  map[int]struct{} // pre-forked zygote/usap/<pre-initialized> PIDs that may specialize later
	guardedPIDs   map[int]string   // pid -> base guarded package
	packagesKey   string
	pollCount     uint64

	// Optional overrides for deterministic unit tests (default to /proc, /sys/fs/cgroup, real dumpsys).
	procRoot    string
	cgroupRoot  string
	queryFgFunc func(ctx context.Context) string
}

var pkgActivityRe = regexp.MustCompile(`u\d+\s+([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)`)

func (d *AndroidForegroundDetector) getProcRoot() string {
	if d.procRoot != "" {
		return d.procRoot
	}
	return "/proc"
}

func (d *AndroidForegroundDetector) getCgroupRoot() string {
	if d.cgroupRoot != "" {
		return d.cgroupRoot
	}
	return "/sys/fs/cgroup"
}

func (d *AndroidForegroundDetector) DetectGuardedForeground(ctx context.Context, packages []string) (string, error) {
	if len(packages) == 0 {
		return "", nil
	}
	// Step 1: Zero-exec delta /proc scan — returns immediately if none of the guarded games are running.
	running := d.findRunningPackages(packages)
	if len(running) == 0 {
		return "", nil
	}

	// Step 2: Filter matched guarded PIDs to non-cached (oom_score_adj <= 200) and non-frozen
	// BEFORE forking dumpsys, avoiding continuous dumpsys storms while a game sits cached in RAM.
	activeCandidates := make(map[string][]int)
	for pkg, pids := range running {
		for _, pid := range pids {
			adj, ok := d.readOomScoreAdj(pid)
			if !ok || adj > 200 {
				continue
			}
			if d.isPIDFrozen(pid) {
				continue
			}
			activeCandidates[pkg] = append(activeCandidates[pkg], pid)
		}
	}
	if len(activeCandidates) == 0 {
		return "", nil
	}

	// Step 3: Check current focused window / resumed activity via lightweight dumpsys.
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	qfn := d.queryFgFunc
	if qfn == nil {
		qfn = queryForegroundPackage
	}
	if fgPkg := qfn(checkCtx); fgPkg != "" {
		for _, target := range packages {
			if fgPkg == target {
				return target, nil
			}
		}
		return "", nil
	}

	// Fallback if dumpsys was temporarily unavailable: check oom_score_adj <= 0 (foreground app)
	for pkg, pids := range activeCandidates {
		for _, pid := range pids {
			if d.isForegroundOom(pid) {
				return pkg, nil
			}
		}
	}
	return "", nil
}

func isVolatileAppProc(procName string) bool {
	if procName == "" {
		return true
	}
	low := strings.ToLower(procName)
	return strings.HasPrefix(low, "usap") ||
		strings.HasPrefix(low, "zygote") ||
		strings.HasPrefix(low, "webview_zygote") ||
		strings.Contains(low, "<pre-initialized>")
}

func matchGuardedPackage(procName string, targets map[string]struct{}) (string, bool) {
	if procName == "" {
		return "", false
	}
	if _, ok := targets[procName]; ok {
		return procName, true
	}
	// Handle sub-processes like com.tencent.tmgp.sgame:xg_vip_service
	if colon := strings.IndexByte(procName, ':'); colon > 0 {
		base := procName[:colon]
		if _, ok := targets[base]; ok {
			return base, true
		}
	}
	return "", false
}

func (d *AndroidForegroundDetector) readLastPID() int {
	raw, err := os.ReadFile(filepath.Join(d.getProcRoot(), "loadavg"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 5 {
		return 0
	}
	pid, err := strconv.Atoi(fields[4])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func (d *AndroidForegroundDetector) readCmdline(pid int) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(d.getProcRoot(), strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", false
	}
	if len(raw) == 0 {
		return "", true
	}
	if idx := bytes.IndexByte(raw, 0); idx >= 0 {
		raw = raw[:idx]
	}
	return string(raw), true
}

func (d *AndroidForegroundDetector) findRunningPackages(packages []string) map[string][]int {
	d.mu.Lock()
	defer d.mu.Unlock()

	pkgKey := strings.Join(packages, "\x00")
	if d.packagesKey != pkgKey || d.knownSafePIDs == nil {
		d.knownSafePIDs = make(map[int]struct{})
		d.volatilePIDs = make(map[int]struct{})
		d.guardedPIDs = make(map[int]string)
		d.lastSeenPID = 0
		d.packagesKey = pkgKey
		d.pollCount = 0
	}

	targets := make(map[string]struct{}, len(packages))
	for _, p := range packages {
		targets[p] = struct{}{}
	}

	d.pollCount++
	curLastPID := d.readLastPID()
	needPrune := d.pollCount%30 == 0
	// If PID counter wrapped around, reset knownSafePIDs so recycled PIDs are re-checked.
	if curLastPID > 0 && d.lastSeenPID > 0 && curLastPID < d.lastSeenPID {
		d.knownSafePIDs = make(map[int]struct{})
	}

	// Fast path: /proc/loadavg lastPID unchanged since previous poll -> no new processes forked.
	// Only re-check tracked guarded PIDs and any pre-forked volatile (usap/zygote) PIDs.
	if d.pollCount > 1 && curLastPID > 0 && curLastPID == d.lastSeenPID && !needPrune {
		for pid := range d.volatilePIDs {
			procName, ok := d.readCmdline(pid)
			if !ok {
				delete(d.volatilePIDs, pid)
				continue
			}
			if pkg, matched := matchGuardedPackage(procName, targets); matched {
				delete(d.volatilePIDs, pid)
				d.guardedPIDs[pid] = pkg
			} else if !isVolatileAppProc(procName) {
				delete(d.volatilePIDs, pid)
				d.knownSafePIDs[pid] = struct{}{}
			}
		}
		found := make(map[string][]int)
		for pid := range d.guardedPIDs {
			procName, ok := d.readCmdline(pid)
			if !ok {
				delete(d.guardedPIDs, pid)
				continue
			}
			if pkg, matched := matchGuardedPackage(procName, targets); matched {
				d.guardedPIDs[pid] = pkg
				found[pkg] = append(found[pkg], pid)
			} else {
				delete(d.guardedPIDs, pid)
				if isVolatileAppProc(procName) {
					d.volatilePIDs[pid] = struct{}{}
				} else {
					d.knownSafePIDs[pid] = struct{}{}
				}
			}
		}
		return found
	}

	procDir := d.getProcRoot()
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	if curLastPID > 0 {
		d.lastSeenPID = curLastPID
	}

	var alive map[int]struct{}
	if needPrune {
		alive = make(map[int]struct{}, len(entries))
	}
	filterSystemUID := d.procRoot == ""

	found := make(map[string][]int)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if alive != nil {
			alive[pid] = struct{}{}
		}
		if _, safe := d.knownSafePIDs[pid]; safe {
			continue
		}
		if _, tracked := d.guardedPIDs[pid]; tracked {
			procName, ok := d.readCmdline(pid)
			if !ok {
				delete(d.guardedPIDs, pid)
				continue
			}
			if pkg, matched := matchGuardedPackage(procName, targets); matched {
				d.guardedPIDs[pid] = pkg
				found[pkg] = append(found[pkg], pid)
			} else {
				delete(d.guardedPIDs, pid)
				if isVolatileAppProc(procName) {
					d.volatilePIDs[pid] = struct{}{}
				} else {
					d.knownSafePIDs[pid] = struct{}{}
				}
			}
			continue
		}

		// New PID (or volatile PID): inspect UID / cmdline once and classify.
		isSystemUID := false
		if filterSystemUID {
			if info, ierr := e.Info(); ierr == nil {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid < 10000 {
					isSystemUID = true
				}
			}
		}
		procName, ok := d.readCmdline(pid)
		if !ok {
			delete(d.volatilePIDs, pid)
			continue
		}
		if isSystemUID {
			// System/kernel processes cannot be third-party games, EXCEPT unspecialized
			// zygote/usap pool processes which later setuid(>=10000) and specialize into apps.
			if procName != "" && isVolatileAppProc(procName) {
				d.volatilePIDs[pid] = struct{}{}
			} else {
				delete(d.volatilePIDs, pid)
				d.knownSafePIDs[pid] = struct{}{}
			}
			continue
		}
		if pkg, matched := matchGuardedPackage(procName, targets); matched {
			delete(d.volatilePIDs, pid)
			d.guardedPIDs[pid] = pkg
			found[pkg] = append(found[pkg], pid)
			continue
		}
		if isVolatileAppProc(procName) {
			d.volatilePIDs[pid] = struct{}{}
			continue
		}
		delete(d.volatilePIDs, pid)
		d.knownSafePIDs[pid] = struct{}{}
	}

	if alive != nil {
		for pid := range d.knownSafePIDs {
			if _, ok := alive[pid]; !ok {
				delete(d.knownSafePIDs, pid)
			}
		}
		for pid := range d.volatilePIDs {
			if _, ok := alive[pid]; !ok {
				delete(d.volatilePIDs, pid)
			}
		}
		for pid := range d.guardedPIDs {
			if _, ok := alive[pid]; !ok {
				delete(d.guardedPIDs, pid)
			}
		}
	}
	return found
}

func (d *AndroidForegroundDetector) readOomScoreAdj(pid int) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(d.getProcRoot(), strconv.Itoa(pid), "oom_score_adj"))
	if err != nil {
		return 0, false
	}
	adj, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return adj, true
}

func (d *AndroidForegroundDetector) isForegroundOom(pid int) bool {
	adj, ok := d.readOomScoreAdj(pid)
	return ok && adj <= 0
}

func (d *AndroidForegroundDetector) isPIDFrozen(pid int) bool {
	if pid <= 0 {
		return false
	}
	pidStr := strconv.Itoa(pid)
	procDir := d.getProcRoot()
	if raw, err := os.ReadFile(filepath.Join(procDir, pidStr, "cgroup")); err == nil {
		cgRoot := d.getCgroupRoot()
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, ":", 3)
			if len(parts) == 3 && parts[0] == "0" && parts[1] == "" && parts[2] != "" {
				cgDir := filepath.Join(cgRoot, strings.TrimPrefix(parts[2], "/"))
				if evRaw, err := os.ReadFile(filepath.Join(cgDir, "cgroup.events")); err == nil {
					for _, evLine := range strings.Split(string(evRaw), "\n") {
						fields := strings.Fields(evLine)
						if len(fields) >= 2 && fields[0] == "frozen" && fields[1] == "1" {
							return true
						}
					}
				}
				if frRaw, err := os.ReadFile(filepath.Join(cgDir, "cgroup.freeze")); err == nil {
					if strings.TrimSpace(string(frRaw)) == "1" {
						return true
					}
				}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(procDir, pidStr, "wchan")); err == nil {
		if strings.Contains(strings.ToLower(string(raw)), "freezer") {
			return true
		}
	}
	return false
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
