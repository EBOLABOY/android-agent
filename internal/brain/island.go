package brain

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultIslandTag          = "agent_live"
	DefaultIslandID           = 9900
	DefaultIslandClickURI     = "http://127.0.0.1:8765/ui"
	defaultIslandMinUpdate    = 220 * time.Millisecond
	defaultIslandDoneClear    = 3 * time.Second
	defaultIslandConfirmClear = 5 * time.Minute
	defaultIslandPreviewHold  = 8 * time.Second
)

var (
	stageBracketPrefixRe = regexp.MustCompile(`^\[[^\]]+\]\s*`)
	ratioInTextRe        = regexp.MustCompile(`\((\d{1,4})\s*/\s*(\d{1,4})\)`)
	pctInTextRe          = regexp.MustCompile(`(\d{1,3})%`)
)

// LiveProgressUpdate describes a real-time progress payload rendered on HyperOS 3.0 Super Island
// and the ongoing silent Focus notification card.
type LiveProgressUpdate struct {
	Tag       string
	ID        int
	State     string // starting | thinking | acting | workflow | confirm | done | stopped | error
	Title     string
	Text      string
	ShortText string
	SubText   string
	Step      int
	Max       int
	Progress  int // 0..100
	ClickURI  string
	Focus     bool
	Ongoing   bool
}

// LiveProgressClient abstracts the device notification transport (AgentKernel / fallback).
type LiveProgressClient interface {
	UpdateLiveProgress(ctx context.Context, u LiveProgressUpdate) error
	CancelLiveProgress(ctx context.Context, tag string, id int) error
}

type pendingIslandItem struct {
	u          LiveProgressUpdate
	gen        uint64
	force      bool
	clearAfter time.Duration
}

type enqueueOpts struct {
	force      bool
	resetState bool
	confirm    bool
	clearAfter time.Duration
}

// IslandController drives HyperOS 3.0 Focus / Super Island progress capsules cleanly
// as a non-blocking coalescing single-worker actor:
// - 0 notifications when idle
// - 0 caller blocking inside Agent.dispatchEvent or workflow observers
// - 220ms trailing-edge coalescing so fast bursts never drop the latest step
// - 3s auto-clear on completion/cancel, 5m persistent warning on SOUL confirmation
// - synchronous generation-safe cancellation when game-guard triggers
type IslandController struct {
	mu                sync.Mutex
	ipcMu             sync.Mutex // serializes UpdateLiveProgress vs CancelLiveProgress (lock order: ipcMu -> mu)
	client            LiveProgressClient
	tag               string
	id                int
	clickURI          string
	minUpdateInterval time.Duration
	doneClearDelay    time.Duration
	confirmClearDelay time.Duration

	active          bool
	needsConfirm    bool
	doneSent        bool
	gen             uint64
	lastSentAt      time.Time
	lastState       string
	lastTitle       string
	lastShortText   string
	lastProgress    int
	currentRunID    string
	suppressedRunID string

	pending    *pendingIslandItem
	clearTimer *time.Timer
	wakeCh     chan struct{}
	stopCh     chan struct{}
	workerOnce sync.Once
	stopOnce   sync.Once

	channelName string
}

func NewIslandController(client LiveProgressClient) *IslandController {
	return &IslandController{
		client:            client,
		tag:               DefaultIslandTag,
		id:                DefaultIslandID,
		clickURI:          DefaultIslandClickURI,
		minUpdateInterval: defaultIslandMinUpdate,
		doneClearDelay:    defaultIslandDoneClear,
		confirmClearDelay: defaultIslandConfirmClear,
		wakeCh:            make(chan struct{}, 1),
		stopCh:            make(chan struct{}),
		channelName:       "hyperos-focus-v3",
	}
}

func (c *IslandController) ensureWorker() {
	c.workerOnce.Do(func() {
		go c.workerLoop()
	})
}

// Close terminates the background coalescing worker cleanly.
func (c *IslandController) Close() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

