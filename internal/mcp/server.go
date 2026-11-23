package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"android-agent/internal/backend"
	"android-agent/internal/cdp"
	"android-agent/internal/config"
	"android-agent/internal/guard"
)

const (
	ServerName          = "android-agent"
	ServerVersion       = "0.1.0-phase1"
	DefaultProtoVersion = "2025-03-26"
)

type Server struct {
	cfg     *config.Config
	backend backend.DeviceBackend
	guard   *guard.Guard
	cdp     *cdp.Client
	started time.Time
	logger  func(format string, args ...any)

	handler http.Handler

	sessMu   sync.RWMutex
	sessions map[string]time.Time

	srvMu      sync.Mutex
	listener   net.Listener
	httpServer *http.Server
	listening  bool
	actualAddr string
	closed     bool

	sseMu      sync.Mutex
	sseCancels map[int]context.CancelFunc
	nextSSEID  int
}

func NewServer(cfg *config.Config, be backend.DeviceBackend, g *guard.Guard, cdpClient *cdp.Client, logger func(string, ...any)) *Server {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	s := &Server{
		cfg:        cfg,
		backend:    be,
		guard:      g,
		cdp:        cdpClient,
		started:    time.Now(),
		logger:     logger,
		sessions:   make(map[string]time.Time),
		sseCancels: make(map[int]context.CancelFunc),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/health", s.handleStatus)
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/pause", s.handlePause)
	mux.HandleFunc("/api/v1/resume", s.handleResume)

	s.handler = s.authMiddleware(mux)
	return s
}

// Start binds the TCP listener and begins serving HTTP requests.
func (s *Server) Start() error {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.closed {
		return errors.New("server is permanently closed")
	}
	if s.listening {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.listener = ln
	s.httpServer = hs
	s.listening = true
	s.actualAddr = ln.Addr().String()

	go func(h *http.Server, l net.Listener) {
		if err := h.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger("http serve ended: %v", err)
		}
	}(hs, ln)

	s.logger("mcp server listening on %s", s.actualAddr)
	return nil
}

// StopForGuard completely closes the TCP listener and terminates active SSE streams
// while a guarded game (e.g. com.tencent.tmgp.sgame) is in the foreground.
func (s *Server) StopForGuard(reason string) {
	s.cancelAllSSE()
	s.srvMu.Lock()
	hs := s.httpServer
	ln := s.listener
	wasListening := s.listening
	s.listening = false
	s.httpServer = nil
	s.listener = nil
	s.srvMu.Unlock()

	if !wasListening {
		return
	}
	s.logger("mcp server stopping listener for guard (%s)", reason)
	if hs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_ = hs.Shutdown(ctx)
		cancel()
		_ = hs.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// StartAfterGuard re-binds the TCP listener after the guarded game leaves the foreground.
func (s *Server) StartAfterGuard() {
	if err := s.Start(); err != nil {
		s.logger("mcp server failed to restart after guard: %v", err)
	}
}

func (s *Server) IsListening() bool {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.listening
}

func (s *Server) Addr() string {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.actualAddr
}

// Close permanently shuts down the server.
func (s *Server) Close() {
	s.cancelAllSSE()
	s.srvMu.Lock()
	s.closed = true
	hs := s.httpServer
	ln := s.listener
	s.listening = false
	s.httpServer = nil
	s.listener = nil
	s.srvMu.Unlock()

	if hs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = hs.Shutdown(ctx)
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
}

func (s *Server) cancelAllSSE() {
	s.sseMu.Lock()
	cancels := s.sseCancels
	s.sseCancels = make(map[int]context.CancelFunc)
	s.sseMu.Unlock()
	for _, fn := range cancels {
		fn()
	}
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	expected := []byte(s.cfg.BearerToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="android-agent"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "missing or malformed Authorization Bearer token",
			})
			return
		}
		got := []byte(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")))
		if len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="android-agent", error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "invalid Authorization Bearer token",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) buildStatus(ctx context.Context) map[string]any {
	resp := map[string]any{
		"server":       ServerName,
		"version":      ServerVersion,
		"backend":      s.backend.Name(),
		"uptime_sec":   int64(time.Since(s.started).Seconds()),
		"listen_addr":  s.cfg.ListenAddr,
		"guard":        s.guard.Status(),
		"cdp":          s.cdp.Probe(ctx),
		"sys_socket":   "@" + strings.TrimPrefix(s.cfg.SysSocket, "@"),
		"sys_phase2":   "planned (not enabled in phase 1)",
		"default_shot": map[string]int{"width": s.cfg.ScreenshotWidth, "jpeg_quality": s.cfg.ScreenshotJPEGQuality},
	}
	if blocked, _ := s.guard.IsActionBlocked(); !blocked {
		if scr, err := s.backend.ScreenInfo(ctx); err == nil {
			resp["screen"] = scr
		}
	}
	return resp
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.buildStatus(r.Context()))
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
	st := s.guard.PauseManual(body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guard": st})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	st := s.guard.ResumeManual()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guard": st})
}

