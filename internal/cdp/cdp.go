package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var ErrUnavailable = errors.New("browser devtools abstract socket unavailable")

type frozenError struct{}

func (frozenError) Error() string        { return "cdp target process is frozen" }
func (frozenError) IsTargetFrozen() bool { return true }

var ErrTargetFrozen = frozenError{}

var (
	procRoot   = "/proc"
	cgroupRoot = "/sys/fs/cgroup"
)

type SocketInfo struct {
	Socket  string `json:"socket"`
	Name    string `json:"name"`
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
	Frozen  bool   `json:"frozen,omitempty"`
	OomAdj  int    `json:"oom_adj"`
}

type Client struct {
	configuredSocket string
	autoDetect       bool
	dialer           func(ctx context.Context, sock string) (net.Conn, error)
	mu               sync.RWMutex
	activeSocket     string
	nextID           atomic.Int64
}

type Status struct {
	Available bool           `json:"available"`
	Socket    string         `json:"socket"`
	Version   map[string]any `json:"version,omitempty"`
	Message   string         `json:"message,omitempty"`
}

type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl,omitempty"`
}

// NewClient creates a CDP client targeting an Android abstract Unix socket
// (e.g. "chrome_devtools_remote", which maps to "\x00chrome_devtools_remote" and is used
// by both Chrome and Microsoft Edge on Android). Set socketName="auto" or "" to auto-probe
// @chrome_devtools_remote and any *_devtools_remote* sockets in /proc/net/unix.
func NewClient(socketName string) *Client {
	clean := strings.TrimPrefix(strings.TrimSpace(socketName), "@")
	auto := clean == "" || strings.EqualFold(clean, "auto")
	if auto {
		clean = "chrome_devtools_remote"
	}
	return &Client{
		configuredSocket: clean,
		autoDetect:       auto,
		activeSocket:     clean,
		dialer: func(ctx context.Context, sock string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "unix", "\x00"+sock)
			if err != nil {
				return nil, err
			}
			// Abstract sockets like @chrome_devtools_remote have no _<pid> suffix, and Linux
			// accepts AF_UNIX stream connections into the backlog even when the browser is
			// frozen by HyperOS cgroup freezer. Inspect SO_PEERCRED immediately (<0.1ms) so
			// Probe() never stalls for seconds and workflows receive ErrTargetFrozen to wake it.
			if uc, ok := conn.(*net.UnixConn); ok {
				if rc, scErr := uc.SyscallConn(); scErr == nil {
					var peerPID int
					_ = rc.Control(func(fd uintptr) {
						if cred, gerr := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); gerr == nil && cred != nil {
							peerPID = int(cred.Pid)
						}
					})
					if peerPID > 0 && isPIDFrozen(peerPID) {
						_ = conn.Close()
						return nil, fmt.Errorf("%w: @%s (pid=%d)", ErrTargetFrozen, sock, peerPID)
					}
				}
			}
			return conn, nil
		},
	}
}

// ListSockets discovers all active *_devtools_remote* abstract Unix sockets on the device
// along with their owning process name, frozen state, and oom_score_adj when a PID suffix
// is present (e.g. webview_devtools_remote_787).
func ListSockets() []SocketInfo {
	names := discoverDevToolsSockets()
	out := make([]SocketInfo, 0, len(names))
	for _, name := range names {
		info := SocketInfo{
			Socket: "@" + name,
			Name:   name,
		}
		if pid := parseSocketPID(name); pid > 0 {
			info.PID = pid
			info.Process = readProcCmdline(pid)
			info.Frozen = isPIDFrozen(pid)
			if adj, ok := readProcOomAdj(pid); ok {
				info.OomAdj = adj
			}
		}
		out = append(out, info)
	}
	return out
}

func parseSocketPID(name string) int {
	if idx := strings.LastIndexByte(name, '_'); idx > 0 && idx+1 < len(name) {
		if pid, err := strconv.Atoi(name[idx+1:]); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

func readProcCmdline(pid int) string {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil || len(raw) == 0 {
		return ""
	}
	if i := strings.IndexByte(string(raw), 0); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(string(raw))
}

func readProcOomAdj(pid int) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "oom_score_adj"))
	if err != nil {
		return 0, false
	}
	adj, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return adj, true
}

