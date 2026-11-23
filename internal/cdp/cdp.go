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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrUnavailable = errors.New("browser devtools abstract socket unavailable")

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
			return d.DialContext(ctx, "unix", "\x00"+sock)
		},
	}
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
	// Try configured/primary socket first
	if conn, err := c.dialer(ctx, c.configuredSocket); err == nil {
		c.mu.Lock()
		c.activeSocket = c.configuredSocket
		c.mu.Unlock()
		return conn, nil
	} else if !c.autoDetect && c.configuredSocket != "chrome_devtools_remote" {
		return nil, err
	}

	// Auto-probe other *_devtools_remote* abstract sockets exposed by Chromium/Edge/WebView
	for _, cand := range discoverDevToolsSockets() {
		if cand == c.configuredSocket {
			continue
		}
		if conn, err := c.dialer(ctx, cand); err == nil {
			c.mu.Lock()
			c.activeSocket = cand
			c.mu.Unlock()
			return conn, nil
		}
	}
	return nil, fmt.Errorf("connect @%s: no active *_devtools_remote* socket found", c.configuredSocket)
}

func discoverDevToolsSockets() []string {
	raw, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var primary, others []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
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
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
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
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	br, err := wsHandshake(conn, wsPath)
	if err != nil {
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
		opcode, msg, err := wsReadFrame(br)
		if err != nil {
			return nil, fmt.Errorf("cdp read frame: %w", err)
		}
		switch opcode {
		case 0x8: // close
			return nil, errors.New("cdp websocket closed before response")
		case 0x9: // ping -> pong
			_ = wsWriteFrame(conn, 0xA, msg)
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
	var out []byte
	var firstOpcode byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return 0, nil, err
		}
		fin := (h[0] & 0x80) != 0
		op := h[0] & 0x0F
		if firstOpcode == 0 {
			firstOpcode = op
		}
		masked := (h[1] & 0x80) != 0
		length := uint64(h[1] & 0x7F)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
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
				return 0, nil, err
			}
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range buf {
				buf[i] ^= mask[i%4]
			}
		}
		out = append(out, buf...)
		if fin {
			return firstOpcode, out, nil
		}
	}
}
