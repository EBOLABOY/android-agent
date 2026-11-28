package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"android-agent/internal/backend"
)

type stubFrozenErr struct{ msg string }

func (e stubFrozenErr) Error() string        { return e.msg }
func (e stubFrozenErr) IsTargetFrozen() bool { return true }

type fakeBackend struct {
	launched   []string
	tappedNode []int
	uiTree     *backend.UITreeResult
	launchErr  error
	notifs     []string
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) ScreenInfo(context.Context) (backend.ScreenInfo, error) {
	return backend.ScreenInfo{Width: 1080, Height: 2400, Density: 440}, nil
}
func (f *fakeBackend) Screenshot(context.Context, int, int) (*backend.ScreenshotResult, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeBackend) Tap(context.Context, int, int) error                  { return nil }
func (f *fakeBackend) LongPress(context.Context, int, int, int) error       { return nil }
func (f *fakeBackend) Swipe(context.Context, int, int, int, int, int) error { return nil }
func (f *fakeBackend) KeyEvent(context.Context, string) error               { return nil }
func (f *fakeBackend) InputText(context.Context, string, string, bool) (string, error) {
	return "clipboard", nil
}
func (f *fakeBackend) SetClipboard(context.Context, string) error { return nil }
func (f *fakeBackend) GetClipboard(context.Context) (string, error) {
	return "", nil
}
func (f *fakeBackend) ForegroundApp(context.Context) (backend.ForegroundInfo, error) {
	return backend.ForegroundInfo{}, nil
}
func (f *fakeBackend) LaunchApp(_ context.Context, pkg string) error {
	f.launched = append(f.launched, pkg)
	return f.launchErr
}
func (f *fakeBackend) OpenDeepLink(context.Context, string, string) error { return nil }
func (f *fakeBackend) PostNotification(_ context.Context, _, title, _ string) error {
	f.notifs = append(f.notifs, title)
	return nil
}
func (f *fakeBackend) UpdateLiveProgress(context.Context, backend.LiveProgressSpec) error { return nil }
func (f *fakeBackend) CancelLiveProgress(context.Context, string, int) error              { return nil }
func (f *fakeBackend) UITree(context.Context, backend.UITreeOptions) (*backend.UITreeResult, error) {
	return f.uiTree, nil
}
func (f *fakeBackend) TapNode(_ context.Context, id int) (*backend.UINode, int, int, error) {
	f.tappedNode = append(f.tappedNode, id)
	return &backend.UINode{ID: id}, 100, 200, nil
}
func (f *fakeBackend) ScrollNode(context.Context, int, string, float64, int) ([4]int, [4]int, error) {
	return [4]int{}, [4]int{}, nil
}
func (f *fakeBackend) StopMicroKernel(string) {}
func (f *fakeBackend) MicroKernelRunning() bool {
	return false
}
func (f *fakeBackend) TouchStatus() backend.TouchStatusInfo {
	return backend.TouchStatusInfo{}
}

func TestSearchQueryPoolSizeAndUniqueness(t *testing.T) {
	if len(SearchQueryPool) < 80 {
		t.Fatalf("expected >= 80 queries in SearchQueryPool, got %d", len(SearchQueryPool))
	}
	seen := make(map[string]bool)
	for _, q := range SearchQueryPool {
		low := strings.ToLower(strings.TrimSpace(q))
		if low == "" {
			t.Fatal("empty query in SearchQueryPool")
		}
		if seen[low] {
			t.Fatalf("duplicate query in SearchQueryPool: %q", q)
		}
		seen[low] = true
	}
}

func TestCleanHeadlineToQuery(t *testing.T) {
	q1 := CleanHeadlineToQuery("“Trump proposes new tariffs on China, Europe and other trade partners today!”")
	if len([]rune(q1)) < 6 || len([]rune(q1)) > 42 || strings.ContainsAny(q1, `“”,!`) {
		t.Fatalf("unexpected cleaned EN query: %q", q1)
	}
	q2 := CleanHeadlineToQuery("【最新】小米13澎湃OS升级指南：续航与拍照实测")
	if len([]rune(q2)) < 6 || len([]rune(q2)) > 42 || strings.ContainsAny(q2, "【】：") {
		t.Fatalf("unexpected cleaned ZH query: %q", q2)
	}
	if CleanHeadlineToQuery("hi") != "" {
		t.Fatal("expected short headline to be rejected")
	}
}

