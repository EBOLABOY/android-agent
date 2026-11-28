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
	lastKey          string
	lastDeepLink     [2]string
	clip             string
	shotOrigW        int
	shotOrigH        int
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
	f.mu.Lock()
	origW, origH := f.shotOrigW, f.shotOrigH
	f.mu.Unlock()
	if origW <= 0 || origH <= 0 {
		origW, origH = 1080, 2400
	}
	img := image.NewRGBA(image.Rect(0, 0, origW, origH))
	for y := 0; y < 100 && y < origH; y++ {
		for x := 0; x < 100 && x < origW; x++ {
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
func (f *fakeBackend) LongPress(_ context.Context, x, y, _ int) error {
	f.mu.Lock()
	f.lastTap = [2]int{x, y}
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Swipe(_ context.Context, x1, y1, x2, y2, _ int) error {
	f.mu.Lock()
	f.lastSwipe = [4]int{x1, y1, x2, y2}
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) KeyEvent(_ context.Context, kc string) error {
	f.mu.Lock()
	f.lastKey = kc
	f.mu.Unlock()
	return nil
}
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
func (f *fakeBackend) OpenDeepLink(_ context.Context, uri, pkg string) error {
	f.mu.Lock()
	f.lastDeepLink = [2]string{uri, pkg}
	f.mu.Unlock()
	return nil
}
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

	// 1. Unauthorized request must be rejected with 401 and non-identifying WWW-Authenticate header
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("health req: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	if wa := resp.Header.Get("WWW-Authenticate"); wa != "Bearer" || strings.Contains(wa, "android-agent") || strings.Contains(wa, "realm=") {
		t.Fatalf("unexpected WWW-Authenticate on missing token: %q", wa)
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

	// Wrong token -> 401 without identifying realm
	r, _ := postMCP("wrong-token", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", r.StatusCode)
	}
	if wa := r.Header.Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Bearer") || strings.Contains(wa, "android-agent") || strings.Contains(wa, "realm=") {
		t.Fatalf("unexpected WWW-Authenticate on invalid token: %q", wa)
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

func TestV034LandscapeCoordsStrictOriginHostScheduleToolSpecsAndSessions(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.BearerToken = "tok-v034-initial-secret"
	cfg.PauseFlagPath = ""
	cfg.MemoryDir = filepath.Join(tmpDir, "memory")
	cfg.CDPSocket = "nonexistent_test_cdp_socket_034"

	fb := &fakeBackend{shotOrigW: 2400, shotOrigH: 1080}
	det := &testDetector{}
	g := guard.New(guard.Options{Packages: []string{"com.tencent.tmgp.sgame"}, PollInterval: time.Second, Detector: det})
	srv := NewServer(cfg, fb, g, cdp.NewClient(cfg.CDPSocket), nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()
	baseURL := "http://" + srv.Addr()

	// 1. ToolSpecs() hides agent_pause and agent_resume from in-process LLM brain
	specs := srv.ToolSpecs()
	defs := toolDefinitions()
	if len(defs) != 28 {
		t.Fatalf("expected 28 total MCP tools, got %d", len(defs))
	}
	if len(specs) != len(defs)-2 {
		t.Fatalf("expected ToolSpecs() to hide 2 pause/resume tools (%d), got %d", len(defs)-2, len(specs))
	}
	for _, sp := range specs {
		if sp.Name == "agent_pause" || sp.Name == "agent_resume" {
			t.Fatalf("ToolSpecs() must hide %s from LLM brain", sp.Name)
		}
	}
	if out := srv.ExecuteTool(context.Background(), "agent_pause", map[string]any{"reason": "llm"}); !out.IsError {
		t.Fatalf("expected ExecuteTool(agent_pause) to fail for LLM brain, got %+v", out)
	}

	// 2. Landscape resolveCoords uses cached screenshot (2400x1080 -> 720x324) with separate scaleX/scaleY
	shotRes := srv.callTool(context.Background(), "phone_screenshot", map[string]any{"width": 720})
	if shotRes["isError"] == true {
		t.Fatalf("landscape phone_screenshot failed: %v", shotRes)
	}
	tapRes := srv.callTool(context.Background(), "phone_tap", map[string]any{"x": 360, "y": 162, "coord_space": "image"})
	if tapRes["isError"] == true {
		t.Fatalf("landscape phone_tap failed: %v", tapRes)
	}
	if fb.lastTap != [2]int{1200, 540} {
		t.Fatalf("expected landscape tap [1200 540], got %v", fb.lastTap)
	}
	swipeRes := srv.callTool(context.Background(), "phone_swipe", map[string]any{
		"x1": 180, "y1": 162, "x2": 540, "y2": 162, "coord_space": "image",
	})
	if swipeRes["isError"] == true {
		t.Fatalf("landscape phone_swipe failed: %v", swipeRes)
	}
	if fb.lastSwipe != [4]int{600, 540, 1800, 540} {
		t.Fatalf("expected landscape swipe [600 540 1800 540], got %v", fb.lastSwipe)
	}

	// 3. Strict Origin & Host validation
	// 3a. Empty Host -> 403
	reqEmptyHost := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	reqEmptyHost.Host = ""
	reqEmptyHost.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, reqEmptyHost)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for empty Host header, got %d", rec.Code)
	}

	// 3b. Arbitrary non-bound IP Host -> 403
	reqForeignIP := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	reqForeignIP.Host = "192.168.99.99:8765"
	reqForeignIP.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	rec = httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, reqForeignIP)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-bound IP Host header, got %d", rec.Code)
	}

	// 3c. Helper allows exact bound host
	if !isAllowedHostHeader("192.168.100.100:8765", "192.168.100.100") {
		t.Fatal("expected bound host 192.168.100.100:8765 to be allowed")
	}
	if !isAllowedHostHeader("[::1]:8765", "127.0.0.1") {
		t.Fatal("expected loopback IPv6 [::1]:8765 to be allowed")
	}

	// 3d. Arbitrary loopback port in Origin MUST be rejected (403)
	for _, badOrigin := range []string{
		"http://127.0.0.1:3000",
		"http://localhost:3000",
		"http://localhost:" + strings.Split(srv.Addr(), ":")[1],
		"https://" + srv.Addr(),
		"http://" + srv.Addr() + "/extra",
	} {
		reqBadOri, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/status", nil)
		reqBadOri.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
		reqBadOri.Header.Set("Origin", badOrigin)
		respBadOri, err := http.DefaultClient.Do(reqBadOri)
		if err != nil {
			t.Fatalf("request with Origin %q failed: %v", badOrigin, err)
		}
		respBadOri.Body.Close()
		if respBadOri.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for Origin %q against Host %q, got %d", badOrigin, srv.Addr(), respBadOri.StatusCode)
		}
	}

	// 3e. Exact scheme://host:port Origin MUST succeed (200)
	reqGoodOri, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/status", nil)
	reqGoodOri.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	reqGoodOri.Header.Set("Origin", baseURL)
	respGoodOri, err := http.DefaultClient.Do(reqGoodOri)
	if err != nil {
		t.Fatalf("exact Origin request failed: %v", err)
	}
	respGoodOri.Body.Close()
	if respGoodOri.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for exact matching Origin %q, got %d", baseURL, respGoodOri.StatusCode)
	}

	// 3f. Dynamic SHA-256 Bearer token comparison under cfgMu
	srv.cfgMu.Lock()
	srv.cfg.BearerToken = "tok-v034-rotated-secret"
	srv.cfgMu.Unlock()

	reqOldTok, _ := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	reqOldTok.Header.Set("Authorization", "Bearer tok-v034-initial-secret")
	respOldTok, err := http.DefaultClient.Do(reqOldTok)
	if err != nil {
		t.Fatalf("old token req err: %v", err)
	}
	respOldTok.Body.Close()
	if respOldTok.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for rotated-out token, got %d", respOldTok.StatusCode)
	}

	reqNewTok, _ := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	reqNewTok.Header.Set("Authorization", "Bearer tok-v034-rotated-secret")
	respNewTok, err := http.DefaultClient.Do(reqNewTok)
	if err != nil {
		t.Fatalf("new token req err: %v", err)
	}
	respNewTok.Body.Close()
	if respNewTok.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for rotated-in token, got %d", respNewTok.StatusCode)
	}

	// 4. POST /api/v1/schedule strict HH:MM validation
	invalidSchedules := []map[string]any{
		{"enabled": true, "entries": []map[string]any{{"job": "daily_all", "window_start": "25:00", "window_end": "23:59"}}},
		{"enabled": true, "entries": []map[string]any{{"job": "daily_all", "window_start": "8:15", "window_end": "09:30"}}},
		{"enabled": true, "entries": []map[string]any{{"job": "daily_all", "window_start": "08:15", "window_end": "24:00"}}},
		{"enabled": true, "entries": []map[string]any{{"job": "daily_all", "window_start": "09:30", "window_end": "09:30"}}},
		{"enabled": true, "entries": []map[string]any{{"job": "daily_all", "window_start": "10:00", "window_end": "09:00"}}},
	}
	for i, sched := range invalidSchedules {
		b, _ := json.Marshal(sched)
		reqS, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/schedule", bytes.NewReader(b))
		reqS.Header.Set("Authorization", "Bearer tok-v034-rotated-secret")
		reqS.Header.Set("Content-Type", "application/json")
		respS, err := http.DefaultClient.Do(reqS)
		if err != nil {
			t.Fatalf("schedule case %d err: %v", i, err)
		}
		respS.Body.Close()
		if respS.StatusCode != http.StatusBadRequest {
			t.Fatalf("schedule case %d expected 400, got %d", i, respS.StatusCode)
		}
	}
	validSched, _ := json.Marshal(map[string]any{
		"enabled": true,
		"entries": []map[string]any{
			{"id": "morning", "job": "daily_all", "enabled": true, "window_start": "00:00", "window_end": "23:59"},
		},
	})
	reqValidS, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/schedule", bytes.NewReader(validSched))
	reqValidS.Header.Set("Authorization", "Bearer tok-v034-rotated-secret")
	reqValidS.Header.Set("Content-Type", "application/json")
	respValidS, err := http.DefaultClient.Do(reqValidS)
	if err != nil {
		t.Fatalf("valid schedule err: %v", err)
	}
	respValidS.Body.Close()
	if respValidS.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid 00:00..23:59 schedule, got %d", respValidS.StatusCode)
	}

	// 5. MCP batch session validation + >2h session eviction
	srv.sessMu.Lock()
	srv.sessions["stale-session-id"] = time.Now().Add(-3 * time.Hour)
	srv.sessions["fresh-session-id"] = time.Now()
	srv.sessMu.Unlock()

	batchPayload := []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	reqBatchBad, _ := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(batchPayload))
	reqBatchBad.Header.Set("Authorization", "Bearer tok-v034-rotated-secret")
	reqBatchBad.Header.Set("Content-Type", "application/json")
	reqBatchBad.Header.Set("Mcp-Session-Id", "stale-session-id")
	respBatchBad, err := http.DefaultClient.Do(reqBatchBad)
	if err != nil {
		t.Fatalf("batch stale session req err: %v", err)
	}
	respBatchBad.Body.Close()
	if respBatchBad.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for expired Mcp-Session-Id on batch, got %d", respBatchBad.StatusCode)
	}
	srv.sessMu.RLock()
	_, staleStillThere := srv.sessions["stale-session-id"]
	_, freshStillThere := srv.sessions["fresh-session-id"]
	srv.sessMu.RUnlock()
	if staleStillThere || !freshStillThere {
		t.Fatalf("expected stale session evicted and fresh kept: stale=%v fresh=%v", staleStillThere, freshStillThere)
	}

	// 6. ExecuteTool redacts sensitive clipboard secrets & keeps summary line clean
	_ = fb.SetClipboard(context.Background(), "验证码: 849201 token=sk-live1234567890abcdef")
	clipOut := srv.ExecuteTool(context.Background(), "phone_clipboard_get", map[string]any{})
	if clipOut.IsError {
		t.Fatalf("ExecuteTool phone_clipboard_get failed: %+v", clipOut)
	}
	firstLine := strings.SplitN(clipOut.Text, "\n", 2)[0]
	if strings.Contains(firstLine, "849201") || strings.Contains(firstLine, "sk-live") {
		t.Fatalf("clipboard summary line leaked secrets: %q", firstLine)
	}
	if strings.Contains(clipOut.Text, "849201") || strings.Contains(clipOut.Text, "sk-live1234567890abcdef") {
		t.Fatalf("clipboard output did not redact secrets: %q", clipOut.Text)
	}

	// 7. MCP run_ms_rewards / run_jd_daily serialized through s.jobs
	blockJob := make(chan struct{})
	if _, err := srv.jobs.Start("daily_all", func(ctx context.Context, _ func(string)) (any, error) {
		select {
		case <-blockJob:
		case <-ctx.Done():
		}
		return map[string]any{"ok": true}, nil
	}); err != nil {
		t.Fatalf("start blocking job: %v", err)
	}
	defer close(blockJob)
	busyRes := srv.callTool(context.Background(), "run_ms_rewards", map[string]any{})
	if busyRes["isError"] != true {
		t.Fatalf("expected run_ms_rewards to reject when s.jobs already busy, got %v", busyRes)
	}
	busyJD := srv.callTool(context.Background(), "run_jd_daily", map[string]any{})
	if busyJD["isError"] != true {
		t.Fatalf("expected run_jd_daily to reject when s.jobs already busy, got %v", busyJD)
	}
}

