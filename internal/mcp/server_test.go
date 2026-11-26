package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"android-agent/internal/backend"
	"android-agent/internal/brain"
	"android-agent/internal/cdp"
	"android-agent/internal/config"
	"android-agent/internal/guard"
	"android-agent/internal/imgutil"
)

type fakeBackend struct {
	mu               sync.Mutex
	lastTap          [2]int
	lastSwipe        [4]int
	lastText         string
	clip             string
	microKernelStops int
	liveUpdates      []backend.LiveProgressSpec
	liveCancels      []string
	eventLog         []string
	cachedNodes      map[int]backend.UINode
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
func (f *fakeBackend) LaunchApp(_ context.Context, _ string) error                   { return nil }
func (f *fakeBackend) OpenDeepLink(_ context.Context, _, _ string) error        { return nil }
func (f *fakeBackend) PostNotification(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeBackend) UpdateLiveProgress(_ context.Context, spec backend.LiveProgressSpec) error {
	f.mu.Lock()
	f.liveUpdates = append(f.liveUpdates, spec)
	f.eventLog = append(f.eventLog, "update_live")
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) CancelLiveProgress(_ context.Context, tag string, id int) error {
	f.mu.Lock()
	f.liveCancels = append(f.liveCancels, fmt.Sprintf("%s#%d", tag, id))
	f.eventLog = append(f.eventLog, "cancel_live")
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) UITree(_ context.Context, opts backend.UITreeOptions) (*backend.UITreeResult, error) {
	raw := []backend.RawUINode{
		{Cls: "android.widget.EditText", Text: "搜索商家、商品", Bounds: [4]int{60, 120, 960, 220}, Flags: "ce", Depth: 3},
		{Cls: "android.widget.Button", Text: "外卖", Bounds: [4]int{80, 300, 260, 420}, Flags: "c", Depth: 3},
		{Cls: "androidx.recyclerview.widget.RecyclerView", Bounds: [4]int{0, 450, 1080, 2200}, Flags: "s", Depth: 2},
	}
	res := backend.PurifyUITree(raw, "com.sankuai.meituan", 1080, 2400, opts)
	f.mu.Lock()
	f.cachedNodes = make(map[int]backend.UINode, len(res.Nodes))
	for _, n := range res.Nodes {
		f.cachedNodes[n.ID] = n
	}
	f.mu.Unlock()
	return res, nil
}
func (f *fakeBackend) TapNode(ctx context.Context, id int) (*backend.UINode, int, int, error) {
	f.mu.Lock()
	n, ok := f.cachedNodes[id]
	f.mu.Unlock()
	if !ok {
		return nil, 0, 0, context.DeadlineExceeded
	}
	x, y := backend.TapPointForNode(n, 1080, 2400)
	_ = f.Tap(ctx, x, y)
	return &n, x, y, nil
}
func (f *fakeBackend) ScrollNode(_ context.Context, id int, direction string, distance float64, _ int) ([4]int, [4]int, error) {
	f.mu.Lock()
	n := f.cachedNodes[id]
	f.mu.Unlock()
	if n.Bounds[2] <= n.Bounds[0] {
		n.Bounds = [4]int{0, 450, 1080, 2200}
	}
	x1, y1, x2, y2, err := backend.ScrollCoordsForBounds(n.Bounds, direction, distance)
	f.mu.Lock()
	f.lastSwipe = [4]int{x1, y1, x2, y2}
	f.mu.Unlock()
	return n.Bounds, [4]int{x1, y1, x2, y2}, err
}
func (f *fakeBackend) StopMicroKernel(_ string) {
	f.mu.Lock()
	f.microKernelStops++
	f.eventLog = append(f.eventLog, "stop_kernel")
	f.mu.Unlock()
}
func (f *fakeBackend) MicroKernelRunning() bool { return false }
func (f *fakeBackend) TouchStatus() backend.TouchStatusInfo {
	return backend.TouchStatusInfo{LastMethod: "fake-touch"}
}

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
	cfg.MemoryDir = filepath.Join(t.TempDir(), "memory")
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

	// 8b. phone_ui_tree + phone_tap_node + phone_scroll_node
	_, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      81,
		"method":  "tools/call",
		"params":  map[string]any{"name": "phone_ui_tree"},
	})
	uiTxt := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(uiTxt, "[1] Input") || !strings.Contains(uiTxt, "搜索商家、商品") {
		t.Fatalf("unexpected phone_ui_tree output: %s", uiTxt)
	}
	_, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      82,
		"method":  "tools/call",
		"params":  map[string]any{"name": "phone_tap_node", "arguments": map[string]any{"id": 2}},
	})
	if out["result"].(map[string]any)["isError"] == true {
		t.Fatalf("phone_tap_node failed: %v", out)
	}
	if fb.lastTap[0] < 80 || fb.lastTap[0] > 260 || fb.lastTap[1] < 300 || fb.lastTap[1] > 420 {
		t.Fatalf("tap_node outside node bounds: %v", fb.lastTap)
	}
	_, out = postMCP(cfg.BearerToken, sid, map[string]any{
		"jsonrpc": "2.0",
		"id":      83,
		"method":  "tools/call",
		"params":  map[string]any{"name": "phone_scroll_node", "arguments": map[string]any{"id": 3, "direction": "down"}},
	})
	if out["result"].(map[string]any)["isError"] == true || fb.lastSwipe[1] <= fb.lastSwipe[3] {
		t.Fatalf("phone_scroll_node invalid swipe: %v out=%v", fb.lastSwipe, out)
	}

	// 9. Game-guard entering com.tencent.tmgp.sgame MUST close the TCP listener and stop micro-kernel
	det.set("com.tencent.tmgp.sgame")
	g.CheckOnce(context.Background())
	if fb.microKernelStops == 0 {
		t.Fatal("expected StopMicroKernel to be called when sgame enters foreground")
	}
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

func TestWebUIAuthAndLLMConfigPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	tmpCfgPath := filepath.Join(tmpDir, "config.json")
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.BearerToken = "tok-ui-test-123456"
	cfg.PauseFlagPath = ""
	cfg.MemoryDir = filepath.Join(tmpDir, "memory")
	cfg.CDPSocket = "nonexistent_test_cdp_socket_888"

	det := &testDetector{}
	g := guard.New(guard.Options{Packages: []string{"com.tencent.tmgp.sgame"}, PollInterval: time.Second, ResumeGrace: 10 * time.Millisecond, Detector: det})
	srv := NewServer(cfg, &fakeBackend{}, g, cdp.NewClient(cfg.CDPSocket), nil)
	srv.SetConfigPath(tmpCfgPath)
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()
	baseURL := "http://" + srv.Addr()

	// 1. GET /ui returns HTML shell without exposing token or API keys
	rUI, err := http.Get(baseURL + "/ui")
	if err != nil {
		t.Fatalf("GET /ui: %v", err)
	}
	uiBytes, _ := io.ReadAll(rUI.Body)
	rUI.Body.Close()
	if rUI.StatusCode != http.StatusOK || !strings.Contains(string(uiBytes), "Xiaomi 13 Agent") {
		t.Fatalf("unexpected /ui status=%d", rUI.StatusCode)
	}
	if strings.Contains(string(uiBytes), cfg.BearerToken) {
		t.Fatal("/ui must never embed bearer token")
	}

	// 2. Protected v0.2 endpoints must reject unauthenticated requests
	for _, path := range []string{"/api/v1/llm/config", "/api/v1/jobs/current"} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 on %s, got %d", path, resp.StatusCode)
		}
	}

	// 3. Save LLM config via authenticated POST and verify masking + persistence
	payload, _ := json.Marshal(map[string]any{
		"provider": "openai",
		"base_url": "https://api.deepseek.com/v1",
		"api_key":  "sk-abcdef1234567890wxyz",
		"model":    "deepseek-chat",
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/llm/config", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	req.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/llm/config: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on POST /api/v1/llm/config, got %d", postResp.StatusCode)
	}

	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/llm/config", nil)
	getReq.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET /api/v1/llm/config: %v", err)
	}
	defer getResp.Body.Close()
	var llmOut map[string]any
	_ = json.NewDecoder(getResp.Body).Decode(&llmOut)
	if llmOut["api_key_masked"] != "sk-a...wxyz" || llmOut["configured"] != true {
		t.Fatalf("unexpected llm config response: %+v", llmOut)
	}
	if _, err := config.Load(tmpCfgPath, false); err != nil {
		t.Fatalf("persisted config load failed: %v", err)
	}

	// 4. Verify /api/v1/memory GET + POST
	memReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/memory", nil)
	memReq.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	memResp, err := http.DefaultClient.Do(memReq)
	if err != nil || memResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/memory failed: status=%v err=%v", memResp, err)
	}
	memResp.Body.Close()

	memBody, _ := json.Marshal(map[string]any{"name": "MEMORY.md", "content": "# User\n- 喜欢少冰\n"})
	memPost, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/memory", bytes.NewReader(memBody))
	memPost.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	memPost.Header.Set("Content-Type", "application/json")
	memPostResp, err := http.DefaultClient.Do(memPost)
	if err != nil || memPostResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/memory failed: %v", err)
	}
	memPostResp.Body.Close()

	// 5. Verify /api/v1/schedule POST + GET persistence
	schedBody, _ := json.Marshal(map[string]any{
		"enabled": true,
		"entries": []map[string]any{{
			"id":            "daily-all",
			"job":           "daily_all",
			"enabled":       true,
			"window_start":  "09:00",
			"window_end":    "10:00",
			"defer_on_game": true,
		}},
	})
	schedReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/schedule", bytes.NewReader(schedBody))
	schedReq.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	schedReq.Header.Set("Content-Type", "application/json")
	schedResp, err := http.DefaultClient.Do(schedReq)
	if err != nil || schedResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/schedule failed: %v", err)
	}
	schedResp.Body.Close()

	reloaded, err := config.Load(tmpCfgPath, false)
	if err != nil || !reloaded.Schedule.Enabled || len(reloaded.Schedule.Entries) != 1 {
		t.Fatalf("expected persisted schedule config, got %+v err=%v", reloaded, err)
	}
}

