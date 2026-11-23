package backend

import "context"

type ScreenshotResult struct {
	JPEG          []byte
	Width         int
	Height        int
	OrigWidth     int
	OrigHeight    int
	CaptureMethod string
	ElapsedMs     int64
}

type ForegroundInfo struct {
	Package  string `json:"package"`
	Activity string `json:"activity"`
}

type ScreenInfo struct {
	Width   int `json:"width"`
	Height  int `json:"height"`
	Density int `json:"density"`
}

// DeviceBackend abstracts low-level Android operations so Phase 1 (Root Shell + app_process64)
// can be seamlessly replaced by Phase 2 (LSPosed @agent_sys in-memory daemon) without changing MCP.
type DeviceBackend interface {
	Name() string
	ScreenInfo(ctx context.Context) (ScreenInfo, error)
	Screenshot(ctx context.Context, targetWidth, quality int) (*ScreenshotResult, error)
	Tap(ctx context.Context, x, y int) error
	LongPress(ctx context.Context, x, y, durationMs int) error
	Swipe(ctx context.Context, x1, y1, x2, y2, durationMs int) error
	KeyEvent(ctx context.Context, keycode string) error
	InputText(ctx context.Context, text string, method string, submit bool) (string, error)
	SetClipboard(ctx context.Context, text string) error
	GetClipboard(ctx context.Context) (string, error)
	ForegroundApp(ctx context.Context) (ForegroundInfo, error)
	LaunchApp(ctx context.Context, pkg string) error
}
