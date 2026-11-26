package brain

import (
	"context"
	"encoding/json"
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
	maxScheduleHistory       = 30
)

type ScheduleEntryState struct {
	ID            string `json:"id"`
	Job           string `json:"job"`
	Enabled       bool   `json:"enabled"`
	WindowStart   string `json:"window_start"`
	WindowEnd     string `json:"window_end"`
	DeferOnGame   bool   `json:"defer_on_game"`
	NextRunAt     string `json:"next_run_at,omitempty"`
	LastRunDate   string `json:"last_run_date,omitempty"`
	LastState     string `json:"last_state,omitempty"`
	LastMessage   string `json:"last_message,omitempty"`
}

type ScheduleHistoryItem struct {
	EntryID     string  `json:"entry_id"`
	Job         string  `json:"job"`
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
}

// JobRunFunc executes a scheduled job by name inside a JobManager worker.
type JobRunFunc func(ctx context.Context, jobName string, logFn func(string)) (any, error)

// Scheduler runs lightweight randomized daily cron jobs while respecting game-guard pauses.
type Scheduler struct {
	mu          sync.Mutex
	cfg         config.ScheduleConfig
	guard       *guard.Guard
	jobs        *JobManager
	runJob      JobRunFunc
	historyPath string
	logger      func(string, ...any)

	runtimes    map[string]*entryRuntime
	history     []ScheduleHistoryItem
	lastCheckAt time.Time
	tickEvery   time.Duration
	randIntn    func(int) int
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
	}
	s.loadHistory()
	s.syncRuntimesLocked(time.Now())
	return s
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
	s.loadHistory()
}

func (s *Scheduler) UpdateConfig(cfg config.ScheduleConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	// Clear existing planned dates so edited windows re-randomize immediately.
	s.runtimes = make(map[string]*entryRuntime, len(cfg.Entries))
	s.syncRuntimesLocked(time.Now())
}

func (s *Scheduler) syncRuntimesLocked(now time.Time) {
	today := now.Format("2006-01-02")
	activeIDs := make(map[string]struct{}, len(s.cfg.Entries))
	for i, e := range s.cfg.Entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			id = fmt.Sprintf("job-%d-%s", i+1, e.Job)
		}
		e.ID = id
		activeIDs[id] = struct{}{}

		rt, ok := s.runtimes[id]
		if !ok {
			rt = &entryRuntime{entry: e}
			s.runtimes[id] = rt
		} else {
			rt.entry = e
		}
		if rt.plannedDate != today {
			rt.plannedDate = today
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

// Run ticks every tickEvery until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.Tick(ctx, time.Now())
	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			s.Tick(ctx, t)
		}
	}
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
			s.mu.Unlock()
			return
		}
		due = append(due, rt)
	}
	s.mu.Unlock()

	for _, rt := range due {
		s.triggerEntry(ctx, rt, now, today)
	}
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
					Job:         rt.entry.Job,
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
					Job:         rt.entry.Job,
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

	if s.jobs == nil || s.runJob == nil {
		return
	}

	// Mark today's attempt before starting to prevent duplicate concurrent launches on next tick.
	s.mu.Lock()
	rt.lastRunDate = today
	rt.lastState = "running"
	rt.lastMessage = "started by heartbeat scheduler"
	jobName := rt.entry.Job
	entryID := rt.entry.ID
	s.mu.Unlock()

	_, err := s.jobs.Start("cron:"+jobName, func(jctx context.Context, logFn func(string)) (any, error) {
		started := time.Now()
		res, runErr := s.runJob(jctx, jobName, logFn)
		elapsed := float64(int(time.Since(started).Seconds()*10)) / 10

		s.mu.Lock()
		if runErr != nil {
			rt.lastState = "failed"
			rt.lastMessage = runErr.Error()
		} else {
			rt.lastState = "completed"
			rt.lastMessage = fmt.Sprintf("completed in %.1fs", elapsed)
		}
		s.recordHistoryLocked(ScheduleHistoryItem{
			EntryID:     entryID,
			Job:         jobName,
			State:       rt.lastState,
			Message:     rt.lastMessage,
			TriggeredAt: started.Format(time.RFC3339),
			ElapsedSec:  elapsed,
		})
		s.mu.Unlock()
		return res, runErr
	})
	if err != nil {
		// Busy with manual job: retry in 5 minutes cleanly.
		s.mu.Lock()
		rt.lastRunDate = ""
		rt.nextRunAt = now.Add(5 * time.Minute)
		rt.lastState = "deferred"
		rt.lastMessage = err.Error()
		s.mu.Unlock()
	} else {
		s.logger("schedule: triggered %s (%s)", entryID, jobName)
	}
}

func (s *Scheduler) Status() SchedulerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncRuntimesLocked(time.Now())

	entries := make([]ScheduleEntryState, 0, len(s.cfg.Entries))
	for _, e := range s.cfg.Entries {
		rt := s.runtimes[e.ID]
		st := ScheduleEntryState{
			ID:          e.ID,
			Job:         e.Job,
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