func isPIDFrozen(pid int) bool {
	if pid <= 0 {
		return false
	}
	pidStr := strconv.Itoa(pid)
	if raw, err := os.ReadFile(filepath.Join(procRoot, pidStr, "cgroup")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, ":", 3)
			if len(parts) == 3 && parts[0] == "0" && parts[1] == "" && parts[2] != "" {
				cgDir := filepath.Join(cgroupRoot, strings.TrimPrefix(parts[2], "/"))
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
	if raw, err := os.ReadFile(filepath.Join(procRoot, pidStr, "wchan")); err == nil {
		if strings.Contains(strings.ToLower(string(raw)), "freezer") {
			return true
		}
	}
	return false
}

// rankSocketCandidates orders candidates by:
// 1) non-frozen before frozen
// 2) lowest oom_score_adj
// 3) main process (no ':' suffix) before sub-process (':push', ':web', etc.)
func rankSocketCandidates(cands []SocketInfo) []SocketInfo {
	ranked := make([]SocketInfo, len(cands))
	copy(ranked, cands)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Frozen != b.Frozen {
			return !a.Frozen
		}
		if a.OomAdj != b.OomAdj {
			return a.OomAdj < b.OomAdj
		}
		aSub := strings.Contains(a.Process, ":")
		bSub := strings.Contains(b.Process, ":")
		if aSub != bSub {
			return !aSub
		}
		return false
	})
	return ranked
}

// ForSocket returns a Client bound to a specific socket selector:
// - "" -> returns c unchanged (default socket, e.g. @chrome_devtools_remote)
// - "webview" -> best ranked discovered @webview_devtools_remote* socket
// - package substring (e.g. "com.jingdong.app.mall" or "jingdong") -> best ranked matching PID's webview socket
// - exact socket name (with or without leading '@') -> that exact socket
func (c *Client) ForSocket(selector string) (*Client, error) {
	sel := strings.TrimPrefix(strings.TrimSpace(selector), "@")
	if sel == "" {
		return c, nil
	}
	if strings.EqualFold(sel, "webview") {
		var cands []SocketInfo
		for _, s := range ListSockets() {
			if strings.HasPrefix(s.Name, "webview_devtools_remote") {
				cands = append(cands, s)
			}
		}
		if len(cands) == 0 {
			return nil, fmt.Errorf("%w: no @webview_devtools_remote* socket active", ErrUnavailable)
		}
		best := rankSocketCandidates(cands)[0]
		if best.Frozen {
			return nil, fmt.Errorf("%w: %s (pid=%d)", ErrTargetFrozen, best.Socket, best.PID)
		}
		return NewClient(best.Name), nil
	}
	// If selector looks like a package/process name rather than a devtools socket name, match by owning process
	if !strings.Contains(sel, "devtools_remote") {
		low := strings.ToLower(sel)
		var cands []SocketInfo
		for _, s := range ListSockets() {
			if s.Process != "" && strings.Contains(strings.ToLower(s.Process), low) {
				cands = append(cands, s)
			}
		}
		if len(cands) == 0 {
			return nil, fmt.Errorf("%w: no devtools socket found for process matching %q", ErrUnavailable, sel)
		}
		best := rankSocketCandidates(cands)[0]
		if best.Frozen {
			return nil, fmt.Errorf("%w: %s (pid=%d)", ErrTargetFrozen, best.Socket, best.PID)
		}
		return NewClient(best.Name), nil
	}
	if pid := parseSocketPID(sel); pid > 0 && isPIDFrozen(pid) {
		return nil, fmt.Errorf("%w: @%s (pid=%d)", ErrTargetFrozen, sel, pid)
	}
	return NewClient(sel), nil
}

func (c *Client) SocketDisplay() string {
	c.mu.RLock()
	sock := c.activeSocket
	c.mu.RUnlock()
	if sock == "" {
		sock = c.configuredSocket
	}
	return "@" + sock
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	// Check if configured target process is frozen before attempting dial.
	var frozenCand *SocketInfo
	if pid := parseSocketPID(c.configuredSocket); pid > 0 && isPIDFrozen(pid) {
		if !c.autoDetect {
			return nil, fmt.Errorf("%w: @%s (pid=%d)", ErrTargetFrozen, c.configuredSocket, pid)
		}
		frozenCand = &SocketInfo{Socket: "@" + c.configuredSocket, Name: c.configuredSocket, PID: pid, Frozen: true}
	} else {
		// Try configured/primary socket first
		if conn, err := c.dialer(ctx, c.configuredSocket); err == nil {
			c.mu.Lock()
			c.activeSocket = c.configuredSocket
			c.mu.Unlock()
			return conn, nil
		} else if !c.autoDetect {
			return nil, err
		}
	}

	// Auto-probe other *_devtools_remote* abstract sockets strictly when autoDetect is enabled.
	for _, cand := range rankSocketCandidates(ListSockets()) {
		if cand.Name == c.configuredSocket {
			continue
		}
		if cand.Frozen {
			if frozenCand == nil {
				fc := cand
				frozenCand = &fc
			}
			continue
		}
		if conn, err := c.dialer(ctx, cand.Name); err == nil {
			c.mu.Lock()
			c.activeSocket = cand.Name
			c.mu.Unlock()
			return conn, nil
		}
	}
	if frozenCand != nil {
		return nil, fmt.Errorf("%w: %s (pid=%d)", ErrTargetFrozen, frozenCand.Socket, frozenCand.PID)
	}
	return nil, fmt.Errorf("connect @%s: no active *_devtools_remote* socket found", c.configuredSocket)
}

