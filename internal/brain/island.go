package brain

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	DefaultIslandTag           = "agent_live"
	DefaultIslandID            = 9900
	defaultIslandMinUpdate     = 350 * time.Millisecond
	defaultIslandDoneClear     = 3 * time.Second
	defaultIslandConfirmClear  = 5 * time.Minute
)

// LiveProgressUpdate describes a real-time progress payload rendered on HyperOS Super Island
// and the ongoing silent notification shade.
type LiveProgressUpdate struct {
	Tag       string
	ID        int
	Title     string
	Text      string
	ShortText string
	SubText   string
	Step      int
	Max       int
	Focus     bool
	Ongoing   bool
}

// LiveProgressClient abstracts the device notification transport (AgentKernel / fallback).
type LiveProgressClient interface {
	UpdateLiveProgress(ctx context.Context, u LiveProgressUpdate) error
	CancelLiveProgress(ctx context.Context, tag string, id int) error
}

// IslandController drives HyperOS 3.0 Focus / Super Island progress capsules cleanly
// during active Agent turns: 0 notifications when idle, throttled updates during work,
// 3s auto-clear on completion/cancel, persistent warning on SOUL confirmation, and
// millisecond cancellation when game-guard triggers.
type IslandController struct {
	mu                sync.Mutex
	client            LiveProgressClient
	tag               string
	id                int
	minUpdateInterval time.Duration
	doneClearDelay    time.Duration
	confirmClearDelay time.Duration

	active          bool
	needsConfirm    bool
	gen             uint64
	lastSentAt      time.Time
	clearTimer      *time.Timer
	channelName     string
}

func NewIslandController(client LiveProgressClient) *IslandController {
	return &IslandController{
		client:            client,
		tag:               DefaultIslandTag,
		id:                DefaultIslandID,
		minUpdateInterval: defaultIslandMinUpdate,
		doneClearDelay:    defaultIslandDoneClear,
		confirmClearDelay: defaultIslandConfirmClear,
		channelName:       "hyperos-focus-v3",
	}
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
		"tag":                c.tag,
		"id":                 c.id,
	}
}