func TestCheckCaptchaAndTaskFilters(t *testing.T) {
	if err := CheckCaptcha("请拖动滑块完成安全验证"); err == nil {
		t.Fatal("expected captcha error for Chinese slider")
	}
	if err := CheckCaptcha("Please verify you are human"); err == nil {
		t.Fatal("expected captcha error for human verification")
	}
	if err := CheckCaptcha("东东农场 再浇83.52%结果"); err != nil {
		t.Fatalf("unexpected captcha false positive: %v", err)
	}
	if !ForbiddenTaskRe.MatchString("去快手极速版领金币") {
		t.Fatal("expected ForbiddenTaskRe to match 快手极速版")
	}
	if !SafeBrowseRe.MatchString("精选活动推荐(0/9) 浏览6秒") {
		t.Fatal("expected SafeBrowseRe to match 精选活动推荐")
	}
}

func TestExploreRulesReadToEarnAndURL(t *testing.T) {
	if got := MapExploreQuery("Explore videogames +10 已激活"); got != "Minecraft" {
		t.Fatalf("expected Minecraft for videogames card, got %q", got)
	}
	if got := MapExploreQuery("Explore sports highlights"); !strings.Contains(got, "NBA") {
		t.Fatalf("expected sports rule match, got %q", got)
	}
	if got := MapExploreQuery("Explore best books"); !strings.Contains(got, "books") {
		t.Fatalf("expected books rule match, got %q", got)
	}
	if got := altExploreQuery("Explore videogames"); strings.Contains(strings.ToLower(got), "explore official") {
		t.Fatalf("altExploreQuery must not produce 'Explore official', got %q", got)
	}
	if got := resolveExploreURL("/redeem?x=1"); got != "https://rewards.bing.com/redeem?x=1" {
		t.Fatalf("unexpected relative resolveExploreURL: %q", got)
	}
	if got := resolveExploreURL("https://www.bing.com/search?q=test&OCID=ABC"); got != "https://www.bing.com/search?q=test&OCID=ABC" {
		t.Fatalf("unexpected absolute resolveExploreURL: %q", got)
	}
	if !IsReadToEarnFull("30") || !IsReadToEarnFull("30/30") || IsReadToEarnFull("21/30") {
		t.Fatal("IsReadToEarnFull failed to match 30 and 30/30 properly")
	}
	if got := MapExploreQuery("扩展你的词汇量 在必应上搜索你不理解的单词含义 +10 已激活"); got != "serendipity definition" {
		t.Fatalf("expected vocabulary query, got %q", got)
	}
	if got := altExploreQuery("Expand your vocabulary"); got != "ephemeral meaning" {
		t.Fatalf("expected vocabulary alt query, got %q", got)
	}
	if got := MapExploreQuery("现在几点？ 在必应上搜索，查看不同时间区域的时间。 +10 已激活"); got != "what time is it in Tokyo" {
		t.Fatalf("expected world clock query, got %q", got)
	}
	if got := altExploreQuery("What time is it? time zone"); got != "time in London" {
		t.Fatalf("expected world clock alt query, got %q", got)
	}
}

func TestSearchQueryBatchCountAndBingHeadlineFilter(t *testing.T) {
	if got := SearchQueryBatchCount(200, 200); got != 0 {
		t.Fatalf("expected 0 when already capped, got %d", got)
	}
	if got := SearchQueryBatchCount(45, 200); got < 45 {
		t.Fatalf("expected >=45 queries for 45/200 (31+ searches needed), got %d", got)
	}
	if got := SearchQueryBatchCount(0, 200); got < 56 {
		t.Fatalf("expected >=56 queries for 0/200 (40 searches + headroom), got %d", got)
	}
	if got := SearchQueryBatchCount(185, 200); got < 28 {
		t.Fatalf("expected >=28 minimum batch count, got %d", got)
	}

	good := backend.UINode{ID: 1, Label: "How solid state batteries are transforming EV range in 2026", Clickable: true, Bounds: [4]int{60, 800, 980, 920}}
	if !isBingNewsHeadlineNode(good, 1080, 2400) {
		t.Fatal("expected valid news headline node to pass filter")
	}
	video := backend.UINode{ID: 2, Label: "Watch Video: Amazing highlights from last night", Clickable: true, Bounds: [4]int{60, 800, 980, 920}}
	if isBingNewsHeadlineNode(video, 1080, 2400) {
		t.Fatal("expected video node to be filtered out")
	}
	floatOverlay := backend.UINode{ID: 3, Label: "Floating SSH Terminal Window Status Bar Text", Clickable: true, Bounds: [4]int{760, 560, 1060, 660}}
	if isBingNewsHeadlineNode(floatOverlay, 1080, 2400) {
		t.Fatal("expected top-right floating window node to be filtered out")
	}
}

