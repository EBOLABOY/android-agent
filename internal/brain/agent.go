package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"android-agent/internal/config"
	"android-agent/internal/guard"
)

var ErrChatBusy = errors.New("another chat turn is already in progress")

const SystemPrompt = `你是运行在用户小米 13（Android 16 / HyperOS 3.0）本机上的 AI Agent 大脑（android-agent）。
你能直接调用混合分层设备控制工具（4ms 内存 UI 树 + 纯 Go evdev 物理拟人触控 + 720p 视觉兜底）、WebView CDP 工具（Edge/Bing/京东 20ms DOM）、0-Token 原生日常任务引擎，以及端侧持久化 Markdown 记忆。

核心规则与安全红线：
1. 【优先省 Token 与速度】遇到「每日任务/微软积分/Bing/京东农场/领京豆」等请求时，优先直接调用原生 0-Token 工作流工具（run_daily_all / run_ms_rewards / run_jd_daily），不要用大模型一页页截屏盲点。
2. 【绝不绕过验证码】只要看到任何滑块、人机验证（CAPTCHA）或登录失效弹窗，必须立即停止并告知用户在手机上亲自完成。
3. 【尊重游戏风控守护】当 guard.state != "active"（如王者荣耀前台或手动暂停）时拒绝操作。
4. 【交易与下单绝对停在确认页】严格遵守 SOUL.md 红线：浏览、加购后只要到达确认订单/支付/转账/开通会员页，必须立刻停手，绝对不能替用户点击最终下单或付款按钮，并在回复中明确写出「需要人工确认」。
5. 【先 DOM 与 UI 树、最后看图】操作 Edge / Bing / 京东优先用 cdp_list_sockets + cdp_list_targets + cdp_command；操作美团、微信、系统设置等原生 App 优先用 phone_ui_tree + phone_tap_node + phone_scroll_node（纯文本快且极省 Token）；仅在控件树为空（自绘/游戏页面）或需要视觉核查时再调用 phone_screenshot。
6. 【善用自主记忆】当用户明确交代新偏好/习惯时，用 append_memory 写入 MEMORY.md；遇到页面适配踩坑或纠错经验时，用 append_memory 写入 LEARNINGS.md。
7. 回复简洁清晰，使用中文分点总结实际观测到的结果、数值与耗时。`