// --- Standard MCP Streamable HTTP (2025-03-26) ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleMCPPost(w, r)
	case http.MethodGet:
		s.handleMCPGetSSE(w, r)
	case http.MethodDelete:
		sid := r.Header.Get("Mcp-Session-Id")
		if sid != "" {
			s.sessMu.Lock()
			delete(s.sessions, sid)
			s.sessMu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPGetSSE(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	s.sseMu.Lock()
	s.nextSSEID++
	id := s.nextSSEID
	s.sseCancels[id] = cancel
	s.sseMu.Unlock()
	defer func() {
		cancel()
		s.sseMu.Lock()
		delete(s.sseCancels, id)
		s.sseMu.Unlock()
	}()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleMCPPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "failed to read request body"},
		})
		return
	}
	trimmed := bytesTrimSpace(body)
	if len(trimmed) == 0 {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "empty body"},
		})
		return
	}

	// Validate existing Mcp-Session-Id if provided (except for initialize)
	reqSessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))

	// Handle JSON-RPC batch or single message
	if trimmed[0] == '[' {
		var batch []rpcRequest
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "invalid JSON batch"},
			})
			return
		}
		var responses []rpcResponse
		var newSessionID string
		for _, req := range batch {
			resp, sid := s.dispatchRPC(r.Context(), req)
			if sid != "" {
				newSessionID = sid
			}
			if resp != nil {
				responses = append(responses, *resp)
			}
		}
		if newSessionID != "" {
			w.Header().Set("Mcp-Session-Id", newSessionID)
		}
		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, responses)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "invalid JSON-RPC request"},
		})
		return
	}

	if req.Method != "initialize" && reqSessionID != "" {
		s.sessMu.RLock()
		_, exists := s.sessions[reqSessionID]
		s.sessMu.RUnlock()
		if !exists {
			writeJSON(w, http.StatusNotFound, rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32001, Message: "unknown or expired Mcp-Session-Id"},
			})
			return
		}
	}

	resp, newSessionID := s.dispatchRPC(r.Context(), req)
	if newSessionID != "" {
		w.Header().Set("Mcp-Session-Id", newSessionID)
	}
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("MCP-Protocol-Version", DefaultProtoVersion)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) dispatchRPC(ctx context.Context, req rpcRequest) (*rpcResponse, string) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		protoVer := DefaultProtoVersion
		if params.ProtocolVersion != "" {
			protoVer = params.ProtocolVersion
		}
		sid := newSessionID()
		s.sessMu.Lock()
		s.sessions[sid] = time.Now()
		s.sessMu.Unlock()
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protoVer,
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]any{
					"name":    ServerName,
					"version": ServerVersion,
				},
			},
		}, sid

	case "notifications/initialized", "notifications/cancelled":
		return nil, ""

	case "ping":
		if isNotification {
			return nil, ""
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}, ""

	case "tools/list":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": toolDefinitions(),
			},
		}, ""

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid tools/call params"},
			}, ""
		}
		result := s.callTool(ctx, params.Name, params.Arguments)
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}, ""

	default:
		if isNotification {
			return nil, ""
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}, ""
	}
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}

	// Control / read-only guard tools remain accessible during manual pause
	switch name {
	case "phone_status":
		return textJSONResult(s.buildStatus(ctx), false)
	case "agent_pause":
		st := s.guard.PauseManual(getString(args, "reason", ""))
		return textJSONResult(map[string]any{"ok": true, "guard": st}, false)
	case "agent_resume":
		st := s.guard.ResumeManual()
		return textJSONResult(map[string]any{"ok": true, "guard": st}, false)
	}

	// Enforce game / manual anti-risk guard before any device or CDP action
	if blocked, reason := s.guard.IsActionBlocked(); blocked {
		return textErrorResult(reason)
	}

	switch name {
	case "phone_screenshot":
		width := getInt(args, "width", s.cfg.ScreenshotWidth)
		quality := getInt(args, "quality", s.cfg.ScreenshotJPEGQuality)
		res, err := s.backend.Screenshot(ctx, width, quality)
		if err != nil {
			return textErrorResult(err.Error())
		}
		meta, _ := json.Marshal(map[string]any{
			"screen_width":   res.OrigWidth,
			"screen_height":  res.OrigHeight,
			"image_width":    res.Width,
			"image_height":   res.Height,
			"jpeg_bytes":     len(res.JPEG),
			"quality":        quality,
			"capture_method": res.CaptureMethod,
			"elapsed_ms":     res.ElapsedMs,
			"scale_x":        float64(res.OrigWidth) / float64(res.Width),
			"scale_y":        float64(res.OrigHeight) / float64(res.Height),
		})
		return map[string]any{
			"content": []map[string]any{
				{
					"type":     "image",
					"data":     base64.StdEncoding.EncodeToString(res.JPEG),
					"mimeType": "image/jpeg",
				},
				{
					"type": "text",
					"text": string(meta),
				},
			},
			"isError": false,
		}

	case "phone_tap":
		x, okX := getIntStrict(args, "x")
		y, okY := getIntStrict(args, "y")
		if !okX || !okY {
			return textErrorResult("x and y integer coordinates are required")
		}
		px, py, err := s.resolveCoords(ctx, x, y, getString(args, "coord_space", "physical"), getInt(args, "image_width", s.cfg.ScreenshotWidth))
		if err != nil {
			return textErrorResult(err.Error())
		}
		if err := s.backend.Tap(ctx, px, py); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "x": px, "y": py}, false)

	case "phone_long_press":
		x, okX := getIntStrict(args, "x")
		y, okY := getIntStrict(args, "y")
		if !okX || !okY {
			return textErrorResult("x and y integer coordinates are required")
		}
		dur := getInt(args, "duration_ms", 800)
		px, py, err := s.resolveCoords(ctx, x, y, getString(args, "coord_space", "physical"), getInt(args, "image_width", s.cfg.ScreenshotWidth))
		if err != nil {
			return textErrorResult(err.Error())
		}
		if err := s.backend.LongPress(ctx, px, py, dur); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "x": px, "y": py, "duration_ms": dur}, false)

	case "phone_swipe":
		x1, ok1 := getIntStrict(args, "x1")
		y1, ok2 := getIntStrict(args, "y1")
		x2, ok3 := getIntStrict(args, "x2")
		y2, ok4 := getIntStrict(args, "y2")
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return textErrorResult("x1, y1, x2, y2 integer coordinates are required")
		}
		dur := getInt(args, "duration_ms", 300)
		space := getString(args, "coord_space", "physical")
		imgW := getInt(args, "image_width", s.cfg.ScreenshotWidth)
		px1, py1, err := s.resolveCoords(ctx, x1, y1, space, imgW)
		if err != nil {
			return textErrorResult(err.Error())
		}
		px2, py2, err := s.resolveCoords(ctx, x2, y2, space, imgW)
		if err != nil {
			return textErrorResult(err.Error())
		}
		if err := s.backend.Swipe(ctx, px1, py1, px2, py2, dur); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "x1": px1, "y1": py1, "x2": px2, "y2": py2, "duration_ms": dur}, false)

	case "phone_keyevent":
		key := getString(args, "keycode", "")
		if err := s.backend.KeyEvent(ctx, key); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "keycode": key}, false)

	case "phone_input_text":
		text := getString(args, "text", "")
		method := getString(args, "method", "auto")
		submit := getBool(args, "submit", false)
		used, err := s.backend.InputText(ctx, text, method, submit)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "method": used, "length": len([]rune(text)), "submitted": submit}, false)

	case "phone_clipboard_set":
		text := getString(args, "text", "")
		if err := s.backend.SetClipboard(ctx, text); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "length": len([]rune(text))}, false)

	case "phone_clipboard_get":
		text, err := s.backend.GetClipboard(ctx)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "text": text}, false)

	case "phone_foreground_app":
		info, err := s.backend.ForegroundApp(ctx)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(info, false)

	case "phone_launch_app":
		pkg := getString(args, "package", "")
		if err := s.backend.LaunchApp(ctx, pkg); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "package": pkg}, false)

	case "cdp_status":
		return textJSONResult(s.cdp.Probe(ctx), false)

	case "cdp_list_targets":
		targets, err := s.cdp.ListTargets(ctx)
		if err != nil {
			if errors.Is(err, cdp.ErrUnavailable) {
				return textJSONResult(map[string]any{
					"available": false,
					"socket":    s.cdp.SocketDisplay(),
					"targets":   []any{},
					"message":   err.Error(),
				}, false)
			}
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"available": true,
			"socket":    s.cdp.SocketDisplay(),
			"targets":   targets,
		}, false)

	case "cdp_command":
		targetID := getString(args, "target_id", "")
		method := getString(args, "method", "")
		var params map[string]any
		if rawParams, ok := args["params"].(map[string]any); ok {
			params = rawParams
		}
		res, err := s.cdp.SendCommand(ctx, targetID, method, params)
		if err != nil {
			if errors.Is(err, cdp.ErrUnavailable) {
				return textJSONResult(map[string]any{
					"available": false,
					"socket":    s.cdp.SocketDisplay(),
					"message":   err.Error(),
				}, true)
			}
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"ok":     true,
			"method": method,
			"result": json.RawMessage(res),
		}, false)

	default:
		return textErrorResult(fmt.Sprintf("unknown tool: %s", name))
	}
}

