package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"android-agent/internal/config"
	"android-agent/internal/guard"
)

const (
	defaultHeartbeatInterval = 30 * time.Second
	maxSleep                 = 60 * time.Second
	maxScheduleHistory       = 30
	maxScheduleRetries       = 2
	scheduleRetryBackoff     = 5 * time.Minute
)

type ScheduleEntryState struct {
	ID          string `json:"id"`
	TaskType    string `json:"task_type"`
	Job         string `json:"job"`
	Prompt      string `json:"prompt,omitempty"`
	Enabled     bool   `json:"enabled"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	DeferOnGame bool   `json:"defer_on_game"`
	NextRunAt   string `json:"next_run_at,omitempty"`
	LastRunDate string `json:"last_run_date,omitempty"`
	LastState   string `json:"last_state,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

type ScheduleHistoryItem struct {
	EntryID     string  `json:"entry_id"`
	TaskType    string  `json:"task_type,omitempty"`
	Job         string  `json:"job"`
	Prompt      string  `json:"prompt,omitempty"`
	State       string  `json:"state"` // "completed" | "failed" | "deferred" | "skipped"
	Message     string  `json:"message,omitempty"`
	TriggeredAt string  `json:"triggered_at"`
	ElapsedSec  float64 `json:"elapsed_sec,omitempty"`
}

type SchedulerStatus struct {
	Enabled     bool                  `json:"enabled"`
	Entries     []ScheduleEntryState  `json:"entries"`
	History     []ScheduleHistoryItem `json:"history,omitempty"`
	LastCheckAt string                `json:"last_check_at,omitempty"`
}

type entryRuntime struct {
	entry         config.ScheduleEntry
	plannedDate   string
	nextRunAt     time.Time
	windowEndTime time.Time
	lastRunDate   string
	lastState     string
	lastMessage   string
	retryCount    int
}

// JobRunFunc executes a scheduled job by name inside a JobManager worker.
type JobRunFunc func(ctx context.Context, jobName string, logFn func(string)) (any, error)

// PromptRunFunc executes a custom scheduled natural-language AI prompt turn.
type PromptRunFunc func(ctx context.Context, prompt string) (any, error)

// Scheduler runs lightweight randomized daily cron jobs while respecting game-guard pauses.
type Scheduler struct {
	mu             sync.Mutex
	cfg            config.ScheduleConfig
	guard          *guard.Guard
	jobs           *JobManager
	runJob         JobRunFunc
	promptRunner   PromptRunFunc
	historyPath    string
	logger         func(string, ...any)
	screenUnlocked func(context.Context) bool

	runtimes    map[string]*entryRuntime
	history     []ScheduleHistoryItem
	lastCheckAt time.Time
	tickEvery   time.Duration
	randIntn    func(int) int
	nowFn       func() time.Time
	wakeCh      chan struct{}
}

func (s *Scheduler) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func NewScheduler(
	cfg config.ScheduleConfig,
	g *guard.Guard,
	jobs *JobManager,
	historyPath string,
	runJob JobRunFunc,
	logger func(string, ...any),
) *Scheduler {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	s := &Scheduler{
		cfg:         cfg,
		guard:       g,
		jobs:        jobs,
		runJob:      runJob,
		historyPath: strings.TrimSpace(historyPath),
		logger:      logger,
		runtimes:    make(map[string]*entryRuntime),
		tickEvery:   defaultHeartbeatInterval,
		randIntn:    rand.Intn,
		nowFn:       time.Now,
		wakeCh:      make(chan struct{}, 1),
	}
	now := s.now()
	s.syncRuntimesLocked(now)
	s.loadHistory()
	return s
}

// SetScreenUnlockChecker configures an optional display/lockscreen check invoked before
// triggering scheduled jobs. When it returns false, the job is deferred instead of burning the day.
func (s *Scheduler) SetScreenUnlockChecker(fn func(context.Context) bool) {
	s.mu.Lock()
	s.screenUnlocked = fn
	s.mu.Unlock()
}

// SetPromptRunner binds the autonomous AI Agent executor used when ScheduleEntry.TaskType == "prompt".
func (s *Scheduler) SetPromptRunner(fn func(ctx context.Context, prompt string) (any, error)) {
	s.mu.Lock()
	s.promptRunner = fn
	s.mu.Unlock()
}

func normalizeScheduleEntry(idx int, e config.ScheduleEntry) config.ScheduleEntry {
	e.TaskType = e.NormalizedTaskType()
	e.Job = strings.TrimSpace(e.Job)
	e.Prompt = strings.TrimSpace(e.Prompt)
	e.ID = strings.TrimSpace(e.ID)
	if e.ID == "" {
		if e.TaskType == "prompt" {
			e.ID = fmt.Sprintf("prompt-%d", idx+1)
		} else {
			job := e.Job
			if job == "" {
				job = "workflow"
			}
			e.ID = fmt.Sprintf("job-%d-%s", idx+1, job)
		}
	}
	if e.TaskType == "prompt" && e.Job == "" {
		e.Job = "ai_prompt"
	}
	return e
}

func (s *Scheduler) loadHistory() {
	if s.historyPath == "" {
		return
	}
	raw, err := os.ReadFile(s.historyPath)
	if err != nil {
		return
	}
	var hist []ScheduleHistoryItem
	if err := json.Unmarshal(raw, &hist); err == nil {
		if len(hist) > maxScheduleHistory {
			hist = hist[len(hist)-maxScheduleHistory:]
		}
		s.history = hist
		s.hydrateRuntimesFromHistoryLocked(s.now())
	}
}

func (s *Scheduler) hydrateRuntimesFromHistoryLocked(now time.Time) {
	if len(s.history) == 0 {
		return
	}
	today := now.Format("2006-01-02")
	for _, item := range s.history {
		ts, err := time.Parse(time.RFC3339, item.TriggeredAt)
		if err != nil {
			continue
		}
		if ts.In(now.Location()).Format("2006-01-02") != today {
			continue
		}
		rt := s.runtimes[item.EntryID]
		if rt == nil {
			continue
		}
		switch item.State {
		case "completed", "skipped":
			rt.lastRunDate = today
			rt.lastState = item.State
			rt.lastMessage = item.Message
		case "failed":
			// Only mark today consumed if this wasn't an intermediate retry-scheduled failure.
			if !strings.HasPrefix(item.Message, "retry ") {
				rt.lastRunDate = today
			}
			rt.lastState = item.State
			rt.lastMessage = item.Message
		case "deferred":
			if rt.lastRunDate != today {
				rt.lastState = "deferred"
				rt.lastMessage = item.Message
			}
		}
	}
}

func (s *Scheduler) saveHistoryLocked() {
	if s.historyPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.historyPath), 0o700)
	data, err := json.MarshalIndent(s.history, "", "  ")
	if err != nil {
		return
	}
	_ = writeAtomicFile(s.historyPath, append(data, '\n'), 0o600)
}

