package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Summary   string `json:"summary,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

type ChatTurnResult struct {
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

	cfg     config.LLMConfig
	exec    ToolExecutor
	guard   *guard.Guard
	memory  *MemoryStore
	history []ChatMessage
}

func NewAgent(cfg config.LLMConfig, exec ToolExecutor, g *guard.Guard) *Agent {
	return &Agent{
		cfg:   cfg,
		exec:  exec,
		guard: g,
	}
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

	runCtx, cancel := context.WithCancel(ctx)
	a.activeMu.Lock()
	a.activeCancel = cancel
	a.activeReason = ""
	a.activeMu.Unlock()
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

	for steps < maxSteps {
		if err := runCtx.Err(); err != nil {
			a.activeMu.Lock()
			reason := a.activeReason
			a.activeMu.Unlock()
			if reason != "" {
				return nil, fmt.Errorf("chat cancelled (%s): %w", reason, err)
			}
			return nil, err
		}
		steps++
		resp, err := callLLM(runCtx, cfg, sysPrompt, history, tools)
		if err != nil {
			return nil, err
		}

		history = append(history, ChatMessage{
			Role:      "assistant",
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			a.saveHistory(history)
			return &ChatTurnResult{
				Reply:             resp.Text,
				ToolsUsed:         toolsUsed,
				ToolTrace:         trace,
				NeedsConfirmation: replyNeedsConfirmation(resp.Text),
				Steps:             steps,
				ElapsedSec:        float64(int(time.Since(t0).Seconds()*10)) / 10,
			}, nil
		}

		for _, tc := range resp.ToolCalls {
			if a.guard != nil {
				if blocked, reason := a.guard.IsActionBlocked(); blocked {
					return nil, fmt.Errorf("stopped by guard: %s", reason)
				}
			}
			toolsUsed = append(toolsUsed, tc.Name)
			toolStart := time.Now()
			out := a.exec.ExecuteTool(runCtx, tc.Name, tc.Args)

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

			trace = append(trace, ToolStep{
				Step:      len(trace) + 1,
				Tool:      tc.Name,
				Category:  classifyToolCategory(tc.Name),
				Summary:   summarizeToolOutcome(out),
				IsError:   out.IsError,
				ElapsedMs: time.Since(toolStart).Milliseconds(),
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
	return &ChatTurnResult{
		Reply:             reply,
		ToolsUsed:         toolsUsed,
		ToolTrace:         trace,
		NeedsConfirmation: replyNeedsConfirmation(reply),
		Steps:             steps,
		ElapsedSec:        float64(int(time.Since(t0).Seconds()*10)) / 10,
	}, nil
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

func summarizeToolOutcome(out ToolOutput) string {
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

func replyNeedsConfirmation(reply string) bool {
	if reply == "" {
		return false
	}
	keywords := []string{"需要人工确认", "人工操作", "停在确认", "确认订单", "确认页", "自行付款", "亲自完成", "滑块", "验证码"}
	for _, kw := range keywords {
		if strings.Contains(reply, kw) {
			return true
		}
	}
	return false
}

func (a *Agent) saveHistory(h []ChatMessage) {
	// Keep the last 24 messages cleanly aligned on a user turn boundary
	const maxKeep = 24
	if len(h) > maxKeep {
		trimmed := h[len(h)-maxKeep:]
		for i, m := range trimmed {
			if m.Role == "user" && m.Text != "" {
				h = trimmed[i:]
				break
			}
		}
	}
	a.mu.Lock()
	a.history = h
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
}

func NewJobManager() *JobManager {
	return &JobManager{
		status: JobStatus{State: "idle"},
	}
}

func (jm *JobManager) Start(name string, fn func(ctx context.Context, logFn func(string)) (any, error)) (JobStatus, error) {
	jm.mu.Lock()
	if jm.status.State == "running" {
		cur := jm.copyStatusLocked()
		jm.mu.Unlock()
		return cur, fmt.Errorf("job %q is already running", cur.Name)
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
		logFn := func(line string) {
			jm.appendLog(id, line)
		}
		res, err := fn(ctx, logFn)
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