func (c *IslandController) IsActive() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *IslandController) Status() map[string]any {
	if c == nil {
		return map[string]any{"enabled": false}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{
		"enabled":            c.client != nil,
		"channel":            c.channelName,
		"active":             c.active,
		"needs_confirmation": c.needsConfirm,
		"state":              c.lastState,
		"title":              c.lastTitle,
		"short_text":         c.lastShortText,
		"progress":           c.lastProgress,
		"click_uri":          c.clickURI,
		"tag":                c.tag,
		"id":                 c.id,
	}
}

// HandleEvent translates structured Agent RunEvents into non-blocking coalesced Super Island updates.
func (c *IslandController) HandleEvent(ev RunEvent) {
	if c == nil || c.client == nil {
		return
	}

	c.mu.Lock()
	if ev.Type == "run_start" {
		c.currentRunID = ev.RunID
		c.suppressedRunID = ""
	} else if ev.RunID != "" && ev.RunID == c.suppressedRunID {
		// Ignore late events emitted by an already guard-cleared run so we never
		// respawn the capsule/microkernel while a guarded game is in the foreground.
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	switch ev.Type {
	case "run_start":
		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = DefaultMaxSteps
		}
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "starting",
			Title:     "Agent 准备执行",
			Text:      truncateIslandText(cleanIslandBody(ev.Summary), 48, "正在初始化任务上下文..."),
			ShortText: "准备执行",
			SubText:   "AI",
			Step:      0,
			Max:       maxS,
			Progress:  4,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}, enqueueOpts{force: true, resetState: true})

	case "thinking":
		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = DefaultMaxSteps
		}
		step := ev.Step
		if step < 1 {
			step = 1
		}
		pct := calcProgressPct(step, maxS)
		bodyText := fmt.Sprintf("第 %d/%d 步 · 正在分析界面与下一步操作", step, maxS)
		shortText := fmt.Sprintf("思考 %d/%d", step, maxS)
		if maxS >= 40 {
			bodyText = fmt.Sprintf("第 %d 步 · 推理", step)
			shortText = fmt.Sprintf("第 %d 步", step)
		}
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "thinking",
			Title:     "Agent 正在推理",
			Text:      bodyText,
			ShortText: shortText,
			SubText:   "推理",
			Step:      step,
			Max:       maxS,
			Progress:  pct,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}, enqueueOpts{})

	case "tool_start":
		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = DefaultMaxSteps
		}
		step := ev.Step
		if step < 1 {
			step = 1
		}
		state, title, short := formatToolIslandTitles(ev.Tool, ev.Label)
		detail := cleanIslandBody(ev.Summary)
		if maxS >= 40 {
			if detail == "" {
				detail = fmt.Sprintf("第 %d 步 · %s", step, short)
			} else if !strings.HasPrefix(detail, "第 ") {
				detail = fmt.Sprintf("第 %d 步 · %s", step, detail)
			}
		} else if detail == "" {
			detail = fmt.Sprintf("第 %d/%d 步 · %s", step, maxS, short)
		}
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     state,
			Title:     title,
			Text:      truncateIslandText(detail, 56, title),
			ShortText: short,
			SubText:   "控机",
			Step:      step,
			Max:       maxS,
			Progress:  calcProgressPct(step, maxS),
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}, enqueueOpts{})

	case "needs_confirmation":
		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = 1
		}
		step := ev.Step
		if step <= 0 {
			step = maxS
		}
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "confirm",
			Title:     "需要人工核对",
			Text:      truncateIslandText(cleanIslandBody(ev.Summary), 56, "已按安全红线停在确认页，请亲自核对后再操作"),
			ShortText: "待确认",
			SubText:   "核对",
			Step:      step,
			Max:       maxS,
			Progress:  calcProgressPct(step, maxS),
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}, enqueueOpts{force: true, confirm: true, clearAfter: c.confirmClearDelay})

	case "done":
		c.mu.Lock()
		hadConfirm := c.needsConfirm || ev.NeedsConfirmation
		if c.doneSent && c.needsConfirm {
			c.mu.Unlock()
			return
		}
		c.doneSent = true
		c.mu.Unlock()

		if hadConfirm {
			maxS := ev.MaxSteps
			if maxS <= 0 {
				maxS = 1
			}
			c.enqueue(LiveProgressUpdate{
				Tag:       c.tag,
				ID:        c.id,
				State:     "confirm",
				Title:     "需要人工核对",
				Text:      truncateIslandText(cleanIslandBody(ev.Reply), 56, "已停在确认页，等待你亲自核对并操作"),
				ShortText: "待确认",
				SubText:   "暂停",
				Step:      maxS,
				Max:       maxS,
				Progress:  100,
				ClickURI:  c.clickURI,
				Focus:     true,
				Ongoing:   false,
			}, enqueueOpts{force: true, confirm: true, clearAfter: c.confirmClearDelay})
			return
		}

		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = 1
		}
		summary := "指令已执行完毕"
		if ev.ElapsedSec > 0 || ev.Steps > 0 {
			summary = fmt.Sprintf("耗时 %.1fs · 共 %d 步", ev.ElapsedSec, ev.Steps)
		}
		doneTitle := "任务已完成"
		doneShort := "已完成"
		doneSub := "完成"
		if ev.CheckpointPaused {
			doneTitle = "阶段检查点"
			doneShort = "待继续"
			doneSub = "暂停"
		}
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "done",
			Title:     doneTitle,
			Text:      summary,
			ShortText: doneShort,
			SubText:   doneSub,
			Step:      maxS,
			Max:       maxS,
			Progress:  100,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}, enqueueOpts{force: true, clearAfter: c.doneClearDelay})

	case "cancelled":
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "stopped",
			Title:     "任务已停止",
			Text:      truncateIslandText(cleanIslandBody(ev.Error), 48, "当前任务已被取消"),
			ShortText: "已停止",
			SubText:   "停止",
			Step:      0,
			Max:       0,
			Progress:  0,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}, enqueueOpts{force: true, clearAfter: c.doneClearDelay})

	case "error":
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "error",
			Title:     "任务执行异常",
			Text:      truncateIslandText(cleanIslandBody(ev.Error), 56, "任务执行失败"),
			ShortText: "执行异常",
			SubText:   "异常",
			Step:      0,
			Max:       0,
			Progress:  0,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}, enqueueOpts{force: true, clearAfter: c.doneClearDelay})
	}
}