func (s *Scheduler) recordHistoryLocked(item ScheduleHistoryItem) {
	s.history = append(s.history, item)
	if len(s.history) > maxScheduleHistory {
		s.history = s.history[len(s.history)-maxScheduleHistory:]
	}
	s.saveHistoryLocked()
}

func (s *Scheduler) SetHistoryPath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyPath = strings.TrimSpace(path)
	s.syncRuntimesLocked(s.now())
	s.loadHistory()
}

func (s *Scheduler) UpdateConfig(cfg config.ScheduleConfig) {
	s.mu.Lock()
	prev := s.runtimes
	s.cfg = cfg
	s.runtimes = make(map[string]*entryRuntime, len(cfg.Entries))
	now := s.now()
	today := now.Format("2006-01-02")
	normalized := make([]config.ScheduleEntry, len(cfg.Entries))
	for i, raw := range cfg.Entries {
		e := normalizeScheduleEntry(i, raw)
		normalized[i] = e
		rt := &entryRuntime{entry: e, plannedDate: today}
		if old, ok := prev[e.ID]; ok {
			rt.lastRunDate = old.lastRunDate
			rt.lastState = old.lastState
			rt.lastMessage = old.lastMessage
		}
		// Force re-randomization of today's window if user edited the window.
		rt.nextRunAt, rt.windowEndTime = s.pickRandomTrigger(now, e.WindowStart, e.WindowEnd)
		s.runtimes[e.ID] = rt
	}
	s.cfg.Entries = normalized
	s.mu.Unlock()

	if s.wakeCh != nil {
		select {
		case s.wakeCh <- struct{}{}:
		default:
		}
	}
}