func (s *Server) resolveCoords(ctx context.Context, x, y int, space string, imageWidth int) (int, int, error) {
	if strings.ToLower(strings.TrimSpace(space)) != "image" {
		return x, y, nil
	}
	if imageWidth <= 0 {
		imageWidth = s.cfg.ScreenshotWidth
	}
	scr, err := s.backend.ScreenInfo(ctx)
	if err != nil || scr.Width <= 0 || scr.Height <= 0 {
		return 0, 0, fmt.Errorf("cannot scale image coordinates without screen size: %v", err)
	}
	if scr.Width <= imageWidth {
		return x, y, nil
	}
	scale := float64(scr.Width) / float64(imageWidth)
	return int(math.Round(float64(x) * scale)), int(math.Round(float64(y) * scale)), nil
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "phone_status",
			"description": "Get Xiaomi 13 agent status: screen resolution, backend mode, game-guard state, and Chrome CDP socket availability.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "phone_screenshot",
			"description": "Capture the phone screen locally, resize proportionally (default width 720px), compress to JPEG (default quality 70), and return image + coordinate metadata.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"width":   map[string]any{"type": "integer", "description": "Target image width in pixels (default 720)"},
					"quality": map[string]any{"type": "integer", "description": "JPEG quality 1-100 (default 70)"},
				},
			},
		},
		{
			"name":        "phone_tap",
			"description": "Tap on screen coordinates. Supports physical screen coordinates (default, e.g. 1080x2400) or screenshot image coordinates (coord_space='image').",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x":           map[string]any{"type": "integer"},
					"y":           map[string]any{"type": "integer"},
					"coord_space": map[string]any{"type": "string", "enum": []string{"physical", "image"}, "description": "Coordinate space (default physical)"},
					"image_width": map[string]any{"type": "integer", "description": "Reference screenshot width when coord_space='image' (default 720)"},
				},
				"required": []string{"x", "y"},
			},
		},
		{
			"name":        "phone_long_press",
			"description": "Long press on screen coordinates for duration_ms milliseconds.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x":           map[string]any{"type": "integer"},
					"y":           map[string]any{"type": "integer"},
					"duration_ms": map[string]any{"type": "integer", "description": "Press duration in ms (default 800)"},
					"coord_space": map[string]any{"type": "string", "enum": []string{"physical", "image"}},
					"image_width": map[string]any{"type": "integer"},
				},
				"required": []string{"x", "y"},
			},
		},
		{
			"name":        "phone_swipe",
			"description": "Swipe from (x1, y1) to (x2, y2) over duration_ms milliseconds.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x1":          map[string]any{"type": "integer"},
					"y1":          map[string]any{"type": "integer"},
					"x2":          map[string]any{"type": "integer"},
					"y2":          map[string]any{"type": "integer"},
					"duration_ms": map[string]any{"type": "integer", "description": "Swipe duration in ms (default 300)"},
					"coord_space": map[string]any{"type": "string", "enum": []string{"physical", "image"}},
					"image_width": map[string]any{"type": "integer"},
				},
				"required": []string{"x1", "y1", "x2", "y2"},
			},
		},
		{
			"name":        "phone_keyevent",
			"description": "Send an Android keyevent (e.g. HOME, BACK, RECENTS, ENTER, DEL, PASTE, POWER, WAKEUP, VOLUME_UP, VOLUME_DOWN, or numeric keycode).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"keycode": map[string]any{"type": "string"},
				},
				"required": []string{"keycode"},
			},
		},
		{
			"name":        "phone_input_text",
			"description": "Input text into the currently focused field. Supports full Chinese/UTF-8 via app_process64 clipboard + KEYCODE_PASTE, or fast ASCII input.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":   map[string]any{"type": "string"},
					"method": map[string]any{"type": "string", "enum": []string{"auto", "clipboard", "input"}, "description": "Input strategy (default auto)"},
					"submit": map[string]any{"type": "boolean", "description": "Send ENTER after inputting text"},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "phone_clipboard_set",
			"description": "Write UTF-8 / Chinese text directly to the Android clipboard.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string"},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "phone_clipboard_get",
			"description": "Read current plain text from the Android clipboard.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "phone_foreground_app",
			"description": "Get the currently focused foreground package and activity name.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "phone_launch_app",
			"description": "Launch an installed Android app by package name (e.g. com.tencent.mm).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"package": map[string]any{"type": "string"},
				},
				"required": []string{"package"},
			},
		},
		{
			"name":        "agent_pause",
			"description": "Manually pause all device interaction and CDP tools (anti-risk guard).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{"type": "string"},
				},
			},
		},
		{
			"name":        "agent_resume",
			"description": "Resume device interaction tools after a manual pause.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "cdp_status",
			"description": "Probe local Chrome DevTools abstract Unix socket (@chrome_devtools_remote). Gracefully reports unavailable if Chrome is not installed/running.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "cdp_list_targets",
			"description": "List inspectable Chrome tabs/targets via @chrome_devtools_remote (gracefully degrades if unavailable).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "cdp_command",
			"description": "Execute a Chrome DevTools Protocol JSON-RPC command (e.g. Page.navigate, Runtime.evaluate) directly over @chrome_devtools_remote.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_id": map[string]any{"type": "string", "description": "Optional Chrome target ID (defaults to first page target)"},
					"method":    map[string]any{"type": "string", "description": "CDP method name, e.g. Runtime.evaluate"},
					"params":    map[string]any{"type": "object", "description": "CDP method parameters"},
				},
				"required": []string{"method"},
			},
		},
	}
}

func textJSONResult(v any, isErr bool) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(b)},
		},
		"isError": isErr,
	}
}

func textErrorResult(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": msg},
		},
		"isError": true,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func getString(m map[string]any, key, def string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func getInt(m map[string]any, key string, def int) int {
	if v, ok := getIntStrict(m, key); ok {
		return v
	}
	return def
}

func getIntStrict(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(math.Round(n)), true
	case int:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func getBool(m map[string]any, key string, def bool) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}
