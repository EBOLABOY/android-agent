package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"android-agent/internal/backend"
	"android-agent/internal/cdp"
	"android-agent/internal/config"
	"android-agent/internal/guard"
	"android-agent/internal/imgutil"
)

type fakeBackend struct {
	mu       sync.Mutex
	lastTap  [2]int
	lastText string
	clip     string
}

func (f *fakeBackend) Name() string { return "fake-test-backend" }
func (f *fakeBackend) ScreenInfo(_ context.Context) (backend.ScreenInfo, error) {
	return backend.ScreenInfo{Width: 1080, Height: 2400, Density: 440}, nil
}
func (f *fakeBackend) Screenshot(_ context.Context, targetWidth, quality int) (*backend.ScreenshotResult, error) {
	img := image.NewRGBA(image.Rect(0, 0, 1080, 2400))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 20, G: 180, B: 90, A: 255})
		}
	}
	c, err := imgutil.CompressImage(img, targetWidth, quality)
	if err != nil {
		return nil, err
	}
	return &backend.ScreenshotResult{
		JPEG:          c.JPEG,
		Width:         c.Width,
		Height:        c.Height,
		OrigWidth:     c.OrigWidth,
		OrigHeight:    c.OrigHeight,
		CaptureMethod: "fake",
		ElapsedMs:     5,
	}, nil
}
func (f *fakeBackend) Tap(_ context.Context, x, y int) error {
	f.mu.Lock()
	f.lastTap = [2]int{x, y}
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) LongPress(_ context.Context, _, _, _ int) error    { return nil }
func (f *fakeBackend) Swipe(_ context.Context, _, _, _, _, _ int) error  { return nil }
func (f *fakeBackend) KeyEvent(_ context.Context, _ string) error        { return nil }
func (f *fakeBackend) InputText(_ context.Context, text, _ string, _ bool) (string, error) {
	f.mu.Lock()
	f.lastText = text
	f.clip = text
	f.mu.Unlock()
	return "clipboard-paste", nil
}
func (f *fakeBackend) SetClipboard(_ context.Context, text string) error {
	f.mu.Lock()
	f.clip = text
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) GetClipboard(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clip, nil
}
func (f *fakeBackend) ForegroundApp(_ context.Context) (backend.ForegroundInfo, error) {
	return backend.ForegroundInfo{Package: "com.miui.home", Activity: ".launcher.Launcher"}, nil
}
func (f *fakeBackend) LaunchApp(_ context.Context, _ string) error { return nil }

type testDetector struct {
	mu  sync.Mutex
	pkg string
}

func (d *testDetector) set(pkg string) {
	d.mu.Lock()
	d.pkg = pkg
	d.mu.Unlock()
}

func (d *testDetector) DetectGuardedForeground(_ context.Context, _ []string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pkg, nil
}