type ToolStep struct {
	Step      int    `json:"step"`
	Tool      string `json:"tool"`
	Category  string `json:"category"` // "ui_tree" | "touch" | "cdp" | "vision" | "memory" | "workflow" | "system"
	Label     string `json:"label,omitempty"`
	Summary   string `json:"summary,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// RunEvent is emitted in real time during an Agent turn and stored in runEventStore for Run Replay.
type RunEvent struct {
	RunID             string     `json:"run_id"`
	Seq               int        `json:"seq"`
	Type              string     `json:"type"` // run_start | thinking | tool_start | tool_end | text_delta | needs_confirmation | done | cancelled | error
	TS                string     `json:"ts"`
	Step              int        `json:"step,omitempty"`
	Steps             int        `json:"steps,omitempty"`
	MaxSteps          int        `json:"max_steps,omitempty"`
	Tool              string     `json:"tool,omitempty"`
	Category          string     `json:"category,omitempty"`
	Label             string     `json:"label,omitempty"`
	Summary           string     `json:"summary,omitempty"`
	Delta             string     `json:"delta,omitempty"`
	Reply             string     `json:"reply,omitempty"`
	Error             string     `json:"error,omitempty"`
	IsError           bool       `json:"is_error,omitempty"`
	ElapsedMs         int64      `json:"elapsed_ms,omitempty"`
	ElapsedSec        float64    `json:"elapsed_sec,omitempty"`
	NeedsConfirmation bool       `json:"needs_confirmation,omitempty"`
	ToolsUsed         []string   `json:"tools_used,omitempty"`
	ToolTrace         []ToolStep `json:"tool_trace,omitempty"`
}

type RunReplaySnapshot struct {
	RunID           string     `json:"run_id"`
	NotFound        bool       `json:"not_found,omitempty"`
	UserMessage     string     `json:"user_message,omitempty"`
	Active          bool       `json:"active"`
	Finished        bool       `json:"finished"`
	LastSeq         int        `json:"last_seq"`
	AccumulatedText string     `json:"accumulated_text,omitempty"`
	Events          []RunEvent `json:"events"`
}

// DeviceLease provides mutual exclusion over phone screen/CDP control
// so interactive Agent chat turns and background JobManager workflows cannot collide.
type DeviceLease struct {
	mu    sync.Mutex
	owner string
}

func NewDeviceLease() *DeviceLease {
	return &DeviceLease{}
}

// TryLock attempts to acquire the device lease for owner.
// Returns ("", true) if acquired, or (currentOwner, false) if already held.
func (l *DeviceLease) TryLock(owner string) (string, bool) {
	if l == nil {
		return "", true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != "" {
		return l.owner, false
	}
	l.owner = owner
	return "", true
}

// Unlock releases the device lease if held by owner.
func (l *DeviceLease) Unlock(owner string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner == owner {
		l.owner = ""
	}
}

// Owner returns the current lease holder (empty if unlocked).
func (l *DeviceLease) Owner() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.owner
}

type EventSink func(ev RunEvent)

const (
	maxReplayStepEvents  = 128
	maxReplayDeltaEvents = 384
)

// runEventStore keeps an in-memory replay buffer for the latest Agent run so mobile clients
// that get suspended when switching to another App can catch up seamlessly on resume.
type runEventStore struct {
	mu          sync.Mutex
	runID       string
	userMessage string
	active      bool
	finished    bool
	lastSeq     int
	accumText   strings.Builder
	steps       []RunEvent // structural events (run_start/thinking/tool_start/tool_end/needs_confirmation/done/cancelled/error)
	deltas      []RunEvent // text_delta events
	subs        map[int]chan RunEvent
	nextSubID   int
}

func newRunEventStore() *runEventStore {
	return &runEventStore{
		subs: make(map[int]chan RunEvent),
	}
}

func (s *runEventStore) beginRun(runID, userMessage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runID = runID
	s.userMessage = userMessage
	s.active = true
	s.finished = false
	s.lastSeq = 0
	s.accumText.Reset()
	s.steps = s.steps[:0]
	s.deltas = s.deltas[:0]
	for id, ch := range s.subs {
		close(ch)
		delete(s.subs, id)
	}
}

func (s *runEventStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return
	}
	s.runID = ""
	s.userMessage = ""
	s.finished = false
	s.lastSeq = 0
	s.accumText.Reset()
	s.steps = nil
	s.deltas = nil
}

func (s *runEventStore) emit(ev RunEvent) RunEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.RunID = s.runID
	s.lastSeq++
	ev.Seq = s.lastSeq
	if ev.TS == "" {
		ev.TS = time.Now().Format(time.RFC3339Nano)
	}
	if ev.Type == "text_delta" {
		s.accumText.WriteString(ev.Delta)
		s.deltas = append(s.deltas, ev)
		if len(s.deltas) > maxReplayDeltaEvents {
			s.deltas = append([]RunEvent(nil), s.deltas[len(s.deltas)-maxReplayDeltaEvents:]...)
		}
	} else {
		if ev.Type == "tool_start" {
			s.accumText.Reset()
		} else if ev.Type == "done" && ev.Reply != "" {
			s.accumText.Reset()
			s.accumText.WriteString(ev.Reply)
		}
		s.steps = append(s.steps, ev)
		if len(s.steps) > maxReplayStepEvents {
			s.steps = append([]RunEvent(nil), s.steps[len(s.steps)-maxReplayStepEvents:]...)
		}
		if ev.Type == "done" || ev.Type == "cancelled" || ev.Type == "error" {
			s.active = false
			s.finished = true
		}
	}

	for id, ch := range s.subs {
		select {
		case ch <- ev:
		default:
			// Slow/disconnected subscriber: close channel so it switches cleanly to replay polling.
			close(ch)
			delete(s.subs, id)
		}
	}
	return ev
}

func (s *runEventStore) snapshot(runID string, afterSeq int) RunReplaySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != "" && runID != s.runID {
		return RunReplaySnapshot{
			RunID:    runID,
			NotFound: true,
			Events:   []RunEvent{},
		}
	}
	if s.runID == "" {
		return RunReplaySnapshot{
			Events: []RunEvent{},
		}
	}
	events := s.collectAfterLocked(afterSeq)
	return RunReplaySnapshot{
		RunID:           s.runID,
		UserMessage:     s.userMessage,
		Active:          s.active,
		Finished:        s.finished,
		LastSeq:         s.lastSeq,
		AccumulatedText: s.accumText.String(),
		Events:          events,
	}
}

func (s *runEventStore) subscribe(runID string, afterSeq int) (RunReplaySnapshot, <-chan RunEvent, func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != "" && runID != s.runID {
		return RunReplaySnapshot{RunID: runID, NotFound: true, Events: []RunEvent{}}, nil, func() {}, false
	}
	if s.runID == "" {
		return RunReplaySnapshot{Events: []RunEvent{}}, nil, func() {}, false
	}
	snap := RunReplaySnapshot{
		RunID:           s.runID,
		UserMessage:     s.userMessage,
		Active:          s.active,
		Finished:        s.finished,
		LastSeq:         s.lastSeq,
		AccumulatedText: s.accumText.String(),
		Events:          s.collectAfterLocked(afterSeq),
	}
	if s.finished {
		return snap, nil, func() {}, true
	}
	s.nextSubID++
	id := s.nextSubID
	ch := make(chan RunEvent, 128)
	s.subs[id] = ch
	unsub := func() {
		s.mu.Lock()
		if existing, ok := s.subs[id]; ok {
			close(existing)
			delete(s.subs, id)
		}
		s.mu.Unlock()
	}
	return snap, ch, unsub, true
}

func (s *runEventStore) collectAfterLocked(afterSeq int) []RunEvent {
	out := make([]RunEvent, 0, 32)
	for _, ev := range s.steps {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	for _, ev := range s.deltas {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Seq < out[j].Seq
	})
	return out
}

const (
	maxSavedTurns            = 40
	maxHydratedTurnExchanges = 12
)

// SavedTurn represents a completed, cancelled, or failed Agent turn persisted to chat_history.json
// so /ui can render full rich historical timelines (including collapsible ToolTrace) across restarts.
type SavedTurn struct {
	RunID             string     `json:"run_id"`
	Timestamp         string     `json:"timestamp"`
	UserMessage       string     `json:"user_message"`
	Reply             string     `json:"reply,omitempty"`
	Error             string     `json:"error,omitempty"`
	Cancelled         bool       `json:"cancelled,omitempty"`
	ElapsedSec        float64    `json:"elapsed_sec"`
	Steps             int        `json:"steps"`
	NeedsConfirmation bool       `json:"needs_confirmation,omitempty"`
	ToolTrace         []ToolStep `json:"tool_trace,omitempty"`
}

type ChatTurnResult struct {
	RunID             string     `json:"run_id,omitempty"`
	Reply             string     `json:"reply"`
	ToolsUsed         []string   `json:"tools_used,omitempty"`
	ToolTrace         []ToolStep `json:"tool_trace,omitempty"`
	NeedsConfirmation bool       `json:"needs_confirmation,omitempty"`
	Steps             int        `json:"steps"`
	ElapsedSec        float64    `json:"elapsed_sec"`
}

type Agent struct {
	mu           sync.Mutex
	chatMu       sync.Mutex
	activeMu     sync.Mutex
	activeCancel context.CancelFunc
	activeReason string
	runSeq       uint64

	cfg         config.LLMConfig
	exec        ToolExecutor
	guard       *guard.Guard
	memory      *MemoryStore
	lease       *DeviceLease
	history     []ChatMessage
	historyFile string
	savedTurns  []SavedTurn
	events      *runEventStore
	obsMu       sync.RWMutex
	obsObserver EventSink
}

func NewAgent(cfg config.LLMConfig, exec ToolExecutor, g *guard.Guard) *Agent {
	a := &Agent{
		cfg:    cfg,
		exec:   exec,
		guard:  g,
		events: newRunEventStore(),
	}
	if envFile := strings.TrimSpace(os.Getenv("AGENT_CHAT_HISTORY_FILE")); envFile != "" {
		a.SetHistoryFile(envFile)
	}
	return a
}

// SetHistoryFile binds a persistent JSON path (0600) for SavedTurns, loading existing turns
// and hydrating in-memory LLM conversation context with up to the last 12 completed exchanges.
func (a *Agent) SetHistoryFile(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.historyFile = strings.TrimSpace(path)
	if a.historyFile == "" {
		return
	}
	a.loadSavedTurnsLocked()
}

// SavedTurns returns a deep copy of the persisted rich conversation turns (up to 40).
func (a *Agent) SavedTurns() []SavedTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.savedTurns) == 0 {
		return []SavedTurn{}
	}
	out := make([]SavedTurn, len(a.savedTurns))
	for i, t := range a.savedTurns {
		t.ToolTrace = sanitizeToolTrace(t.ToolTrace)
		out[i] = t
	}
	return out
}

func sanitizeToolTrace(trace []ToolStep) []ToolStep {
	if len(trace) == 0 {
		return nil
	}
	out := make([]ToolStep, len(trace))
	for i, st := range trace {
		if isSensitiveTool(st.Tool) {
			st.Summary = summarizeToolOutcome(st.Tool, ToolOutput{IsError: st.IsError})
		}
		out[i] = st
	}
	return out
}

func sanitizeSavedTurn(t SavedTurn) SavedTurn {
	t.ToolTrace = sanitizeToolTrace(t.ToolTrace)
	return t
}

func (a *Agent) loadSavedTurnsLocked() {
	if a.historyFile == "" {
		return
	}
	raw, err := os.ReadFile(a.historyFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && len(a.savedTurns) > 0 {
			a.persistSavedTurnsLocked()
		}
		return
	}
	var turns []SavedTurn
	if err := json.Unmarshal(raw, &turns); err != nil {
		return
	}
	if len(turns) > maxSavedTurns {
		turns = turns[len(turns)-maxSavedTurns:]
	}
	cleaned := make([]SavedTurn, len(turns))
	for i, t := range turns {
		cleaned[i] = sanitizeSavedTurn(t)
	}
	a.savedTurns = cleaned

	// Hydrate LLM context with up to the last ~12 completed user/assistant text exchanges.
	var validExchanges []SavedTurn
	for _, t := range cleaned {
		if !t.Cancelled && t.Error == "" && strings.TrimSpace(t.UserMessage) != "" && strings.TrimSpace(t.Reply) != "" {
			validExchanges = append(validExchanges, t)
		}
	}
	if len(validExchanges) > maxHydratedTurnExchanges {
		validExchanges = validExchanges[len(validExchanges)-maxHydratedTurnExchanges:]
	}
	if len(validExchanges) > 0 {
		hydrated := make([]ChatMessage, 0, len(validExchanges)*2)
		for _, t := range validExchanges {
			hydrated = append(hydrated,
				ChatMessage{Role: "user", Text: t.UserMessage},
				ChatMessage{Role: "assistant", Text: t.Reply},
			)
		}
		a.history = hydrated
	}
}

func (a *Agent) persistSavedTurnsLocked() {
	if a.historyFile == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(a.historyFile), 0o700)
	turns := a.savedTurns
	if turns == nil {
		turns = []SavedTurn{}
	}
	data, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return
	}
	_ = writeAtomicFile(a.historyFile, append(data, '\n'), 0o600)
}

func (a *Agent) recordSavedTurn(t SavedTurn) {
	t = sanitizeSavedTurn(t)
	a.mu.Lock()
	a.savedTurns = append(a.savedTurns, t)
	if len(a.savedTurns) > maxSavedTurns {
		a.savedTurns = append([]SavedTurn(nil), a.savedTurns[len(a.savedTurns)-maxSavedTurns:]...)
	}
	a.persistSavedTurnsLocked()
	a.mu.Unlock()
}

func (a *Agent) SetDeviceLease(lease *DeviceLease) {
	a.mu.Lock()
	a.lease = lease
	a.mu.Unlock()
}

func (a *Agent) getDeviceLease() *DeviceLease {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lease
}

func (a *Agent) SetEventObserver(fn EventSink) {
	a.obsMu.Lock()
	a.obsObserver = fn
	a.obsMu.Unlock()
}

func (a *Agent) IsRunning() bool {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	return a.activeCancel != nil
}

func (a *Agent) ReplayEvents(runID string, afterSeq int) RunReplaySnapshot {
	return a.events.snapshot(strings.TrimSpace(runID), afterSeq)
}

func (a *Agent) SubscribeRun(runID string, afterSeq int) (RunReplaySnapshot, <-chan RunEvent, func(), bool) {
	return a.events.subscribe(strings.TrimSpace(runID), afterSeq)
}

func (a *Agent) SetMemoryStore(m *MemoryStore) {
	a.mu.Lock()
	a.memory = m
	a.mu.Unlock()
}

func (a *Agent) UpdateConfig(cfg config.LLMConfig) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *Agent) GetConfig() config.LLMConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *Agent) ClearHistory() {
	a.mu.Lock()
	a.history = nil
	a.savedTurns = nil
	if a.historyFile != "" {
		_ = os.MkdirAll(filepath.Dir(a.historyFile), 0o700)
		_ = writeAtomicFile(a.historyFile, []byte("[]\n"), 0o600)
	}
	a.mu.Unlock()
	if !a.IsRunning() {
		a.events.clear()
	}
}

func (a *Agent) History() []ChatMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ChatMessage, len(a.history))
	copy(out, a.history)
	return out
}

// CancelActive immediately aborts an in-flight Chat turn (e.g. when game-guard triggers).
func (a *Agent) CancelActive(reason string) bool {
	a.activeMu.Lock()
	cancel := a.activeCancel
	if cancel != nil {
		a.activeReason = strings.TrimSpace(reason)
	}
	a.activeMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (a *Agent) Chat(ctx context.Context, userMessage string) (*ChatTurnResult, error) {
	return a.ChatStream(ctx, userMessage, nil)
}

func (a *Agent) nextRunID() string {
	a.mu.Lock()
	a.runSeq++
	id := fmt.Sprintf("run-%d-%d", time.Now().Unix(), a.runSeq)
	a.mu.Unlock()
	return id
}

func (a *Agent) dispatchEvent(sink EventSink, ev RunEvent) {
	recorded := a.events.emit(ev)
	if sink != nil {
		sink(recorded)
	}
	a.obsMu.RLock()
	obs := a.obsObserver
	a.obsMu.RUnlock()
	if obs != nil {
		obs(recorded)
	}
}

// StartChatAsync starts a detached background ReAct turn and immediately returns its run_id
// so SSE subscribers or mobile webviews that suspend can detach/reattach cleanly without losing execution.
func (a *Agent) StartChatAsync(userMessage string) (string, error) {
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" {
		return "", errors.New("message is empty")
	}
	if a.guard != nil {
		if blocked, reason := a.guard.IsActionBlocked(); blocked {
			return "", fmt.Errorf("agent guard active: %s", reason)
		}
	}
	if !a.chatMu.TryLock() {
		return "", ErrChatBusy
	}

	runID := a.nextRunID()
	lease := a.getDeviceLease()
	leaseOwner := "chat:" + runID
	if holder, ok := lease.TryLock(leaseOwner); !ok {
		a.chatMu.Unlock()
		return "", fmt.Errorf("device busy (%s is running)", holder)
	}

	// Bind activeCancel synchronously BEFORE spawning the goroutine so immediate CancelActive/IsRunning never races.
	runCtx, cancel := context.WithCancel(context.Background())
	a.activeMu.Lock()
	a.activeCancel = cancel
	a.activeReason = ""
	a.activeMu.Unlock()

	a.events.beginRun(runID, userMessage)
	go func() {
		defer a.chatMu.Unlock()
		defer lease.Unlock(leaseOwner)
		_, _ = a.runLocked(runCtx, cancel, lease, leaseOwner, runID, userMessage, nil)
	}()
	return runID, nil
}

func (a *Agent) ChatStream(ctx context.Context, userMessage string, sink EventSink) (*ChatTurnResult, error) {
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" {
		return nil, errors.New("message is empty")
	}
	if a.guard != nil {
		if blocked, reason := a.guard.IsActionBlocked(); blocked {
			return nil, fmt.Errorf("agent guard active: %s", reason)
		}
	}
	if !a.chatMu.TryLock() {
		return nil, ErrChatBusy
	}
	defer a.chatMu.Unlock()

	runID := a.nextRunID()
	lease := a.getDeviceLease()
	leaseOwner := "chat:" + runID
	if holder, ok := lease.TryLock(leaseOwner); !ok {
		return nil, fmt.Errorf("device busy (%s is running)", holder)
	}
	defer lease.Unlock(leaseOwner)

	runCtx, cancel := context.WithCancel(ctx)
	a.activeMu.Lock()
	a.activeCancel = cancel
	a.activeReason = ""
	a.activeMu.Unlock()

	a.events.beginRun(runID, userMessage)
	return a.runLocked(runCtx, cancel, lease, leaseOwner, runID, userMessage, sink)
}

func isWorkflowTool(name string) bool {
	return name == "run_daily_all" || name == "run_ms_rewards" || name == "run_jd_daily"
}

func (a *Agent) runLocked(runCtx context.Context, cancel context.CancelFunc, lease *DeviceLease, leaseOwner, runID, userMessage string, sink EventSink) (*ChatTurnResult, error) {
	defer func() {
		cancel()
		a.activeMu.Lock()
		a.activeCancel = nil
		a.activeReason = ""
		a.activeMu.Unlock()
	}()

	t0 := time.Now()
	a.mu.Lock()
	cfg := a.cfg
	mem := a.memory
	history := append([]ChatMessage(nil), a.history...)
	a.mu.Unlock()

	sysPrompt := SystemPrompt
	if mem != nil {
		if sec := mem.BuildPromptSection(); sec != "" {
			sysPrompt += "\n\n" + sec
		}
	}

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}

	history = append(history, ChatMessage{Role: "user", Text: userMessage})
	tools := a.exec.ToolSpecs()
	var toolsUsed []string
	var trace []ToolStep
	steps := 0
	lastCallSig := ""
	repeatCount := 0
	repeatErrCount := 0

	a.dispatchEvent(sink, RunEvent{
		Type:     "run_start",
		MaxSteps: maxSteps,
		Summary:  userMessage,
	})

	finishCancelled := func(err error) (*ChatTurnResult, error) {
		a.activeMu.Lock()
		reason := a.activeReason
		a.activeMu.Unlock()
		msg := "任务已停止"
		if reason != "" {
			msg = fmt.Sprintf("任务已停止 (%s)", reason)
		}
		elapsed := float64(int(time.Since(t0).Seconds()*10)) / 10
		a.recordSavedTurn(SavedTurn{
			RunID:       runID,
			Timestamp:   t0.Format(time.RFC3339),
			UserMessage: userMessage,
			Error:       msg,
			Cancelled:   true,
			ElapsedSec:  elapsed,
			Steps:       steps,
			ToolTrace:   trace,
		})
		a.dispatchEvent(sink, RunEvent{
			Type:       "cancelled",
			Error:      msg,
			ElapsedSec: elapsed,
			Steps:      steps,
			ToolsUsed:  toolsUsed,
			ToolTrace:  trace,
		})
		if reason != "" {
			return nil, fmt.Errorf("chat cancelled (%s): %w", reason, err)
		}
		return nil, err
	}

	finishErr := func(err error) (*ChatTurnResult, error) {
		if runCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled)) {
			return finishCancelled(err)
		}
		elapsed := float64(int(time.Since(t0).Seconds()*10)) / 10
		a.recordSavedTurn(SavedTurn{
			RunID:       runID,
			Timestamp:   t0.Format(time.RFC3339),
			UserMessage: userMessage,
			Error:       err.Error(),
			ElapsedSec:  elapsed,
			Steps:       steps,
			ToolTrace:   trace,
		})
		a.dispatchEvent(sink, RunEvent{
			Type:       "error",
			Error:      err.Error(),
			ElapsedSec: elapsed,
			Steps:      steps,
			ToolsUsed:  toolsUsed,
			ToolTrace:  trace,
		})
		return nil, err
	}

	for steps < maxSteps {
		if err := runCtx.Err(); err != nil {
			return finishCancelled(err)
		}
		steps++
		a.dispatchEvent(sink, RunEvent{
			Type:     "thinking",
			Step:     steps,
			MaxSteps: maxSteps,
			Summary:  fmt.Sprintf("第 %d 步推理规划中...", steps),
		})

		compactInFlightHistory(history)
		resp, err := callLLMStream(runCtx, cfg, sysPrompt, history, tools, StreamCallbacks{
			OnTextDelta: func(delta string) {
				if delta == "" {
					return
				}
				a.dispatchEvent(sink, RunEvent{
					Type:     "text_delta",
					Step:     steps,
					MaxSteps: maxSteps,
					Delta:    delta,
				})
			},
		})
		if err != nil {
			return finishErr(err)
		}

		history = append(history, ChatMessage{
			Role:      "assistant",
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			a.saveHistory(history)
			needsConfirm := replyNeedsConfirmation(resp.Text)
			if needsConfirm {
				a.dispatchEvent(sink, RunEvent{
					Type:              "needs_confirmation",
					Step:              steps,
					MaxSteps:          maxSteps,
					NeedsConfirmation: true,
					Summary:           "需要人工确认",
				})
			}
			elapsed := float64(int(time.Since(t0).Seconds()*10)) / 10
			a.recordSavedTurn(SavedTurn{
				RunID:             runID,
				Timestamp:         t0.Format(time.RFC3339),
				UserMessage:       userMessage,
				Reply:             resp.Text,
				ElapsedSec:        elapsed,
				Steps:             steps,
				NeedsConfirmation: needsConfirm,
				ToolTrace:         trace,
			})
			a.dispatchEvent(sink, RunEvent{
				Type:              "done",
				Step:              steps,
				MaxSteps:          maxSteps,
				Reply:             resp.Text,
				NeedsConfirmation: needsConfirm,
				ElapsedSec:        elapsed,
				ToolsUsed:         toolsUsed,
				ToolTrace:         trace,
			})
			return &ChatTurnResult{
				RunID:             runID,
				Reply:             resp.Text,
				ToolsUsed:         toolsUsed,
				ToolTrace:         trace,
				NeedsConfirmation: needsConfirm,
				Steps:             steps,
				ElapsedSec:        elapsed,
			}, nil
		}

		for _, tc := range resp.ToolCalls {
			if a.guard != nil {
				if blocked, reason := a.guard.IsActionBlocked(); blocked {
					err := fmt.Errorf("stopped by guard: %s", reason)
					return finishCancelled(err)
				}
			}
			cat := classifyToolCategory(tc.Name)
			toolLabel, actionDesc := friendlyToolDescriptions(tc.Name, tc.Args)
			a.dispatchEvent(sink, RunEvent{
				Type:     "tool_start",
				Step:     steps,
				MaxSteps: maxSteps,
				Tool:     tc.Name,
				Category: cat,
				Label:    toolLabel,
				Summary:  actionDesc,
			})

			toolsUsed = append(toolsUsed, tc.Name)
			toolStart := time.Now()
			// Yield device lease across native workflow tools that route through JobManager so they don't self-deadlock.
			yieldedLease := false
			if lease != nil && isWorkflowTool(tc.Name) {
				lease.Unlock(leaseOwner)
				yieldedLease = true
			}
			out := a.exec.ExecuteTool(runCtx, tc.Name, tc.Args)
			if yieldedLease {
				for attempt := 0; attempt < 20; attempt++ {
					if _, ok := lease.TryLock(leaseOwner); ok {
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
			}
			elapsedMs := time.Since(toolStart).Milliseconds()

			sigBytes, _ := json.Marshal(tc.Args)
			sig := tc.Name + ":" + string(sigBytes)
			if sig == lastCallSig {
				repeatCount++
				if out.IsError {
					repeatErrCount++
				} else {
					repeatErrCount = 0
				}
			} else {
				lastCallSig = sig
				repeatCount = 1
				if out.IsError {
					repeatErrCount = 1
				} else {
					repeatErrCount = 0
				}
			}

			toolText := out.Text
			if repeatCount >= 3 || repeatErrCount >= 2 {
				toolText += "\n[系统防死循环提示] 检测到连续重复调用相同工具且未取得进展，请立即更换操作策略（如滑动、截屏兜底）或直接向用户汇报当前状态。"
			}

			stepSum := summarizeToolOutcome(tc.Name, out)
			if stepSum == "" {
				stepSum = actionDesc
			}
			stepEntry := ToolStep{
				Step:      len(trace) + 1,
				Tool:      tc.Name,
				Category:  cat,
				Label:     toolLabel,
				Summary:   stepSum,
				IsError:   out.IsError,
				ElapsedMs: elapsedMs,
			}
			trace = append(trace, stepEntry)

			a.dispatchEvent(sink, RunEvent{
				Type:      "tool_end",
				Step:      steps,
				MaxSteps:  maxSteps,
				Tool:      tc.Name,
				Category:  cat,
				Label:     toolLabel,
				Summary:   stepSum,
				IsError:   out.IsError,
				ElapsedMs: elapsedMs,
			})

			history = append(history, ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Text:       toolText,
				ImageJPEG:  out.ImageJPEG,
			})
		}
	}

	a.saveHistory(history)
	reply := "已达到单轮最大工具调用步数限制，请查看当前结果或继续指示下一步。"
	elapsed := float64(int(time.Since(t0).Seconds()*10)) / 10
	needsConfirm := replyNeedsConfirmation(reply)
	a.recordSavedTurn(SavedTurn{
		RunID:             runID,
		Timestamp:         t0.Format(time.RFC3339),
		UserMessage:       userMessage,
		Reply:             reply,
		ElapsedSec:        elapsed,
		Steps:             steps,
		NeedsConfirmation: needsConfirm,
		ToolTrace:         trace,
	})
	a.dispatchEvent(sink, RunEvent{
		Type:              "done",
		Step:              steps,
		MaxSteps:          maxSteps,
		Reply:             reply,
		NeedsConfirmation: needsConfirm,
		ElapsedSec:        elapsed,
		ToolsUsed:         toolsUsed,
		ToolTrace:         trace,
	})
	return &ChatTurnResult{
		RunID:             runID,
		Reply:             reply,
		ToolsUsed:         toolsUsed,
		ToolTrace:         trace,
		NeedsConfirmation: needsConfirm,
		Steps:             steps,
		ElapsedSec:        elapsed,
	}, nil
}

// friendlyToolDescriptions returns (label, summary) with crisp, emoji-free Chinese text
// suitable for both /ui ToolTrace timelines and HyperOS Super Island capsules.
func friendlyToolDescriptions(toolName string, args map[string]any) (string, string) {
	switch toolName {
	case "phone_ui_tree":
		return "读取页面结构", "解析当前界面控件树"
	case "phone_tap_node":
		if id, ok := args["id"]; ok {
			return "点击控件", fmt.Sprintf("点击节点 #%v", id)
		}
		return "点击控件", "点击界面控件"
	case "phone_tap":
		return "点击坐标", fmt.Sprintf("点击屏幕 (%v, %v)", args["x"], args["y"])
	case "phone_long_press":
		return "长按坐标", fmt.Sprintf("长按屏幕 (%v, %v)", args["x"], args["y"])
	case "phone_scroll_node":
		dir, _ := args["direction"].(string)
		return "滑动页面", fmt.Sprintf("向%s滚动页面区域", friendlyDirection(dir))
	case "phone_swipe":
		return "滑动页面", "执行拟人滑动手势"
	case "phone_input_text":
		return "输入文本", "输入文本内容（已脱敏）"
	case "phone_tap_and_type":
		return "点击并输入", "聚焦输入框并填入文本（已脱敏）"
	case "phone_clipboard_set":
		return "写入剪贴板", "写入剪贴板（已脱敏）"
	case "phone_clipboard_get":
		return "读取剪贴板", "读取剪贴板（已脱敏）"
	case "phone_keyevent":
		return "系统按键", fmt.Sprintf("发送按键 %v", args["keycode"])
	case "phone_screenshot":
		return "屏幕快照", "捕获 720p 屏幕画面"
	case "phone_launch_app":
		pkg, _ := args["package"].(string)
		appName := friendlyPackageName(pkg)
		return "打开应用", fmt.Sprintf("打开 %s", appName)
	case "phone_open_deeplink":
		pkg, _ := args["package"].(string)
		if strings.TrimSpace(pkg) != "" {
			return "跳转链接", fmt.Sprintf("跳转 %s 指定页面", friendlyPackageName(pkg))
		}
		return "跳转链接", "打开应用内深链页面"
	case "phone_foreground_app":
		return "查询前台", "查询当前前台应用"
	case "phone_status":
		return "设备状态", "检查设备与守护状态"
	case "phone_notify":
		return "系统通知", "发送本地状态通知"
	case "cdp_list_sockets", "cdp_list_targets", "cdp_status":
		return "探测网页", "获取 WebView CDP 页面目标"
	case "cdp_command":
		method, _ := args["method"].(string)
		if method == "" {
			return "操作网页", "执行 WebView CDP 指令"
		}
		return "操作网页", fmt.Sprintf("执行 CDP %s", method)
	case "read_memory":
		file, _ := args["file"].(string)
		if strings.TrimSpace(file) != "" {
			return "读取记忆", fmt.Sprintf("读取记忆 %s", strings.TrimSpace(file))
		}
		return "读取记忆", "读取端侧记忆"
	case "append_memory":
		file, _ := args["file"].(string)
		if strings.TrimSpace(file) != "" {
			return "写入记忆", fmt.Sprintf("写入 %s", strings.TrimSpace(file))
		}
		return "写入记忆", "写入端侧记忆"
	case "run_daily_all":
		return "全套日常任务", "运行微软积分与京东日常工作流"
	case "run_ms_rewards":
		return "微软积分任务", "运行 Edge 与 Bing 积分工作流"
	case "run_jd_daily":
		return "京东日常任务", "运行东东农场与京豆签到工作流"
	default:
		return toolName, fmt.Sprintf("调用 %s", toolName)
	}
}

func friendlyDirection(dir string) string {
	switch strings.ToLower(strings.TrimSpace(dir)) {
	case "up":
		return "上"
	case "left":
		return "左"
	case "right":
		return "右"
	default:
		return "下"
	}
}

func friendlyPackageName(pkg string) string {
	switch strings.TrimSpace(pkg) {
	case "com.sankuai.meituan":
		return "美团"
	case "com.jingdong.app.mall":
		return "京东"
	case "com.tencent.mm":
		return "微信"
	case "com.eg.android.AlipayGphone":
		return "支付宝"
	case "com.microsoft.emmx":
		return "Edge"
	case "com.microsoft.bing":
		return "Bing"
	case "com.taobao.taobao":
		return "淘宝"
	default:
		if pkg == "" {
			return "应用"
		}
		parts := strings.Split(pkg, ".")
		return parts[len(parts)-1]
	}
}

func classifyToolCategory(name string) string {
	switch name {
	case "phone_ui_tree":
		return "ui_tree"
	case "phone_tap", "phone_long_press", "phone_swipe", "phone_tap_node", "phone_scroll_node", "phone_tap_and_type":
		return "touch"
	case "phone_screenshot":
		return "vision"
	case "cdp_list_sockets", "cdp_status", "cdp_list_targets", "cdp_command":
		return "cdp"
	case "read_memory", "append_memory":
		return "memory"
	case "run_daily_all", "run_ms_rewards", "run_jd_daily":
		return "workflow"
	default:
		return "system"
	}
}

func isSensitiveTool(name string) bool {
	switch name {
	case "phone_input_text", "phone_clipboard_set", "phone_clipboard_get", "phone_tap_and_type":
		return true
	default:
		return false
	}
}

func redactSensitiveToolCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(calls))
	for i, tc := range calls {
		if len(tc.Args) > 0 {
			cpArgs := make(map[string]any, len(tc.Args))
			for k, v := range tc.Args {
				if isSensitiveTool(tc.Name) && (k == "text" || k == "value" || k == "content") {
					cpArgs[k] = "[REDACTED]"
				} else {
					cpArgs[k] = v
				}
			}
			tc.Args = cpArgs
		}
		out[i] = tc
	}
	return out
}

func summarizeToolOutcome(toolName string, out ToolOutput) string {
	if isSensitiveTool(toolName) {
		if out.IsError {
			return "操作失败（敏感内容已脱敏）"
		}
		switch toolName {
		case "phone_input_text":
			return "已输入文本（内容已脱敏）"
		case "phone_tap_and_type":
			return "已点击并输入文本（内容已脱敏）"
		case "phone_clipboard_set":
			return "剪贴板已写入（内容已脱敏）"
		case "phone_clipboard_get":
			return "已读取剪贴板（内容已脱敏）"
		}
		return "敏感内容已脱敏"
	}
	if len(out.ImageJPEG) > 0 {
		kb := len(out.ImageJPEG) / 1024
		if kb <= 0 {
			kb = 1
		}
		return fmt.Sprintf("屏幕快照 (%d KB)", kb)
	}
	trimmed := strings.TrimSpace(out.Text)
	if trimmed == "" {
		if out.IsError {
			return "执行出错"
		}
		return "已完成"
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			if errMsg, ok := obj["error"].(string); ok && strings.TrimSpace(errMsg) != "" {
				return truncateSummaryRunes("错误: "+strings.TrimSpace(errMsg), 68)
			}
			switch toolName {
			case "phone_tap_node":
				if node, ok := obj["node"].(map[string]any); ok {
					lbl, _ := node["label"].(string)
					if strings.TrimSpace(lbl) != "" {
						return truncateSummaryRunes(fmt.Sprintf("已点击「%s」", strings.TrimSpace(lbl)), 68)
					}
				}
			case "phone_foreground_app":
				if pkg, ok := obj["package"].(string); ok && strings.TrimSpace(pkg) != "" {
					return fmt.Sprintf("前台应用: %s", friendlyPackageName(pkg))
				}
			case "phone_launch_app":
				if pkg, ok := obj["package"].(string); ok && strings.TrimSpace(pkg) != "" {
					return fmt.Sprintf("已启动 %s", friendlyPackageName(pkg))
				}
			case "phone_status":
				if g, ok := obj["guard"].(map[string]any); ok {
					if st, ok := g["state"].(string); ok && st != "" {
						return fmt.Sprintf("设备就绪 · 守护 %s", st)
					}
				}
			}
		}
	}
	first := trimmed
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = strings.TrimSpace(first[:idx])
	}
	if toolName == "phone_ui_tree" {
		lines := strings.Split(trimmed, "\n")
		if len(lines) > 1 {
			return truncateSummaryRunes(fmt.Sprintf("%s · %d 行节点", strings.TrimSpace(lines[0]), len(lines)-1), 68)
		}
	}
	return truncateSummaryRunes(first, 68)
}

func truncateSummaryRunes(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > maxRunes {
		return string(r[:maxRunes-1]) + "…"
	}
	return string(r)
}

var confirmNegationMarkers = []string{
	"无需", "不需要", "不用", "不必",
	"未发现", "没有发现", "未遇到", "没有遇到",
	"未出现", "没有出现", "未触发", "没有触发",
	"未检测到", "没有检测到", "未见",
	"没有", "并未", "并非", "不存在",
	"无验证码", "无滑块", "无须",
}

func isNegatedInClause(reply string, matchByteIdx, kwLen int) bool {
	endIdx := matchByteIdx + kwLen
	if endIdx > len(reply) {
		endIdx = len(reply)
	}
	clause := reply[:endIdx]
	// Stop at the nearest clause/sentence boundary before the match so earlier clauses don't negate later ones.
	prefix := reply[:matchByteIdx]
	if sepIdx := strings.LastIndexAny(prefix, "。！？\n;；,，:："); sepIdx >= 0 {
		clause = reply[sepIdx+1 : endIdx]
		prefix = prefix[sepIdx+1:]
	}
	// Limit inspection to the local phrase leading into the keyword.
	pr := []rune(prefix)
	if len(pr) > 12 {
		clause = string(pr[len(pr)-12:]) + reply[matchByteIdx:endIdx]
	}
	for _, neg := range confirmNegationMarkers {
		if strings.Contains(clause, neg) {
			return true
		}
	}
	return false
}

func replyNeedsConfirmation(reply string) bool {
	if reply == "" {
		return false
	}
	keywords := []string{
		"需要人工确认", "人工操作", "停在确认", "确认订单", "确认页",
		"自行付款", "亲自完成", "滑块", "验证码",
		"请完成支付", "登录失效", "支付密码", "请手动确认",
	}
	for _, kw := range keywords {
		searchFrom := 0
		for searchFrom < len(reply) {
			rel := strings.Index(reply[searchFrom:], kw)
			if rel < 0 {
				break
			}
			idx := searchFrom + rel
			if !isNegatedInClause(reply, idx, len(kw)) {
				return true
			}
			searchFrom = idx + len(kw)
		}
	}
	return false
}

// compactInFlightHistory bounds token & memory usage during multi-step ReAct turns:
// - Keeps only the latest 1 ImageJPEG across the conversation in-flight.
// - Collapses verbose phone_ui_tree outputs older than 2 assistant steps.
func compactInFlightHistory(history []ChatMessage) {
	// 1. Keep only the latest 1 screenshot JPEG in-flight.
	seenImg := false
	for i := len(history) - 1; i >= 0; i-- {
		if len(history[i].ImageJPEG) > 0 {
			if seenImg {
				history[i].ImageJPEG = nil
			} else {
				seenImg = true
			}
		}
	}

	// 2. Collapse phone_ui_tree outputs that are older than 2 assistant steps.
	assistantAfter := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			assistantAfter++
			continue
		}
		if history[i].Role == "tool" && history[i].ToolName == "phone_ui_tree" && assistantAfter >= 2 {
			if len(history[i].Text) > 160 {
				firstLine := history[i].Text
				if nl := strings.IndexByte(firstLine, '\n'); nl >= 0 {
					firstLine = strings.TrimSpace(firstLine[:nl])
				}
				if len([]rune(firstLine)) > 64 {
					firstLine = string([]rune(firstLine)[:64]) + "…"
				}
				history[i].Text = fmt.Sprintf("[旧 UI 树已压缩: %s]", firstLine)
			}
		}
	}
}

func (a *Agent) saveHistory(h []ChatMessage) {
	// Strip all ImageJPEG blobs and redact sensitive tool args/outputs so neither screenshots
	// nor passwords/clipboard contents are ever retained in memory or re-uploaded on future turns.
	cleaned := make([]ChatMessage, len(h))
	for i, m := range h {
		m.ImageJPEG = nil
		if len(m.ToolCalls) > 0 {
			m.ToolCalls = redactSensitiveToolCalls(m.ToolCalls)
		}
		if m.Role == "tool" && isSensitiveTool(m.ToolName) {
			m.Text = summarizeToolOutcome(m.ToolName, ToolOutput{IsError: false})
		}
		cleaned[i] = m
	}
	h = cleaned

	// Hard-trim to at most 24 messages while keeping valid user/assistant/tool boundaries.
	const maxKeep = 24
	if len(h) > maxKeep {
		prefix := h[:len(h)-maxKeep]
		trimmed := h[len(h)-maxKeep:]
		cut := -1
		for i, m := range trimmed {
			if m.Role == "user" && m.Text != "" {
				cut = i
				break
			}
		}
		if cut >= 0 {
			h = trimmed[cut:]
		} else {
			// No user message inside the last 24 messages (e.g. a >12-step tool turn).
			// Skip leading orphan "tool" messages so providers never reject the sequence.
			start := 0
			for start < len(trimmed) && trimmed[start].Role == "tool" {
				start++
			}
			tail := trimmed[start:]
			// Prepend the most recent user message from before the cut if room allows.
			lastUserIdx := -1
			for i := len(prefix) - 1; i >= 0; i-- {
				if prefix[i].Role == "user" && prefix[i].Text != "" {
					lastUserIdx = i
					break
				}
			}
			if lastUserIdx >= 0 {
				if len(tail) >= maxKeep {
					// Drop leading assistant+its tool responses to make room for the user message.
					drop := 1
					for drop < len(tail) && tail[drop].Role == "tool" {
						drop++
					}
					tail = tail[drop:]
				}
				res := make([]ChatMessage, 0, 1+len(tail))
				res = append(res, prefix[lastUserIdx])
				res = append(res, tail...)
				h = res
			} else {
				h = tail
			}
		}
	}

	final := make([]ChatMessage, len(h))
	copy(final, h)
	a.mu.Lock()
	a.history = final
	a.mu.Unlock()
}

const (
	maxJobLogs       = 300
	maxRecentJobLogs = 80
)

// WorkflowProgressEvent carries structured real-time stage & percentage updates from JobManager
// to subscribers such as IslandController (Super Island) and WebUI status consumers.
type WorkflowProgressEvent struct {
	Job   string `json:"job"`
	Step  int    `json:"step"`
	Total int    `json:"total"`
	Pct   int    `json:"pct"`
	Stage string `json:"stage"`
	Done  bool   `json:"done"`
	Err   string `json:"err,omitempty"`
}

// JobStatus represents the current/last background automation job started from /ui, Cron, or API.
type JobStatus struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	State        string   `json:"state"` // "idle" | "running" | "completed" | "failed" | "cancelled"
	StartedAt    string   `json:"started_at,omitempty"`
	FinishedAt   string   `json:"finished_at,omitempty"`
	ElapsedSec   float64  `json:"elapsed_sec,omitempty"`
	Step         int      `json:"step,omitempty"`
	TotalSteps   int      `json:"total_steps,omitempty"`
	ProgressPct  int      `json:"progress_pct,omitempty"`
	CurrentStage string   `json:"current_stage,omitempty"`
	RecentLogs   []string `json:"recent_logs,omitempty"`
	Logs         []string `json:"logs,omitempty"`
	Result       any      `json:"result,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// JobSnapshot aliases JobStatus for callers referencing snapshot terminology.
type JobSnapshot = JobStatus

var (
	workflowStageMarkerRe = regexp.MustCompile(`^\s*\[(?:[^\]]*?\s+)?(\d+)\s*/\s*(\d+)\]\s*(.+)$`)
	workflowSubCounterRe  = regexp.MustCompile(`[（(]\s*(\d+)\s*/\s*(\d+)`)
)

func parseWorkflowStageLine(line string) (step, total, pct int, stage string, ok bool) {
	m := workflowStageMarkerRe.FindStringSubmatch(strings.TrimSpace(line))
	if len(m) != 4 {
		return 0, 0, 0, "", false
	}
	step, err1 := strconv.Atoi(m[1])
	total, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || total <= 0 || step <= 0 {
		return 0, 0, 0, "", false
	}
	if step > total {
		step = total
	}
	stage = strings.TrimSpace(m[3])
	baseLow := (step - 1) * 100 / total
	baseHigh := step * 100 / total
	span := baseHigh - baseLow

	if sub := workflowSubCounterRe.FindStringSubmatch(stage); len(sub) == 3 {
		cur, e1 := strconv.Atoi(sub[1])
		capV, e2 := strconv.Atoi(sub[2])
		if e1 == nil && e2 == nil && capV > 0 {
			if cur < 0 {
				cur = 0
			}
			if cur > capV {
				cur = capV
			}
			pct = baseLow + (cur*span)/capV
		} else {
			pct = baseLow + span/4
		}
	} else {
		pct = baseLow + span/3
		if pct == 0 {
			pct = 5
		}
	}
	if pct > 99 {
		pct = 99
	}
	if pct < 0 {
		pct = 0
	}
	return step, total, pct, stage, true
}

type JobManager struct {
	mu             sync.Mutex
	seq            int
	status         JobStatus
	startedTime    time.Time
	cancel         context.CancelFunc
	lease          *DeviceLease
	notifyProgress bool
	doneEmitted    bool

	obsMu       sync.RWMutex
	progressObs func(WorkflowProgressEvent)
}

func NewJobManager() *JobManager {
	return &JobManager{
		status: JobStatus{State: "idle"},
	}
}

func (jm *JobManager) SetDeviceLease(lease *DeviceLease) {
	jm.mu.Lock()
	jm.lease = lease
	jm.mu.Unlock()
}

func (jm *JobManager) SetProgressObserver(fn func(WorkflowProgressEvent)) {
	jm.obsMu.Lock()
	jm.progressObs = fn
	jm.obsMu.Unlock()
}

func (jm *JobManager) emitProgress(ev WorkflowProgressEvent) {
	jm.obsMu.RLock()
	obs := jm.progressObs
	jm.obsMu.RUnlock()
	if obs != nil {
		obs(ev)
	}
}

func appendBoundedLog(slice []string, entry string, limit int) []string {
	slice = append(slice, entry)
	if len(slice) > limit {
		out := make([]string, limit)
		copy(out, slice[len(slice)-limit:])
		return out
	}
	return slice
}

func (jm *JobManager) Start(name string, fn func(ctx context.Context, logFn func(string)) (any, error)) (JobStatus, error) {
	return jm.startInternal(name, true, true, fn)
}

func (jm *JobManager) startPromptJob(name string, fn func(ctx context.Context, logFn func(string)) (any, error)) (JobStatus, error) {
	return jm.startInternal(name, false, false, fn)
}

func (jm *JobManager) startInternal(name string, acquireLease, notifyProgress bool, fn func(ctx context.Context, logFn func(string)) (any, error)) (JobStatus, error) {
	jm.mu.Lock()
	if jm.status.State == "running" {
		cur := jm.copyStatusLocked()
		jm.mu.Unlock()
		return cur, fmt.Errorf("job %q is already running", cur.Name)
	}
	lease := jm.lease
	leaseOwner := "job:" + name
	if acquireLease {
		if holder, ok := lease.TryLock(leaseOwner); !ok {
			cur := jm.copyStatusLocked()
			jm.mu.Unlock()
			return cur, fmt.Errorf("device busy (%s is running)", holder)
		}
	} else if lease != nil {
		if holder := lease.Owner(); holder != "" {
			cur := jm.copyStatusLocked()
			jm.mu.Unlock()
			return cur, fmt.Errorf("device busy (%s is running)", holder)
		}
	}

	jm.seq++
	id := fmt.Sprintf("job-%d", jm.seq)
	ctx, cancel := context.WithCancel(context.Background())
	jm.cancel = cancel
	now := time.Now()
	jm.startedTime = now
	jm.notifyProgress = notifyProgress
	jm.doneEmitted = false
	firstLog := fmt.Sprintf("[%s] started %s", now.Format("15:04:05"), name)
	jm.status = JobStatus{
		ID:           id,
		Name:         name,
		State:        "running",
		StartedAt:    now.Format(time.RFC3339),
		CurrentStage: "正在启动",
		RecentLogs:   []string{firstLog},
		Logs:         []string{firstLog},
	}
	snap := jm.copyStatusLocked()
	jm.mu.Unlock()

	if notifyProgress {
		jm.emitProgress(WorkflowProgressEvent{
			Job:   name,
			Step:  0,
			Total: 0,
			Pct:   0,
			Stage: "正在启动",
			Done:  false,
		})
	}

	go func() {
		defer cancel()
		released := false
		releaseLease := func() {
			if acquireLease && !released {
				released = true
				lease.Unlock(leaseOwner)
			}
		}
		defer releaseLease()

		logFn := func(line string) {
			jm.appendLogForID(id, line)
		}
		res, err := fn(ctx, logFn)
		releaseLease()
		jm.finish(id, now, res, err)
	}()
	return snap, nil
}

// AppendLog appends a timestamped line to the active job's RecentLogs (ring buffer of 80)
// and auto-parses `[step/total]` stage markers to drive live progress percentages.
func (jm *JobManager) AppendLog(line string) {
	jm.mu.Lock()
	id := jm.status.ID
	running := jm.status.State == "running"
	jm.mu.Unlock()
	if !running || id == "" {
		return
	}
	jm.appendLogForID(id, line)
}

func (jm *JobManager) appendLogForID(id, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var ev *WorkflowProgressEvent
	jm.mu.Lock()
	if jm.status.ID != id || jm.status.State != "running" {
		jm.mu.Unlock()
		return
	}
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line)
	jm.status.Logs = appendBoundedLog(jm.status.Logs, entry, maxJobLogs)
	jm.status.RecentLogs = appendBoundedLog(jm.status.RecentLogs, entry, maxRecentJobLogs)

	if step, total, pct, stage, ok := parseWorkflowStageLine(line); ok {
		jm.status.Step = step
		jm.status.TotalSteps = total
		if pct > jm.status.ProgressPct {
			jm.status.ProgressPct = pct
		}
		jm.status.CurrentStage = stage
		if jm.notifyProgress {
			ev = &WorkflowProgressEvent{
				Job:   jm.status.Name,
				Step:  jm.status.Step,
				Total: jm.status.TotalSteps,
				Pct:   jm.status.ProgressPct,
				Stage: jm.status.CurrentStage,
				Done:  false,
			}
		}
	}
	jm.mu.Unlock()

	if ev != nil {
		jm.emitProgress(*ev)
	}
}

