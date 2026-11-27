package backend

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
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

	"android-agent/internal/imgutil"
)

//go:embed cliphelper.jar
var embeddedClipHelperJar []byte

var (
	wmSizeRe      = regexp.MustCompile(`(\d+)x(\d+)`)
	wmDensityRe   = regexp.MustCompile(`(\d+)`)
	fgRecordRe    = regexp.MustCompile(`u\d+\s+([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)`)
	safeKeycodeRe = regexp.MustCompile(`^[A-Z0-9_]+$`)
	safePkgRe     = regexp.MustCompile(`^[a-zA-Z0-9_]+(\.[a-zA-Z0-9_]+)+$`)
)

var friendlyKeycodes = map[string]string{
	"HOME":        "KEYCODE_HOME",
	"BACK":        "KEYCODE_BACK",
	"RECENTS":     "KEYCODE_APP_SWITCH",
	"APP_SWITCH":  "KEYCODE_APP_SWITCH",
	"ENTER":       "KEYCODE_ENTER",
	"DEL":         "KEYCODE_DEL",
	"BACKSPACE":   "KEYCODE_DEL",
	"PASTE":       "KEYCODE_PASTE",
	"COPY":        "KEYCODE_COPY",
	"CUT":         "KEYCODE_CUT",
	"TAB":         "KEYCODE_TAB",
	"ESC":         "KEYCODE_ESCAPE",
	"ESCAPE":      "KEYCODE_ESCAPE",
	"SPACE":       "KEYCODE_SPACE",
	"POWER":       "KEYCODE_POWER",
	"WAKEUP":      "KEYCODE_WAKEUP",
	"SLEEP":       "KEYCODE_SLEEP",
	"VOLUME_UP":   "KEYCODE_VOLUME_UP",
	"VOLUME_DOWN": "KEYCODE_VOLUME_DOWN",
	"DPAD_UP":     "KEYCODE_DPAD_UP",
	"DPAD_DOWN":   "KEYCODE_DPAD_DOWN",
	"DPAD_LEFT":   "KEYCODE_DPAD_LEFT",
	"DPAD_RIGHT":  "KEYCODE_DPAD_RIGHT",
	"DPAD_CENTER": "KEYCODE_DPAD_CENTER",
	"MENU":        "KEYCODE_MENU",
	"SEARCH":      "KEYCODE_SEARCH",
	"MOVE_HOME":   "KEYCODE_MOVE_HOME",
	"MOVE_END":    "KEYCODE_MOVE_END",
}

type ShellBackend struct {
	clipHelperPath string
	shellTimeout   time.Duration

	clipMu          sync.Mutex
	clipHelperReady bool

	kernel *MicroKernel

	uiCacheMu  sync.RWMutex
	uiCache    map[int]UINode
	uiCachePkg string
	uiCacheW   int
	uiCacheH   int

	screenMu       sync.RWMutex
	cachedScreen   ScreenInfo
	cachedScreenAt time.Time
	screenQueryFn  func(ctx context.Context) (ScreenInfo, error)

	touchMu          sync.Mutex
	evTouch          *EvdevTouch
	touchProbe       TouchDeviceInfo
	touchLastProbeAt time.Time
	touchProbeFailN  int
	touchRetryBase   time.Duration
	touchProbeFunc   func() (TouchDeviceInfo, error)

	touchMethodMu sync.RWMutex
	lastTouchMeth string

	rotMu     sync.RWMutex
	lastRot   int
	lastRotAt time.Time

	screenGateMu       sync.Mutex
	lastGateAt         time.Time
	lastGateErr        error
	screenGateFunc     func(ctx context.Context) error
	backlightCheckFunc func() bool
	policyQueryFunc    func(ctx context.Context) (string, error)
}

const (
	screenInfoCacheTTL    = 60 * time.Second
	defaultClipHelperPath = "/data/local/tmp/.ag_cache/helper.jar"
	legacyClipHelperPath  = "/data/local/tmp/android_agent_cliphelper.jar"
)

func NewShellBackend(clipHelperPath string, shellTimeout time.Duration) *ShellBackend {
	if clipHelperPath == "" || clipHelperPath == legacyClipHelperPath {
		clipHelperPath = defaultClipHelperPath
	}
	if shellTimeout <= 0 {
		shellTimeout = 10 * time.Second
	}
	b := &ShellBackend{
		clipHelperPath: clipHelperPath,
		shellTimeout:   shellTimeout,
		uiCache:        make(map[int]UINode),
		lastTouchMeth:  "uninitialized",
	}
	b.kernel = NewMicroKernel(b.ensureClipHelper, defaultKernelIdleTimeout)
	b.kernel.SetOnStop(b.cleanupClipHelper)
	b.screenGateFunc = b.checkScreenInteractive
	return b
}

