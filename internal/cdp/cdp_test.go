package cdp

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCDPGracefulDegradationWhenUnavailable(t *testing.T) {
	c := NewClient("nonexistent_chrome_devtools_remote_test_99999")
	st := c.Probe(context.Background())
	if st.Available {
		t.Fatalf("expected unavailable, got %+v", st)
	}
	if _, err := c.ListTargets(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestCDPAbstractUnixSocketEndToEnd(t *testing.T) {
	sockName := fmt.Sprintf("android_agent_cdp_test_%d", 123456)
	ln, err := net.Listen("unix", "\x00"+sockName)
	if err != nil {
		t.Fatalf("listen abstract unix socket: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Browser":"Chrome/132.0","Protocol-Version":"1.3"}`)
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"PAGE1","type":"page","title":"Example","url":"https://example.com","webSocketDebuggerUrl":"ws://localhost/devtools/page/PAGE1"}]`)
	})
	mux.HandleFunc("/devtools/page/PAGE1", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(h[:])

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer does not support hijack")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		_ = rw.Flush()

		br := bufio.NewReader(conn)
		_, payload, err := wsReadFrame(br)
		if err != nil {
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(payload, &req)
		resp := fmt.Sprintf(`{"id":%d,"result":{"value":"title:%s"}}`, req.ID, req.Method)
		// Server-to-client frame (unmasked)
		frame := append([]byte{0x81, byte(len(resp))}, []byte(resp)...)
		_, _ = conn.Write(frame)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	c := NewClient(sockName)
	st := c.Probe(context.Background())
	if !st.Available {
		t.Fatalf("expected probe available, got %+v", st)
	}
	targets, err := c.ListTargets(context.Background())
	if err != nil || len(targets) != 1 || targets[0].ID != "PAGE1" {
		t.Fatalf("unexpected targets: %+v, err=%v", targets, err)
	}
	res, err := c.SendCommand(context.Background(), "PAGE1", "Runtime.evaluate", map[string]any{"expression": "document.title"})
	if err != nil {
		t.Fatalf("SendCommand failed: %v", err)
	}
	if !strings.Contains(string(res), "title:Runtime.evaluate") {
		t.Fatalf("unexpected CDP result: %s", string(res))
	}
}

func TestCDPParseProcNetUnixFiltersListeningOnly(t *testing.T) {
	raw := strings.Join([]string{
		"Num       RefCount Protocol Flags    Type St Inode Path",
		"0000000000000001: 00000002 00000000 00010000 0001 01 1001 @webview_devtools_remote_2001",
		"0000000000000002: 00000003 00000000 00000000 0001 03 1002 @webview_devtools_remote_2002",
		"0000000000000003: 00000002 00000000 00010000 0001 03 1003 @webview_devtools_remote_2003",
		"0000000000000004: 00000002 00000000 00010000 0001 01 1004 @chrome_devtools_remote",
		"0000000000000005: 00000002 00000000 00010000 0001 01 1005 @unrelated_socket",
	}, "\n")

	got := parseProcNetUnix(raw)
	if len(got) != 2 || got[0] != "chrome_devtools_remote" || got[1] != "webview_devtools_remote_2001" {
		t.Fatalf("expected only listening devtools sockets (primary first), got %v", got)
	}
}

func setupFakeCDPProc(t *testing.T, unixContent string, procs map[int]struct {
	cmdline      string
	oomAdj       string
	wchan        string
	cgroupPath   string
	cgroupEvents string
}) {
	t.Helper()
	tmp := t.TempDir()
	fakeProc := filepath.Join(tmp, "proc")
	fakeCg := filepath.Join(tmp, "cgroup")

	if err := os.MkdirAll(filepath.Join(fakeProc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeProc, "net", "unix"), []byte(unixContent), 0o644); err != nil {
		t.Fatal(err)
	}
	for pid, info := range procs {
		pdir := filepath.Join(fakeProc, fmt.Sprintf("%d", pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if info.cmdline != "" {
			_ = os.WriteFile(filepath.Join(pdir, "cmdline"), []byte(info.cmdline+"\x00"), 0o644)
		}
		if info.oomAdj != "" {
			_ = os.WriteFile(filepath.Join(pdir, "oom_score_adj"), []byte(info.oomAdj+"\n"), 0o644)
		}
		if info.wchan != "" {
			_ = os.WriteFile(filepath.Join(pdir, "wchan"), []byte(info.wchan+"\n"), 0o644)
		}
		if info.cgroupPath != "" {
			_ = os.WriteFile(filepath.Join(pdir, "cgroup"), []byte("0::"+info.cgroupPath+"\n"), 0o644)
			cgDir := filepath.Join(fakeCg, strings.TrimPrefix(info.cgroupPath, "/"))
			_ = os.MkdirAll(cgDir, 0o755)
			if info.cgroupEvents != "" {
				_ = os.WriteFile(filepath.Join(cgDir, "cgroup.events"), []byte(info.cgroupEvents), 0o644)
			}
		}
	}

	origProc, origCg := procRoot, cgroupRoot
	procRoot = fakeProc
	cgroupRoot = fakeCg
	t.Cleanup(func() {
		procRoot = origProc
		cgroupRoot = origCg
	})
}

func TestCDPFrozenDetectionRankingAndNoCrossSocketFallback(t *testing.T) {
	unixContent := strings.Join([]string{
		"Num       RefCount Protocol Flags    Type St Inode Path",
		"0000000000000001: 00000002 00000000 00010000 0001 01 101 @webview_devtools_remote_3001",
		"0000000000000002: 00000002 00000000 00010000 0001 01 102 @webview_devtools_remote_3002",
		"0000000000000003: 00000002 00000000 00010000 0001 01 103 @webview_devtools_remote_3003",
		"0000000000000004: 00000002 00000000 00010000 0001 01 104 @webview_devtools_remote_3004",
	}, "\n")
	setupFakeCDPProc(t, unixContent, map[int]struct {
		cmdline      string
		oomAdj       string
		wchan        string
		cgroupPath   string
		cgroupEvents string
	}{
		3001: {
			cmdline:      "com.microsoft.bing",
			oomAdj:       "900",
			cgroupPath:   "/apps/uid_10029/pid_3001",
			cgroupEvents: "populated 1\nfrozen 1\n",
		},
		3002: {
			cmdline: "com.jingdong.app.mall:push",
			oomAdj:  "100",
			wchan:   "do_epoll_wait",
		},
		3003: {
			cmdline: "com.jingdong.app.mall",
			oomAdj:  "100",
			wchan:   "do_epoll_wait",
		},
		3004: {
			cmdline: "com.frozen.wchan.app",
			oomAdj:  "0",
			wchan:   "do_freezer_trap",
		},
	})

	socks := ListSockets()
	if len(socks) != 4 {
		t.Fatalf("expected 4 sockets, got %+v", socks)
	}
	byPID := make(map[int]SocketInfo)
	for _, s := range socks {
		byPID[s.PID] = s
	}
	if !byPID[3001].Frozen || byPID[3001].OomAdj != 900 {
		t.Fatalf("expected pid 3001 frozen (cgroup.events) with oom 900, got %+v", byPID[3001])
	}
	if !byPID[3004].Frozen {
		t.Fatalf("expected pid 3004 frozen (wchan freezer), got %+v", byPID[3004])
	}
	if byPID[3003].Frozen || byPID[3003].OomAdj != 100 {
		t.Fatalf("expected pid 3003 non-frozen with oom 100, got %+v", byPID[3003])
	}

	base := NewClient("chrome_devtools_remote")

	// Multi-process JD ranking: main process (3003) must beat :push (3002) at same oom_adj.
	jdClient, err := base.ForSocket("jingdong")
	if err != nil {
		t.Fatalf("ForSocket(jingdong) failed: %v", err)
	}
	if jdClient.SocketDisplay() != "@webview_devtools_remote_3003" {
		t.Fatalf("expected main process webview_devtools_remote_3003, got %s", jdClient.SocketDisplay())
	}

	// Generic "webview" selector must skip frozen Bing (3001) and frozen wchan app (3004) and pick active JD main (3003).
	wvClient, err := base.ForSocket("webview")
	if err != nil {
		t.Fatalf("ForSocket(webview) failed: %v", err)
	}
	if wvClient.SocketDisplay() != "@webview_devtools_remote_3003" {
		t.Fatalf("expected active main webview_devtools_remote_3003, got %s", wvClient.SocketDisplay())
	}

	// Selecting a frozen process must return ErrTargetFrozen (with IsTargetFrozen()==true).
	_, err = base.ForSocket("bing")
	if !errors.Is(err, ErrTargetFrozen) {
		t.Fatalf("expected ErrTargetFrozen for frozen bing, got %v", err)
	}
	if !ErrTargetFrozen.IsTargetFrozen() {
		t.Fatal("expected ErrTargetFrozen.IsTargetFrozen() == true")
	}

	// Explicit "chrome_devtools_remote" (!autoDetect) must NOT silently fall back to webview sockets!
	var dialed []string
	base.dialer = func(_ context.Context, sock string) (net.Conn, error) {
		dialed = append(dialed, sock)
		if sock == "webview_devtools_remote_3003" {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}
		return nil, errors.New("connection refused")
	}
	if _, err := base.dial(context.Background()); err == nil {
		t.Fatal("expected explicit chrome_devtools_remote dial to fail without falling back to webview")
	}
	if len(dialed) != 1 || dialed[0] != "chrome_devtools_remote" {
		t.Fatalf("expected explicit client to only try chrome_devtools_remote, dialed=%v", dialed)
	}

	// Auto-detect client ("auto") SHOULD fall back to non-frozen ranked socket (3003).
	autoClient := NewClient("auto")
	autoClient.dialer = func(_ context.Context, sock string) (net.Conn, error) {
		if sock == "webview_devtools_remote_3003" {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}
		return nil, errors.New("connection refused")
	}
	conn, err := autoClient.dial(context.Background())
	if err != nil {
		t.Fatalf("expected auto client to fall back to active webview_devtools_remote_3003, got %v", err)
	}
	_ = conn.Close()
	if autoClient.SocketDisplay() != "@webview_devtools_remote_3003" {
		t.Fatalf("expected active socket @webview_devtools_remote_3003, got %s", autoClient.SocketDisplay())
	}

	// Direct dial of frozen socket via httpGet must preserve ErrTargetFrozen.
	frozenDirect := NewClient("webview_devtools_remote_3001")
	if _, err := frozenDirect.ListTargets(context.Background()); !errors.Is(err, ErrTargetFrozen) {
		t.Fatalf("expected ListTargets on frozen socket to preserve ErrTargetFrozen, got %v", err)
	}
}

func TestCDPWSReadFrameContextCancelAndInterleavedControlFrames(t *testing.T) {
	// 1. Context cancellation unblocks wsReadFrameCtx immediately instead of hanging.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err := wsReadFrameCtx(ctx, c1, bufio.NewReader(c1))
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected immediate unblock on cancel, took %v", elapsed)
	}

	// 2. Fragmented text frame with interleaved Ping (0x9) and Pong (0xA) control frames.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pongReceived := make(chan []byte, 1)
	go func() {
		// Fragment 1: FIN=0, op=0x1 (text), "hello "
		_, _ = serverConn.Write(append([]byte{0x01, 6}, []byte("hello ")...))
		// Interleaved Ping: FIN=1, op=0x9, "hi"
		_, _ = serverConn.Write(append([]byte{0x89, 2}, []byte("hi")...))
		// Read Pong echoed by client
		sbr := bufio.NewReader(serverConn)
		op, pongPayload, rerr := wsReadFrame(sbr)
		if rerr == nil && op == 0xA {
			pongReceived <- pongPayload
		}
		// Interleaved Pong: FIN=1, op=0xA, "ign"
		_, _ = serverConn.Write(append([]byte{0x8A, 3}, []byte("ign")...))
		// Fragment 2: FIN=1, op=0x0 (continuation), "world"
		_, _ = serverConn.Write(append([]byte{0x80, 5}, []byte("world")...))
	}()

	op, payload, err := wsReadFrameCtx(context.Background(), clientConn, bufio.NewReader(clientConn))
	if err != nil {
		t.Fatalf("wsReadFrameCtx failed: %v", err)
	}
	if op != 0x1 || string(payload) != "hello world" {
		t.Fatalf("expected op=0x1 payload='hello world', got op=0x%x payload=%q", op, string(payload))
	}
	select {
	case p := <-pongReceived:
		if string(p) != "hi" {
			t.Fatalf("expected pong payload 'hi', got %q", string(p))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Pong reply to interleaved Ping")
	}
}