func TestRunDailyAllReturnsErrorOnSubWorkflowFailure(t *testing.T) {
	be := &fakeBackend{launchErr: errors.New("screen locked")}
	r := NewRunner(be, nil, nil, 720, nil)
	rep, err := r.RunDailyAll(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error from RunDailyAll when both sub-workflows fail")
	}
	if rep == nil || rep.MSError == "" || rep.JDError == "" {
		t.Fatalf("expected partial report with both MSError and JDError, got %+v", rep)
	}
	if len(be.notifs) == 0 || be.notifs[0] == "每日任务完成" {
		t.Fatalf("expected failure notification title, got %v", be.notifs)
	}
}

func TestFrozenCDPRetryLaunchesAppAndSucceeds(t *testing.T) {
	be := &fakeBackend{}
	r := NewRunner(be, nil, nil, 720, nil)
	r.unfreezeDelay = time.Millisecond

	calls := 0
	r.cdpSendFn = func(_ context.Context, _ string, _ string, method string, _ map[string]any) (json.RawMessage, error) {
		if method == "Page.bringToFront" {
			return json.RawMessage(`{}`), nil
		}
		calls++
		if calls == 1 {
			return nil, stubFrozenErr{msg: "target renderer frozen"}
		}
		return json.RawMessage(`{"result":{"type":"string","value":"ok"}}`), nil
	}

	var out string
	if err := r.evalJS(context.Background(), "", "tid-1", "location.href", false, &out); err != nil {
		t.Fatalf("expected frozen retry to succeed, got err=%v", err)
	}
	if out != "ok" || calls != 2 {
		t.Fatalf("expected 2 eval calls and out=ok, got calls=%d out=%q", calls, out)
	}
	if len(be.launched) != 1 || be.launched[0] != EdgePkg {
		t.Fatalf("expected LaunchApp(%s) on frozen CDP error, got %v", EdgePkg, be.launched)
	}
}

func TestTapNodeByLabelsPrefersRewardsNode(t *testing.T) {
	be := &fakeBackend{
		uiTree: &backend.UITreeResult{
			Nodes: []backend.UINode{
				{ID: 1, Label: "Home"},
				{ID: 7, Label: "Rewards"},
			},
		},
	}
	r := NewRunner(be, nil, nil, 720, nil)
	tapped, err := r.tapNodeByLabels(context.Background(), "Rewards", "积分")
	if err != nil || !tapped {
		t.Fatalf("expected Rewards node tapped, tapped=%v err=%v", tapped, err)
	}
	if len(be.tappedNode) != 1 || be.tappedNode[0] != 7 {
		t.Fatalf("expected TapNode(7), got %v", be.tappedNode)
	}
}

func TestWorkflowStructuredStageLogs(t *testing.T) {
	be := &fakeBackend{launchErr: errors.New("stop early after stage marker")}
	var msLogs []string
	rMS := NewRunner(be, nil, nil, 720, func(line string) {
		msLogs = append(msLogs, line)
	})
	_, _ = rMS.RunMSRewards(context.Background(), MSRewardsOptions{})
	if len(msLogs) == 0 || !strings.Contains(msLogs[0], "[阶段 1/4] 检查 Edge Daily Set") {
		t.Fatalf("expected [阶段 1/4] marker in RunMSRewards, got %v", msLogs)
	}

	var jdLogs []string
	rJD := NewRunner(be, nil, nil, 720, func(line string) {
		jdLogs = append(jdLogs, line)
	})
	_, _ = rJD.RunJDDaily(context.Background(), JDDailyOptions{})
	if len(jdLogs) == 0 || !strings.Contains(jdLogs[0], "[JD 1/3] 领京豆签到") {
		t.Fatalf("expected [JD 1/3] marker in RunJDDaily, got %v", jdLogs)
	}

	var allLogs []string
	rAll := NewRunner(be, nil, nil, 720, func(line string) {
		allLogs = append(allLogs, line)
	})
	_, _ = rAll.RunDailyAll(context.Background())
	var sawStage1Of7, sawStage5Of7 bool
	for _, l := range allLogs {
		if strings.Contains(l, "[阶段 1/7] 检查 Edge Daily Set") {
			sawStage1Of7 = true
		}
		if strings.Contains(l, "[阶段 5/7] 领京豆签到") {
			sawStage5Of7 = true
		}
	}
	if !sawStage1Of7 || !sawStage5Of7 {
		t.Fatalf("expected monotonic [阶段 1/7] and [阶段 5/7] markers in RunDailyAll, got %v", allLogs)
	}
}
