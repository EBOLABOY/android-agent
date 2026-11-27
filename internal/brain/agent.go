package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	events      *runEventStore
	obsMu       sync.RWMutex
	obsObserver EventSink
}

func NewAgent(cfg config.LLMConfig, exec ToolExecutor, g *guard.Guard) *Agent {
	return &Agent{
		cfg:    cfg,
		exec:   exec,
		guard:  g,
		events: newRunEventStore(),
	}
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
	a.mu.Unlock()
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
		a.dispatchEvent(sink, RunEvent{
			Type:       "cancelled",
			Error:      msg,
			ElapsedSec: float64(int(time.Since(t0).Seconds()*10)) / 10,
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
		a.dispatchEvent(sink, RunEvent{
			Type:       "error",
			Error:      err.Error(),
			ElapsedSec: float64(int(time.Since(t0).Seconds()*10)) / 10,
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
			uiDesc, islandShort := friendlyToolDescriptions(tc.Name, tc.Args)
			a.dispatchEvent(sink, RunEvent{
				Type:     "tool_start",
				Step:     steps,
				MaxSteps: maxSteps,
				Tool:     tc.Name,
				Category: cat,
				Label:    islandShort,
				Summary:  uiDesc,
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
			stepEntry := ToolStep{
				Step:      len(trace) + 1,
				Tool:      tc.Name,
				Category:  cat,
				Label:     uiDesc,
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
				Label:     uiDesc,
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

func friendlyToolDescriptions(toolName string, args map[string]any) (string, string) {
	switch toolName {
	case "phone_ui_tree":
		return "正在解析当前界面控件树...", "读界面"
	case "phone_tap_node":
		if id, ok := args["id"]; ok {
			return fmt.Sprintf("正在点击控件 [%v]...", id), fmt.Sprintf("点#%v", id)
		}
		return "正在点击界面控件...", "点控件"
	case "phone_tap":
		return fmt.Sprintf("正在点击坐标 (%v, %v)...", args["x"], args["y"]), "点击"
	case "phone_long_press":
		return fmt.Sprintf("正在长按坐标 (%v, %v)...", args["x"], args["y"]), "长按"
	case "phone_scroll_node":
		dir, _ := args["direction"].(string)
		if dir == "" {
			dir = "down"
		}
		return fmt.Sprintf("正在向 %s 滑动页面...", dir), "滑页面"
	case "phone_swipe":
		return "正在执行拟人滑动...", "滑动"
	case "phone_input_text":
		return "正在输入文本内容（已脱敏）...", "输入"
	case "phone_clipboard_set":
		return "正在写入剪贴板（已脱敏）...", "写剪贴板"
	case "phone_clipboard_get":
		return "正在读取剪贴板...", "读剪贴板"
	case "phone_keyevent":
		return fmt.Sprintf("正在发送按键 %v...", args["keycode"]), "按键"
	case "phone_screenshot":
		return "正在截屏视觉兜底...", "截屏"
	case "phone_launch_app":
		pkg, _ := args["package"].(string)
		appName := friendlyPackageName(pkg)
		return fmt.Sprintf("正在打开 %s...", appName), "开" + appName
	case "phone_foreground_app":
		return "正在查询前台应用...", "查前台"
	case "cdp_list_sockets", "cdp_list_targets", "cdp_status":
		return "正在探测 WebView CDP 页面...", "查DOM"
	case "cdp_command":
		return fmt.Sprintf("正在执行 CDP %v...", args["method"]), "控DOM"
	case "read_memory":
		return "正在读取端侧记忆...", "读记忆"
	case "append_memory":
		return "正在写入端侧记忆...", "写记忆"
	case "run_daily_all":
		return "正在运行每日一键全清 (0-Token)...", "全清"
	case "run_ms_rewards":
		return "正在运行微软积分任务...", "微软分"
	case "run_jd_daily":
		return "正在运行京东日常任务...", "京东豆"
	default:
		return fmt.Sprintf("正在调用 %s...", toolName), toolName
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
	case "phone_tap", "phone_long_press", "phone_swipe", "phone_tap_node", "phone_scroll_node":
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
	case "phone_input_text", "phone_clipboard_set", "phone_clipboard_get":
		return true
	default:
		return false
	}
}

func summarizeToolOutcome(toolName string, out ToolOutput) string {
	if isSensitiveTool(toolName) {
		if out.IsError {
			return "操作失败（敏感内容已脱敏）"
		}
		switch toolName {
		case "phone_input_text":
			return "已输入文本（内容已脱敏）"
		case "phone_clipboard_set":
			return "剪贴板已写入（内容已脱敏）"
		case "phone_clipboard_get":
			return "已读取剪贴板（内容已脱敏）"
		}
		return "[敏感内容已脱敏]"
	}
	if len(out.ImageJPEG) > 0 {
		return fmt.Sprintf("截屏视觉图像 (%d KB)", len(out.ImageJPEG)/1024)
	}
	first := strings.TrimSpace(out.Text)
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = strings.TrimSpace(first[:idx])
	}
	r := []rune(first)
	if len(r) > 72 {
		return string(r[:71]) + "…"
	}
	return first
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
	// Strip all ImageJPEG blobs so old screenshots are never re-uploaded on future turns.
	cleaned := make([]ChatMessage, len(h))
	for i, m := range h {
		m.ImageJPEG = nil
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

// JobStatus represents the current/last background automation job started from /ui or API.
type JobStatus struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	State      string   `json:"state"` // "idle" | "running" | "completed" | "failed" | "cancelled"
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	ElapsedSec float64  `json:"elapsed_sec,omitempty"`
	Logs       []string `json:"logs,omitempty"`
	Result     any      `json:"result,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type JobManager struct {
	mu     sync.Mutex
	seq    int
	status JobStatus
	cancel context.CancelFunc
	lease  *DeviceLease
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

func (jm *JobManager) Start(name string, fn func(ctx context.Context, logFn func(string)) (any, error)) (JobStatus, error) {
	jm.mu.Lock()
	if jm.status.State == "running" {
		cur := jm.copyStatusLocked()
		jm.mu.Unlock()
		return cur, fmt.Errorf("job %q is already running", cur.Name)
	}
	lease := jm.lease
	leaseOwner := "job:" + name
	if holder, ok := lease.TryLock(leaseOwner); !ok {
		cur := jm.copyStatusLocked()
		jm.mu.Unlock()
		return cur, fmt.Errorf("device busy (%s is running)", holder)
	}
	jm.seq++
	id := fmt.Sprintf("job-%d", jm.seq)
	ctx, cancel := context.WithCancel(context.Background())
	jm.cancel = cancel
	now := time.Now()
	jm.status = JobStatus{
		ID:        id,
		Name:      name,
		State:     "running",
		StartedAt: now.Format(time.RFC3339),
		Logs:      []string{fmt.Sprintf("[%s] started %s", now.Format("15:04:05"), name)},
	}
	snap := jm.copyStatusLocked()
	jm.mu.Unlock()

	go func() {
		defer cancel()
		released := false
		releaseLease := func() {
			if !released {
				released = true
				lease.Unlock(leaseOwner)
			}
		}
		defer releaseLease()

		logFn := func(line string) {
			jm.appendLog(id, line)
		}
		res, err := fn(ctx, logFn)
		releaseLease()
		jm.finish(id, now, res, err)
	}()
	return snap, nil
}

func (jm *JobManager) appendLog(id, line string) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if jm.status.ID != id {
		return
	}
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line)
	jm.status.Logs = append(jm.status.Logs, entry)
	if len(jm.status.Logs) > 300 {
		jm.status.Logs = jm.status.Logs[len(jm.status.Logs)-300:]
	}
}

func (jm *JobManager) finish(id string, started time.Time, res any, err error) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if jm.status.ID != id {
		return
	}
	jm.cancel = nil
	jm.status.FinishedAt = time.Now().Format(time.RFC3339)
	jm.status.ElapsedSec = float64(int(time.Since(started).Seconds()*10)) / 10
	jm.status.Result = res
	if err != nil {
		if errors.Is(err, context.Canceled) {
			jm.status.State = "cancelled"
		} else {
			jm.status.State = "failed"
		}
		jm.status.Error = err.Error()
		jm.status.Logs = append(jm.status.Logs, fmt.Sprintf("[%s] ended (%s): %v", time.Now().Format("15:04:05"), jm.status.State, err))
	} else {
		jm.status.State = "completed"
		jm.status.Logs = append(jm.status.Logs, fmt.Sprintf("[%s] completed in %.1fs", time.Now().Format("15:04:05"), jm.status.ElapsedSec))
	}
}

func (jm *JobManager) Cancel(reason string) bool {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if jm.status.State != "running" || jm.cancel == nil {
		return false
	}
	if reason != "" {
		jm.status.Logs = append(jm.status.Logs, fmt.Sprintf("[%s] cancelling: %s", time.Now().Format("15:04:05"), reason))
	}
	jm.cancel()
	return true
}

func (jm *JobManager) Status() JobStatus {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	return jm.copyStatusLocked()
}

func (jm *JobManager) copyStatusLocked() JobStatus {
	cp := jm.status
	if len(jm.status.Logs) > 0 {
		cp.Logs = make([]string, len(jm.status.Logs))
		copy(cp.Logs, jm.status.Logs)
	}
	return cp
}