func discoverDevToolsSockets() []string {
	raw, err := os.ReadFile(filepath.Join(procRoot, "net", "unix"))
	if err != nil {
		return nil
	}
	return parseProcNetUnix(string(raw))
}

func parseProcNetUnix(raw string) []string {
	seen := make(map[string]struct{})
	var primary, others []string
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		// Only accept listening sockets (SO_ACCEPTCON=00010000, SS_UNCONNECTED=01).
		if fields[3] != "00010000" || fields[5] != "01" {
			continue
		}
		path := fields[len(fields)-1]
		if !strings.HasPrefix(path, "@") {
			continue
		}
		name := strings.TrimPrefix(path, "@")
		if !strings.Contains(name, "devtools_remote") {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if name == "chrome_devtools_remote" || name == "edge_devtools_remote" || name == "msedge_devtools_remote" {
			primary = append(primary, name)
		} else {
			others = append(others, name)
		}
	}
	return append(primary, others...)
}

func (c *Client) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return c.dial(ctx)
			},
			DisableKeepAlives: true,
		},
	}
}

// Probe checks whether the Chrome DevTools socket is reachable without failing startup.
func (c *Client) Probe(ctx context.Context) Status {
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()

	ver, err := c.Version(probeCtx)
	if err != nil {
		return Status{
			Available: false,
			Socket:    c.SocketDisplay(),
			Message:   fmt.Sprintf("Chrome CDP unavailable on %s (Chrome not installed or not running): %v", c.SocketDisplay(), err),
		}
	}
	return Status{
		Available: true,
		Socket:    c.SocketDisplay(),
		Version:   ver,
		Message:   "Chrome CDP socket connected",
	}
}

func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	body, err := c.httpGet(ctx, "/json/version")
	if err != nil {
		return nil, err
	}
	var res map[string]any
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("parse /json/version: %w", err)
	}
	return res, nil
}

func (c *Client) ListTargets(ctx context.Context) ([]Target, error) {
	body, err := c.httpGet(ctx, "/json/list")
	if err != nil {
		return nil, err
	}
	var targets []Target
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("parse /json/list: %w", err)
	}
	return targets, nil
}

