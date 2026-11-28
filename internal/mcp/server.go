package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"android-agent/internal/backend"
	"android-agent/internal/brain"
	"android-agent/internal/cdp"
	"android-agent/internal/config"
	"android-agent/internal/guard"
	"android-agent/internal/webui"
	"android-agent/internal/workflows"
)

const (
	ServerName          = "android-agent"
	ServerVersion       = "0.4.2"
	DefaultProtoVersion = "2025-03-26"
	mcpSessionTTL       = 2 * time.Hour
)

var (
	autoObserveFastSettle   = 350 * time.Millisecond
	autoObserveLaunchSettle = 900 * time.Millisecond
)

type backendIslandAdapter struct {
	be backend.DeviceBackend
}

func (a *backendIslandAdapter) UpdateLiveProgress(ctx context.Context, u brain.LiveProgressUpdate) error {
	if a == nil || a.be == nil {
		return nil
	}
	step, maxV := u.Step, u.Max
	if u.Ongoing && u.Max >= 40 && u.Progress > 0 {
		step, maxV = u.Progress, 100
	}
	return a.be.UpdateLiveProgress(ctx, backend.LiveProgressSpec{
		Tag:       u.Tag,
		ID:        u.ID,
		State:     u.State,
		Title:     u.Title,
		Text:      u.Text,
		ShortText: u.ShortText,
		SubText:   u.SubText,
		Step:      step,
		Max:       maxV,
		Progress:  u.Progress,
		ClickURI:  u.ClickURI,
		Focus:     u.Focus,
		Ongoing:   u.Ongoing,
	})
}

func (a *backendIslandAdapter) CancelLiveProgress(ctx context.Context, tag string, id int) error {
	if a == nil || a.be == nil {
		return nil
	}
	return a.be.CancelLiveProgress(ctx, tag, id)
}

type Server struct {
	cfg        *config.Config
	configPath string
	backend    backend.DeviceBackend
	guard      *guard.Guard
	cdp        *cdp.Client
	runner     *workflows.Runner
	memory     *brain.MemoryStore
	agent      *brain.Agent
	island     *brain.IslandController
	jobs       *brain.JobManager
	scheduler  *brain.Scheduler
	started    time.Time
	logger     func(format string, args ...any)

	handler http.Handler

	cfgMu    sync.RWMutex
	sessMu   sync.RWMutex
	sessions map[string]time.Time

	shotMu   sync.RWMutex
	lastShot screenshotMeta

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

type screenshotMeta struct {
	origWidth  int
	origHeight int
	width      int
	height     int
	capturedAt time.Time
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
		jobs:       brain.NewJobManager(),
		started:    time.Now(),
		logger:     logger,
		sessions:   make(map[string]time.Time),
		sseCancels: make(map[int]context.CancelFunc),
	}
	s.runner = workflows.NewRunner(be, cdpClient, g, cfg.ScreenshotWidth, func(line string) {
		logger("workflow: %s", line)
	})
	s.memory = brain.NewMemoryStore(cfg.MemoryDir)
	_ = s.memory.EnsureDefaults()
	s.agent = brain.NewAgent(cfg.LLM, s, g)
	s.agent.SetMemoryStore(s.memory)
	if strings.TrimSpace(cfg.MemoryDir) != "" {
		s.agent.SetHistoryFile(filepath.Join(filepath.Dir(strings.TrimSpace(cfg.MemoryDir)), "chat_history.json"))
	}
	lease := brain.NewDeviceLease()
	s.agent.SetDeviceLease(lease)
	s.jobs.SetDeviceLease(lease)
	s.island = brain.NewIslandController(&backendIslandAdapter{be: be})
	s.agent.SetEventObserver(s.island.HandleEvent)
	s.jobs.SetProgressObserver(func(ev brain.WorkflowProgressEvent) {
		step, total := ev.Step, ev.Total
		if ev.Pct > 0 || ev.Done {
			step, total = ev.Pct, 100
		}
		s.island.HandleWorkflowProgress(ev.Job, step, total, ev.Stage, ev.Done, ev.Err)
	})
	s.scheduler = brain.NewScheduler(cfg.Schedule, g, s.jobs, "", s.runWorkflowJob, logger)
	s.scheduler.SetPromptRunner(func(ctx context.Context, prompt string) (any, error) {
		return s.agent.Chat(ctx, prompt)
	})
	if u, ok := be.(interface{ IsScreenUnlocked(context.Context) bool }); ok {
		s.scheduler.SetScreenUnlockChecker(u.IsScreenUnlocked)
	}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/mcp", s.handleMCP)
	apiMux.HandleFunc("/health", s.handleStatus)
	apiMux.HandleFunc("/api/v1/status", s.handleStatus)
	apiMux.HandleFunc("/api/v1/bootstrap", s.handleBootstrap)
	apiMux.HandleFunc("/api/v1/pause", s.handlePause)
	apiMux.HandleFunc("/api/v1/resume", s.handleResume)
	apiMux.HandleFunc("/api/v1/jobs/current", s.handleJobCurrent)
	apiMux.HandleFunc("/api/v1/jobs/run", s.handleJobRun)
	apiMux.HandleFunc("/api/v1/jobs/cancel", s.handleJobCancel)
	apiMux.HandleFunc("/api/v1/chat", s.handleChat)
	apiMux.HandleFunc("/api/v1/chat/stream", s.handleChatStream)
	apiMux.HandleFunc("/api/v1/chat/events", s.handleChatEvents)
	apiMux.HandleFunc("/api/v1/chat/cancel", s.handleChatCancel)
	apiMux.HandleFunc("/api/v1/chat/history", s.handleChatHistory)
	apiMux.HandleFunc("/api/v1/chat/clear", s.handleChatClear)
	apiMux.HandleFunc("/api/v1/llm/config", s.handleLLMConfig)
	apiMux.HandleFunc("/api/v1/llm/test", s.handleLLMTest)
	apiMux.HandleFunc("/api/v1/memory", s.handleMemory)
	apiMux.HandleFunc("/api/v1/schedule", s.handleSchedule)
	apiMux.HandleFunc("/api/v1/schedule/run", s.handleScheduleRun)
	apiMux.HandleFunc("/api/v1/screen/preview", s.handleScreenPreview)
	apiMux.HandleFunc("/api/v1/screen/action", s.handleScreenAction)
	apiMux.HandleFunc("/api/v1/island/test", s.handleIslandTest)

	authedAPI := s.authMiddleware(apiMux)
	rootMux := http.NewServeMux()
	rootMux.HandleFunc("/ui", webui.HandleUI)
	rootMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui", http.StatusFound)
			return
		}
		authedAPI.ServeHTTP(w, r)
	})

	s.handler = s.originHostMiddleware(rootMux)
	return s
}

func (s *Server) SetConfigPath(path string) {
	s.cfgMu.Lock()
	s.configPath = path
	s.cfgMu.Unlock()
	if strings.TrimSpace(path) != "" && s.scheduler != nil {
		s.scheduler.SetHistoryPath(filepath.Join(filepath.Dir(path), "schedule_history.json"))
	}
}

func (s *Server) StartScheduler(ctx context.Context) {
	if s.scheduler != nil {
		s.scheduler.Run(ctx)
	}
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
	if s.agent != nil {
		s.agent.CancelActive("guard blocked: " + reason)
	}
	if s.jobs != nil {
		s.jobs.Cancel("guard blocked: " + reason)
	}
	if s.island != nil {
		s.island.ClearImmediate(reason)
	}
	if s.backend != nil {
		s.backend.StopMicroKernel(reason)
	}
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
	if s.agent != nil {
		s.agent.CancelActive("server shutdown")
	}
	if s.island != nil {
		s.island.ClearImmediate("server shutdown")
		s.island.Close()
	}
	if s.backend != nil {
		s.backend.StopMicroKernel("server shutdown")
	}
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

func stripHostPort(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(strings.Trim(h, "[]"))
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func normalizeAuthority(scheme, hostport string) (string, bool) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", false
	}
	var host, port string
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host = strings.ToLower(strings.Trim(h, "[]"))
		port = strings.TrimSpace(p)
	} else {
		// Reject malformed host:port (e.g. unbracketed IPv6 or stray colon)
		if strings.HasPrefix(hostport, "[") {
			if !strings.HasSuffix(hostport, "]") {
				return "", false
			}
			host = strings.ToLower(strings.Trim(hostport, "[]"))
		} else if strings.Contains(hostport, ":") {
			return "", false
		} else {
			host = strings.ToLower(hostport)
		}
	}
	if host == "" {
		return "", false
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port), true
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", true
	}
	return host, true
}