func TestSecurityHardeningOriginHostCSPSoulAndGuardChat(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.BearerToken = "tok-sec-test-123456"
	cfg.PauseFlagPath = ""
	cfg.MemoryDir = filepath.Join(tmpDir, "memory")
	cfg.CDPSocket = "nonexistent_test_cdp_socket_999"

	startedLLM := make(chan struct{})
	releaseLLM := make(chan struct{})
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-startedLLM:
		default:
			close(startedLLM)
		}
		select {
		case <-r.Context().Done():
		case <-releaseLLM:
		}
	}))
	defer llmSrv.Close()
	defer close(releaseLLM)
	cfg.LLM = config.LLMConfig{
		Provider:   "openai",
		BaseURL:    llmSrv.URL,
		APIKey:     "sk-test",
		Model:      "test-model",
		TimeoutSec: 5,
	}

	det := &testDetector{}
	g := guard.New(guard.Options{Packages: []string{"com.tencent.tmgp.sgame"}, PollInterval: time.Second, Detector: det})
	srv := NewServer(cfg, &fakeBackend{}, g, cdp.NewClient(cfg.CDPSocket), nil)
	g.SetListener(srv)
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()
	baseURL := "http://" + srv.Addr()

	// 1. /ui security headers (CSP, XFO, nosniff)
	rUI, err := http.Get(baseURL + "/ui")
	if err != nil {
		t.Fatalf("GET /ui: %v", err)
	}
	rUI.Body.Close()
	if rUI.Header.Get("X-Frame-Options") != "DENY" ||
		rUI.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(rUI.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("missing security headers on /ui: %+v", rUI.Header)
	}

	// 2. DNS Rebinding Host rejection -> 403
	reqHost, _ := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	reqHost.Host = "evil.attacker.com:8765"
	reqHost.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	respHost, err := http.DefaultClient.Do(reqHost)
	if err != nil {
		t.Fatalf("Host request err: %v", err)
	}
	respHost.Body.Close()
	if respHost.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for foreign Host header, got %d", respHost.StatusCode)
	}

	// 3. Cross-origin Origin rejection -> 403
	reqOrigin, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/status", nil)
	reqOrigin.Header.Set("Origin", "https://evil.example")
	reqOrigin.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	respOrigin, err := http.DefaultClient.Do(reqOrigin)
	if err != nil {
		t.Fatalf("Origin request err: %v", err)
	}
	respOrigin.Body.Close()
	if respOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin Origin header, got %d", respOrigin.StatusCode)
	}

	// 4. Remote non-loopback POST to SOUL.md must be rejected -> 403
	soulBody, _ := json.Marshal(map[string]any{"name": "SOUL.md", "content": "# hacked"})
	reqSoul := httptest.NewRequest(http.MethodPost, "/api/v1/memory", bytes.NewReader(soulBody))
	reqSoul.RemoteAddr = "192.168.1.88:44444"
	reqSoul.Host = "127.0.0.1:8765"
	reqSoul.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	reqSoul.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, reqSoul)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback SOUL.md modification, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 5. Concurrent chat returns 409 Conflict, and game-guard StopForGuard cancels in-flight chat
	chatPayload := []byte(`{"message":"正在执行多步操作"}`)
	firstDone := make(chan int, 1)
	go func() {
		req1, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/chat", bytes.NewReader(chatPayload))
		req1.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
		req1.Header.Set("Content-Type", "application/json")
		resp1, err := http.DefaultClient.Do(req1)
		if err != nil {
			firstDone <- -1
			return
		}
		resp1.Body.Close()
		firstDone <- resp1.StatusCode
	}()

	<-startedLLM
	req2, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/chat", bytes.NewReader(chatPayload))
	req2.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("concurrent chat req failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on concurrent /api/v1/chat, got %d", resp2.StatusCode)
	}

	// Trigger game-guard while first chat is waiting on LLM -> must abort promptly!
	det.set("com.tencent.tmgp.sgame")
	g.CheckOnce(context.Background())

	select {
	case code := <-firstDone:
		if code == http.StatusOK {
			t.Fatalf("expected in-flight chat to abort on StopForGuard, got 200")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("in-flight chat was not cancelled immediately by StopForGuard")
	}
}