func (s *Scheduler) syncRuntimesLocked(now time.Time) {
	today := now.Format("2006-01-02")
	activeIDs := make(map[string]struct{}, len(s.cfg.Entries))
	for i, raw := range s.cfg.Entries {
		e := normalizeScheduleEntry(i, raw)
		s.cfg.Entries[i] = e
		activeIDs[e.ID] = struct{}{}

		rt, ok := s.runtimes[e.ID]
		if !ok {
			rt = &entryRuntime{entry: e}
			s.runtimes[e.ID] = rt
		} else {
			rt.entry = e
		}
		if rt.plannedDate != today {
			rt.plannedDate = today
			rt.retryCount = 0
			rt.nextRunAt, rt.windowEndTime = s.pickRandomTrigger(now, e.WindowStart, e.WindowEnd)
		}
	}
	for id := range s.runtimes {
		if _, ok := activeIDs[id]; !ok {
			delete(s.runtimes, id)
		}
	}
}

func (s *Scheduler) pickRandomTrigger(now time.Time, startStr, endStr string) (time.Time, time.Time) {
	sh, sm, ok1 := parseHHMM(startStr)
	eh, em, ok2 := parseHHMM(endStr)
	if !ok1 {
		sh, sm = 8, 15
	}
	if !ok2 {
		eh, em = 9, 30
	}
	y, m, d := now.Date()
	loc := now.Location()
	start := time.Date(y, m, d, sh, sm, 0, 0, loc)
	end := time.Date(y, m, d, eh, em, 0, 0, loc)
	if end.Before(start) {
		end = start.Add(30 * time.Minute)
	}
	spanSec := int(end.Sub(start).Seconds())
	if spanSec <= 0 {
		return start, end
	}
	offsetSec := s.randIntn(spanSec + 1)
	return start.Add(time.Duration(offsetSec) * time.Second), end
}

// Run dynamically sleeps until the next scheduled window (or sleeps with 0 wakeups while disabled).
func (s *Scheduler) Run(ctx context.Context) {
	for {
		now := time.Now()
		s.Tick(ctx, now)

		delay, ok := s.nextWakeDelay(time.Now())
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-s.wakeCh:
				continue
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (s *Scheduler) nextWakeDelay(now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncRuntimesLocked(now)
	if !s.cfg.Enabled {
		return 0, false
	}

	today := now.Format("2006-01-02")
	enabledCount := 0
	var earliest time.Duration = -1

	for _, e := range s.cfg.Entries {
		rt := s.runtimes[e.ID]
		if rt == nil || !rt.entry.Enabled {
			continue
		}
		enabledCount++
		if rt.lastRunDate == today {
			continue
		}
		if now.Before(rt.nextRunAt) {
			d := rt.nextRunAt.Sub(now)
			if earliest < 0 || d < earliest {
				earliest = d
			}
		} else {
			// Due now or deferred within window: recheck every heartbeat interval.
			earliest = s.tickEvery
			break
		}
	}

	if enabledCount == 0 {
		return 0, false
	}
	if earliest < 0 {
		// All enabled entries have finished/skipped today; wake shortly after local midnight.
		y, m, d := now.Date()
		tomorrow := time.Date(y, m, d+1, 0, 0, 5, 0, now.Location())
		earliest = tomorrow.Sub(now)
	}
	if earliest <= 0 {
		earliest = time.Second
	}
	if earliest > maxSleep {
		earliest = maxSleep
	}
	return earliest, true
}

// Tick evaluates due scheduled entries at timestamp now (exported for deterministic tests).
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	today := now.Format("2006-01-02")

	s.mu.Lock()
	s.lastCheckAt = now
	s.syncRuntimesLocked(now)
	if !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}

	var due []*entryRuntime
	for _, e := range s.cfg.Entries {
		rt := s.runtimes[e.ID]
		if rt == nil || !rt.entry.Enabled {
			continue
		}
		if rt.lastRunDate == today {
			continue
		}
		if now.Before(rt.nextRunAt) {
			continue
		}
		// Avoid catching up hours after the configured window has passed unless deferred.
		if rt.lastState != "deferred" && now.After(rt.windowEndTime.Add(30*time.Minute)) {
			rt.lastRunDate = today
			rt.lastState = "skipped"
			rt.lastMessage = "missed daily window (daemon started after window end)"
			continue
		}
		due = append(due, rt)
	}
	s.mu.Unlock()

	for _, rt := range due {
		s.triggerEntry(ctx, rt, now, today)
	}
}