func (s *Server) boundHost() string {
	s.srvMu.Lock()
	actual := s.actualAddr
	s.srvMu.Unlock()
	if h := stripHostPort(actual); h != "" && h != "0.0.0.0" && h != "::" {
		return h
	}
	s.cfgMu.RLock()
	listenAddr := ""
	if s.cfg != nil {
		listenAddr = s.cfg.ListenAddr
	}
	s.cfgMu.RUnlock()
	if h := stripHostPort(listenAddr); h != "" && h != "0.0.0.0" && h != "::" {
		return h
	}
	return ""
}

func isAllowedHostHeader(hostHeader, boundHost string) bool {
	if strings.TrimSpace(hostHeader) == "" {
		return false
	}
	h := stripHostPort(hostHeader)
	if h == "" {
		return false
	}
	if isLoopbackHost(h) {
		return true
	}
	if boundHost != "" && strings.EqualFold(h, boundHost) {
		return true
	}
	return false
}

func isAllowedOriginHeader(originHeader, reqHostHeader string, isTLS bool) bool {
	originHeader = strings.TrimSpace(originHeader)
	if originHeader == "" {
		return true
	}
	reqHostHeader = strings.TrimSpace(reqHostHeader)
	if reqHostHeader == "" {
		return false
	}
	u, err := url.Parse(originHeader)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	expectedScheme := "http"
	if isTLS {
		expectedScheme = "https"
	}
	if !strings.EqualFold(u.Scheme, expectedScheme) {
		return false
	}
	originAuth, ok1 := normalizeAuthority(expectedScheme, u.Host)
	reqAuth, ok2 := normalizeAuthority(expectedScheme, reqHostHeader)
	if !ok1 || !ok2 {
		return false
	}
	return strings.EqualFold(originAuth, reqAuth)
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	h := stripHostPort(remoteAddr)
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// originHostMiddleware mitigates DNS Rebinding and cross-origin browser requests.
func (s *Server) originHostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedHostHeader(r.Host, s.boundHost()) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "forbidden Host header (DNS rebinding protection)",
			})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !isAllowedOriginHeader(origin, r.Host, r.TLS != nil) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "forbidden Origin header",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) getBearerToken() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg == nil {
		return ""
	}
	return s.cfg.BearerToken
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "missing or malformed Authorization Bearer token",
			})
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		expected := s.getBearerToken()
		gotDigest := sha256.Sum256([]byte(got))
		expDigest := sha256.Sum256([]byte(expected))
		if expected == "" || subtle.ConstantTimeCompare(gotDigest[:], expDigest[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
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
		"server":              ServerName,
		"version":             ServerVersion,
		"backend":             s.backend.Name(),
		"microkernel_running": s.backend.MicroKernelRunning(),
		"touch":               s.backend.TouchStatus(),
		"uptime_sec":          int64(time.Since(s.started).Seconds()),
		"listen_addr":  s.cfg.ListenAddr,
		"guard":               s.guard.Status(),
		"island":              s.island.Status(),
		"chat_active":         s.agent.IsRunning(),
		"cdp":                 s.cdp.Probe(ctx),
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

func (s *Server) handleJobCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.jobs.Status())
}

func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if blocked, reason := s.guard.IsActionBlocked(); blocked {
		writeJSON(w, http.StatusConflict, map[string]any{"error": reason})
		return
	}
	var body struct {
		Job string `json:"job"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	jobName := strings.TrimSpace(body.Job)
	st, err := s.jobs.Start(jobName, func(ctx context.Context, logFn func(string)) (any, error) {
		return s.runWorkflowJob(ctx, jobName, logFn)
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": st})
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) runWorkflowJob(ctx context.Context, jobName string, logFn func(string)) (any, error) {
	rnr := workflows.NewRunner(s.backend, s.cdp, s.guard, s.cfg.ScreenshotWidth, logFn)
	switch strings.TrimSpace(jobName) {
	case "daily_all":
		return rnr.RunDailyAll(ctx)
	case "ms_rewards":
		return rnr.RunMSRewards(ctx, workflows.MSRewardsOptions{SearchIntervalSec: 6.0, ReadCount: 10})
	case "jd_daily":
		return rnr.RunJDDaily(ctx, workflows.JDDailyOptions{BrowseSec: 8.5, MaxBrowse: 24, WaterMode: "tasks"})
	default:
		return nil, fmt.Errorf("unknown job %q (use daily_all, ms_rewards, or jd_daily)", jobName)
	}
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "manual UI cancel"
	}
	cancelled := s.jobs.Cancel(reason)
	if s.island != nil && cancelled && !s.agent.IsRunning() {
		s.island.ClearImmediate("workflow cancelled")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cancelled": cancelled, "job": s.jobs.Status()})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	res, err := s.agent.Chat(r.Context(), body.Message)
	if err != nil {
		if errors.Is(err, brain.ErrChatBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message string `json:"message"`
		Async   bool   `json:"async"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	runID, err := s.agent.StartChatAsync(body.Message)
	if err != nil {
		if errors.Is(err, brain.ErrChatBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if body.Async {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":     true,
			"run_id": runID,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	snap, ch, unsub, found := s.agent.SubscribeRun(runID, 0)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
		return
	}
	defer unsub()

	ctx, cancel := context.WithCancel(r.Context())
	s.sseMu.Lock()
	s.nextSSEID++
	sseID := s.nextSSEID
	s.sseCancels[sseID] = cancel
	s.sseMu.Unlock()
	defer func() {
		cancel()
		s.sseMu.Lock()
		delete(s.sseCancels, sseID)
		s.sseMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, werr := io.WriteString(w, ": connected\n\n"); werr != nil {
		return
	}
	flusher.Flush()

	writeSSE := func(ev brain.RunEvent) bool {
		b, merr := json.Marshal(ev)
		if merr != nil {
			return true
		}
		if _, werr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b); werr != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, ev := range snap.Events {
		if !writeSSE(ev) {
			return
		}
		if ev.Type == "done" || ev.Type == "cancelled" || ev.Type == "error" {
			return
		}
	}
	if snap.Finished || ch == nil {
		return
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, werr := io.WriteString(w, ": heartbeat\n\n"); werr != nil {
				return
			}
			flusher.Flush()
		case ev, more := <-ch:
			if !more {
				return
			}
			if !writeSSE(ev) {
				return
			}
			if ev.Type == "done" || ev.Type == "cancelled" || ev.Type == "error" {
				return
			}
		}
	}
}

func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	runID := strings.TrimSpace(q.Get("run_id"))
	afterSeq := 0
	if rawSeq := strings.TrimSpace(q.Get("after_seq")); rawSeq != "" {
		if n, err := strconv.Atoi(rawSeq); err == nil && n >= 0 {
			afterSeq = n
		}
	}
	waitMs := 0
	if rawWait := strings.TrimSpace(q.Get("wait_ms")); rawWait != "" {
		if n, err := strconv.Atoi(rawWait); err == nil && n > 0 {
			waitMs = n
			if waitMs > 12000 {
				waitMs = 12000
			}
		}
	}

	if waitMs <= 0 {
		writeJSON(w, http.StatusOK, s.agent.ReplayEvents(runID, afterSeq))
		return
	}

	snap, ch, unsub, found := s.agent.SubscribeRun(runID, afterSeq)
	if !found {
		writeJSON(w, http.StatusOK, snap)
		return
	}
	defer unsub()

	if !snap.Active || snap.Finished || len(snap.Events) > 0 || ch == nil {
		writeJSON(w, http.StatusOK, snap)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	s.sseMu.Lock()
	s.nextSSEID++
	lpID := s.nextSSEID
	s.sseCancels[lpID] = cancel
	s.sseMu.Unlock()
	defer func() {
		cancel()
		s.sseMu.Lock()
		delete(s.sseCancels, lpID)
		s.sseMu.Unlock()
	}()

	waitTimer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
	defer waitTimer.Stop()

	select {
	case <-ctx.Done():
		writeJSON(w, http.StatusOK, s.agent.ReplayEvents(snap.RunID, afterSeq))
		return
	case <-waitTimer.C:
		writeJSON(w, http.StatusOK, s.agent.ReplayEvents(snap.RunID, afterSeq))
		return
	case ev, ok := <-ch:
		if ok && ev.Type != "done" && ev.Type != "cancelled" && ev.Type != "error" {
			// Coalesce for 35ms to batch rapid streaming token deltas into a single KSU shell response.
			coalTimer := time.NewTimer(35 * time.Millisecond)
		coalesceLoop:
			for {
				select {
				case <-ctx.Done():
					break coalesceLoop
				case <-coalTimer.C:
					break coalesceLoop
				case ev2, ok2 := <-ch:
					if !ok2 || ev2.Type == "done" || ev2.Type == "cancelled" || ev2.Type == "error" {
						break coalesceLoop
					}
				}
			}
			coalTimer.Stop()
		}
		writeJSON(w, http.StatusOK, s.agent.ReplayEvents(snap.RunID, afterSeq))
		return
	}
}

func (s *Server) handleChatCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cancelled := s.agent.CancelActive("manual UI stop")
	if s.island != nil && !cancelled && !s.agent.IsRunning() {
		s.island.ClearImmediate("manual UI stop")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"cancelled": cancelled,
	})
}