func TestMCPAuthToolsAndGameGuardListenerShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.BearerToken = "super-secret-token-123"
	cfg.PauseFlagPath = ""
	cfg.CDPSocket = "nonexistent_test_cdp_socket_777"

	det := &testDetector{}
	g := guard.New(guard.Options{
		Packages:     []string{"com.tencent.tmgp.sgame"},
		PollInterval: time.Second,
		ResumeGrace:  10 * time.Millisecond,
		Detector:     det,
	})
	fb := &fakeBackend{}
	srv := NewServer(cfg, fb, g, cdp.NewClient(cfg.CDPSocket), nil)
	g.SetListener(srv)

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	// Fix ListenAddr to actual ephemeral port so StopForGuard/StartAfterGuard re-binds cleanly
	addr := srv.Addr()
	cfg.ListenAddr = addr
	baseURL := "http://" + addr

	// 1. Unauthorized request must be rejected with 401
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("health req: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	postMCP := func(token, sid string, payload map[string]any) (*http.Response, map[string]any) {
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("mcp request failed: %v", err)
		}
		defer r.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r, out
	}

	// Wrong token -> 401
	r, _ := postMCP("wrong-token", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", r.StatusCode)
	}

	// 2. MCP Initialize + Session ID
	r, out := postMCP(cfg.BearerToken, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("initialize status=%d out=%v", r.StatusCode, out)
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("expected Mcp-Session-Id header")
	}

	// 3. tools/list
	r, out = postMCP(cfg.BearerToken, sid, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("tools/list failed: %d", r.StatusCode)
	}
	resMap := out["result"].(map[string]any)
	tools := resMap["tools"].([]any)
	if len(tools) < 12 {
		t.Fatalf("expected >=12 tools, got %d", len(tools))
	}

	// 4. Screenshot returns valid 720x1600 JPEG
	r, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params":  map[string]any{"name": "phone_screenshot", "arguments": map[string]any{}},
	})
	content := out["result"].(map[string]any)["content"].([]any)
	imgPart := content[0].(map[string]any)
	if imgPart["mimeType"] != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %v", imgPart["mimeType"])
	}
	jpgBytes, err := base64.StdEncoding.DecodeString(imgPart["data"].(string))
	if err != nil {
		t.Fatalf("decode screenshot b64: %v", err)
	}
	jcfg, err := jpeg.DecodeConfig(bytes.NewReader(jpgBytes))
	if err != nil || jcfg.Width != 720 || jcfg.Height != 1600 {
		t.Fatalf("expected 720x1600 jpeg, got %dx%d err=%v", jcfg.Width, jcfg.Height, err)
	}

	// 5. Image coordinate scaling (360, 800 in 720px image -> 540, 1200 on 1080x2400 screen)
	_, _ = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "phone_tap",
			"arguments": map[string]any{"x": 360, "y": 800, "coord_space": "image"},
		},
	})
	if fb.lastTap != [2]int{540, 1200} {
		t.Fatalf("expected scaled tap [540 1200], got %v", fb.lastTap)
	}

	// 6. Chinese input_text
	_, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "phone_input_text",
			"arguments": map[string]any{"text": "你好，小米13澎湃OS"},
		},
	})
	if fb.lastText != "你好，小米13澎湃OS" {
		t.Fatalf("expected Chinese text, got %q", fb.lastText)
	}

	// 7. CDP graceful degradation
	_, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "tools/call",
		"params":  map[string]any{"name": "cdp_status"},
	})
	txt := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, `"available":false`) {
		t.Fatalf("expected graceful unavailable cdp_status, got %s", txt)
	}

	// 8. Manual pause blocks phone_tap, resume restores it
	_, _ = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params":  map[string]any{"name": "agent_pause", "arguments": map[string]any{"reason": "test"}},
	})
	_, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "tools/call",
		"params":  map[string]any{"name": "phone_tap", "arguments": map[string]any{"x": 10, "y": 10}},
	})
	if out["result"].(map[string]any)["isError"] != true {
		t.Fatalf("expected isError=true when manually paused, got %v", out)
	}
	_, _ = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "tools/call",
		"params":  map[string]any{"name": "agent_resume"},
	})

	// 9. Game-guard entering com.tencent.tmgp.sgame MUST close the TCP listener completely
	det.set("com.tencent.tmgp.sgame")
	g.CheckOnce(context.Background())
	if srv.IsListening() {
		t.Fatal("expected server listener to be closed while sgame is active")
	}
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("expected TCP connection refused while sgame is active")
	}

	// Leaving sgame re-binds the TCP listener cleanly
	det.set("")
	g.CheckOnce(context.Background())
	time.Sleep(15 * time.Millisecond)
	g.CheckOnce(context.Background())
	if !srv.IsListening() {
		t.Fatal("expected server listener to restart after sgame exited foreground")
	}
	r, _ = postMCP(cfg.BearerToken, sid, map[string]any{"jsonrpc": "2.0", "id": 10, "method": "ping"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected ping 200 after resume, got %d", r.StatusCode)
	}
}