type lockableBackend struct {
	*fakeBackend
	mu       sync.Mutex
	unlocked bool
}

func (l *lockableBackend) IsScreenUnlocked(_ context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unlocked
}

func TestV040BootstrapLongPollScreenRemoteLLMTestIslandAndCompositeTools(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.BearerToken = "tok-v040-secret-abc"
	cfg.PauseFlagPath = ""
	cfg.MemoryDir = filepath.Join(tmpDir, "memory")
	cfg.CDPSocket = "nonexistent_test_cdp_socket_040"

	llmGate := make(chan struct{}, 2)
	llmRelease := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(llmRelease) }) }
	defer closeRelease()

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "Reply OK") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		select {
		case llmGate <- struct{}{}:
		default:
		}
		select {
		case <-llmRelease:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"极速\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"长轮询\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer llmSrv.Close()

	cfg.LLM = config.LLMConfig{
		Provider:   "openai",
		BaseURL:    llmSrv.URL,
		APIKey:     "sk-v040-test-key",
		Model:      "v040-fast-model",
		MaxSteps:   10,
		TimeoutSec: 5,
	}

	fb := &fakeBackend{}
	lb := &lockableBackend{fakeBackend: fb, unlocked: true}
	det := &testDetector{}
	g := guard.New(guard.Options{Packages: []string{"com.tencent.tmgp.sgame"}, PollInterval: time.Second, Detector: det})
	srv := NewServer(cfg, lb, g, cdp.NewClient(cfg.CDPSocket), nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()
	baseURL := "http://" + srv.Addr()

	doJSON := func(method, path string, body any) (int, map[string]any) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, baseURL+path, rdr)
		if err != nil {
			t.Fatalf("new req %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// 1. 1-RTT GET /api/v1/bootstrap
	code, boot := doJSON(http.MethodGet, "/api/v1/bootstrap", nil)
	if code != http.StatusOK {
		t.Fatalf("bootstrap expected 200, got %d: %+v", code, boot)
	}
	for _, key := range []string{"status", "jobs", "saved_turns", "history", "llm", "schedule", "memory_meta"} {
		if _, ok := boot[key]; !ok {
			t.Fatalf("bootstrap missing key %q: %+v", key, boot)
		}
	}
	stMap, _ := boot["status"].(map[string]any)
	if stMap["version"] != "0.4.1" {
		t.Fatalf("expected version 0.4.1 in bootstrap status, got %v", stMap["version"])
	}

	// 2. POST /api/v1/llm/test
	code, llmTest := doJSON(http.MethodPost, "/api/v1/llm/test", map[string]any{})
	if code != http.StatusOK || llmTest["ok"] != true || llmTest["preview"] != "OK" {
		t.Fatalf("unexpected /api/v1/llm/test response: code=%d body=%+v", code, llmTest)
	}

	// 3. GET /api/v1/screen/preview?with_tree=1 & POST /api/v1/screen/action
	code, prev := doJSON(http.MethodGet, "/api/v1/screen/preview?with_tree=1", nil)
	if code != http.StatusOK || prev["ok"] != true {
		t.Fatalf("screen preview failed: code=%d body=%+v", code, prev)
	}
	imgURL, _ := prev["image_url"].(string)
	if !strings.HasPrefix(imgURL, "data:image/jpeg;base64,") || prev["package"] != "com.miui.home" {
		t.Fatalf("unexpected screen preview payload: pkg=%v len=%d", prev["package"], len(imgURL))
	}
	if int(prev["node_count"].(float64)) < 2 {
		t.Fatalf("expected >=2 purified UI nodes in preview, got %v", prev["node_count"])
	}

	// Tap in 720x1600 image space -> scaled to 1080x2400 physical space
	code, tapAct := doJSON(http.MethodPost, "/api/v1/screen/action", map[string]any{
		"action": "tap", "x": 360, "y": 800, "coord_space": "image",
	})
	if code != http.StatusOK || tapAct["ok"] != true || fb.lastTap != [2]int{540, 1200} {
		t.Fatalf("screen action tap failed: code=%d out=%+v lastTap=%v", code, tapAct, fb.lastTap)
	}
	// Tap UI node directly from Screen tab inspector
	code, nodeAct := doJSON(http.MethodPost, "/api/v1/screen/action", map[string]any{
		"action": "tap_node", "node_id": 2,
	})
	if code != http.StatusOK || nodeAct["ok"] != true {
		t.Fatalf("screen action tap_node failed: code=%d out=%+v", code, nodeAct)
	}
	// Key & swipe actions
	code, _ = doJSON(http.MethodPost, "/api/v1/screen/action", map[string]any{"action": "key", "keycode": "HOME"})
	if code != http.StatusOK || fb.lastKey != "HOME" {
		t.Fatalf("screen action key failed: code=%d lastKey=%q", code, fb.lastKey)
	}
	code, _ = doJSON(http.MethodPost, "/api/v1/screen/action", map[string]any{
		"action": "swipe", "x": 360, "y": 1200, "x2": 360, "y2": 400, "coord_space": "image",
	})
	if code != http.StatusOK || fb.lastSwipe != [4]int{540, 1800, 540, 600} {
		t.Fatalf("screen action swipe failed: code=%d lastSwipe=%v", code, fb.lastSwipe)
	}

	// Lockscreen gate blocks remote screen actions
	lb.mu.Lock()
	lb.unlocked = false
	lb.mu.Unlock()
	code, _ = doJSON(http.MethodPost, "/api/v1/screen/action", map[string]any{"action": "tap", "x": 100, "y": 100})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when screen is locked, got %d", code)
	}
	lb.mu.Lock()
	lb.unlocked = true
	lb.mu.Unlock()

	// 4. POST /api/v1/island/test across states
	for _, st := range []string{"thinking", "acting", "workflow", "confirm", "done"} {
		code, isl := doJSON(http.MethodPost, "/api/v1/island/test", map[string]any{"state": st})
		if code != http.StatusOK || isl["ok"] != true {
			t.Fatalf("island test %q failed: code=%d body=%+v", st, code, isl)
		}
	}

	// 5. Composite MCP tools: phone_open_deeplink & phone_tap_and_type (+ privacy redaction)
	dlRes := srv.callTool(context.Background(), "phone_open_deeplink", map[string]any{
		"uri":     "openapp.jdmobile://virtual",
		"package": "com.jingdong.app.mall",
	})
	if dlRes["isError"] == true || fb.lastDeepLink != [2]string{"openapp.jdmobile://virtual", "com.jingdong.app.mall"} {
		t.Fatalf("phone_open_deeplink failed: %+v lastDeepLink=%v", dlRes, fb.lastDeepLink)
	}
	blockedDL := srv.callTool(context.Background(), "phone_open_deeplink", map[string]any{
		"uri":     "sgame://launch",
		"package": "com.tencent.tmgp.sgame",
	})
	if blockedDL["isError"] != true {
		t.Fatalf("expected guarded package deeplink to be rejected, got %+v", blockedDL)
	}

	ttOut := srv.ExecuteTool(context.Background(), "phone_tap_and_type", map[string]any{
		"node_id": 1,
		"text":    "sk-secretvalue12345678",
		"submit":  true,
		"wait_ms": 20,
	})
	if ttOut.IsError || fb.lastText != "sk-secretvalue12345678" {
		t.Fatalf("phone_tap_and_type failed: out=%+v lastText=%q", ttOut, fb.lastText)
	}
	if strings.Contains(ttOut.Text, "sk-secretvalue12345678") {
		t.Fatalf("phone_tap_and_type leaked secret in ToolOutput: %q", ttOut.Text)
	}

	// 6. Schedule prompt task validation + /api/v1/jobs/cancel
	code, schedOut := doJSON(http.MethodPost, "/api/v1/schedule", map[string]any{
		"enabled": true,
		"entries": []map[string]any{
			{"id": "morning-all", "task_type": "workflow", "job": "daily_all", "enabled": true, "window_start": "08:00", "window_end": "09:30", "defer_on_game": true},
			{"id": "noon-ai", "task_type": "prompt", "prompt": "检查京东待收货快递进度并记录到记忆", "enabled": true, "window_start": "12:00", "window_end": "13:00", "defer_on_game": true},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("expected multi-entry workflow+prompt schedule to succeed, got %d %+v", code, schedOut)
	}

	// 7. Long-polling GET /api/v1/chat/events?wait_ms=...
	code, startChat := doJSON(http.MethodPost, "/api/v1/chat/stream", map[string]any{
		"message": "测试长轮询合并唤醒",
		"async":   true,
	})
	if code != http.StatusAccepted {
		t.Fatalf("expected 202 async chat start, got %d %+v", code, startChat)
	}
	runID, _ := startChat["run_id"].(string)
	<-llmGate

	// Drain initial run_start / thinking events first so after_seq catches up to current LastSeq
	_, initialSnap := doJSON(http.MethodGet, fmt.Sprintf("/api/v1/chat/events?run_id=%s&after_seq=0", runID), nil)
	curSeq := int(initialSnap["last_seq"].(float64))

	// Trigger release 120ms after starting a 3000ms long-poll; verify long-poll wakes promptly (<800ms) with batched deltas!
	go func() {
		time.Sleep(120 * time.Millisecond)
		closeRelease()
	}()
	tPoll := time.Now()
	code, lpSnap := doJSON(http.MethodGet, fmt.Sprintf("/api/v1/chat/events?run_id=%s&after_seq=%d&wait_ms=3000", runID, curSeq), nil)
	pollElapsed := time.Since(tPoll)
	if code != http.StatusOK {
		t.Fatalf("long-poll failed: %d %+v", code, lpSnap)
	}
	if pollElapsed < 100*time.Millisecond || pollElapsed > 1200*time.Millisecond {
		t.Fatalf("expected long-poll to wake around ~130-250ms, took %v (snap=%+v)", pollElapsed, lpSnap)
	}
	evs, _ := lpSnap["events"].([]any)
	if len(evs) == 0 {
		t.Fatalf("expected batched events from long-poll wakeup, got %+v", lpSnap)
	}

	// Wait for the async chat turn to finish and verify SavedTurns persistence in bootstrap & chat/history
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.agent.IsRunning() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	code, histOut := doJSON(http.MethodGet, "/api/v1/chat/history", nil)
	if code != http.StatusOK {
		t.Fatalf("chat/history failed: %d %+v", code, histOut)
	}
	savedTurns, _ := histOut["saved_turns"].([]any)
	if len(savedTurns) == 0 {
		t.Fatalf("expected non-empty saved_turns after completed chat run, got %+v", histOut)
	}
	code, bootAfter := doJSON(http.MethodGet, "/api/v1/bootstrap", nil)
	if code != http.StatusOK || len(bootAfter["saved_turns"].([]any)) == 0 {
		t.Fatalf("expected saved_turns in bootstrap, got code=%d %+v", code, bootAfter)
	}

	// 8. POST /api/v1/schedule/run -> TriggerNow on custom AI prompt entry ("noon-ai")
	code, runPrompt := doJSON(http.MethodPost, "/api/v1/schedule/run", map[string]any{"entry_id": "noon-ai"})
	if code != http.StatusOK || runPrompt["ok"] != true {
		t.Fatalf("expected /api/v1/schedule/run on noon-ai to succeed, got %d %+v", code, runPrompt)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(srv.agent.SavedTurns()) >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(srv.agent.SavedTurns()) < 2 {
		t.Fatalf("expected scheduled prompt run to persist a second SavedTurn, got %d", len(srv.agent.SavedTurns()))
	}

	// 9. Workflow progress observer drives IslandController ("workflow" state + progress_pct)
	_, err := srv.jobs.Start("ms_rewards", func(_ context.Context, logFn func(string)) (any, error) {
		logFn("[阶段 2/4] 执行必应每日搜索 (65/100)")
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("srv.jobs.Start failed: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.jobs.Status().State == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(260 * time.Millisecond) // allow IslandController coalescer flush
	fb.mu.Lock()
	foundWorkflowIsland := false
	for _, u := range fb.liveUpdates {
		if u.State == "workflow" && u.Progress > 0 {
			foundWorkflowIsland = true
			break
		}
	}
	fb.mu.Unlock()
	if !foundWorkflowIsland {
		t.Fatalf("expected workflow progress observer to drive IslandController workflow state")
	}
}

func TestV041AutoObserveExecuteToolAndDefaults(t *testing.T) {
	oldFast := autoObserveFastSettle
	oldLaunch := autoObserveLaunchSettle
	autoObserveFastSettle = 15 * time.Millisecond
	autoObserveLaunchSettle = 25 * time.Millisecond
	defer func() {
		autoObserveFastSettle = oldFast
		autoObserveLaunchSettle = oldLaunch
	}()

	defCfg := config.Default()
	if defCfg.LLM.MaxSteps != 80 {
		t.Fatalf("expected default LLM.MaxSteps=80, got %d", defCfg.LLM.MaxSteps)
	}
	if ServerVersion != "0.4.1" {
		t.Fatalf("expected ServerVersion=0.4.1, got %q", ServerVersion)
	}

	defCfg.ListenAddr = "127.0.0.1:0"
	defCfg.BearerToken = "tok-v041-auto-observe"
	defCfg.PauseFlagPath = ""
	defCfg.MemoryDir = filepath.Join(t.TempDir(), "memory")
	defCfg.CDPSocket = "nonexistent_test_cdp_socket_041"

	fb := &fakeBackend{}
	det := &testDetector{}
	g := guard.New(guard.Options{Packages: []string{"com.tencent.tmgp.sgame"}, PollInterval: time.Second, Detector: det})
	srv := NewServer(defCfg, fb, g, cdp.NewClient(defCfg.CDPSocket), nil)
	defer srv.Close()

	// 1. phone_keyevent (HOME) auto-observes UI tree AND populates cachedNodes without calling phone_ui_tree!
	outKey := srv.ExecuteTool(context.Background(), "phone_keyevent", map[string]any{"keycode": "KEYCODE_HOME"})
	if outKey.IsError {
		t.Fatalf("ExecuteTool(phone_keyevent) failed: %+v", outKey)
	}
	if !strings.Contains(outKey.Text, "[操作后最新界面]\nApp: com.sankuai.meituan") || !strings.Contains(outKey.Text, `[2] Btn "外卖"`) {
		t.Fatalf("expected [操作后最新界面] appended to mutating tool output, got %q", outKey.Text)
	}

	// 2. Immediately call phone_tap_node using [2] from the auto-observed tree (no prior phone_ui_tree needed!)
	outTapNode := srv.ExecuteTool(context.Background(), "phone_tap_node", map[string]any{"id": 2})
	if outTapNode.IsError {
		t.Fatalf("ExecuteTool(phone_tap_node) using auto-observed node id failed: %+v", outTapNode)
	}
	if !strings.Contains(outTapNode.Text, `"label":"外卖"`) || !strings.Contains(outTapNode.Text, "[操作后最新界面]") {
		t.Fatalf("expected action JSON + [操作后最新界面] on phone_tap_node, got %q", outTapNode.Text)
	}

	// 3. observe:false skips auto-observation
	outNoObs := srv.ExecuteTool(context.Background(), "phone_launch_app", map[string]any{
		"package": "com.sankuai.meituan",
		"observe": false,
	})
	if outNoObs.IsError || strings.Contains(outNoObs.Text, "[操作后最新界面]") {
		t.Fatalf("expected observe:false to skip [操作后最新界面], got %+v", outNoObs)
	}

	// 4. Error result skips auto-observation
	outErr := srv.ExecuteTool(context.Background(), "phone_tap", map[string]any{"x": 100}) // missing y
	if !outErr.IsError || strings.Contains(outErr.Text, "[操作后最新界面]") {
		t.Fatalf("expected error tool output not to append [操作后最新界面], got %+v", outErr)
	}
}