func (s *Server) buildLLMConfigView() map[string]any {
	c := s.agent.GetConfig()
	return map[string]any{
		"provider":       c.Provider,
		"base_url":       c.BaseURL,
		"api_key_masked": config.MaskSecret(c.APIKey),
		"configured":     strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.Model) != "",
		"model":          c.Model,
		"max_steps":      c.MaxSteps,
		"timeout_sec":    c.TimeoutSec,
	}
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	files := s.memory.List()
	totalBytes := 0
	for _, f := range files {
		totalBytes += f.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      s.buildStatus(r.Context()),
		"jobs":        s.jobs.Status(),
		"saved_turns": s.agent.SavedTurns(),
		"active_run":  s.agent.ReplayEvents("", 0),
		"history":     sanitizeChatHistory(s.agent.History()),
		"llm":         s.buildLLMConfigView(),
		"schedule":    s.scheduler.Status(),
		"memory_meta": map[string]any{
			"files":          files,
			"total_bytes":    totalBytes,
			"max_file_bytes": 16 * 1024,
		},
	})
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":    sanitizeChatHistory(s.agent.History()),
		"saved_turns": s.agent.SavedTurns(),
	})
}

func (s *Server) handleChatClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.agent.ClearHistory()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLLMConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.buildLLMConfigView())
	case http.MethodPost:
		var body struct {
			Provider   *string `json:"provider"`
			BaseURL    *string `json:"base_url"`
			APIKey     *string `json:"api_key"`
			Model      *string `json:"model"`
			MaxSteps   *int    `json:"max_steps"`
			TimeoutSec *int    `json:"timeout_sec"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		s.cfgMu.Lock()
		llm := s.cfg.LLM
		if body.Provider != nil {
			p := strings.ToLower(strings.TrimSpace(*body.Provider))
			if p != "openai" && p != "gemini" {
				s.cfgMu.Unlock()
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider must be openai or gemini"})
				return
			}
			llm.Provider = p
		}
		if body.BaseURL != nil {
			llm.BaseURL = strings.TrimSpace(*body.BaseURL)
		}
		if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
			llm.APIKey = strings.TrimSpace(*body.APIKey)
		}
		if body.Model != nil {
			llm.Model = strings.TrimSpace(*body.Model)
		}
		if body.MaxSteps != nil && *body.MaxSteps > 0 {
			llm.MaxSteps = *body.MaxSteps
		}
		if body.TimeoutSec != nil && *body.TimeoutSec > 0 {
			llm.TimeoutSec = *body.TimeoutSec
		}
		s.cfg.LLM = llm
		cfgPath := s.configPath
		var saveErr error
		if cfgPath != "" {
			saveErr = config.Save(cfgPath, s.cfg)
		}
		s.cfgMu.Unlock()
		s.agent.UpdateConfig(llm)
		if saveErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": saveErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"provider":       llm.Provider,
			"base_url":       llm.BaseURL,
			"api_key_masked": config.MaskSecret(llm.APIKey),
			"configured":     strings.TrimSpace(llm.APIKey) != "" && strings.TrimSpace(llm.Model) != "",
			"model":          llm.Model,
			"max_steps":      llm.MaxSteps,
			"timeout_sec":    llm.TimeoutSec,
		})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLLMTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Provider *string `json:"provider"`
		BaseURL  *string `json:"base_url"`
		APIKey   *string `json:"api_key"`
		Model    *string `json:"model"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
	}

	llm := s.agent.GetConfig()
	if body.Provider != nil && strings.TrimSpace(*body.Provider) != "" {
		llm.Provider = strings.ToLower(strings.TrimSpace(*body.Provider))
	}
	if body.BaseURL != nil && strings.TrimSpace(*body.BaseURL) != "" {
		llm.BaseURL = strings.TrimSpace(*body.BaseURL)
	}
	if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		llm.APIKey = strings.TrimSpace(*body.APIKey)
	}
	if body.Model != nil && strings.TrimSpace(*body.Model) != "" {
		llm.Model = strings.TrimSpace(*body.Model)
	}

	provider := strings.ToLower(strings.TrimSpace(llm.Provider))
	if provider == "" {
		provider = "openai"
	}
	if provider != "openai" && provider != "gemini" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"elapsed_ms": int64(0),
			"model":      llm.Model,
			"provider":   provider,
			"error":      "provider must be openai or gemini",
		})
		return
	}
	if strings.TrimSpace(llm.APIKey) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"elapsed_ms": int64(0),
			"model":      llm.Model,
			"provider":   provider,
			"error":      "API Key 未配置，请填写密钥后再试",
		})
		return
	}
	if strings.TrimSpace(llm.Model) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"elapsed_ms": int64(0),
			"model":      "",
			"provider":   provider,
			"error":      "Model 未配置，请填写模型名称",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	t0 := time.Now()
	preview, err := probeLLMOnce(ctx, provider, llm.BaseURL, llm.APIKey, llm.Model)
	elapsedMs := time.Since(t0).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"elapsed_ms": elapsedMs,
			"model":      llm.Model,
			"provider":   provider,
			"error":      redactSensitiveText(err.Error()),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"elapsed_ms": elapsedMs,
		"model":      llm.Model,
		"provider":   provider,
		"preview":    redactSensitiveText(preview),
	})
}