// UpdateProgress explicitly updates step/total/pct/stage on the currently running job
// and notifies any registered ProgressObserver.
func (jm *JobManager) UpdateProgress(step, total, pct int, stage string) {
	var ev *WorkflowProgressEvent
	jm.mu.Lock()
	if jm.status.State != "running" {
		jm.mu.Unlock()
		return
	}
	if step >= 0 {
		jm.status.Step = step
	}
	if total >= 0 {
		jm.status.TotalSteps = total
	}
	if pct < 0 {
		if total > 0 && step > 0 {
			pct = (step * 100) / total
		} else {
			pct = jm.status.ProgressPct
		}
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	jm.status.ProgressPct = pct
	if strings.TrimSpace(stage) != "" {
		jm.status.CurrentStage = strings.TrimSpace(stage)
	}
	if jm.notifyProgress {
		ev = &WorkflowProgressEvent{
			Job:   jm.status.Name,
			Step:  jm.status.Step,
			Total: jm.status.TotalSteps,
			Pct:   jm.status.ProgressPct,
			Stage: jm.status.CurrentStage,
			Done:  false,
		}
	}
	jm.mu.Unlock()

	if ev != nil {
		jm.emitProgress(*ev)
	}
}

func (jm *JobManager) finish(id string, started time.Time, res any, err error) {
	var ev *WorkflowProgressEvent
	jm.mu.Lock()
	if jm.status.ID != id {
		jm.mu.Unlock()
		return
	}
	jm.cancel = nil
	now := time.Now()
	jm.status.FinishedAt = now.Format(time.RFC3339)
	jm.status.ElapsedSec = float64(int(now.Sub(started).Seconds()*10)) / 10
	jm.status.Result = res

	if jm.status.State == "cancelled" {
		if err != nil && jm.status.Error == "" {
			jm.status.Error = err.Error()
		}
		endEntry := fmt.Sprintf("[%s] ended (cancelled)", now.Format("15:04:05"))
		jm.status.Logs = appendBoundedLog(jm.status.Logs, endEntry, maxJobLogs)
		jm.status.RecentLogs = appendBoundedLog(jm.status.RecentLogs, endEntry, maxRecentJobLogs)
		if jm.notifyProgress && !jm.doneEmitted {
			jm.doneEmitted = true
			ev = &WorkflowProgressEvent{
				Job:   jm.status.Name,
				Step:  jm.status.Step,
				Total: jm.status.TotalSteps,
				Pct:   jm.status.ProgressPct,
				Stage: "已取消",
				Done:  true,
				Err:   jm.status.Error,
			}
		}
		jm.mu.Unlock()
		if ev != nil {
			jm.emitProgress(*ev)
		}
		return
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			jm.status.State = "cancelled"
			jm.status.CurrentStage = "已取消"
		} else {
			jm.status.State = "failed"
			jm.status.CurrentStage = "执行失败"
		}
		jm.status.Error = err.Error()
		endEntry := fmt.Sprintf("[%s] ended (%s): %v", now.Format("15:04:05"), jm.status.State, err)
		jm.status.Logs = appendBoundedLog(jm.status.Logs, endEntry, maxJobLogs)
		jm.status.RecentLogs = appendBoundedLog(jm.status.RecentLogs, endEntry, maxRecentJobLogs)
		if jm.notifyProgress && !jm.doneEmitted {
			jm.doneEmitted = true
			ev = &WorkflowProgressEvent{
				Job:   jm.status.Name,
				Step:  jm.status.Step,
				Total: jm.status.TotalSteps,
				Pct:   jm.status.ProgressPct,
				Stage: jm.status.CurrentStage,
				Done:  true,
				Err:   jm.status.Error,
			}
		}
	} else {
		jm.status.State = "completed"
		jm.status.ProgressPct = 100
		if jm.status.TotalSteps > 0 {
			jm.status.Step = jm.status.TotalSteps
		}
		jm.status.CurrentStage = "已完成"
		endEntry := fmt.Sprintf("[%s] completed in %.1fs", now.Format("15:04:05"), jm.status.ElapsedSec)
		jm.status.Logs = appendBoundedLog(jm.status.Logs, endEntry, maxJobLogs)
		jm.status.RecentLogs = appendBoundedLog(jm.status.RecentLogs, endEntry, maxRecentJobLogs)
		if jm.notifyProgress && !jm.doneEmitted {
			jm.doneEmitted = true
			ev = &WorkflowProgressEvent{
				Job:   jm.status.Name,
				Step:  jm.status.Step,
				Total: jm.status.TotalSteps,
				Pct:   100,
				Stage: "已完成",
				Done:  true,
			}
		}
	}
	jm.mu.Unlock()

	if ev != nil {
		jm.emitProgress(*ev)
	}
}