// Close stops the micro-kernel and waits for any in-flight evdev touch gesture to release cleanly.
func (b *ShellBackend) Close() {
	if b.kernel != nil {
		b.kernel.Stop("backend close")
	}
	b.touchMu.Lock()
	ev := b.evTouch
	b.touchMu.Unlock()
	if ev != nil {
		ev.Drain()
	}
}

func (b *ShellBackend) Name() string {
	return "hybrid-layered-v2"
}

func (b *ShellBackend) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.shellTimeout)
}

func (b *ShellBackend) queryScreenInfo(ctx context.Context) (ScreenInfo, error) {
	if b.screenQueryFn != nil {
		return b.screenQueryFn(ctx)
	}
	var info ScreenInfo
	if out, err := exec.CommandContext(ctx, "/system/bin/cmd", "window", "size").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		// Prefer Override size if present (usually the last line)
		for i := len(lines) - 1; i >= 0; i-- {
			if m := wmSizeRe.FindStringSubmatch(lines[i]); len(m) == 3 {
				info.Width, _ = strconv.Atoi(m[1])
				info.Height, _ = strconv.Atoi(m[2])
				break
			}
		}
	}
	if out, err := exec.CommandContext(ctx, "/system/bin/cmd", "window", "density").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if m := wmDensityRe.FindStringSubmatch(lines[i]); len(m) == 2 {
				info.Density, _ = strconv.Atoi(m[1])
				break
			}
		}
	}
	if info.Width > 0 && info.Height > 0 {
		return info, nil
	}
	return ScreenInfo{}, errors.New("unable to query screen size via wm")
}

func (b *ShellBackend) ScreenInfo(ctx context.Context) (ScreenInfo, error) {
	b.screenMu.RLock()
	cached := b.cachedScreen
	cachedAt := b.cachedScreenAt
	b.screenMu.RUnlock()
	if cached.Width > 0 && cached.Height > 0 && !cachedAt.IsZero() && time.Since(cachedAt) < screenInfoCacheTTL {
		return cached, nil
	}

	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	if info, err := b.queryScreenInfo(cctx); err == nil && info.Width > 0 && info.Height > 0 {
		b.screenMu.Lock()
		b.cachedScreen = info
		b.cachedScreenAt = time.Now()
		b.screenMu.Unlock()
		return info, nil
	}
	if cached.Width > 0 && cached.Height > 0 {
		return cached, nil
	}
	return ScreenInfo{}, errors.New("unable to query screen size via wm")
}

func (b *ShellBackend) Screenshot(ctx context.Context, targetWidth, quality int) (*ScreenshotResult, error) {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	start := time.Now()
	// Fast path: raw RGBA screencap (~250ms on Xiaomi 13 vs ~1170ms for `screencap -p`)
	rawOut, rawErr := exec.CommandContext(cctx, "/system/bin/screencap").Output()
	if rawErr == nil && len(rawOut) > 16 {
		if compressed, err := imgutil.CompressRawScreencap(rawOut, targetWidth, quality); err == nil {
			b.screenMu.Lock()
			b.cachedScreen.Width = compressed.OrigWidth
			b.cachedScreen.Height = compressed.OrigHeight
			b.screenMu.Unlock()
			return &ScreenshotResult{
				JPEG:          compressed.JPEG,
				Width:         compressed.Width,
				Height:        compressed.Height,
				OrigWidth:     compressed.OrigWidth,
				OrigHeight:    compressed.OrigHeight,
				CaptureMethod: "screencap-raw",
				ElapsedMs:     time.Since(start).Milliseconds(),
			}, nil
		}
	}

	// Fallback: `screencap -p` (PNG)
	pngOut, pngErr := exec.CommandContext(cctx, "/system/bin/screencap", "-p").Output()
	if pngErr != nil {
		return nil, fmt.Errorf("screencap failed (raw=%v, png=%v)", rawErr, pngErr)
	}
	compressed, err := imgutil.CompressPNG(pngOut, targetWidth, quality)
	if err != nil {
		return nil, fmt.Errorf("compress screencap png: %w", err)
	}
	b.screenMu.Lock()
	b.cachedScreen.Width = compressed.OrigWidth
	b.cachedScreen.Height = compressed.OrigHeight
	b.screenMu.Unlock()
	return &ScreenshotResult{
		JPEG:          compressed.JPEG,
		Width:         compressed.Width,
		Height:        compressed.Height,
		OrigWidth:     compressed.OrigWidth,
		OrigHeight:    compressed.OrigHeight,
		CaptureMethod: "screencap-png-fallback",
		ElapsedMs:     time.Since(start).Milliseconds(),
	}, nil
}