func probeLLMOnce(ctx context.Context, provider, baseURL, apiKey, model string) (string, error) {
	if provider == "gemini" {
		base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
		if base == "" {
			base = "https://generativelanguage.googleapis.com/v1beta"
		}
		cleanModel := strings.TrimPrefix(strings.TrimSpace(model), "models/")
		endpoint := fmt.Sprintf("%s/models/%s:generateContent", base, url.PathEscape(cleanModel))
		payload, _ := json.Marshal(map[string]any{
			"contents": []map[string]any{
				{"role": "user", "parts": []map[string]any{{"text": "Reply OK"}}},
			},
			"generationConfig": map[string]any{
				"maxOutputTokens": 16,
			},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		resp.Body.Close()
		if rerr != nil {
			return "", rerr
		}
		var parsed struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &parsed)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if parsed.Error != nil && parsed.Error.Message != "" {
				return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, parsed.Error.Message)
			}
			return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 140))
		}
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", errors.New(parsed.Error.Message)
		}
		var preview string
		if len(parsed.Candidates) > 0 {
			for _, p := range parsed.Candidates[0].Content.Parts {
				if strings.TrimSpace(p.Text) != "" {
					preview = strings.TrimSpace(p.Text)
					break
				}
			}
		}
		if preview == "" {
			preview = "OK"
		}
		return truncateRunes(preview, 64), nil
	}

	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	endpoint := base + "/chat/completions"
	if strings.HasSuffix(base, "/chat/completions") {
		endpoint = base
	}

	sendOpenAIProbe := func(withMaxTokens bool) (int, []byte, error) {
		bodyMap := map[string]any{
			"model": model,
			"messages": []map[string]any{
				{"role": "user", "content": "Reply OK"},
			},
		}
		if withMaxTokens {
			bodyMap["max_tokens"] = 16
		}
		payload, _ := json.Marshal(bodyMap)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		resp.Body.Close()
		return resp.StatusCode, raw, rerr
	}

	code, raw, err := sendOpenAIProbe(true)
	if err != nil {
		return "", err
	}
	if code == http.StatusBadRequest && strings.Contains(string(raw), "max_tokens") && ctx.Err() == nil {
		code, raw, err = sendOpenAIProbe(false)
		if err != nil {
			return "", err
		}
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if code < 200 || code >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("HTTP %d: %s", code, parsed.Error.Message)
		}
		return "", fmt.Errorf("HTTP %d: %s", code, truncateRunes(strings.TrimSpace(string(raw)), 140))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("empty choices from upstream")
	}
	preview := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if preview == "" {
		preview = strings.TrimSpace(parsed.Choices[0].Message.ReasoningContent)
	}
	if preview == "" {
		preview = "OK"
	}
	return truncateRunes(preview, 64), nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"files": s.memory.List(),
		})
	case http.MethodPost:
		var body struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		if strings.EqualFold(strings.TrimSpace(body.Name), brain.MemorySoulFile) && !isLoopbackRemoteAddr(r.RemoteAddr) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "SOUL.md can only be modified from a local loopback session",
			})
			return
		}
		if err := s.memory.Write(body.Name, body.Content); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    true,
			"files": s.memory.List(),
		})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func parseStrictHHMM(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, idx := range []int{0, 1, 3, 4} {
		if s[idx] < '0' || s[idx] > '9' {
			return 0, false
		}
	}
	hh := int(s[0]-'0')*10 + int(s[1]-'0')
	mm := int(s[3]-'0')*10 + int(s[4]-'0')
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.scheduler.Status())
	case http.MethodPost:
		rawBytes, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read body"})
			return
		}
		var body config.ScheduleConfig
		if err := json.Unmarshal(rawBytes, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}

		for i, e := range body.Entries {
			rawType := strings.ToLower(strings.TrimSpace(e.TaskType))
			if rawType != "" && rawType != "workflow" && rawType != "prompt" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid task_type %q (expected workflow or prompt)", e.TaskType)})
				return
			}
			taskType := e.NormalizedTaskType()
			promptText := strings.TrimSpace(e.Prompt)
			job := strings.TrimSpace(e.Job)
			if taskType == "prompt" {
				if promptText == "" {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt is required when task_type is prompt"})
					return
				}
				if job == "" {
					job = "ai_prompt"
				}
			} else {
				switch job {
				case "daily_all", "ms_rewards", "jd_daily":
				default:
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid job %q", e.Job)})
					return
				}
			}
			body.Entries[i].TaskType = taskType
			body.Entries[i].Prompt = promptText
			body.Entries[i].Job = job
			startStr := strings.TrimSpace(e.WindowStart)
			endStr := strings.TrimSpace(e.WindowEnd)
			startMin, okStart := parseStrictHHMM(startStr)
			if !okStart {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid window_start %q (expected HH:MM 00:00..23:59)", e.WindowStart)})
				return
			}
			endMin, okEnd := parseStrictHHMM(endStr)
			if !okEnd {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid window_end %q (expected HH:MM 00:00..23:59)", e.WindowEnd)})
				return
			}
			if endMin <= startMin {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("window_end %q must be after window_start %q", endStr, startStr)})
				return
			}
			body.Entries[i].WindowStart = startStr
			body.Entries[i].WindowEnd = endStr
			if strings.TrimSpace(e.ID) == "" {
				if taskType == "prompt" {
					body.Entries[i].ID = fmt.Sprintf("prompt-%d", i+1)
				} else {
					body.Entries[i].ID = fmt.Sprintf("job-%d-%s", i+1, job)
				}
			} else {
				body.Entries[i].ID = strings.TrimSpace(e.ID)
			}
		}
		s.cfgMu.Lock()
		s.cfg.Schedule = body
		cfgPath := s.configPath
		var saveErr error
		if cfgPath != "" {
			saveErr = config.Save(cfgPath, s.cfg)
		}
		s.cfgMu.Unlock()
		s.scheduler.UpdateConfig(body)
		if saveErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": saveErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.scheduler.Status())
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if blocked, reason := s.guard.IsActionBlocked(); blocked {
		writeJSON(w, http.StatusConflict, map[string]any{"error": reason})
		return
	}
	var body struct {
		ID      string `json:"id"`
		EntryID string `json:"entry_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	entryID := strings.TrimSpace(body.EntryID)
	if entryID == "" {
		entryID = strings.TrimSpace(body.ID)
	}
	if entryID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "entry_id is required"})
		return
	}
	if err := s.scheduler.TriggerNow(entryID); err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"entry_id": entryID,
		"schedule": s.scheduler.Status(),
	})
}

func (s *Server) handleScreenPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if blocked, reason := s.guard.IsActionBlocked(); blocked {
		writeJSON(w, http.StatusConflict, map[string]any{"error": reason})
		return
	}
	q := r.URL.Query()
	width := s.cfg.ScreenshotWidth
	if rawW := strings.TrimSpace(q.Get("width")); rawW != "" {
		if n, err := strconv.Atoi(rawW); err == nil && n >= 240 && n <= 1440 {
			width = n
		}
	}
	quality := s.cfg.ScreenshotJPEGQuality
	if rawQ := strings.TrimSpace(q.Get("quality")); rawQ != "" {
		if n, err := strconv.Atoi(rawQ); err == nil && n >= 25 && n <= 95 {
			quality = n
		}
	}
	withTree := q.Get("with_tree") == "1" || strings.EqualFold(q.Get("with_tree"), "true")

	ctx := r.Context()
	var (
		wg      sync.WaitGroup
		shotRes *backend.ScreenshotResult
		shotErr error
		fgInfo  backend.ForegroundInfo
		treeRes *backend.UITreeResult
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		shotRes, shotErr = s.backend.Screenshot(ctx, width, quality)
	}()
	go func() {
		defer wg.Done()
		fgInfo, _ = s.backend.ForegroundApp(ctx)
		if withTree {
			treeRes, _ = s.backend.UITree(ctx, backend.UITreeOptions{})
		}
	}()
	wg.Wait()

	if shotErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": shotErr.Error()})
		return
	}
	if shotRes.OrigWidth > 0 && shotRes.OrigHeight > 0 && shotRes.Width > 0 && shotRes.Height > 0 {
		s.shotMu.Lock()
		s.lastShot = screenshotMeta{
			origWidth:  shotRes.OrigWidth,
			origHeight: shotRes.OrigHeight,
			width:      shotRes.Width,
			height:     shotRes.Height,
			capturedAt: time.Now(),
		}
		s.shotMu.Unlock()
	}

	pkg := fgInfo.Package
	if pkg == "" && treeRes != nil {
		pkg = treeRes.Package
	}
	out := map[string]any{
		"ok":             true,
		"image_url":      "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(shotRes.JPEG),
		"width":          shotRes.Width,
		"height":         shotRes.Height,
		"orig_width":     shotRes.OrigWidth,
		"orig_height":    shotRes.OrigHeight,
		"jpeg_bytes":     len(shotRes.JPEG),
		"capture_method": shotRes.CaptureMethod,
		"elapsed_ms":     shotRes.ElapsedMs,
		"package":        pkg,
		"activity":       fgInfo.Activity,
		"ui_tree":        "",
		"node_count":     0,
		"nodes":          []backend.UINode{},
	}
	if treeRes != nil {
		out["ui_tree"] = treeRes.Text
		out["node_count"] = treeRes.NodeCount
		out["tree_elapsed_ms"] = treeRes.ElapsedMs
		if len(treeRes.Nodes) > 0 {
			out["nodes"] = treeRes.Nodes
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleScreenAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if blocked, reason := s.guard.IsActionBlocked(); blocked {
		writeJSON(w, http.StatusConflict, map[string]any{"error": reason})
		return
	}
	if u, ok := s.backend.(interface{ IsScreenUnlocked(context.Context) bool }); ok && !u.IsScreenUnlocked(r.Context()) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "手机当前处于息屏或锁屏状态，请先解锁屏幕"})
		return
	}

	var body struct {
		Action     string `json:"action"`
		X          *int   `json:"x"`
		Y          *int   `json:"y"`
		X1         *int   `json:"x1"`
		Y1         *int   `json:"y1"`
		X2         *int   `json:"x2"`
		Y2         *int   `json:"y2"`
		NodeID     int    `json:"node_id"`
		ID         int    `json:"id"`
		Keycode    string `json:"keycode"`
		DurationMs int    `json:"duration_ms"`
		CoordSpace string `json:"coord_space"`
		ImageWidth int    `json:"image_width"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	ctx := r.Context()
	action := strings.ToLower(strings.TrimSpace(body.Action))
	space := strings.TrimSpace(body.CoordSpace)
	if space == "" {
		space = "image"
	}

	switch action {
	case "tap":
		if body.X == nil || body.Y == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "x and y are required for tap"})
			return
		}
		px, py, err := s.resolveCoords(ctx, *body.X, *body.Y, space, body.ImageWidth)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.backend.Tap(ctx, px, py); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "tap", "x": px, "y": py})

	case "tap_node":
		nid := body.NodeID
		if nid <= 0 {
			nid = body.ID
		}
		if nid <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "positive node_id is required"})
			return
		}
		node, px, py, err := s.backend.TapNode(ctx, nid)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"action": "tap_node",
			"id":     nid,
			"label":  node.Label,
			"x":      px,
			"y":      py,
		})

	case "key":
		kc := strings.TrimSpace(body.Keycode)
		if kc == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "keycode is required for key"})
			return
		}
		if err := s.backend.KeyEvent(ctx, kc); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "key", "keycode": kc})

	case "swipe":
		sx, sy := body.X, body.Y
		if sx == nil {
			sx = body.X1
		}
		if sy == nil {
			sy = body.Y1
		}
		if sx == nil || sy == nil || body.X2 == nil || body.Y2 == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "x/y (or x1/y1) and x2/y2 are required for swipe"})
			return
		}
		dur := body.DurationMs
		if dur <= 0 {
			dur = 320
		}
		px1, py1, err := s.resolveCoords(ctx, *sx, *sy, space, body.ImageWidth)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		px2, py2, err := s.resolveCoords(ctx, *body.X2, *body.Y2, space, body.ImageWidth)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.backend.Swipe(ctx, px1, py1, px2, py2, dur); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"action":      "swipe",
			"x1":          px1,
			"y1":          py1,
			"x2":          px2,
			"y2":          py2,
			"duration_ms": dur,
		})

	case "long_press":
		if body.X == nil || body.Y == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "x and y are required for long_press"})
			return
		}
		dur := body.DurationMs
		if dur <= 0 {
			dur = 700
		}
		px, py, err := s.resolveCoords(ctx, *body.X, *body.Y, space, body.ImageWidth)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.backend.LongPress(ctx, px, py, dur); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "long_press", "x": px, "y": py, "duration_ms": dur})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unsupported screen action %q", body.Action)})
	}
}