// Cancel immediately cancels the running workflow context and marks status "cancelled".
// Accepts an optional reason string for backward compatibility with both Cancel() and Cancel(reason).
func (jm *JobManager) Cancel(reasons ...string) bool {
	reason := strings.TrimSpace(strings.Join(reasons, "; "))
	var ev *WorkflowProgressEvent
	jm.mu.Lock()
	if jm.status.State != "running" || jm.cancel == nil {
		jm.mu.Unlock()
		return false
	}
	cancelFn := jm.cancel
	jm.cancel = nil
	now := time.Now()
	jm.status.State = "cancelled"
	jm.status.FinishedAt = now.Format(time.RFC3339)
	if !jm.startedTime.IsZero() {
		jm.status.ElapsedSec = float64(int(now.Sub(jm.startedTime).Seconds()*10)) / 10
	}
	jm.status.CurrentStage = "已取消"
	if reason != "" {
		jm.status.Error = reason
	} else {
		jm.status.Error = "cancelled"
	}
	logMsg := "cancelled"
	if reason != "" {
		logMsg = "cancelled: " + reason
	}
	entry := fmt.Sprintf("[%s] %s", now.Format("15:04:05"), logMsg)
	jm.status.Logs = appendBoundedLog(jm.status.Logs, entry, maxJobLogs)
	jm.status.RecentLogs = appendBoundedLog(jm.status.RecentLogs, entry, maxRecentJobLogs)
	if jm.notifyProgress && !jm.doneEmitted {
		jm.doneEmitted = true
		ev = &WorkflowProgressEvent{
			Job:   jm.status.Name,
			Step:  jm.status.Step,
			Total: jm.status.TotalSteps,
			Pct:   jm.status.ProgressPct,
			Stage: "已取消",
			Done:  true,
			Err:   jm.status.Error,
		}
	}
	jm.mu.Unlock()

	cancelFn()
	if ev != nil {
		jm.emitProgress(*ev)
	}
	return true
}

func (jm *JobManager) Status() JobStatus {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	return jm.copyStatusLocked()
}

func (jm *JobManager) copyStatusLocked() JobStatus {
	cp := jm.status
	if cp.State == "running" && !jm.startedTime.IsZero() {
		cp.ElapsedSec = float64(int(time.Since(jm.startedTime).Seconds()*10)) / 10
	}
	if len(jm.status.Logs) > 0 {
		cp.Logs = make([]string, len(jm.status.Logs))
		copy(cp.Logs, jm.status.Logs)
	}
	if len(jm.status.RecentLogs) > 0 {
		cp.RecentLogs = make([]string, len(jm.status.RecentLogs))
		copy(cp.RecentLogs, jm.status.RecentLogs)
	}
	return cp
}