func (b *ShellBackend) ensureTouch() *EvdevTouch {
	b.touchMu.Lock()
	defer b.touchMu.Unlock()

	if b.evTouch != nil {
		return b.evTouch
	}
	if !b.touchLastProbeAt.IsZero() && b.touchProbeFailN > 0 {
		base := b.touchRetryBase
		if base <= 0 {
			base = 5 * time.Second
		}
		shift := b.touchProbeFailN - 1
		if shift > 4 {
			shift = 4
		}
		backoff := base << shift
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
		if time.Since(b.touchLastProbeAt) < backoff {
			return nil
		}
	}

	probeFn := b.touchProbeFunc
	if probeFn == nil {
		probeFn = ProbeTouchscreen
	}
	info, err := probeFn()
	b.touchLastProbeAt = time.Now()
	if err != nil {
		info.ProbeError = err.Error()
		info.Available = false
	}
	b.touchProbe = info
	if info.Available {
		b.evTouch = NewEvdevTouch(info)
		b.touchProbeFailN = 0
		return b.evTouch
	}
	b.touchProbeFailN++
	return nil
}

func (b *ShellBackend) setLastTouchMethod(m string) {
	b.touchMethodMu.Lock()
	b.lastTouchMeth = m
	b.touchMethodMu.Unlock()
}

func (b *ShellBackend) TouchStatus() TouchStatusInfo {
	ev := b.ensureTouch()
	b.touchMethodMu.RLock()
	meth := b.lastTouchMeth
	b.touchMethodMu.RUnlock()
	b.touchMu.Lock()
	dev := b.touchProbe
	b.touchMu.Unlock()
	if ev != nil {
		dev = ev.Device()
	}
	return TouchStatusInfo{
		Device:     dev,
		LastMethod: meth,
	}
}

func (b *ShellBackend) portraitDimensions(ctx context.Context) (int, int) {
	b.screenMu.RLock()
	w, h := b.cachedScreen.Width, b.cachedScreen.Height
	b.screenMu.RUnlock()
	if w <= 0 || h <= 0 {
		if scr, err := b.ScreenInfo(ctx); err == nil {
			w, h = scr.Width, scr.Height
		}
	}
	if w > h && h > 0 {
		w, h = h, w
	}
	if w <= 0 || h <= 0 {
		return 1080, 2400
	}
	return w, h
}

func (b *ShellBackend) isPortrait(ctx context.Context) bool {
	b.rotMu.RLock()
	cachedRot, cachedAt := b.lastRot, b.lastRotAt
	b.rotMu.RUnlock()
	if !cachedAt.IsZero() && time.Since(cachedAt) < 800*time.Millisecond {
		return cachedRot == 0
	}

	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/system/bin/dumpsys", "window", "displays").Output()
	if err != nil {
		return false
	}
	s := string(out)
	if strings.Contains(s, "mCurrentRotation=ROTATION_0") {
		b.rotMu.Lock()
		b.lastRot = 0
		b.lastRotAt = time.Now()
		b.rotMu.Unlock()
		return true
	}
	rot := -1
	switch {
	case strings.Contains(s, "mCurrentRotation=ROTATION_90"):
		rot = 1
	case strings.Contains(s, "mCurrentRotation=ROTATION_180"):
		rot = 2
	case strings.Contains(s, "mCurrentRotation=ROTATION_270"):
		rot = 3
	}
	if rot >= 0 {
		b.rotMu.Lock()
		b.lastRot = rot
		b.lastRotAt = time.Now()
		b.rotMu.Unlock()
	}
	return false
}