func (s *Server) handleIslandTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if blocked, reason := s.guard.IsActionBlocked(); blocked {
		writeJSON(w, http.StatusConflict, map[string]any{"error": reason})
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
	}
	state := strings.ToLower(strings.TrimSpace(body.State))
	if state == "" {
		state = "thinking"
	}

	switch state {
	case "thinking", "acting", "workflow", "confirm", "done", "stopped", "error":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid island state %q (expected thinking, acting, workflow, confirm, or done)", body.State)})
		return
	}

	if err := s.island.PreviewDemo(r.Context(), state); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
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

func (s *Server) evictExpiredSessionsLocked(now time.Time) {
	cutoff := now.Add(-mcpSessionTTL)
	for sid, lastSeen := range s.sessions {
		if lastSeen.Before(cutoff) {
			delete(s.sessions, sid)
		}
	}
}

func (s *Server) validateSession(sid string) bool {
	now := time.Now()
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	s.evictExpiredSessionsLocked(now)
	lastSeen, exists := s.sessions[sid]
	if !exists || now.Sub(lastSeen) > mcpSessionTTL {
		delete(s.sessions, sid)
		return false
	}
	s.sessions[sid] = now
	return true
}

func (s *Server) handleMCPGetSSE(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if reqSessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id")); reqSessionID != "" && !s.validateSession(reqSessionID) {
		writeJSON(w, http.StatusNotFound, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32001, Message: "unknown or expired Mcp-Session-Id"},
		})
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
		if len(batch) == 0 {
			writeJSON(w, http.StatusBadRequest, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32600, Message: "empty JSON-RPC batch"},
			})
			return
		}
		if reqSessionID != "" {
			needsSession := false
			for _, req := range batch {
				if req.Method != "initialize" {
					needsSession = true
					break
				}
			}
			if needsSession && !s.validateSession(reqSessionID) {
				writeJSON(w, http.StatusNotFound, rpcResponse{
					JSONRPC: "2.0",
					Error:   &rpcError{Code: -32001, Message: "unknown or expired Mcp-Session-Id"},
				})
				return
			}
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
		w.Header().Set("MCP-Protocol-Version", DefaultProtoVersion)
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
		if !s.validateSession(reqSessionID) {
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
		now := time.Now()
		s.sessMu.Lock()
		s.evictExpiredSessionsLocked(now)
		s.sessions[sid] = now
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
		if res.OrigWidth > 0 && res.OrigHeight > 0 && res.Width > 0 && res.Height > 0 {
			s.shotMu.Lock()
			s.lastShot = screenshotMeta{
				origWidth:  res.OrigWidth,
				origHeight: res.OrigHeight,
				width:      res.Width,
				height:     res.Height,
				capturedAt: time.Now(),
			}
			s.shotMu.Unlock()
		}
		scaleX, scaleY := 1.0, 1.0
		if res.Width > 0 && res.OrigWidth > 0 {
			scaleX = float64(res.OrigWidth) / float64(res.Width)
		}
		if res.Height > 0 && res.OrigHeight > 0 {
			scaleY = float64(res.OrigHeight) / float64(res.Height)
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
			"scale_x":        scaleX,
			"scale_y":        scaleY,
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
		px, py, err := s.resolveCoords(ctx, x, y, getString(args, "coord_space", "physical"), getInt(args, "image_width", 0))
		if err != nil {
			return textErrorResult(err.Error())
		}
		if err := s.backend.Tap(ctx, px, py); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "x": px, "y": py}, false)

	case "phone_ui_tree":
		opts := backend.UITreeOptions{
			MaxDepth:  getInt(args, "max_depth", 0),
			MaxNodes:  getInt(args, "max_nodes", 0),
			MaxTokens: getInt(args, "max_tokens", 0),
		}
		res, err := s.backend.UITree(ctx, opts)
		if err != nil {
			return textErrorResult(fmt.Sprintf("ui_tree unavailable (%v); fall back to phone_screenshot or cdp_*", err))
		}
		outText := fmt.Sprintf("Timing: %dms | raw=%d | truncated=%v\n%s", res.ElapsedMs, res.RawCount, res.Truncated, res.Text)
		if res.Empty && res.FallbackHint != "" {
			outText += "\nHint: " + res.FallbackHint
		}
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": outText},
			},
			"isError": false,
		}

	case "phone_tap_node":
		id, ok := getIntStrict(args, "id")
		if !ok || id <= 0 {
			return textErrorResult("positive integer node id is required")
		}
		node, x, y, err := s.backend.TapNode(ctx, id)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"ok":    true,
			"id":    id,
			"label": node.Label,
			"type":  node.Type,
			"x":     x,
			"y":     y,
		}, false)

	case "phone_scroll_node":
		id := getInt(args, "id", 0)
		dir := getString(args, "direction", "down")
		dur := getInt(args, "duration_ms", 340)
		dist := 0.65
		if v, ok := args["distance"].(float64); ok && v > 0 {
			dist = v
		}
		bounds, swipe, err := s.backend.ScrollNode(ctx, id, dir, dist, dur)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"ok":          true,
			"id":          id,
			"direction":   dir,
			"bounds":      bounds,
			"swipe":       swipe,
			"duration_ms": dur,
		}, false)

	case "phone_long_press":
		x, okX := getIntStrict(args, "x")
		y, okY := getIntStrict(args, "y")
		if !okX || !okY {
			return textErrorResult("x and y integer coordinates are required")
		}
		dur := getInt(args, "duration_ms", 800)
		px, py, err := s.resolveCoords(ctx, x, y, getString(args, "coord_space", "physical"), getInt(args, "image_width", 0))
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
		imgW := getInt(args, "image_width", 0)
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
		if s.isGuardedPackage(pkg) {
			return textErrorResult(fmt.Sprintf("package %q is restricted by game-guard", pkg))
		}
		if err := s.backend.LaunchApp(ctx, pkg); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "package": pkg}, false)

	case "phone_open_deeplink":
		uriStr := strings.TrimSpace(getString(args, "uri", ""))
		if uriStr == "" {
			return textErrorResult("uri is required")
		}
		pkg := strings.TrimSpace(getString(args, "package", ""))
		if s.isGuardedPackage(pkg) {
			return textErrorResult(fmt.Sprintf("package %q is restricted by game-guard", pkg))
		}
		if err := s.backend.OpenDeepLink(ctx, uriStr, pkg); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "uri": uriStr, "package": pkg}, false)

	case "phone_tap_and_type":
		text := getString(args, "text", "")
		if text == "" {
			return textErrorResult("text is required")
		}
		method := getString(args, "method", "auto")
		submit := getBool(args, "submit", false)
		waitMs := getInt(args, "wait_ms", 350)
		if waitMs < 0 {
			waitMs = 0
		} else if waitMs > 2000 {
			waitMs = 2000
		}

		nodeID := getInt(args, "node_id", 0)
		if nodeID <= 0 {
			nodeID = getInt(args, "id", 0)
		}

		var px, py int
		if nodeID > 0 {
			_, tx, ty, err := s.backend.TapNode(ctx, nodeID)
			if err != nil {
				return textErrorResult(err.Error())
			}
			px, py = tx, ty
		} else {
			x, okX := getIntStrict(args, "x")
			y, okY := getIntStrict(args, "y")
			if !okX || !okY {
				return textErrorResult("either node_id > 0 or (x, y) integer coordinates are required")
			}
			tx, ty, err := s.resolveCoords(ctx, x, y, getString(args, "coord_space", "physical"), getInt(args, "image_width", 0))
			if err != nil {
				return textErrorResult(err.Error())
			}
			if err := s.backend.Tap(ctx, tx, ty); err != nil {
				return textErrorResult(err.Error())
			}
			px, py = tx, ty
		}

		if waitMs > 0 {
			timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return textErrorResult(ctx.Err().Error())
			case <-timer.C:
			}
		}
		if blocked, reason := s.guard.IsActionBlocked(); blocked {
			return textErrorResult(reason)
		}
		used, err := s.backend.InputText(ctx, text, method, submit)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"ok":        true,
			"node_id":   nodeID,
			"x":         px,
			"y":         py,
			"method":    used,
			"length":    len([]rune(text)),
			"submitted": submit,
		}, false)

	case "cdp_list_sockets":
		return textJSONResult(map[string]any{
			"sockets": cdp.ListSockets(),
		}, false)

	case "cdp_status":
		client, err := s.cdp.ForSocket(getString(args, "socket", ""))
		if err != nil {
			return textJSONResult(map[string]any{
				"available": false,
				"socket":    getString(args, "socket", s.cdp.SocketDisplay()),
				"message":   err.Error(),
			}, false)
		}
		return textJSONResult(client.Probe(ctx), false)

	case "cdp_list_targets":
		client, err := s.cdp.ForSocket(getString(args, "socket", ""))
		if err != nil {
			return textJSONResult(map[string]any{
				"available": false,
				"socket":    getString(args, "socket", s.cdp.SocketDisplay()),
				"targets":   []any{},
				"message":   err.Error(),
			}, false)
		}
		targets, err := client.ListTargets(ctx)
		if err != nil {
			if errors.Is(err, cdp.ErrUnavailable) {
				return textJSONResult(map[string]any{
					"available": false,
					"socket":    client.SocketDisplay(),
					"targets":   []any{},
					"message":   err.Error(),
				}, false)
			}
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"available": true,
			"socket":    client.SocketDisplay(),
			"targets":   targets,
		}, false)

	case "cdp_command":
		client, err := s.cdp.ForSocket(getString(args, "socket", ""))
		if err != nil {
			return textJSONResult(map[string]any{
				"available": false,
				"socket":    getString(args, "socket", s.cdp.SocketDisplay()),
				"message":   err.Error(),
			}, true)
		}
		targetID := getString(args, "target_id", "")
		method := getString(args, "method", "")
		var params map[string]any
		if rawParams, ok := args["params"].(map[string]any); ok {
			params = rawParams
		}
		res, err := client.SendCommand(ctx, targetID, method, params)
		if err != nil {
			if errors.Is(err, cdp.ErrUnavailable) {
				return textJSONResult(map[string]any{
					"available": false,
					"socket":    client.SocketDisplay(),
					"message":   err.Error(),
				}, true)
			}
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{
			"ok":     true,
			"socket": client.SocketDisplay(),
			"method": method,
			"result": json.RawMessage(res),
		}, false)

	case "run_daily_all":
		rep, err := s.runWorkflowViaJobs(ctx, "daily_all")
		if err != nil {
			return textJSONResult(map[string]any{"error": err.Error(), "report": rep}, true)
		}
		return textJSONResult(rep, false)

	case "run_ms_rewards":
		rep, err := s.runWorkflowViaJobs(ctx, "ms_rewards")
		if err != nil {
			return textJSONResult(map[string]any{"error": err.Error(), "report": rep}, true)
		}
		return textJSONResult(rep, false)

	case "run_jd_daily":
		rep, err := s.runWorkflowViaJobs(ctx, "jd_daily")
		if err != nil {
			return textJSONResult(map[string]any{"error": err.Error(), "report": rep}, true)
		}
		return textJSONResult(rep, false)

	case "post_notification":
		tag := getString(args, "tag", "android_agent")
		title := getString(args, "title", "Android Agent")
		text := getString(args, "text", "")
		if err := s.backend.PostNotification(ctx, tag, title, text); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true}, false)

	case "read_memory":
		name := getString(args, "file", "all")
		content, err := s.memory.Read(name)
		if err != nil {
			return textErrorResult(err.Error())
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": content}},
			"isError": false,
		}

	case "append_memory":
		file := getString(args, "file", brain.MemoryUserFile)
		entry := getString(args, "entry", "")
		if err := s.memory.Append(file, entry); err != nil {
			return textErrorResult(err.Error())
		}
		return textJSONResult(map[string]any{"ok": true, "file": file}, false)

	default:
		return textErrorResult(fmt.Sprintf("unknown tool: %s", name))
	}
}