// HandleWorkflowProgress drives Super Island capsules for background and manual workflows
// (ms_rewards, jd_daily, daily_all) with live percentage bars and clean stage summaries.
func (c *IslandController) HandleWorkflowProgress(jobName string, step, total int, message string, done bool, errStr string) {
	if c == nil || c.client == nil {
		return
	}

	fullLabel, shortPrefix := friendlyWorkflowNames(jobName)
	cleanMsg := cleanWorkflowStage(message)

	if done {
		errClean := cleanIslandBody(errStr)
		if errClean != "" {
			lowerErr := strings.ToLower(errClean)
			switch {
			case strings.Contains(lowerErr, "cancel") || strings.Contains(errClean, "取消") || strings.Contains(errClean, "停止"):
				c.enqueue(LiveProgressUpdate{
					Tag:       c.tag,
					ID:        c.id,
					State:     "stopped",
					Title:     fmt.Sprintf("%s已停止", fullLabel),
					Text:      truncateIslandText(errClean, 52, "日常任务已手动停止"),
					ShortText: "已停止",
					SubText:   "停止",
					ClickURI:  c.clickURI,
					Focus:     true,
					Ongoing:   false,
				}, enqueueOpts{force: true, clearAfter: c.doneClearDelay})
			case strings.Contains(lowerErr, "captcha") || strings.Contains(errClean, "验证码") || strings.Contains(errClean, "滑块") || strings.Contains(errClean, "核对"):
				c.enqueue(LiveProgressUpdate{
					Tag:       c.tag,
					ID:        c.id,
					State:     "confirm",
					Title:     "需要人工核对",
					Text:      truncateIslandText(errClean, 56, "检测到验证环节，请打开页面手动完成"),
					ShortText: "待确认",
					SubText:   "核对",
					Step:      step,
					Max:       total,
					Progress:  calcWorkflowPct(step, total, cleanMsg),
					ClickURI:  c.clickURI,
					Focus:     true,
					Ongoing:   false,
				}, enqueueOpts{force: true, confirm: true, clearAfter: c.confirmClearDelay})
			default:
				c.enqueue(LiveProgressUpdate{
					Tag:       c.tag,
					ID:        c.id,
					State:     "error",
					Title:     fmt.Sprintf("%s执行异常", fullLabel),
					Text:      truncateIslandText(errClean, 56, "自动任务执行遇到问题"),
					ShortText: "执行异常",
					SubText:   "异常",
					ClickURI:  c.clickURI,
					Focus:     true,
					Ongoing:   false,
				}, enqueueOpts{force: true, clearAfter: c.doneClearDelay})
			}
			return
		}

		maxV := total
		if maxV <= 0 {
			maxV = 100
		}
		doneText := cleanMsg
		if doneText == "" {
			doneText = fmt.Sprintf("%s自动化流程已全部完成", fullLabel)
		}
		c.enqueue(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "done",
			Title:     fmt.Sprintf("%s已完成", fullLabel),
			Text:      truncateIslandText(doneText, 56, "全部步骤已完成"),
			ShortText: "已完成",
			SubText:   "完成",
			Step:      maxV,
			Max:       maxV,
			Progress:  100,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}, enqueueOpts{force: true, clearAfter: c.doneClearDelay})
		return
	}

	pct := calcWorkflowPct(step, total, message)
	maxV := total
	stepV := step
	if maxV <= 0 && pct > 0 {
		maxV = 100
		stepV = pct
	}

	title := fmt.Sprintf("%s进行中", fullLabel)
	shortText := fullLabel
	if pct > 0 {
		title = fmt.Sprintf("%s · %d%%", fullLabel, pct)
		shortText = fmt.Sprintf("%s %d%%", shortPrefix, pct)
	} else if total > 0 && step >= 0 {
		shortText = fmt.Sprintf("%s %d/%d", shortPrefix, step, total)
	}

	isStart := (step <= 1 && pct <= 5)
	c.mu.Lock()
	wasInactive := !c.active
	c.suppressedRunID = ""
	c.mu.Unlock()

	c.enqueue(LiveProgressUpdate{
		Tag:       c.tag,
		ID:        c.id,
		State:     "workflow",
		Title:     title,
		Text:      truncateIslandText(cleanMsg, 56, fmt.Sprintf("正在自动执行%s...", fullLabel)),
		ShortText: truncateIslandText(shortText, 10, shortPrefix),
		SubText:   "任务",
		Step:      stepV,
		Max:       maxV,
		Progress:  pct,
		ClickURI:  c.clickURI,
		Focus:     true,
		Ongoing:   true,
	}, enqueueOpts{
		force:      wasInactive || isStart,
		resetState: wasInactive,
	})
}