// IsBacklightScreenOn performs a zero-fork sysfs check to see if the display panel is interactively on.
// Requires DRM dpms=="On" when exposed alongside backlight brightness > 0 so OLED AOD is treated as off.
// Returns true on unknown/non-Android hosts so unit tests and fallback behavior remain intact.
func IsBacklightScreenOn() bool {
	dpmsChecked := false
	dpmsOn := false
	dpmsMatches, _ := filepath.Glob("/sys/class/drm/card*-DSI-*/dpms")
	for _, p := range dpmsMatches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		dpmsChecked = true
		if strings.EqualFold(strings.TrimSpace(string(raw)), "On") {
			dpmsOn = true
		}
	}
	if dpmsChecked && !dpmsOn {
		return false
	}

	patterns := []string{
		"/sys/class/backlight/*/brightness",
		"/sys/class/leds/*backlight*/brightness",
	}
	blChecked := false
	blOn := false
	for _, pat := range patterns {
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

func parseScreenInteractivePolicy(policyOut string) error {
	if strings.Contains(policyOut, "screenState=SCREEN_STATE_OFF") ||
		strings.Contains(policyOut, "screenState=SCREEN_STATE_DOZE") ||
		strings.Contains(policyOut, "screenState=SCREEN_STATE_DOZE_SUSPEND") {
		return errors.New("screen is off; refusing touch injection")
	}
	if strings.Contains(policyOut, "mDreamingLockscreen=true") ||
		strings.Contains(policyOut, "isKeyguardShowing=true") ||
		strings.Contains(policyOut, "mShowingLockscreen=true") {
		return errors.New("device is locked (keyguard showing); refusing touch injection")
	}
	inKeyguard := false
	for _, line := range strings.Split(policyOut, "\n") {
		trim := strings.TrimSpace(line)
		if strings.Contains(trim, "KeyguardServiceDelegate") {
			inKeyguard = true
			continue
		}
		if inKeyguard {
			if strings.HasPrefix(trim, "showing=true") || strings.Contains(trim, "showing=true") {
				return errors.New("device is locked (keyguard showing); refusing touch injection")
			}
			if strings.HasPrefix(trim, "showing=false") || trim == "" {
				inKeyguard = false
			}
		}
	}
	return nil
}

func (b *ShellBackend) queryWindowPolicy(ctx context.Context) (string, error) {
	if b.policyQueryFunc != nil {
		return b.policyQueryFunc(ctx)
	}
	out, err := exec.CommandContext(ctx, "/system/bin/dumpsys", "window", "policy").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (b *ShellBackend) checkScreenInteractive(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	blCheck := b.backlightCheckFunc
	if blCheck == nil {
		blCheck = IsBacklightScreenOn
	}
	if !blCheck() {
		return errors.New("screen is off; refusing touch injection")
	}
	b.screenGateMu.Lock()
	lastAt := b.lastGateAt
	lastErr := b.lastGateErr
	b.screenGateMu.Unlock()
	if !lastAt.IsZero() && time.Since(lastAt) < 800*time.Millisecond {
		return lastErr
	}

	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	out, err := b.queryWindowPolicy(cctx)
	if err != nil {
		// Fail closed: never permit touch/input injection when lockscreen policy cannot be verified.
		return fmt.Errorf("screen policy check failed (refusing touch injection): %w", err)
	}
	gateErr := parseScreenInteractivePolicy(out)
	b.screenGateMu.Lock()
	b.lastGateAt = time.Now()
	b.lastGateErr = gateErr
	b.screenGateMu.Unlock()
	return gateErr
}

// IsScreenUnlocked returns true only when the display is on AND the keyguard is not locked.
func (b *ShellBackend) IsScreenUnlocked(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	return b.enforceScreenInteractive(ctx) == nil
}

func (b *ShellBackend) enforceScreenInteractive(ctx context.Context) error {
	if b.screenGateFunc != nil {
		return b.screenGateFunc(ctx)
	}
	return b.checkScreenInteractive(ctx)
}

func (b *ShellBackend) Tap(ctx context.Context, x, y int) error {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	if err := b.enforceScreenInteractive(cctx); err != nil {
		return err
	}

	if ev := b.ensureTouch(); ev != nil && ev.Available() && b.isPortrait(cctx) {
		sw, sh := b.portraitDimensions(cctx)
		if err := ev.Tap(cctx, x, y, sw, sh); err == nil {
			b.setLastTouchMethod("evdev:" + ev.Device().Path)
			return nil
		} else if cctx.Err() != nil {
			return err
		}
	}

	out, err := exec.CommandContext(cctx, "/system/bin/cmd", "input", "tap", strconv.Itoa(x), strconv.Itoa(y)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("input tap failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	b.setLastTouchMethod("cmd-input-fallback")
	return nil
}

func (b *ShellBackend) LongPress(ctx context.Context, x, y, durationMs int) error {
	if durationMs <= 0 {
		durationMs = 800
	}
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	if err := b.enforceScreenInteractive(cctx); err != nil {
		return err
	}

	if ev := b.ensureTouch(); ev != nil && ev.Available() && b.isPortrait(cctx) {
		sw, sh := b.portraitDimensions(cctx)
		if err := ev.LongPress(cctx, x, y, sw, sh, durationMs); err == nil {
			b.setLastTouchMethod("evdev:" + ev.Device().Path)
			return nil
		} else if cctx.Err() != nil {
			return err
		}
	}
	return b.cmdInputSwipe(cctx, x, y, x, y, durationMs)
}

func (b *ShellBackend) Swipe(ctx context.Context, x1, y1, x2, y2, durationMs int) error {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	if durationMs <= 0 {
		durationMs = 300
	}

	if err := b.enforceScreenInteractive(cctx); err != nil {
		return err
	}

	if ev := b.ensureTouch(); ev != nil && ev.Available() && b.isPortrait(cctx) {
		sw, sh := b.portraitDimensions(cctx)
		if err := ev.Swipe(cctx, x1, y1, x2, y2, sw, sh, durationMs); err == nil {
			b.setLastTouchMethod("evdev:" + ev.Device().Path)
			return nil
		} else if cctx.Err() != nil {
			return err
		}
	}
	return b.cmdInputSwipe(cctx, x1, y1, x2, y2, durationMs)
}

func (b *ShellBackend) cmdInputSwipe(ctx context.Context, x1, y1, x2, y2, durationMs int) error {
	out, err := exec.CommandContext(ctx, "/system/bin/cmd", "input", "swipe",
		strconv.Itoa(x1), strconv.Itoa(y1),
		strconv.Itoa(x2), strconv.Itoa(y2),
		strconv.Itoa(durationMs),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("input swipe failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	b.setLastTouchMethod("cmd-input-fallback")
	return nil
}

func (b *ShellBackend) KeyEvent(ctx context.Context, keycode string) error {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	key := strings.ToUpper(strings.TrimSpace(keycode))
	if key == "" {
		return errors.New("keycode is required")
	}
	if mapped, ok := friendlyKeycodes[key]; ok {
		key = mapped
	}
	if !safeKeycodeRe.MatchString(key) {
		return fmt.Errorf("invalid keycode: %q", keycode)
	}
	out, err := exec.CommandContext(cctx, "/system/bin/cmd", "input", "keyevent", key).CombinedOutput()
	if err != nil {
		return fmt.Errorf("input keyevent %s failed: %w (%s)", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *ShellBackend) InputText(ctx context.Context, text string, method string, submit bool) (string, error) {
	if text == "" {
		return "", errors.New("text is required")
	}
	gateCtx, gateCancel := b.withTimeout(ctx)
	defer gateCancel()
	if err := b.enforceScreenInteractive(gateCtx); err != nil {
		return "", err
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "auto"
	}

	usedMethod := method
	if method == "input" || (method == "auto" && isSafeInputASCII(text)) {
		cctx, cancel := b.withTimeout(ctx)
		defer cancel()
		escaped := strings.ReplaceAll(text, " ", "%s")
		out, err := exec.CommandContext(cctx, "/system/bin/cmd", "input", "text", escaped).CombinedOutput()
		if err == nil {
			usedMethod = "input-text-ascii"
		} else if method == "input" {
			return "", fmt.Errorf("input text failed: %w (%s)", err, strings.TrimSpace(string(out)))
		} else {
			// Fall through to clipboard + paste
			if err := b.SetClipboard(ctx, text); err != nil {
				return "", fmt.Errorf("clipboard input fallback failed: %w", err)
			}
			time.Sleep(60 * time.Millisecond)
			if err := b.KeyEvent(ctx, "279"); err != nil {
				return "", fmt.Errorf("paste keyevent failed: %w", err)
			}
			usedMethod = "clipboard-paste"
		}
	} else {
		// Chinese / UTF-8 / multi-line / special chars -> app_process64 clipboard + KEYCODE_PASTE (279)
		if err := b.SetClipboard(ctx, text); err != nil {
			return "", fmt.Errorf("set clipboard for Chinese/UTF-8 input failed (%v); you may also install ADBKeyboard as an alternative", err)
		}
		time.Sleep(60 * time.Millisecond)
		if err := b.KeyEvent(ctx, "279"); err != nil {
			return "", fmt.Errorf("paste keyevent 279 failed: %w", err)
		}
		usedMethod = "clipboard-paste"
	}

	if submit {
		time.Sleep(50 * time.Millisecond)
		if err := b.KeyEvent(ctx, "KEYCODE_ENTER"); err != nil {
			return usedMethod, fmt.Errorf("submit enter failed: %w", err)
		}
	}
	return usedMethod, nil
}

func isSafeInputASCII(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Only allow safe alphanumeric and basic punctuation for `input text`;
		// everything else (Chinese, spaces, shell metacharacters, newlines) goes through clipboard+paste.
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-' || c == ':' || c == '/' || c == '@' {
			continue
		}
		return false
	}
	return true
}

func isPrivateCacheDir(dir string) bool {
	clean := filepath.Clean(dir)
	return clean != "" && clean != "." && clean != "/" && clean != "/tmp" && clean != "/data/local/tmp"
}

func (b *ShellBackend) ensureClipHelper() (string, error) {
	b.clipMu.Lock()
	defer b.clipMu.Unlock()

	dir := filepath.Dir(b.clipHelperPath)
	if b.clipHelperReady {
		if fileMatchesBytes(b.clipHelperPath, embeddedClipHelperJar) {
			return b.clipHelperPath, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir cliphelper dir: %w", err)
	}
	if isPrivateCacheDir(dir) {
		_ = os.Chmod(dir, 0o700)
		_ = os.Chown(dir, 2000, 2000)
	}
	if fileMatchesBytes(b.clipHelperPath, embeddedClipHelperJar) {
		_ = os.Chmod(b.clipHelperPath, 0o600)
		_ = os.Chown(b.clipHelperPath, 2000, 2000)
		b.clipHelperReady = true
		return b.clipHelperPath, nil
	}
	tmpPath := b.clipHelperPath + ".tmp"
	if err := os.WriteFile(tmpPath, embeddedClipHelperJar, 0o600); err != nil {
		return "", fmt.Errorf("write cliphelper.jar: %w", err)
	}
	_ = os.Chmod(tmpPath, 0o600)
	_ = os.Chown(tmpPath, 2000, 2000)
	if err := os.Rename(tmpPath, b.clipHelperPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("install cliphelper.jar: %w", err)
	}
	if b.clipHelperPath != legacyClipHelperPath {
		_ = os.Remove(legacyClipHelperPath)
	}
	b.clipHelperReady = true
	return b.clipHelperPath, nil
}

func (b *ShellBackend) cleanupClipHelper() {
	b.clipMu.Lock()
	defer b.clipMu.Unlock()

	b.clipHelperReady = false
	if b.clipHelperPath != "" {
		_ = os.Remove(b.clipHelperPath)
		_ = os.Remove(b.clipHelperPath + ".tmp")
		dir := filepath.Dir(b.clipHelperPath)
		if isPrivateCacheDir(dir) {
			_ = os.Remove(dir)
		}
	}
	_ = os.Remove(legacyClipHelperPath)
}

func (b *ShellBackend) runClipHelper(ctx context.Context, action string, stdin string) (string, error) {
	jarPath, err := b.ensureClipHelper()
	if err != nil {
		return "", err
	}
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	// Run app_process64 as shell (uid=2000, gid=2000) so ClipboardService accepts
	// callingPackage="com.android.shell" cleanly on Android 16.
	runOnce := func(asShell bool) (string, string, error) {
		cmd := exec.CommandContext(cctx, "/system/bin/app_process64", "/system/bin", "AgentClip", action)
		cmd.Env = append(os.Environ(), "CLASSPATH="+jarPath)
		if asShell && os.Geteuid() == 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: 2000, Gid: 2000},
			}
		}
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	out, errOut, err := runOnce(true)
	if err == nil {
		return out, nil
	}
	// Fallback attempt without credential switch in case SELinux restricts direct setuid
	if os.Geteuid() == 0 {
		out2, errOut2, err2 := runOnce(false)
		if err2 == nil {
			return out2, nil
		}
		return "", fmt.Errorf("app_process64 AgentClip %s failed (as shell: %v [%s]; as root: %v [%s])",
			action, err, strings.TrimSpace(errOut), err2, strings.TrimSpace(errOut2))
	}
	return "", fmt.Errorf("app_process64 AgentClip %s failed: %v (%s)", action, err, strings.TrimSpace(errOut))
}

func (b *ShellBackend) SetClipboard(ctx context.Context, text string) error {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	if b.kernel != nil {
		if _, err := b.kernel.Call(cctx, kernelRequest{Cmd: "clip_set", Text: text}); err == nil {
			return nil
		}
	}
	_, err := b.runClipHelper(ctx, "set", text)
	return err
}

func (b *ShellBackend) GetClipboard(ctx context.Context) (string, error) {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	if b.kernel != nil {
		if resp, err := b.kernel.Call(cctx, kernelRequest{Cmd: "clip_get"}); err == nil {
			return resp.Text, nil
		}
	}
	return b.runClipHelper(ctx, "get", "")
}

func (b *ShellBackend) StopMicroKernel(reason string) {
	if b.kernel != nil {
		b.kernel.Stop(reason)
	}
	b.cleanupClipHelper()
	b.uiCacheMu.Lock()
	b.uiCache = make(map[int]UINode)
	b.uiCachePkg = ""
	b.uiCacheMu.Unlock()
}

func (b *ShellBackend) MicroKernelRunning() bool {
	return b.kernel != nil && b.kernel.Running()
}

func (b *ShellBackend) UITree(ctx context.Context, opts UITreeOptions) (*UITreeResult, error) {
	opts = opts.normalized()
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	start := time.Now()
	resp, err := b.kernel.Call(cctx, kernelRequest{
		Cmd:      "ui_tree",
		MaxDepth: opts.MaxDepth,
		MaxNodes: opts.MaxNodes,
	})
	if err != nil {
		return nil, err
	}

	screenW, screenH := resp.Width, resp.Height
	if screenW > 0 && screenH > 0 {
		b.rotMu.Lock()
		b.lastRot = resp.Rotation
		b.lastRotAt = time.Now()
		b.rotMu.Unlock()
	}
	if screenW <= 0 || screenH <= 0 {
		b.screenMu.RLock()
		screenW, screenH = b.cachedScreen.Width, b.cachedScreen.Height
		b.screenMu.RUnlock()
		if screenW <= 0 || screenH <= 0 {
			if scr, serr := b.ScreenInfo(cctx); serr == nil {
				screenW, screenH = scr.Width, scr.Height
			} else {
				screenW, screenH = 1080, 2400
			}
		}
	}

	res := PurifyUITree(resp.Nodes, resp.Package, screenW, screenH, opts)
	res.ElapsedMs = time.Since(start).Milliseconds()
	if resp.RawCount > res.RawCount {
		res.RawCount = resp.RawCount
	}
	if resp.Truncated {
		res.Truncated = true
	}

	b.uiCacheMu.Lock()
	b.uiCache = make(map[int]UINode, len(res.Nodes))
	for _, n := range res.Nodes {
		b.uiCache[n.ID] = n
	}
	b.uiCachePkg = res.Package
	b.uiCacheW = res.ScreenWidth
	b.uiCacheH = res.ScreenHeight
	b.uiCacheMu.Unlock()

	return res, nil
}

func (b *ShellBackend) TapNode(ctx context.Context, id int) (*UINode, int, int, error) {
	b.uiCacheMu.RLock()
	node, ok := b.uiCache[id]
	sw, sh := b.uiCacheW, b.uiCacheH
	b.uiCacheMu.RUnlock()
	if !ok {
		return nil, 0, 0, fmt.Errorf("ui node id %d not found in cache; call phone_ui_tree first", id)
	}
	x, y := TapPointForNode(node, sw, sh)
	if err := b.Tap(ctx, x, y); err != nil {
		return &node, x, y, err
	}
	return &node, x, y, nil
}

func (b *ShellBackend) ScrollNode(ctx context.Context, id int, direction string, distance float64, durationMs int) ([4]int, [4]int, error) {
	var bounds [4]int
	b.uiCacheMu.RLock()
	sw, sh := b.uiCacheW, b.uiCacheH
	if id > 0 {
		node, ok := b.uiCache[id]
		if !ok {
			b.uiCacheMu.RUnlock()
			return bounds, [4]int{}, fmt.Errorf("ui node id %d not found in cache; call phone_ui_tree first", id)
		}
		bounds = node.Bounds
	} else {
		bestArea := 0
		for _, n := range b.uiCache {
			if !n.Scrollable {
				continue
			}
			area := (n.Bounds[2] - n.Bounds[0]) * (n.Bounds[3] - n.Bounds[1])
			if area > bestArea {
				bestArea = area
				bounds = n.Bounds
			}
		}
	}
	b.uiCacheMu.RUnlock()

	if bounds[2] <= bounds[0] || bounds[3] <= bounds[1] {
		if sw <= 0 || sh <= 0 {
			if scr, err := b.ScreenInfo(ctx); err == nil {
				sw, sh = scr.Width, scr.Height
			} else {
				sw, sh = 1080, 2400
			}
		}
		bounds = [4]int{60, 260, sw - 60, sh - 240}
	}

	x1, y1, x2, y2, err := ScrollCoordsForBounds(bounds, direction, distance)
	if err != nil {
		return bounds, [4]int{}, err
	}
	if durationMs <= 0 {
		durationMs = 340
	}
	if err := b.Swipe(ctx, x1, y1, x2, y2, durationMs); err != nil {
		return bounds, [4]int{x1, y1, x2, y2}, err
	}
	return bounds, [4]int{x1, y1, x2, y2}, nil
}

func (b *ShellBackend) ForegroundApp(ctx context.Context) (ForegroundInfo, error) {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	if out, err := exec.CommandContext(cctx, "/system/bin/dumpsys", "window", "displays").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "mCurrentFocus=") || strings.Contains(line, "mFocusedApp=") {
				if m := fgRecordRe.FindStringSubmatch(line); len(m) == 3 {
					return ForegroundInfo{Package: m[1], Activity: m[2]}, nil
				}
			}
		}
	}
	if out, err := exec.CommandContext(cctx, "/system/bin/dumpsys", "activity", "activities").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "topResumedActivity=") || strings.Contains(line, "ResumedActivity:") {
				if m := fgRecordRe.FindStringSubmatch(line); len(m) == 3 {
					return ForegroundInfo{Package: m[1], Activity: m[2]}, nil
				}
			}
		}
	}
	return ForegroundInfo{}, errors.New("could not determine foreground app")
}