func (s *Server) runWorkflowViaJobs(ctx context.Context, jobName string) (any, error) {
	type jobOutcome struct {
		res any
		err error
	}
	done := make(chan jobOutcome, 1)
	_, err := s.jobs.Start(jobName, func(jobCtx context.Context, logFn func(string)) (any, error) {
		runCtx, cancel := context.WithCancel(jobCtx)
		defer cancel()
		stopWatch := make(chan struct{})
		defer close(stopWatch)
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-stopWatch:
			}
		}()
		res, rerr := s.runWorkflowJob(runCtx, jobName, func(line string) {
			logFn(line)
			s.logger("workflow[%s]: %s", jobName, line)
		})
		done <- jobOutcome{res: res, err: rerr}
		return res, rerr
	})
	if err != nil {
		return nil, err
	}
	select {
	case out := <-done:
		return out.res, out.err
	case <-ctx.Done():
		s.jobs.Cancel("mcp caller cancelled")
		return nil, ctx.Err()
	}
}

var (
	secretTokenPattern = regexp.MustCompile(`(?i)\b(bearer\s+[a-z0-9._\-]{8,}|(?:sk|nvapi|ghp|gho|pat|xoxb|xoxp|aiza)[a-z0-9_\-]{8,})\b`)
	secretKVPattern    = regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_\-]?key|otp|密码|口令|密钥|验证码)\s*[:=：是为]?\s*)([^\s,;，；"'` + "`" + `]{4,64})`)
)

func redactSensitiveText(s string) string {
	if s == "" {
		return s
	}
	s = secretTokenPattern.ReplaceAllString(s, "[REDACTED]")
	s = secretKVPattern.ReplaceAllString(s, "${1}[REDACTED]")
	return s
}

func (s *Server) isGuardedPackage(pkg string) bool {
	pkg = strings.ToLower(strings.TrimSpace(pkg))
	if pkg == "" {
		return false
	}
	s.cfgMu.RLock()
	var pkgs []string
	if s.cfg != nil {
		pkgs = s.cfg.GuardPackages
	}
	s.cfgMu.RUnlock()
	for _, gp := range pkgs {
		if strings.EqualFold(strings.TrimSpace(gp), pkg) {
			return true
		}
	}
	return false
}

func sanitizeChatHistory(msgs []brain.ChatMessage) []brain.ChatMessage {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]brain.ChatMessage, len(msgs))
	for i, m := range msgs {
		cp := m
		if len(m.ToolCalls) > 0 {
			cp.ToolCalls = make([]brain.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tcc := tc
				if tc.Args != nil {
					argsCopy := make(map[string]any, len(tc.Args))
					for k, v := range tc.Args {
						if (tc.Name == "phone_input_text" || tc.Name == "phone_tap_and_type" || tc.Name == "phone_clipboard_set") && k == "text" {
							argsCopy[k] = "[REDACTED]"
						} else {
							argsCopy[k] = v
						}
					}
					tcc.Args = argsCopy
				}
				cp.ToolCalls[j] = tcc
			}
		}
		if m.Role == "tool" && m.ToolName == "phone_clipboard_get" {
			cp.Text = `{"ok":true,"redacted":true}`
		}
		out[i] = cp
	}
	return out
}

func autoObserveSettleDelay(toolName string) time.Duration {
	switch toolName {
	case "phone_tap", "phone_tap_node", "phone_long_press",
		"phone_swipe", "phone_scroll_node", "phone_keyevent",
		"phone_input_text", "phone_tap_and_type":
		return autoObserveFastSettle
	case "phone_launch_app", "phone_open_deeplink":
		return autoObserveLaunchSettle
	default:
		return 0
	}
}

func shouldAutoObserve(args map[string]any) bool {
	if args == nil {
		return true
	}
	v, ok := args["observe"]
	if !ok || v == nil {
		return true
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		s := strings.ToLower(strings.TrimSpace(val))
		return s != "false" && s != "0" && s != "no"
	case float64:
		return val != 0
	case int:
		return val != 0
	}
	return true
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func (s *Server) isActionGuardBlocked() bool {
	if s.guard == nil {
		return false
	}
	blocked, _ := s.guard.IsActionBlocked()
	return blocked
}

// ToolSpecs implements brain.ToolExecutor for in-process zero-HTTP LLM tool calls.
// agent_pause and agent_resume are intentionally hidden from the in-process LLM brain
// (while remaining available to external MCP clients and HTTP /api/v1/{pause,resume}).
func (s *Server) ToolSpecs() []brain.ToolSpec {
	defs := toolDefinitions()
	specs := make([]brain.ToolSpec, 0, len(defs))
	for _, d := range defs {
		name, _ := d["name"].(string)
		if name == "agent_pause" || name == "agent_resume" {
			continue
		}
		desc, _ := d["description"].(string)
		schema, _ := d["inputSchema"].(map[string]any)
		if autoObserveSettleDelay(name) > 0 && schema != nil {
			clonedSchema := make(map[string]any, len(schema))
			for k, v := range schema {
				clonedSchema[k] = v
			}
			clonedProps := map[string]any{}
			if origProps, ok := schema["properties"].(map[string]any); ok {
				for k, v := range origProps {
					clonedProps[k] = v
				}
			}
			clonedProps["observe"] = map[string]any{
				"type":        "boolean",
				"description": "Auto-capture refreshed [操作后最新界面] UI tree after action settles (default true)",
			}
			clonedSchema["properties"] = clonedProps
			schema = clonedSchema
		}
		specs = append(specs, brain.ToolSpec{
			Name:        name,
			Description: desc,
			Parameters:  schema,
		})
	}
	return specs
}

// ExecuteTool implements brain.ToolExecutor for direct in-process tool dispatch.
func (s *Server) ExecuteTool(ctx context.Context, name string, args map[string]any) brain.ToolOutput {
	if name == "agent_pause" || name == "agent_resume" {
		return brain.ToolOutput{
			Text:    fmt.Sprintf("tool %s is restricted from in-process LLM brain; use /api/v1/pause or /api/v1/resume", name),
			IsError: true,
		}
	}
	res := s.callTool(ctx, name, args)
	isErr, _ := res["isError"].(bool)
	var texts []string
	var imgBytes []byte
	if parts, ok := res["content"].([]map[string]any); ok {
		for _, p := range parts {
			tp, _ := p["type"].(string)
			if tp == "text" {
				if t, ok := p["text"].(string); ok {
					texts = append(texts, t)
				}
			} else if tp == "image" {
				if b64, ok := p["data"].(string); ok {
					imgBytes, _ = base64.StdEncoding.DecodeString(b64)
				}
			}
		}
	}
	outText := strings.Join(texts, "\n")
	switch name {
	case "phone_clipboard_get":
		if !isErr {
			var parsed struct {
				OK   bool   `json:"ok"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(outText), &parsed); err == nil {
				redacted := redactSensitiveText(parsed.Text)
				payload, _ := json.Marshal(map[string]any{"ok": parsed.OK, "text": redacted})
				// Keep first line non-sensitive so RunEvent.Summary never leaks clipboard plaintext.
				outText = fmt.Sprintf("clipboard read ok (len=%d, redacted in summary)\n%s", len([]rune(parsed.Text)), string(payload))
				s.logger("brain tool %s: ok (len=%d, redacted)", name, len([]rune(parsed.Text)))
			}
		}
	case "phone_input_text", "phone_tap_and_type", "phone_clipboard_set":
		rawInput := getString(args, "text", "")
		if rawInput != "" && strings.Contains(outText, rawInput) {
			outText = strings.ReplaceAll(outText, rawInput, "[REDACTED]")
		}
		outText = redactSensitiveText(outText)
		s.logger("brain tool %s: is_err=%v (input len=%d, redacted)", name, isErr, len([]rune(rawInput)))
	}

	if !isErr && s.backend != nil && shouldAutoObserve(args) {
		if settle := autoObserveSettleDelay(name); settle > 0 && ctx.Err() == nil && !s.isActionGuardBlocked() {
			if sleepWithContext(ctx, settle) && !s.isActionGuardBlocked() {
				if treeRes, err := s.backend.UITree(ctx, backend.UITreeOptions{MaxTokens: 450}); err == nil && treeRes != nil && !treeRes.Empty && strings.TrimSpace(treeRes.Text) != "" {
					uiText := redactSensitiveText(strings.TrimSpace(treeRes.Text))
					outText += "\n\n[操作后最新界面]\n" + uiText
				}
			}
		}
	}

	return brain.ToolOutput{
		Text:      outText,
		ImageJPEG: imgBytes,
		IsError:   isErr,
	}
}