// HandleEvent translates structured Agent RunEvents into throttled Super Island updates.
func (c *IslandController) HandleEvent(ev RunEvent) {
	if c == nil || c.client == nil {
		return
	}
	switch ev.Type {
	case "run_start":
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "🤖 Agent 启动执行",
			Text:      truncateIslandText(ev.Summary, 48, "正在初始化任务..."),
			ShortText: "🤖 启动任务",
			SubText:   "Agent",
			Step:      0,
			Max:       ev.MaxSteps,
			Focus:     true,
			Ongoing:   true,
		}, true, true, false)

	case "thinking":
		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = 20
		}
		short := fmt.Sprintf("🤖 %d/%d 思考", ev.Step, maxS)
		text := fmt.Sprintf("第 %d/%d 步：正在分析下一步操作...", ev.Step, maxS)
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "🤖 Agent 执行中",
			Text:      text,
			ShortText: short,
			SubText:   "推理中",
			Step:      ev.Step,
			Max:       maxS,
			Focus:     true,
			Ongoing:   true,
		}, false, false, false)

	case "tool_start":
		maxS := ev.MaxSteps
		if maxS <= 0 {
			maxS = 20
		}
		shortAct := ev.Label
		if shortAct == "" {
			shortAct = ev.Tool
		}
		short := truncateIslandText(fmt.Sprintf("🤖 %d/%d %s", ev.Step, maxS, shortAct), 18, "🤖 执行中")
		detail := ev.Summary
		if detail == "" {
			detail = fmt.Sprintf("正在调用 %s...", ev.Tool)
		}
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "🤖 Agent 控机中",
			Text:      truncateIslandText(detail, 56, detail),
			ShortText: short,
			SubText:   "执行中",
			Step:      ev.Step,
			Max:       maxS,
			Focus:     true,
			Ongoing:   true,
		}, false, false, false)

	case "needs_confirmation":
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "⚠ 需要人工确认",
			Text:      "已按 SOUL.md 红线停在确认页，请核对后再手动付款/确认。",
			ShortText: "⚠ 待人工确认",
			SubText:   "请核对",
			Step:      ev.Step,
			Max:       ev.MaxSteps,
			Focus:     true,
			Ongoing:   true,
		}, true, false, true)
		c.scheduleClear(c.confirmClearDelay)

	case "done":
		c.mu.Lock()
		hadConfirm := c.needsConfirm || ev.NeedsConfirmation
		c.mu.Unlock()
		if hadConfirm {
			// Preserve prominent confirmation banner rather than clearing in 3s.
			c.sendUpdate(LiveProgressUpdate{
				Tag:       c.tag,
				ID:        c.id,
				Title:     "⚠ 待人工确认订单/验证",
				Text:      truncateIslandText(ev.Reply, 56, "已停在确认页，等待你亲自核对并操作。"),
				ShortText: "⚠ 待人工确认",
				SubText:   "已暂停",
				Step:      ev.MaxSteps,
				Max:       ev.MaxSteps,
				Focus:     true,
				Ongoing:   false,
			}, true, false, true)
			c.scheduleClear(c.confirmClearDelay)
			return
		}
		summary := fmt.Sprintf("耗时 %.1fs · 共 %d 步", ev.ElapsedSec, ev.Steps)
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "✓ Agent 任务完成",
			Text:      summary,
			ShortText: "✓ 已完成",
			SubText:   "完成",
			Step:      ev.MaxSteps,
			Max:       ev.MaxSteps,
			Focus:     true,
			Ongoing:   false,
		}, true, false, false)
		c.scheduleClear(c.doneClearDelay)

	case "cancelled":
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "■ Agent 已停止",
			Text:      truncateIslandText(ev.Error, 48, "任务已被取消。"),
			ShortText: "■ 已停止",
			SubText:   "已停止",
			Step:      0,
			Max:       0,
			Focus:     true,
			Ongoing:   false,
		}, true, false, false)
		c.scheduleClear(c.doneClearDelay)

	case "error":
		c.sendUpdate(LiveProgressUpdate{
			Tag:       c.tag,
			ID:        c.id,
			Title:     "✕ Agent 执行出错",
			Text:      truncateIslandText(ev.Error, 56, "任务执行失败"),
			ShortText: "✕ 执行出错",
			SubText:   "失败",
			Step:      0,
			Max:       0,
			Focus:     true,
			Ongoing:   false,
		}, true, false, false)
		c.scheduleClear(c.doneClearDelay)
	}
}

func (c *IslandController) sendUpdate(u LiveProgressUpdate, force bool, resetState bool, confirm bool) {
	c.mu.Lock()
	if resetState {
		if c.clearTimer != nil {
			c.clearTimer.Stop()
			c.clearTimer = nil
		}
		c.needsConfirm = false
		c.gen++
	}
	if confirm {
		c.needsConfirm = true
	}
	now := time.Now()
	if !force && c.active && !c.lastSentAt.IsZero() && now.Sub(c.lastSentAt) < c.minUpdateInterval {
		c.mu.Unlock()
		return
	}
	c.active = true
	c.lastSentAt = now
	client := c.client
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = client.UpdateLiveProgress(ctx, u)
}

func (c *IslandController) scheduleClear(delay time.Duration) {
	if delay <= 0 {
		c.ClearImmediate("auto clear")
		return
	}
	c.mu.Lock()
	if c.clearTimer != nil {
		c.clearTimer.Stop()
	}
	c.gen++
	gen := c.gen
	tag, id := c.tag, c.id
	client := c.client
	c.clearTimer = time.AfterFunc(delay, func() {
		c.mu.Lock()
		if c.gen != gen || !c.active {
			c.mu.Unlock()
			return
		}
		c.active = false
		c.needsConfirm = false
		c.clearTimer = nil
		c.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = client.CancelLiveProgress(ctx, tag, id)
	})
	c.mu.Unlock()
}

// ClearImmediate synchronously cancels any active Super Island capsule & notification
// (must be invoked BEFORE stopping the microkernel on game-guard trigger).
func (c *IslandController) ClearImmediate(_ string) {
	if c == nil || c.client == nil {
		return
	}
	c.mu.Lock()
	if c.clearTimer != nil {
		c.clearTimer.Stop()
		c.clearTimer = nil
	}
	c.gen++
	wasActive := c.active
	c.active = false
	c.needsConfirm = false
	tag, id := c.tag, c.id
	client := c.client
	c.mu.Unlock()

	if !wasActive {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = client.CancelLiveProgress(ctx, tag, id)
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