func (b *ShellBackend) LaunchApp(ctx context.Context, pkg string) error {
	pkg = strings.TrimSpace(pkg)
	if !safePkgRe.MatchString(pkg) {
		return fmt.Errorf("invalid package name: %q", pkg)
	}
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	// /system/bin/monkey lacks a #! shebang on Android 16, so invoke it via /system/bin/sh.
	out, err := exec.CommandContext(cctx, "/system/bin/sh", "/system/bin/monkey", "-p", pkg, "-c", "android.intent.category.LAUNCHER", "1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("launch %s failed: %w (%s)", pkg, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *ShellBackend) OpenDeepLink(ctx context.Context, uri, pkg string) error {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "openapp.jdmobile://") && !strings.HasPrefix(uri, "https://") && !strings.HasPrefix(uri, "http://") {
		return fmt.Errorf("unsupported deep link scheme: %q", uri)
	}
	args := []string{"start", "-a", "android.intent.action.VIEW", "-d", uri}
	pkg = strings.TrimSpace(pkg)
	if pkg != "" {
		if !safePkgRe.MatchString(pkg) {
			return fmt.Errorf("invalid package name: %q", pkg)
		}
		args = append(args, pkg)
	}
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/system/bin/am", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("open deeplink failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *ShellBackend) PostNotification(ctx context.Context, tag, title, text string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "android_agent"
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Android Agent"
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("notification text is empty")
	}
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/system/bin/cmd", "notification", "post", "-S", "bigtext", "-t", title, tag, text).CombinedOutput()
	if err != nil {
		return fmt.Errorf("post notification failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *ShellBackend) UpdateLiveProgress(ctx context.Context, spec LiveProgressSpec) error {
	if b.kernel == nil {
		return b.PostNotification(ctx, spec.Tag, spec.Title, spec.Text)
	}
	focus := spec.Focus
	ongoing := spec.Ongoing
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	_, err := b.kernel.Call(cctx, kernelRequest{
		Cmd:       "notify_progress",
		Tag:       spec.Tag,
		ID:        spec.ID,
		Title:     spec.Title,
		Text:      spec.Text,
		ShortText: spec.ShortText,
		SubText:   spec.SubText,
		Step:      spec.Step,
		Max:       spec.Max,
		Focus:     &focus,
		Ongoing:   &ongoing,
	})
	if err != nil {
		fbCtx, fbCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer fbCancel()
		return b.PostNotification(fbCtx, spec.Tag, spec.Title, spec.Text)
	}
	return nil
}

func (b *ShellBackend) CancelLiveProgress(ctx context.Context, tag string, id int) error {
	if b.kernel == nil || !b.MicroKernelRunning() {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	_, err := b.kernel.Call(cctx, kernelRequest{
		Cmd: "notify_cancel",
		Tag: tag,
		ID:  id,
	})
	return err
}