func isNonRetryableScheduleError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "captcha") ||
		strings.Contains(low, "prize_selection_required") ||
		strings.Contains(low, "login_required")
}

// TriggerNow immediately launches any configured schedule entry by ID (used by /ui [立即试跑]),
// bypassing daily window/enabled gating while still enforcing Game Guard and single-flight locks.
func (s *Scheduler) TriggerNow(entryID string) error {
	entryID = strings.TrimSpace(entryID)
	if entryID == "" {
		return errors.New("entry_id is required")
	}
	if s.guard != nil {
		if blocked, reason := s.guard.IsActionBlocked(); blocked {
			return fmt.Errorf("agent guard active: %s", reason)
		}
	}

	s.mu.Lock()
	now := s.now()
	today := now.Format("2006-01-02")
	s.syncRuntimesLocked(now)
	rt := s.runtimes[entryID]
	if rt == nil {
		s.mu.Unlock()
		return fmt.Errorf("schedule entry %q not found", entryID)
	}
	if rt.lastState == "running" {
		s.mu.Unlock()
		return fmt.Errorf("schedule entry %q is already running", entryID)
	}
	s.mu.Unlock()

	return s.startEntryExecution(rt, now, today, true)
}

func (s *Scheduler) triggerEntry(ctx context.Context, rt *entryRuntime, now time.Time, today string) {
	if s.guard != nil {
		if blocked, reason := s.guard.IsActionBlocked(); blocked {
			s.mu.Lock()
			if rt.entry.DeferOnGame && now.Before(rt.windowEndTime.Add(3*time.Hour)) {
				rt.nextRunAt = now.Add(15 * time.Minute)
				rt.lastState = "deferred"
				rt.lastMessage = reason
				s.recordHistoryLocked(ScheduleHistoryItem{
					EntryID:     rt.entry.ID,
					TaskType:    rt.entry.NormalizedTaskType(),
					Job:         rt.entry.Job,
					Prompt:      rt.entry.Prompt,
					State:       "deferred",
					Message:     reason,
					TriggeredAt: now.Format(time.RFC3339),
				})
				s.logger("schedule: %s deferred (%s)", rt.entry.ID, reason)
			} else {
				rt.lastRunDate = today
				rt.lastState = "skipped"
				rt.lastMessage = reason
				s.recordHistoryLocked(ScheduleHistoryItem{
					EntryID:     rt.entry.ID,
					TaskType:    rt.entry.NormalizedTaskType(),
					Job:         rt.entry.Job,
					Prompt:      rt.entry.Prompt,
					State:       "skipped",
					Message:     reason,
					TriggeredAt: now.Format(time.RFC3339),
				})
				s.logger("schedule: %s skipped today (%s)", rt.entry.ID, reason)
			}
			s.mu.Unlock()
			return
		}
	}

	s.mu.Lock()
	unlockChecker := s.screenUnlocked
	s.mu.Unlock()
	if unlockChecker != nil && !unlockChecker(ctx) {
		s.mu.Lock()
		if now.Before(rt.windowEndTime) {
			next := now.Add(15 * time.Minute)
			if next.After(rt.windowEndTime) {
				next = rt.windowEndTime
			}
			rt.nextRunAt = next
			rt.lastState = "deferred"
			rt.lastMessage = "screen locked or off; deferred"
			s.recordHistoryLocked(ScheduleHistoryItem{
				EntryID:     rt.entry.ID,
				TaskType:    rt.entry.NormalizedTaskType(),
				Job:         rt.entry.Job,
				Prompt:      rt.entry.Prompt,
				State:       "deferred",
				Message:     rt.lastMessage,
				TriggeredAt: now.Format(time.RFC3339),
			})
			s.logger("schedule: %s deferred until %s (screen locked)", rt.entry.ID, next.Format("15:04"))
		} else {
			rt.lastRunDate = today
			rt.lastState = "skipped"
			rt.lastMessage = "screen remained locked past window end"
			s.recordHistoryLocked(ScheduleHistoryItem{
				EntryID:     rt.entry.ID,
				TaskType:    rt.entry.NormalizedTaskType(),
				Job:         rt.entry.Job,
				Prompt:      rt.entry.Prompt,
				State:       "skipped",
				Message:     rt.lastMessage,
				TriggeredAt: now.Format(time.RFC3339),
			})
			s.logger("schedule: %s skipped today (screen remained locked)", rt.entry.ID)
		}
		s.mu.Unlock()
		return
	}

	_ = s.startEntryExecution(rt, now, today, false)
}

