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

	screenMu     sync.RWMutex
	cachedScreen ScreenInfo
}

func NewShellBackend(clipHelperPath string, shellTimeout time.Duration) *ShellBackend {
	if clipHelperPath == "" {
		clipHelperPath = "/data/local/tmp/android_agent_cliphelper.jar"
	}
	if shellTimeout <= 0 {
		shellTimeout = 10 * time.Second
	}
	return &ShellBackend{
		clipHelperPath: clipHelperPath,
		shellTimeout:   shellTimeout,
	}
}

func (b *ShellBackend) Name() string {
	return "shell-fallback-phase1"
}

func (b *ShellBackend) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.shellTimeout)
}

func (b *ShellBackend) ScreenInfo(ctx context.Context) (ScreenInfo, error) {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()

	var info ScreenInfo
	if out, err := exec.CommandContext(cctx, "/system/bin/cmd", "window", "size").Output(); err == nil {
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
	if out, err := exec.CommandContext(cctx, "/system/bin/cmd", "window", "density").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if m := wmDensityRe.FindStringSubmatch(lines[i]); len(m) == 2 {
				info.Density, _ = strconv.Atoi(m[1])
				break
			}
		}
	}
	if info.Width > 0 && info.Height > 0 {
		b.screenMu.Lock()
		b.cachedScreen = info
		b.screenMu.Unlock()
		return info, nil
	}
	b.screenMu.RLock()
	cached := b.cachedScreen
	b.screenMu.RUnlock()
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

func (b *ShellBackend) Tap(ctx context.Context, x, y int) error {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/system/bin/cmd", "input", "tap", strconv.Itoa(x), strconv.Itoa(y)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("input tap failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *ShellBackend) LongPress(ctx context.Context, x, y, durationMs int) error {
	if durationMs <= 0 {
		durationMs = 800
	}
	return b.Swipe(ctx, x, y, x, y, durationMs)
}

func (b *ShellBackend) Swipe(ctx context.Context, x1, y1, x2, y2, durationMs int) error {
	cctx, cancel := b.withTimeout(ctx)
	defer cancel()
	if durationMs <= 0 {
		durationMs = 300
	}
	out, err := exec.CommandContext(cctx, "/system/bin/cmd", "input", "swipe",
		strconv.Itoa(x1), strconv.Itoa(y1),
		strconv.Itoa(x2), strconv.Itoa(y2),
		strconv.Itoa(durationMs),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("input swipe failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
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

func (b *ShellBackend) ensureClipHelper() (string, error) {
	b.clipMu.Lock()
	defer b.clipMu.Unlock()

	if b.clipHelperReady {
		if st, err := os.Stat(b.clipHelperPath); err == nil && st.Size() == int64(len(embeddedClipHelperJar)) {
			return b.clipHelperPath, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(b.clipHelperPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir cliphelper dir: %w", err)
	}
	tmpPath := b.clipHelperPath + ".tmp"
	if err := os.WriteFile(tmpPath, embeddedClipHelperJar, 0o644); err != nil {
		return "", fmt.Errorf("write cliphelper.jar: %w", err)
	}
	_ = os.Chmod(tmpPath, 0o644)
	_ = os.Chown(tmpPath, 2000, 2000)
	if err := os.Rename(tmpPath, b.clipHelperPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("install cliphelper.jar: %w", err)
	}
	b.clipHelperReady = true
	return b.clipHelperPath, nil
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
	_, err := b.runClipHelper(ctx, "set", text)
	return err
}

func (b *ShellBackend) GetClipboard(ctx context.Context) (string, error) {
	return b.runClipHelper(ctx, "get", "")
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