func (s *Server) resolveCoords(ctx context.Context, x, y int, space string, imageWidth int) (int, int, error) {
	if strings.ToLower(strings.TrimSpace(space)) != "image" {
		return x, y, nil
	}
	s.shotMu.RLock()
	shot := s.lastShot
	s.shotMu.RUnlock()

	if shot.origWidth > 0 && shot.origHeight > 0 && shot.width > 0 && shot.height > 0 {
		if imageWidth <= 0 || imageWidth == shot.width {
			scaleX := float64(shot.origWidth) / float64(shot.width)
			scaleY := float64(shot.origHeight) / float64(shot.height)
			return int(math.Round(float64(x) * scaleX)), int(math.Round(float64(y) * scaleY)), nil
		}
		if shot.origWidth <= imageWidth {
			return x, y, nil
		}
		scale := float64(shot.origWidth) / float64(imageWidth)
		return int(math.Round(float64(x) * scale)), int(math.Round(float64(y) * scale)), nil
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
			"name":        "phone_ui_tree",
			"description": "Read current screen's in-memory UI hierarchy via UiAutomation (<50ms, no screenshot), purify invisible/redundant nodes, cache ID->bounds mapping, and return a numbered plain-text node list within 300-500 tokens. Falls back cleanly on self-drawn/WebView pages.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"max_depth":  map[string]any{"type": "integer", "description": "Max traversal depth (default 36)"},
					"max_nodes":  map[string]any{"type": "integer", "description": "Max raw accessibility nodes visited (default 1200)"},
					"max_tokens": map[string]any{"type": "integer", "description": "Approximate token budget for compressed output (default 450)"},
				},
			},
		},
		{
			"name":        "phone_tap_node",
			"description": "Tap a UI element by its [id] from the latest phone_ui_tree output, computing the center with bounded random offset.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "integer", "description": "Node ID from phone_ui_tree"},
				},
				"required": []string{"id"},
			},
		},
		{
			"name":        "phone_scroll_node",
			"description": "Scroll inside a scrollable UI container [id] (or largest scrollable list if id is omitted/0) cached from phone_ui_tree.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          map[string]any{"type": "integer", "description": "Optional scrollable container ID from phone_ui_tree (0 = auto-pick main list)"},
					"direction":   map[string]any{"type": "string", "enum": []string{"down", "up", "left", "right"}, "description": "Content scroll direction (default down)"},
					"distance":    map[string]any{"type": "number", "description": "Fraction of container span to drag (default 0.65)"},
					"duration_ms": map[string]any{"type": "integer", "description": "Swipe duration in ms (default 340)"},
				},
			},
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
			"name":        "phone_open_deeplink",
			"description": "Open an Android deep link or URL intent directly (e.g. openapp.jdmobile://..., imeituan://..., https://...) with an optional target package.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"uri":     map[string]any{"type": "string", "description": "Deep link URI or URL to open"},
					"package": map[string]any{"type": "string", "description": "Optional target Android package name"},
				},
				"required": []string{"uri"},
			},
		},
		{
			"name":        "phone_tap_and_type",
			"description": "Composite single-step action: tap an input field (by node_id from phone_ui_tree or x/y coordinates), wait ~350ms for focus/IME, and type UTF-8/Chinese text (optionally pressing ENTER to submit).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"node_id":     map[string]any{"type": "integer", "description": "Input element [id] from phone_ui_tree (preferred)"},
					"x":           map[string]any{"type": "integer", "description": "Fallback X coordinate when node_id is not used"},
					"y":           map[string]any{"type": "integer", "description": "Fallback Y coordinate when node_id is not used"},
					"coord_space": map[string]any{"type": "string", "enum": []string{"physical", "image"}, "description": "Coordinate space for x/y (default physical)"},
					"image_width": map[string]any{"type": "integer", "description": "Reference screenshot width when coord_space='image'"},
					"text":        map[string]any{"type": "string", "description": "Text to enter into the field"},
					"method":      map[string]any{"type": "string", "enum": []string{"auto", "clipboard", "input"}, "description": "Input method (default auto)"},
					"submit":      map[string]any{"type": "boolean", "description": "Send ENTER/search after typing (default false)"},
					"wait_ms":     map[string]any{"type": "integer", "description": "Focus settle delay in ms between tap and typing (default 350)"},
				},
				"required": []string{"text"},
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
			"name":        "cdp_list_sockets",
			"description": "List all active *_devtools_remote* abstract Unix sockets (e.g. Edge @chrome_devtools_remote and app WebView @webview_devtools_remote_<pid>) with owning process names.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "cdp_status",
			"description": "Probe a DevTools abstract Unix socket (default @chrome_devtools_remote, or specify socket='webview' / package name / exact socket name).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"socket": map[string]any{"type": "string", "description": "Optional socket selector: '', 'webview', package substring (e.g. 'com.jingdong.app.mall'), or exact socket name"},
				},
			},
		},
		{
			"name":        "cdp_list_targets",
			"description": "List inspectable browser/WebView targets via a DevTools abstract Unix socket.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"socket": map[string]any{"type": "string", "description": "Optional socket selector (default @chrome_devtools_remote; use 'webview' or package name for App WebViews)"},
				},
			},
		},
		{
			"name":        "cdp_command",
			"description": "Execute a Chrome DevTools Protocol JSON-RPC command (e.g. Page.navigate, Runtime.evaluate, Input.dispatchMouseEvent) over a DevTools abstract Unix socket.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"socket":    map[string]any{"type": "string", "description": "Optional socket selector (default @chrome_devtools_remote; use 'webview' or package name for App WebViews)"},
					"target_id": map[string]any{"type": "string", "description": "Optional target ID (defaults to first page target)"},
					"method":    map[string]any{"type": "string", "description": "CDP method name, e.g. Runtime.evaluate"},
					"params":    map[string]any{"type": "object", "description": "CDP method parameters"},
				},
				"required": []string{"method"},
			},
		},
		{
			"name":        "run_daily_all",
			"description": "Run complete 0-Token daily automation on Xiaomi 13 (Microsoft Rewards Edge+Bing CDP and JD Farm+Bean scratch) and post an Android notification.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "run_ms_rewards",
			"description": "Run 0-Token Microsoft Rewards automation (Edge CDP Daily Set + trending searches 100/100 + Bing App WebView check-in and 30/30 text news read).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "run_jd_daily",
			"description": "Run 0-Token JD Daily automation (东东农场 sign-in, water bubbles, 6s browse tasks, milestone watering tasks only, plus 天天刮京豆).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "post_notification",
			"description": "Post a summary notification to the Android system notification shade.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string"},
					"text":  map[string]any{"type": "string"},
					"tag":   map[string]any{"type": "string"},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "read_memory",
			"description": "Read on-device Markdown memory files (SOUL.md, MEMORY.md, LEARNINGS.md, or 'all').",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file": map[string]any{"type": "string", "enum": []string{"all", "SOUL.md", "MEMORY.md", "LEARNINGS.md"}, "description": "Memory file name (default all)"},
				},
			},
		},
		{
			"name":        "append_memory",
			"description": "Append a timestamped bullet entry to MEMORY.md (user preferences/habits) or LEARNINGS.md (app pitfalls/fixes). SOUL.md cannot be modified by tools.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file":  map[string]any{"type": "string", "enum": []string{"MEMORY.md", "LEARNINGS.md"}, "description": "Target Markdown file"},
					"entry": map[string]any{"type": "string", "description": "Concise Chinese/English note to remember"},
				},
				"required": []string{"file", "entry"},
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