func (s *Scheduler) startEntryExecution(rt *entryRuntime, now time.Time, today string, manual bool) error {
	s.mu.Lock()
	entry := rt.entry
	taskType := entry.NormalizedTaskType()
	entryID := entry.ID
	jobName := entry.Job
	prompt := strings.TrimSpace(entry.Prompt)
	promptRunner := s.promptRunner
	runJob := s.runJob
	jobs := s.jobs

	if taskType == "prompt" {
		if prompt == "" {
			err := fmt.Errorf("schedule entry %q has empty prompt", entryID)
			if !manual {
				rt.lastRunDate = today
				rt.lastState = "failed"
				rt.lastMessage = err.Error()
			}
			s.mu.Unlock()
			return err
		}
		if promptRunner == nil {
			err := errors.New("prompt runner is not configured")
			if !manual {
				rt.lastRunDate = today
				rt.lastState = "failed"
				rt.lastMessage = err.Error()
			}
			s.mu.Unlock()
			return err
		}
	} else {
		if jobs == nil || runJob == nil {
			s.mu.Unlock()
			return errors.New("workflow job runner is not configured")
		}
	}

	prevRunDate := rt.lastRunDate
	prevState := rt.lastState
	prevMessage := rt.lastMessage

	if !manual {
		rt.lastRunDate = today
		rt.lastMessage = "started by heartbeat scheduler"
	} else {
		if s.cfg.Enabled && entry.Enabled && !now.Before(rt.nextRunAt) && now.Before(rt.windowEndTime.Add(30*time.Minute)) {
			rt.lastRunDate = today
		}
		rt.lastMessage = "triggered manually"
	}
	rt.lastState = "running"
	s.mu.Unlock()

	onComplete := func(started time.Time, res any, runErr error) {
		elapsed := float64(int(time.Since(started).Seconds()*10)) / 10
		s.mu.Lock()
		var histState string
		if runErr != nil {
			if !manual && !isNonRetryableScheduleError(runErr) && rt.retryCount < maxScheduleRetries {
				rt.retryCount++
				rt.lastRunDate = ""
				baseTime := now
				if realNow := s.now(); realNow.After(baseTime) && realNow.Sub(baseTime) < 2*time.Hour {
					baseTime = realNow
				}
				rt.nextRunAt = baseTime.Add(scheduleRetryBackoff)
				rt.lastState = "deferred"
				rt.lastMessage = fmt.Sprintf("retry %d/%d scheduled after error: %v", rt.retryCount, maxScheduleRetries, runErr)
				histState = "failed"
			} else {
				rt.lastState = "failed"
				rt.lastMessage = runErr.Error()
				histState = "failed"
			}
		} else {
			rt.retryCount = 0
			rt.lastState = "completed"
			msg := fmt.Sprintf("completed in %.1fs", elapsed)
			if chatRes, ok := res.(*ChatTurnResult); ok && chatRes != nil && strings.TrimSpace(chatRes.Reply) != "" {
				msg = fmt.Sprintf("completed in %.1fs: %s", elapsed, truncateSummaryRunes(chatRes.Reply, 42))
			}
			rt.lastMessage = msg
			histState = "completed"
		}
		s.recordHistoryLocked(ScheduleHistoryItem{
			EntryID:     entryID,
			TaskType:    taskType,
			Job:         jobName,
			Prompt:      prompt,
			State:       histState,
			Message:     rt.lastMessage,
			TriggeredAt: started.Format(time.RFC3339),
			ElapsedSec:  elapsed,
		})
		s.mu.Unlock()

		if s.wakeCh != nil {
			select {
			case s.wakeCh <- struct{}{}:
			default:
			}
		}
	}

	var startErr error
	if taskType == "prompt" {
		worker := func(jctx context.Context, logFn func(string)) (any, error) {
			started := time.Now()
			if logFn != nil {
				logFn("[阶段 1/1] 执行定时 AI 指令: " + truncateSummaryRunes(prompt, 44))
			}
			res, runErr := promptRunner(jctx, prompt)
			onComplete(started, res, runErr)
			return res, runErr
		}
		if jobs != nil {
			_, startErr = jobs.startPromptJob("cron:prompt:"+entryID, worker)
		} else {
			go func() {
				_, _ = worker(context.Background(), nil)
			}()
		}
	} else {
		_, startErr = jobs.Start("cron:"+jobName, func(jctx context.Context, logFn func(string)) (any, error) {
			started := time.Now()
			res, runErr := runJob(jctx, jobName, logFn)
			onComplete(started, res, runErr)
			return res, runErr
		})
	}

	if startErr != nil {
		s.mu.Lock()
		if manual {
			rt.lastRunDate = prevRunDate
			rt.lastState = prevState
			rt.lastMessage = prevMessage
		} else {
			// Busy with manual job or chat turn: retry in 5 minutes cleanly.
			rt.lastRunDate = ""
			rt.nextRunAt = now.Add(scheduleRetryBackoff)
			rt.lastState = "deferred"
			rt.lastMessage = startErr.Error()
		}
		s.mu.Unlock()
		return startErr
	}

	s.logger("schedule: triggered %s (%s:%s)", entryID, taskType, jobName)
	return nil
}