func (f *fakeBackend) snapshotEvents() ([]backend.LiveProgressSpec, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ups := make([]backend.LiveProgressSpec, len(f.liveUpdates))
	copy(ups, f.liveUpdates)
	ev := make([]string, len(f.eventLog))
	copy(ev, f.eventLog)
	return ups, ev
}

func TestStreamingChatReplayCancelAndGuardIslandClear(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.BearerToken = "tok-stream-test-987654"
	cfg.PauseFlagPath = ""
	cfg.MemoryDir = filepath.Join(tmpDir, "memory")
	cfg.CDPSocket = "nonexistent_test_cdp_socket_999"

	var streamRound sync.Mutex
	roundCount := 0
	holdStarted := make(chan struct{}, 4)
	releaseHold := make(chan struct{})

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		allText := ""
		hasToolResult := false
		for _, m := range body.Messages {
			if role, _ := m["role"].(string); role == "tool" {
				hasToolResult = true
			}
			if s, ok := m["content"].(string); ok {
				allText += s + "\n"
			}
		}

		if strings.Contains(allText, "阻塞直到取消") || strings.Contains(allText, "异步阻塞") {
			select {
			case holdStarted <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-releaseHold:
			}
			return
		}

		streamRound.Lock()
		roundCount++
		n := roundCount
		streamRound.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 && !hasToolResult {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_ui\",\"type\":\"function\",\"function\":{\"name\":\"phone_ui_tree\",\"arguments\":\"{}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"流式\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"完成\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer llmSrv.Close()
	defer close(releaseHold)

	cfg.LLM = config.LLMConfig{
		Provider:   "openai",
		BaseURL:    llmSrv.URL,
		APIKey:     "sk-stream",
		Model:      "stream-model",
		TimeoutSec: 5,
		MaxSteps:   4,
	}

	fb := &fakeBackend{}
	det := &testDetector{}
	g := guard.New(guard.Options{Packages: []string{"com.tencent.tmgp.sgame"}, PollInterval: time.Second, Detector: det})
	srv := NewServer(cfg, fb, g, cdp.NewClient(cfg.CDPSocket), nil)
	g.SetListener(srv)
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()
	baseURL := "http://" + srv.Addr()

	// 1. POST /api/v1/chat/stream (SSE)
	reqSSE, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/chat/stream", bytes.NewReader([]byte(`{"message":"流式检查当前界面"}`)))
	reqSSE.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	reqSSE.Header.Set("Content-Type", "application/json")
	respSSE, err := http.DefaultClient.Do(reqSSE)
	if err != nil {
		t.Fatalf("POST /api/v1/chat/stream err: %v", err)
	}
	if respSSE.StatusCode != http.StatusOK || !strings.Contains(respSSE.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected SSE headers/status: %d %v", respSSE.StatusCode, respSSE.Header)
	}
	var events []brain.RunEvent
	scanner := bufio.NewScanner(respSSE.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var ev brain.RunEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				events = append(events, ev)
			}
		}
	}
	respSSE.Body.Close()
	if err := scanner.Err(); err != nil {
		t.Fatalf("SSE scanner err: %v", err)
	}
	if len(events) < 4 {
		t.Fatalf("expected >=4 SSE events, got %+v", events)
	}
	runID := events[0].RunID
	lastEv := events[len(events)-1]
	if runID == "" || lastEv.Type != "done" || lastEv.Reply != "流式完成" {
		t.Fatalf("unexpected SSE terminal event: %+v", lastEv)
	}

	// 2. GET /api/v1/chat/events?run_id=...&after_seq=1 (Run Replay)
	reqRep, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/chat/events?run_id=%s&after_seq=1", baseURL, runID), nil)
	reqRep.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	respRep, err := http.DefaultClient.Do(reqRep)
	if err != nil {
		t.Fatalf("GET replay err: %v", err)
	}
	var snap brain.RunReplaySnapshot
	_ = json.NewDecoder(respRep.Body).Decode(&snap)
	respRep.Body.Close()
	if snap.RunID != runID || !snap.Finished || snap.Active || len(snap.Events) == 0 {
		t.Fatalf("unexpected replay snapshot: %+v", snap)
	}
	for _, ev := range snap.Events {
		if ev.Seq <= 1 {
			t.Fatalf("expected after_seq=1 filter to omit seq <= 1, got seq=%d", ev.Seq)
		}
	}

	// 3. Async mode (202 Accepted) + POST /api/v1/chat/cancel
	reqAsync, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/chat/stream", bytes.NewReader([]byte(`{"message":"异步阻塞测试","async":true}`)))
	reqAsync.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	reqAsync.Header.Set("Content-Type", "application/json")
	respAsync, err := http.DefaultClient.Do(reqAsync)
	if err != nil {
		t.Fatalf("async stream err: %v", err)
	}
	var asyncResp struct {
		OK    bool   `json:"ok"`
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(respAsync.Body).Decode(&asyncResp)
	respAsync.Body.Close()
	if respAsync.StatusCode != http.StatusAccepted || !asyncResp.OK || asyncResp.RunID == "" {
		t.Fatalf("expected 202 run_id, got %d %+v", respAsync.StatusCode, asyncResp)
	}
	<-holdStarted

	reqCancel, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/chat/cancel", bytes.NewReader([]byte(`{}`)))
	reqCancel.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	reqCancel.Header.Set("Content-Type", "application/json")
	respCancel, err := http.DefaultClient.Do(reqCancel)
	if err != nil {
		t.Fatalf("POST cancel err: %v", err)
	}
	var cancelBody struct {
		OK        bool `json:"ok"`
		Cancelled bool `json:"cancelled"`
	}
	_ = json.NewDecoder(respCancel.Body).Decode(&cancelBody)
	respCancel.Body.Close()
	if respCancel.StatusCode != http.StatusOK || !cancelBody.OK || !cancelBody.Cancelled {
		t.Fatalf("unexpected cancel response: %d %+v", respCancel.StatusCode, cancelBody)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reqCheck, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/chat/events?run_id=%s&after_seq=0", baseURL, asyncResp.RunID), nil)
		reqCheck.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
		rCheck, err := http.DefaultClient.Do(reqCheck)
		if err == nil {
			var s2 brain.RunReplaySnapshot
			_ = json.NewDecoder(rCheck.Body).Decode(&s2)
			rCheck.Body.Close()
			if s2.Finished {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 4. Active SSE run + StopForGuard must clear Super Island BEFORE stopping microkernel
	sseCancelTestDone := make(chan int, 1)
	go func() {
		reqGuard, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/chat/stream", bytes.NewReader([]byte(`{"message":"阻塞直到取消"}`)))
		reqGuard.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
		reqGuard.Header.Set("Content-Type", "application/json")
		respGuard, err := http.DefaultClient.Do(reqGuard)
		if err != nil {
			sseCancelTestDone <- -1
			return
		}
		io.Copy(io.Discard, respGuard.Body)
		respGuard.Body.Close()
		sseCancelTestDone <- respGuard.StatusCode
	}()

	<-holdStarted
	// Wait briefly until run_start triggers UpdateLiveProgress
	waitLive := time.Now().Add(1 * time.Second)
	for time.Now().Before(waitLive) {
		ups, _ := fb.snapshotEvents()
		if len(ups) >= 3 { // run1 + run2 + run3
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	det.set("com.tencent.tmgp.sgame")
	g.CheckOnce(context.Background())

	select {
	case <-sseCancelTestDone:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("active streaming run was not aborted by StopForGuard")
	}

	_, evLog := fb.snapshotEvents()
	cancelIdx, stopIdx := -1, -1
	for i := len(evLog) - 1; i >= 0; i-- {
		if stopIdx == -1 && evLog[i] == "stop_kernel" {
			stopIdx = i
		}
		if cancelIdx == -1 && evLog[i] == "cancel_live" {
			cancelIdx = i
		}
		if stopIdx != -1 && cancelIdx != -1 {
			break
		}
	}
	if cancelIdx == -1 || stopIdx == -1 || cancelIdx > stopIdx {
		t.Fatalf("expected CancelLiveProgress before StopMicroKernel in StopForGuard, got eventLog=%v", evLog)
	}
}