// PreviewDemo renders an immediate showcase Super Island capsule for the requested state
// ("thinking" | "acting" | "workflow" | "confirm" | "done" | "stopped" | "error")
// so the user can test and inspect HyperOS 3.0 Super Island directly from /ui.
func (c *IslandController) PreviewDemo(ctx context.Context, state string) error {
	if c == nil || c.client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	norm := strings.ToLower(strings.TrimSpace(state))
	var u LiveProgressUpdate
	hold := defaultIslandPreviewHold

	switch norm {
	case "acting":
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "acting",
			Title:     "正在读取界面",
			Text:      "第 3/6 步 · 解析屏幕无障碍节点并定位操作按钮",
			ShortText: "读取屏幕",
			SubText:   "控机",
			Step:      3,
			Max:       6,
			Progress:  50,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}
	case "workflow":
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "workflow",
			Title:     "微软积分 · 65%",
			Text:      "阶段 2/4 · 正在执行必应每日搜索 (130/200)",
			ShortText: "积分 65%",
			SubText:   "任务",
			Step:      65,
			Max:       100,
			Progress:  65,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}
	case "confirm":
		hold = 15 * time.Second
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "confirm",
			Title:     "需要人工核对",
			Text:      "已按安全红线停在确认页，点击灵动岛直达控制台",
			ShortText: "待确认",
			SubText:   "核对",
			Step:      7,
			Max:       8,
			Progress:  88,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}
	case "done":
		hold = 5 * time.Second
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "done",
			Title:     "任务已完成",
			Text:      "耗时 1.6s · 共 5 步操作全部完成",
			ShortText: "已完成",
			SubText:   "完成",
			Step:      100,
			Max:       100,
			Progress:  100,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}
	case "stopped":
		hold = 5 * time.Second
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "stopped",
			Title:     "任务已停止",
			Text:      "灵动岛停止态预览 · 点击可打开控制台",
			ShortText: "已停止",
			SubText:   "停止",
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}
	case "error":
		hold = 5 * time.Second
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "error",
			Title:     "任务执行异常",
			Text:      "灵动岛告警态预览 · 点击可查看详细日志",
			ShortText: "执行异常",
			SubText:   "异常",
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   false,
		}
	default:
		u = LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			State:     "thinking",
			Title:     "Agent 正在推理",
			Text:      "第 2/8 步 · 正在结合记忆与页面结构规划动作",
			ShortText: "思考 2/8",
			SubText:   "推理",
			Step:      2,
			Max:       8,
			Progress:  25,
			ClickURI:  c.clickURI,
			Focus:     true,
			Ongoing:   true,
		}
	}

	c.mu.Lock()
	if c.clearTimer != nil {
		c.clearTimer.Stop()
		c.clearTimer = nil
	}
	c.pending = nil
	c.suppressedRunID = ""
	c.needsConfirm = (u.State == "confirm")
	c.doneSent = false
	c.gen++
	previewGen := c.gen
	c.active = true
	c.lastState = u.State
	c.lastTitle = u.Title
	c.lastShortText = u.ShortText
	c.lastProgress = u.Progress
	c.lastSentAt = time.Now()
	client := c.client
	c.mu.Unlock()

	c.ipcMu.Lock()
	defer c.ipcMu.Unlock()

	c.mu.Lock()
	stale := (c.gen != previewGen || !c.active)
	c.mu.Unlock()
	if stale {
		return nil
	}

	pctx, cancel := context.WithTimeout(ctx, 1800*time.Millisecond)
	defer cancel()
	if err := client.UpdateLiveProgress(pctx, u); err != nil {
		return err
	}

	c.mu.Lock()
	if c.gen == previewGen && c.active && hold > 0 {
		c.armClearTimerLocked(previewGen, hold)
	}
	c.mu.Unlock()
	return nil
}

