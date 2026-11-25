package brain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"android-agent/internal/config"
	"android-agent/internal/guard"
)

const SystemPrompt = `你是运行在用户小米 13（Android 16 / HyperOS 3.0）本机上的 AI Agent 大脑（android-agent v0.2.0-ondevice）。
你能直接调用设备控制工具、WebView CDP 工具（Edge/Bing/京东 20ms DOM）以及 0-Token 原生日常任务引擎。

核心规则与安全红线：
1. 【优先省 Token 与速度】遇到「每日任务/微软积分/Bing/京东农场/领京豆」等请求时，优先直接调用原生 0-Token 工作流工具（run_daily_all / run_ms_rewards / run_jd_daily），不要用大模型一页页截屏盲点。
2. 【绝不绕过验证码】只要看到任何滑块、人机验证（CAPTCHA）或登录失效弹窗，必须立即停止并告知用户在手机上亲自完成。
3. 【尊重游戏风控守护】当 guard.state != "active"（如王者荣耀前台或手动暂停）时拒绝操作。
4. 【不越权操作】绝不擅自下单支付、开通试用会员、下载第三方软件（如快手/百度极速版）、盗用他人买家秀照片或上传用户相册私图。
5. 【先查 DOM 再看图】操作 Edge / Bing / 京东优先用 cdp_list_sockets + cdp_list_targets + cdp_command，只在需要核查原生界面时再调用 phone_screenshot。
6. 回复简洁清晰，使用中文分点总结实际观测到的结果、数值与耗时。`

type ChatTurnResult struct {
	Reply      string   `json:"reply"`
	ToolsUsed  []string `json:"tools_used,omitempty"`
	Steps      int      `json:"steps"`
	ElapsedSec float64  `json:"elapsed_sec"`
}

type Agent struct {
	mu      sync.Mutex
	cfg     config.LLMConfig
	exec    ToolExecutor
	guard   *guard.Guard
	history []ChatMessage
}

func NewAgent(cfg config.LLMConfig, exec ToolExecutor, g *guard.Guard) *Agent {
	return &Agent{
		cfg:   cfg,
		exec:  exec,
		guard: g,
	}
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

	t0 := time.Now()
	a.mu.Lock()
	cfg := a.cfg
	history := append([]ChatMessage(nil), a.history...)
	a.mu.Unlock()

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}

	history = append(history, ChatMessage{Role: "user", Text: userMessage})
	tools := a.exec.ToolSpecs()
	var toolsUsed []string
	steps := 0

	for steps < maxSteps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		steps++
		resp, err := callLLM(ctx, cfg, SystemPrompt, history, tools)
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
				Reply:      resp.Text,
				ToolsUsed:  toolsUsed,
				Steps:      steps,
				ElapsedSec: float64(int(time.Since(t0).Seconds()*10)) / 10,
			}, nil
		}

		for _, tc := range resp.ToolCalls {
			if a.guard != nil {
				if blocked, reason := a.guard.IsActionBlocked(); blocked {
					return nil, fmt.Errorf("stopped by guard: %s", reason)
				}
			}
			toolsUsed = append(toolsUsed, tc.Name)
			out := a.exec.ExecuteTool(ctx, tc.Name, tc.Args)
			history = append(history, ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Text:       out.Text,
				ImageJPEG:  out.ImageJPEG,
			})
		}
	}

	a.saveHistory(history)
	return &ChatTurnResult{
		Reply:      "已达到单轮最大工具调用步数限制，请查看当前结果或继续指示下一步。",
		ToolsUsed:  toolsUsed,
		Steps:      steps,
		ElapsedSec: float64(int(time.Since(t0).Seconds()*10)) / 10,
	}, nil
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