func (c *Client) httpGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, ErrTargetFrozen) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cdp http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// SendCommand executes a single CDP JSON-RPC command over WebSocket against a target ID
// (or the first "page" target if targetID is empty).
func (c *Client) SendCommand(ctx context.Context, targetID, method string, params map[string]any) (json.RawMessage, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("cdp method is required")
	}

	wsPath := ""
	if targetID != "" && strings.HasPrefix(targetID, "/") {
		wsPath = targetID
	} else {
		targets, err := c.ListTargets(ctx)
		if err != nil {
			return nil, err
		}
		var chosen *Target
		for i := range targets {
			if targetID != "" && targets[i].ID == targetID {
				chosen = &targets[i]
				break
			}
			if targetID == "" && targets[i].Type == "page" {
				chosen = &targets[i]
				break
			}
		}
		if chosen == nil && targetID == "" && len(targets) > 0 {
			chosen = &targets[0]
		}
		if chosen == nil {
			return nil, fmt.Errorf("no matching CDP target found (target_id=%q)", targetID)
		}
		if chosen.WebSocketDebuggerURL != "" {
			if u, err := url.Parse(chosen.WebSocketDebuggerURL); err == nil && u.RequestURI() != "" {
				wsPath = u.RequestURI()
			}
		}
		if wsPath == "" {
			wsPath = "/devtools/page/" + chosen.ID
		}
	}

	conn, err := c.dial(ctx)
	if err != nil {
		if errors.Is(err, ErrTargetFrozen) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	// Ensure cancellation interrupts handshake and writes immediately instead of waiting for deadline.
	stopWatch := make(chan struct{})
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.SetDeadline(time.Now())
			case <-stopWatch:
			}
		}()
	}
	defer close(stopWatch)

	br, err := wsHandshake(conn, wsPath)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, fmt.Errorf("cdp websocket handshake: %w", ctx.Err())
		}
		return nil, fmt.Errorf("cdp websocket handshake: %w", err)
	}

	reqID := c.nextID.Add(1)
	payload := map[string]any{
		"id":     reqID,
		"method": method,
	}
	if params != nil {
		payload["params"] = params
	}
	rawReq, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := wsWriteFrame(conn, 0x1, rawReq); err != nil {
		return nil, fmt.Errorf("cdp write frame: %w", err)
	}

	// Read frames until we receive the matching response id (skipping CDP events).
	for {
		opcode, msg, err := wsReadFrameCtx(ctx, conn, br)
		if err != nil {
			return nil, fmt.Errorf("cdp read frame: %w", err)
		}
		switch opcode {
		case 0x8: // close
			return nil, errors.New("cdp websocket closed before response")
		case 0x9: // ping -> pong
			_ = wsWriteFrame(conn, 0xA, msg)
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2:
			var env struct {
				ID     int64           `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(msg, &env); err != nil {
				continue
			}
			if env.ID != reqID {
				continue // event or unrelated message
			}
			if env.Error != nil {
				return nil, fmt.Errorf("cdp error %d: %s", env.Error.Code, env.Error.Message)
			}
			if len(env.Result) == 0 {
				return json.RawMessage(`{}`), nil
			}
			return env.Result, nil
		}
	}
}

func wsHandshake(conn net.Conn, path string) (*bufio.Reader, error) {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	wsKey := base64.StdEncoding.EncodeToString(keyBytes)

	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, wsKey,
	)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.Contains(statusLine, "101") {
		return nil, fmt.Errorf("unexpected upgrade status: %s", strings.TrimSpace(statusLine))
	}
	var accept string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(v)
		}
	}
	h := sha1.Sum([]byte(wsKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expected := base64.StdEncoding.EncodeToString(h[:])
	if accept != expected {
		return nil, fmt.Errorf("invalid Sec-WebSocket-Accept: got %q want %q", accept, expected)
	}
	return br, nil
}

func wsWriteFrame(w io.Writer, opcode byte, payload []byte) error {
	var header [14]byte
	header[0] = 0x80 | (opcode & 0x0F) // FIN + opcode
	n := len(payload)
	idx := 2
	switch {
	case n < 126:
		header[1] = 0x80 | byte(n) // Masked client frame
	case n <= 65535:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
		idx = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
		idx = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(header[idx:idx+4], mask[:])
	idx += 4

	if _, err := w.Write(header[:idx]); err != nil {
		return err
	}
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.Write(masked)
	return err
}

func wsReadFrame(br *bufio.Reader) (byte, []byte, error) {
	return wsReadFrameCtx(context.Background(), nil, br)
}

func wsReadFrameCtx(ctx context.Context, conn net.Conn, br *bufio.Reader) (byte, []byte, error) {
	if ctx != nil && ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	if conn != nil && ctx != nil && ctx.Done() != nil {
		stopWatch := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.SetReadDeadline(time.Now())
			case <-stopWatch:
			}
		}()
		defer close(stopWatch)
	}

	var out []byte
	var firstOpcode byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			if ctx != nil && ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, err
		}
		fin := (h[0] & 0x80) != 0
		op := h[0] & 0x0F
		isControl := op >= 0x8
		masked := (h[1] & 0x80) != 0
		length := uint64(h[1] & 0x7F)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				if ctx != nil && ctx.Err() != nil {
					return 0, nil, ctx.Err()
				}
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				if ctx != nil && ctx.Err() != nil {
					return 0, nil, ctx.Err()
				}
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		if length > 16*1024*1024 {
			return 0, nil, fmt.Errorf("ws frame too large: %d", length)
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(br, mask[:]); err != nil {
				if ctx != nil && ctx.Err() != nil {
					return 0, nil, ctx.Err()
				}
				return 0, nil, err
			}
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			if ctx != nil && ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, err
		}
		if masked {
			for i := range buf {
				buf[i] ^= mask[i%4]
			}
		}
		if isControl {
			// RFC 6455 section 5.4/5.5: control frames (opcode >= 0x8) may be injected
			// in the middle of a fragmented data message without corrupting its payload.
			if firstOpcode == 0 {
				return op, buf, nil
			}
			switch op {
			case 0x8: // close
				return op, buf, nil
			case 0x9: // ping -> pong immediately while continuing fragmented read
				if conn != nil {
					_ = wsWriteFrame(conn, 0xA, buf)
				}
			}
			continue
		}
		if firstOpcode == 0 {
			firstOpcode = op
		}
		out = append(out, buf...)
		if fin {
			return firstOpcode, out, nil
		}
	}
}