func (c *IslandController) enqueue(u LiveProgressUpdate, opts enqueueOpts) {
	c.ensureWorker()

	c.mu.Lock()
	if opts.resetState {
		if c.clearTimer != nil {
			c.clearTimer.Stop()
			c.clearTimer = nil
		}
		c.pending = nil
		c.needsConfirm = false
		c.doneSent = false
		c.gen++
	} else if opts.clearAfter > 0 {
		if c.clearTimer != nil {
			c.clearTimer.Stop()
			c.clearTimer = nil
		}
	}
	if opts.confirm {
		c.needsConfirm = true
	}

	c.active = true
	c.lastState = u.State
	c.lastTitle = u.Title
	c.lastShortText = u.ShortText
	c.lastProgress = u.Progress

	mergedForce := opts.force
	if c.pending != nil && c.pending.gen == c.gen && c.pending.force {
		mergedForce = true
	}
	c.pending = &pendingIslandItem{
		u:          u,
		gen:        c.gen,
		force:      mergedForce,
		clearAfter: opts.clearAfter,
	}
	c.mu.Unlock()

	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *IslandController) workerLoop() {
	var delayTimer *time.Timer
	var delayCh <-chan time.Time

	stopDelay := func() {
		if delayTimer != nil {
			if !delayTimer.Stop() {
				select {
				case <-delayTimer.C:
				default:
				}
			}
			delayCh = nil
		}
	}

	for {
		select {
		case <-c.stopCh:
			stopDelay()
			return
		case <-c.wakeCh:
		case <-delayCh:
			delayCh = nil
		}

		for {
			c.mu.Lock()
			item := c.pending
			if item == nil {
				c.mu.Unlock()
				stopDelay()
				break
			}

			now := time.Now()
			if !item.force && !c.lastSentAt.IsZero() {
				elapsed := now.Sub(c.lastSentAt)
				if elapsed < c.minUpdateInterval {
					wait := c.minUpdateInterval - elapsed
					c.mu.Unlock()
					stopDelay()
					if delayTimer == nil {
						delayTimer = time.NewTimer(wait)
					} else {
						delayTimer.Reset(wait)
					}
					delayCh = delayTimer.C
					break
				}
			}

			c.pending = nil
			c.lastSentAt = now
			sendGen := item.gen
			u := item.u
			clearAfter := item.clearAfter
			client := c.client
			c.mu.Unlock()

			stopDelay()

			c.ipcMu.Lock()
			c.mu.Lock()
			stale := (c.gen != sendGen || !c.active)
			c.mu.Unlock()

			if !stale && client != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
				_ = client.UpdateLiveProgress(ctx, u)
				cancel()
			}

			c.mu.Lock()
			if !stale && c.gen == sendGen && c.active && clearAfter > 0 {
				c.armClearTimerLocked(sendGen, clearAfter)
			}
			hasMore := c.pending != nil
			c.mu.Unlock()
			c.ipcMu.Unlock()

			if !hasMore {
				break
			}
		}
	}
}