func (s *Scheduler) Status() SchedulerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncRuntimesLocked(s.now())

	entries := make([]ScheduleEntryState, 0, len(s.cfg.Entries))
	for _, e := range s.cfg.Entries {
		rt := s.runtimes[e.ID]
		st := ScheduleEntryState{
			ID:          e.ID,
			TaskType:    e.NormalizedTaskType(),
			Job:         e.Job,
			Prompt:      e.Prompt,
			Enabled:     e.Enabled,
			WindowStart: e.WindowStart,
			WindowEnd:   e.WindowEnd,
			DeferOnGame: e.DeferOnGame,
		}
		if rt != nil {
			if !rt.nextRunAt.IsZero() {
				st.NextRunAt = rt.nextRunAt.Format(time.RFC3339)
			}
			st.LastRunDate = rt.lastRunDate
			st.LastState = rt.lastState
			st.LastMessage = rt.lastMessage
		}
		entries = append(entries, st)
	}

	hist := make([]ScheduleHistoryItem, len(s.history))
	copy(hist, s.history)
	out := SchedulerStatus{
		Enabled: s.cfg.Enabled,
		Entries: entries,
		History: hist,
	}
	if !s.lastCheckAt.IsZero() {
		out.LastCheckAt = s.lastCheckAt.Format(time.RFC3339)
	}
	return out
}

func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