func (c *IslandController) armClearTimerLocked(gen uint64, delay time.Duration) {
	if c.clearTimer != nil {
		c.clearTimer.Stop()
		c.clearTimer = nil
	}
	tag, id := c.tag, c.id
	client := c.client
	c.clearTimer = time.AfterFunc(delay, func() {
		c.ipcMu.Lock()
		defer c.ipcMu.Unlock()

		c.mu.Lock()
		if c.gen != gen || !c.active {
			c.mu.Unlock()
			return
		}
		c.pending = nil
		c.active = false
		c.needsConfirm = false
		c.doneSent = false
		c.clearTimer = nil
		c.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = client.CancelLiveProgress(ctx, tag, id)
	})
}

// ClearImmediate synchronously cancels any active Super Island capsule & notification
// without racing in-flight worker sends (must be invoked BEFORE stopping microkernel on guard trigger).
func (c *IslandController) ClearImmediate(_ string) {
	if c == nil || c.client == nil {
		return
	}
	c.mu.Lock()
	if c.clearTimer != nil {
		c.clearTimer.Stop()
		c.clearTimer = nil
	}
	c.pending = nil
	if c.currentRunID != "" {
		c.suppressedRunID = c.currentRunID
	}
	c.gen++
	clearGen := c.gen
	wasActive := c.active
	c.active = false
	c.needsConfirm = false
	c.doneSent = false
	tag, id := c.tag, c.id
	client := c.client
	c.mu.Unlock()

	if !wasActive {
		return
	}
	c.ipcMu.Lock()
	defer c.ipcMu.Unlock()

	c.mu.Lock()
	superseded := c.gen != clearGen
	c.mu.Unlock()
	if superseded {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = client.CancelLiveProgress(ctx, tag, id)
}

func calcProgressPct(step, maxSteps int) int {
	if maxSteps <= 0 || step <= 0 {
		return 0
	}
	if maxSteps >= 40 {
		if step >= maxSteps {
			return 95
		}
		pct := 16 + (76*step)/(step+6)
		if pct < 16 {
			pct = 16
		}
		if pct > 92 {
			pct = 92
		}
		return pct
	}
	if step >= maxSteps {
		return 100
	}
	pct := (step * 100) / maxSteps
	if pct < 5 {
		pct = 5
	}
	if pct > 99 {
		pct = 99
	}
	return pct
}

func calcWorkflowPct(step, total int, msg string) int {
	base := 0
	if total > 0 && step > 0 {
		if step >= total {
			base = 100
		} else {
			base = (step * 100) / total
		}
	}
	if m := pctInTextRe.FindStringSubmatch(msg); len(m) == 2 {
		if v, err := strconv.Atoi(m[1]); err == nil && v >= 0 && v <= 100 {
			if base == 0 {
				return v
			}
		}
	}
	if base == 0 {
		if m := ratioInTextRe.FindStringSubmatch(msg); len(m) == 3 {
			cur, err1 := strconv.Atoi(m[1])
			mx, err2 := strconv.Atoi(m[2])
			if err1 == nil && err2 == nil && mx > 0 && cur >= 0 {
				if cur >= mx {
					return 100
				}
				return (cur * 100) / mx
			}
		}
	}
	return base
}

func friendlyWorkflowNames(jobName string) (fullLabel string, shortPrefix string) {
	switch strings.ToLower(strings.TrimSpace(jobName)) {
	case "ms_rewards", "rewards", "run_ms_rewards":
		return "微软积分", "积分"
	case "jd_daily", "jd", "run_jd_daily":
		return "京东日常", "京东"
	case "daily_all", "run_daily_all":
		return "全套日常", "日常"
	default:
		cleaned := cleanIslandBody(jobName)
		if cleaned == "" {
			return "日常任务", "任务"
		}
		return truncateIslandText(cleaned, 8, "日常任务"), truncateIslandText(cleaned, 4, "任务")
	}
}

func formatToolIslandTitles(tool, rawLabel string) (state, title, shortText string) {
	switch strings.TrimSpace(tool) {
	case "phone_ui_tree":
		return "acting", "正在读取界面", "读取屏幕"
	case "phone_screenshot":
		return "acting", "正在获取画面", "获取画面"
	case "phone_tap", "phone_tap_node":
		lbl := cleanIslandBody(rawLabel)
		if lbl != "" && len([]rune(lbl)) <= 6 {
			return "acting", "正在" + lbl, lbl
		}
		return "acting", "正在点击控件", "点击节点"
	case "phone_tap_and_type":
		return "acting", "正在点击并输入", "点击输入"
	case "phone_input_text", "phone_clipboard_set":
		return "acting", "正在输入内容", "输入文本"
	case "phone_swipe", "phone_scroll_node":
		return "acting", "正在滑动页面", "滑动页面"
	case "phone_launch_app", "phone_open_deeplink":
		return "acting", "正在打开应用", "打开应用"
	case "phone_key":
		return "acting", "正在执行按键", "系统按键"
	case "cdp_command", "cdp_list_targets", "cdp_list_sockets":
		return "acting", "正在操控网页", "网页操作"
	case "run_ms_rewards":
		return "workflow", "微软积分进行中", "微软积分"
	case "run_jd_daily":
		return "workflow", "京东日常进行中", "京东日常"
	case "run_daily_all":
		return "workflow", "全套日常进行中", "全套日常"
	case "append_memory", "read_memory":
		return "acting", "正在读写记忆", "读写记忆"
	default:
		lbl := cleanIslandBody(rawLabel)
		if lbl == "" {
			lbl = cleanIslandBody(tool)
		}
		if lbl == "" {
			return "acting", "正在执行操作", "执行操作"
		}
		short := truncateIslandText(lbl, 6, "执行操作")
		if strings.HasPrefix(lbl, "正在") {
			return "acting", truncateIslandText(lbl, 14, "正在执行操作"), short
		}
		return "acting", truncateIslandText("正在"+lbl, 14, "正在执行操作"), short
	}
}

func cleanWorkflowStage(msg string) string {
	s := cleanIslandBody(msg)
	s = stageBracketPrefixRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func cleanIslandBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Strip leading decorative symbols / emojis (🤖, ⚠, ✓, ■, ✕, ⚡, etc.) for clean typography.
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		r := runes[i]
		if unicode.IsSpace(r) || unicode.IsSymbol(r) || (r >= 0x1F000 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF) || r == 0xFE0F {
			i++
			continue
		}
		break
	}
	return strings.Join(strings.Fields(string(runes[i:])), " ")
}

func truncateIslandText(s string, maxRunes int, fallback string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return fallback
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-1]) + "…"
}
