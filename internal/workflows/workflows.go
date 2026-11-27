package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"time"

	"android-agent/internal/backend"
	"android-agent/internal/cdp"
	"android-agent/internal/guard"
)

const (
	EdgePkg = "com.microsoft.emmx"
	BingPkg = "com.microsoft.bing"
	JDPkg   = "com.jingdong.app.mall"

	farmURL = "https://pro.m.jd.com/mall/active/42HV4J3Q87B2xFQMJk81PCc1mEs3/index.html"
	beanURL = "https://pro.m.jd.com/mall/active/4E1fFrgX1eGtWTc5KBNFzXMrh6xn/index.html"
)

var (
	CaptchaRe = regexp.MustCompile(`(?i)安全验证|滑块验证|拖动滑块|人机验证|verify you are human|are you a robot|human verification|captcha|请完成安全验证|请完成验证`)

	ForbiddenTaskRe = regexp.MustCompile(`(?i)快手|百度|极速版|小组件|关注|邀请|好友|下单|购买|支付|开通PLUS|开通会员|金融|借款|白条|信用卡|助力|通过1关|小游戏|旅行一次`)
	SafeBrowseRe    = regexp.MustCompile(`(?i)浏览\d+秒|读\d+s|精选活动推荐|限时领|逛一逛|逛PLUS|逛家电|逛看病|抢爆品|京东直播`)

	headlineNoiseRe = regexp.MustCompile(`["'“”‘’()\[\]{}【】<>《》|｜—–·•:：;；!?！？,，.。]+`)
	multiSpaceRe    = regexp.MustCompile(`\s+`)
)

// SearchQueryPool holds 84+ diverse English & Chinese everyday queries used as supplement
// after live Bing News trending headlines.
var SearchQueryPool = []string{
	"xiaomi 13 camera sensor size",
	"kyoto autumn foliage peak dates",
	"best pour over coffee water ratio",
	"how solid state batteries work",
	"tokyo ramen styles explained",
	"mechanical keyboard tactile vs linear",
	"swiss alps scenic train routes",
	"solar eclipse visibility calendar",
	"espresso extraction time and temperature",
	"japanese matcha grades guide",
	"usb4 vs thunderbolt 4 bandwidth",
	"marathon pacing strategy calculator",
	"history of porcelain tea ware",
	"home studio acoustic treatment basics",
	"oled vs mini led contrast ratio",
	"best indoor low light houseplants",
	"mediterranean diet weekly meal ideas",
	"how noise cancelling headphones work",
	"national parks in utah road trip",
	"sourdough bread hydration percentage",
	"python asyncio gather vs taskgroup",
	"linux kernel scheduler overview",
	"electric vehicle regenerative braking",
	"classical guitar nylon string tension",
	"nordic minimalist desk setup tips",
	"best hiking boots waterproofing wax",
	"how e-ink displays save power",
	"green tea brewing temperature chart",
	"hdr10 plus vs dolby vision differences",
	"wifi 7 multi link operation explained",
	"mechanical watch automatic movement",
	"best cast iron skillet seasoning oil",
	"tokyo tsukiji outer market food guide",
	"how geothermal heat pumps work",
	"camera aperture depth of field chart",
	"portable ssd sequential read speeds",
	"how heat pump dryers save electricity",
	"difference between arabica and robusta beans",
	"best ergonomic office chair lumbar support",
	"titanium vs stainless steel cookware",
	"how polarized sunglasses block glare",
	"best air purifier hepa filter rating",
	"beginners guide to astrophotography settings",
	"why airplanes fly at 35000 feet",
	"how induction cooktops heat pans",
	"minimalist capsule wardrobe essentials",
	"best marathon recovery stretching routine",
	"difference between nvme gen4 and gen5",
	"how lithium iron phosphate batteries age",
	"classic neapolitan pizza dough fermentation",
	"best waterproof trail running shoes",
	"understanding photography histogram exposure",
	"how active noise cancellation filters bass",
	"top botanical gardens to visit in spring",
	"healthy high protein breakfast recipes",
	"how optical image stabilization works",
	"home espresso grinder burr types",
	"best travel backpack carry on dimensions",
	"benefits of sleeping in a cool room",
	"how tidal energy turbines generate power",
	"手冲咖啡水温与研磨度推荐",
	"京都红叶季自由行攻略路线",
	"316L不锈钢与304材质区别对比",
	"运动跑鞋缓震中底材料科普",
	"固态电池量产进展与优势分析",
	"洗碗机软水盐与亮碟剂作用",
	"机械键盘茶轴与红轴手感区别",
	"显示器护眼低蓝光认证标准",
	"婴儿高铁米粉冲调水温建议",
	"家用空气炸锅和烤箱哪个实用",
	"Switch健身环大冒险热量消耗",
	"公路车轮胎胎压与滚阻关系",
	"单反相机光圈快门ISO入门",
	"高铁选座靠窗还是过道视野好",
	"家用全屋WiFi7路由器组网方案",
	"无线蓝牙耳机LDAC与AAC音质差异",
	"地中海饮食一周健康食谱搭配",
	"空调一级能效和三级能效耗电对比",
	"冬季室内加湿器使用注意事项",
	"如何挑选耐用的双肩通勤背包",
	"家庭储能电源功率计算方法",
	"维生素AD与D3每日补充区别",
	"跑完半马后如何科学拉伸恢复",
	"电饭煲柴火饭和快煮模式区别",
}

type topicRule struct {
	re    *regexp.Regexp
	query string
}

var exploreTopicRules = []topicRule{
	{regexp.MustCompile(`(?i)video\s*games?|videogames?|gaming|电子游戏|主机游戏|电竞|游戏`), "Minecraft"},
	{regexp.MustCompile(`(?i)邮轮|启航|cruise`), "best caribbean cruise deals and destinations"},
	{regexp.MustCompile(`(?i)航班|机票|短途旅行|假期|flight|travel|trip|vacation`), "flights from Los Angeles to Tokyo"},
	{regexp.MustCompile(`(?i)保险|insurance`), "best auto and home insurance plans"},
	{regexp.MustCompile(`(?i)贷款|loan|mortgage`), "compare personal and student loan rates"},
	{regexp.MustCompile(`(?i)美容|护发|香氛|护肤|beauty|haircare|fragrance|skincare`), "best skincare and fragrance gift sets"},
	{regexp.MustCompile(`(?i)手机套餐|通话|短信|cell phone|phone plan|wireless plan`), "best unlimited cell phone plans comparison"},
	{regexp.MustCompile(`(?i)网络套餐|宽带|internet plan|broadband|wifi plan`), "compare high speed home internet plans"},
	{regexp.MustCompile(`(?i)餐厅|美食|做饭|晚饭|restaurant|dining|dinner|food`), "best local restaurants near me"},
	{regexp.MustCompile(`(?i)食谱|做菜|recipe|cook`), "easy healthy weeknight dinner recipes"},
	{regexp.MustCompile(`(?i)天气|weather|forecast`), "weather forecast this week"},
	{regexp.MustCompile(`(?i)酒店|住宿|hotel`), "best boutique hotels in kyoto"},
	{regexp.MustCompile(`(?i)汽车|新车|二手车|car|vehicle|suv`), "best hybrid suv fuel economy comparison"},
	{regexp.MustCompile(`(?i)电影|影院|movie|cinema|showtime`), "top rated movies playing in theaters"},
	{regexp.MustCompile(`(?i)体育|运动|足球|篮球|比赛|sports?|football|basketball|soccer|nba|nfl`), "NBA standings and schedule today"},
	{regexp.MustCompile(`(?i)书籍|阅读|小说|图书|books?|reading|novel|author`), "best selling fiction books this year"},
	{regexp.MustCompile(`(?i)宠物|猫|狗|pets?|dog|cat|puppy`), "best healthy dog food brands"},
	{regexp.MustCompile(`(?i)电脑|数码|电子产品|laptop|tablet|headphone|camera|electronics|tech`), "best noise cancelling headphones review"},
	{regexp.MustCompile(`(?i)家居|家具|园艺|装修|home|garden|furniture|kitchen`), "minimalist living room furniture ideas"},
	{regexp.MustCompile(`(?i)工作|求职|简历|job|career|resume`), "remote software engineer jobs"},
	{regexp.MustCompile(`(?i)健身|锻炼|workout|fitness|exercise`), "home strength training workout routine"},
	{regexp.MustCompile(`(?i)歌词|歌曲|音乐|lyrics|song|music`), "Shape of You Ed Sheeran lyrics"},
	{regexp.MustCompile(`(?i)翻译|translate`), "translate good morning to japanese"},
	{regexp.MustCompile(`(?i)股市|股票|finance|stock|market`), "sp 500 index performance today"},
	{regexp.MustCompile(`(?i)健康|症状|health|wellness|vitamin`), "daily stretching routine benefits"},
	{regexp.MustCompile(`(?i)鞋|服装|fashion|shoes|sneakers`), "best running shoes for daily training"},
	{regexp.MustCompile(`(?i)购物|清单|优惠|shopping|deals|discount`), "best shopping deals on wireless headphones"},
}

var (
	exploreBoilerplateRe = regexp.MustCompile(`(?i)在必应上?搜索|在必应上浏览|已激活|\+\d+|以找到|最适合您需求的|explore\s+on\s+bing|search\s+on\s+bing|find\s+out\s+about|learn\s+about|\bexplore\b|\bdiscover\b`)
	readToEarnFullRe     = regexp.MustCompile(`^30(?:/30)?$`)
)

type LogFunc func(line string)

type Runner struct {
	backend       backend.DeviceBackend
	cdp           *cdp.Client
	guard         *guard.Guard
	imageWidth    int
	log           LogFunc
	unfreezeDelay time.Duration
	cdpSendFn     func(ctx context.Context, socketSel, targetID, method string, params map[string]any) (json.RawMessage, error)
}

func NewRunner(be backend.DeviceBackend, cdpClient *cdp.Client, g *guard.Guard, imageWidth int, logFn LogFunc) *Runner {
	if imageWidth <= 0 {
		imageWidth = 720
	}
	if logFn == nil {
		logFn = func(string) {}
	}
	return &Runner{
		backend:       be,
		cdp:           cdpClient,
		guard:         g,
		imageWidth:    imageWidth,
		log:           logFn,
		unfreezeDelay: 1500 * time.Millisecond,
	}
}

func (r *Runner) logf(format string, args ...any) {
	r.log(fmt.Sprintf(format, args...))
}

func (r *Runner) checkGuard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.guard != nil {
		if blocked, reason := r.guard.IsActionBlocked(); blocked {
			return fmt.Errorf("guard blocked: %s", reason)
		}
	}
	return nil
}

func CheckCaptcha(text string) error {
	if CaptchaRe.MatchString(text) {
		return errors.New("CAPTCHA / human verification detected; stopping immediately per safety policy")
	}
	return nil
}

func CleanHeadlineToQuery(text string) string {
	s := headlineNoiseRe.ReplaceAllString(text, " ")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, " ") {
		words := strings.Split(s, " ")
		if len(words) > 7 {
			words = words[:7]
		}
		s = strings.Join(words, " ")
	}
	runes := []rune(s)
	if len(runes) > 42 {
		cut := string(runes[:42])
		if idx := strings.LastIndex(cut, " "); idx > 8 {
			s = cut[:idx]
		} else {
			s = cut
		}
	}
	s = strings.TrimSpace(s)
	n := len([]rune(s))
	if n < 6 || n > 42 {
		return ""
	}
	return s
}

func IsReadToEarnFull(val string) bool {
	return readToEarnFullRe.MatchString(strings.TrimSpace(val))
}

func MapExploreQuery(cardText string) string {
	for _, rule := range exploreTopicRules {
		if rule.re.MatchString(cardText) {
			return rule.query
		}
	}
	cleaned := exploreBoilerplateRe.ReplaceAllString(cardText, " ")
	cleaned = headlineNoiseRe.ReplaceAllString(cleaned, " ")
	cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)
	runes := []rune(cleaned)
	if len(runes) > 48 {
		cleaned = strings.TrimSpace(string(runes[:48]))
	}
	if len([]rune(cleaned)) >= 3 {
		return cleaned
	}
	return "popular travel destinations"
}

func altExploreQuery(cardText string) string {
	if regexp.MustCompile(`(?i)歌词|lyrics|song`).MatchString(cardText) {
		return "周杰伦 稻香 歌词"
	}
	if regexp.MustCompile(`(?i)video\s*games?|videogames?|gaming|电子游戏|主机游戏|电竞|游戏`).MatchString(cardText) {
		return "Elden Ring gameplay and reviews"
	}
	cleaned := exploreBoilerplateRe.ReplaceAllString(cardText, " ")
	cleaned = headlineNoiseRe.ReplaceAllString(cleaned, " ")
	cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)
	if len([]rune(cleaned)) >= 3 {
		r := []rune(cleaned)
		if len(r) > 36 {
			cleaned = strings.TrimSpace(string(r[:36]))
		}
		return cleaned + " guide"
	}
	return MapExploreQuery(cardText)
}

func resolveExploreURL(href string) string {
	h := strings.TrimSpace(href)
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		return h
	}
	if strings.HasPrefix(h, "/") {
		return "https://rewards.bing.com" + h
	}
	return "https://www.bing.com/?form=ML2PCR&OCID=ML2PCR&PUBL=RewardsDO&CREA=ML2PCR&PC=ML2PCR"
}

func (r *Runner) sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return r.checkGuard(ctx)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return r.checkGuard(ctx)
	}
}

func (r *Runner) jitterSleep(ctx context.Context, minSec, maxSec float64) error {
	sec := minSec
	if maxSec > minSec {
		sec = minSec + rand.Float64()*(maxSec-minSec)
	}
	return r.sleepCtx(ctx, time.Duration(sec*float64(time.Second)))
}

func (r *Runner) scaleXY(ctx context.Context, x, y int) (int, int) {
	scr, err := r.backend.ScreenInfo(ctx)
	if err != nil || scr.Width <= 0 || scr.Width <= r.imageWidth {
		return x, y
	}
	scale := float64(scr.Width) / float64(r.imageWidth)
	return int(math.Round(float64(x) * scale)), int(math.Round(float64(y) * scale))
}

func (r *Runner) tapImage(ctx context.Context, x, y int) error {
	if err := r.checkGuard(ctx); err != nil {
		return err
	}
	px, py := r.scaleXY(ctx, x, y)
	return r.backend.Tap(ctx, px, py)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (r *Runner) jitterSwipe(ctx context.Context, x1, y1, x2, y2, durMs, minX, maxX int) error {
	if err := r.checkGuard(ctx); err != nil {
		return err
	}
	jx1 := clampInt(x1+rand.Intn(37)-18, minX, maxX)
	jx2 := clampInt(x2+rand.Intn(37)-18, minX, maxX)
	jy1 := clampInt(y1+rand.Intn(37)-18, 180, 1420)
	jy2 := clampInt(y2+rand.Intn(37)-18, 150, 1380)
	dur := clampInt(durMs+rand.Intn(71)-35, 90, 600)
	px1, py1 := r.scaleXY(ctx, jx1, jy1)
	px2, py2 := r.scaleXY(ctx, jx2, jy2)
	return r.backend.Swipe(ctx, px1, py1, px2, py2, dur)
}

func (r *Runner) cdpClientFor(socketSel string) (*cdp.Client, error) {
	if r.cdp == nil {
		return nil, errors.New("cdp client is not configured")
	}
	return r.cdp.ForSocket(socketSel)
}

func isFrozenCDPError(err error) bool {
	if err == nil {
		return false
	}
	type frozenErr interface {
		IsTargetFrozen() bool
	}
	var fe frozenErr
	if errors.As(err, &fe) && fe.IsTargetFrozen() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "frozen")
}

func packageForSocket(socketSel string) string {
	switch strings.TrimSpace(socketSel) {
	case "", EdgePkg, "edge", "chrome":
		return EdgePkg
	case BingPkg, "bing":
		return BingPkg
	case JDPkg, "jd", "jingdong":
		return JDPkg
	default:
		return socketSel
	}
}

func (r *Runner) rawCDPSend(ctx context.Context, socketSel, targetID, method string, params map[string]any) (json.RawMessage, error) {
	if r.cdpSendFn != nil {
		return r.cdpSendFn(ctx, socketSel, targetID, method, params)
	}
	client, err := r.cdpClientFor(socketSel)
	if err != nil {
		return nil, err
	}
	return client.SendCommand(ctx, targetID, method, params)
}

func (r *Runner) unfreezeTarget(ctx context.Context, socketSel, targetID string) error {
	pkg := packageForSocket(socketSel)
	r.logf("[CDP] Frozen target detected on %q; foregrounding %s and retrying once...", socketSel, pkg)
	if r.backend != nil && pkg != "" {
		if err := r.backend.LaunchApp(ctx, pkg); err != nil {
			return err
		}
	}
	delay := r.unfreezeDelay
	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}
	if err := r.sleepCtx(ctx, delay); err != nil {
		return err
	}
	if targetID != "" {
		bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _ = r.rawCDPSend(bctx, socketSel, targetID, "Page.bringToFront", map[string]any{})
		cancel()
	}
	return nil
}

func (r *Runner) sendCDPCommand(ctx context.Context, socketSel, targetID, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	if err := r.checkGuard(ctx); err != nil {
		return nil, err
	}
	runOnce := func() (json.RawMessage, error) {
		cctx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return r.rawCDPSend(cctx, socketSel, targetID, method, params)
	}
	raw, err := runOnce()
	if err != nil && isFrozenCDPError(err) && ctx.Err() == nil {
		if uerr := r.unfreezeTarget(ctx, socketSel, targetID); uerr != nil {
			return nil, uerr
		}
		return runOnce()
	}
	return raw, err
}

func (r *Runner) evalJS(ctx context.Context, socketSel, targetID, expr string, awaitPromise bool, out any) error {
	params := map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}
	if awaitPromise {
		params["awaitPromise"] = true
	}
	raw, err := r.sendCDPCommand(ctx, socketSel, targetID, "Runtime.evaluate", params, 18*time.Second)
	if err != nil {
		return err
	}
	var env struct {
		Result struct {
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse Runtime.evaluate: %w", err)
	}
	if len(env.ExceptionDetails) > 0 && string(env.ExceptionDetails) != "null" {
		return fmt.Errorf("JS exception: %s", string(env.ExceptionDetails))
	}
	if out != nil && len(env.Result.Value) > 0 && string(env.Result.Value) != "null" {
		if err := json.Unmarshal(env.Result.Value, out); err != nil {
			return fmt.Errorf("decode JS value (%s): %w", string(env.Result.Value), err)
		}
	}
	return nil
}

func (r *Runner) cdpNavigate(ctx context.Context, socketSel, targetID, targetURL string) error {
	_, err := r.sendCDPCommand(ctx, socketSel, targetID, "Page.navigate", map[string]any{"url": targetURL}, 15*time.Second)
	return err
}

func (r *Runner) cdpClickRect(ctx context.Context, socketSel, targetID string, x, y, w, h float64) error {
	cx := x + w/2.0
	cy := y + h/2.0
	for _, tp := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		params := map[string]any{"type": tp, "x": cx, "y": cy}
		if tp != "mouseMoved" {
			params["button"] = "left"
			params["clickCount"] = 1
		}
		if _, err := r.sendCDPCommand(ctx, socketSel, targetID, "Input.dispatchMouseEvent", params, 10*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) tapNodeByLabels(ctx context.Context, labels ...string) (bool, error) {
	if err := r.checkGuard(ctx); err != nil {
		return false, err
	}
	if r.backend == nil {
		return false, nil
	}
	tree, err := r.backend.UITree(ctx, backend.UITreeOptions{})
	if err != nil || tree == nil || len(tree.Nodes) == 0 {
		if err != nil && (ctx.Err() != nil || isGuardError(err)) {
			return false, err
		}
		return false, nil
	}
	// Exact case-insensitive match first, then substring match.
	pickID := -1
	for _, want := range labels {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		for _, n := range tree.Nodes {
			if strings.EqualFold(strings.TrimSpace(n.Label), want) {
				pickID = n.ID
				break
			}
		}
		if pickID >= 0 {
			break
		}
	}
	if pickID < 0 {
		for _, want := range labels {
			wantLow := strings.ToLower(strings.TrimSpace(want))
			if wantLow == "" {
				continue
			}
			for _, n := range tree.Nodes {
				if strings.Contains(strings.ToLower(n.Label), wantLow) {
					pickID = n.ID
					break
				}
			}
			if pickID >= 0 {
				break
			}
		}
	}
	if pickID < 0 {
		return false, nil
	}
	if _, _, _, err := r.backend.TapNode(ctx, pickID); err != nil {
		return false, err
	}
	return true, nil
}

func isGuardError(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "guard") || strings.Contains(low, "locked") || strings.Contains(low, "screen")
}

// ---------- Microsoft Rewards (Edge CDP + Bing WebView CDP, 0 screenshots) ----------

type MSRewardsOptions struct {
	SearchIntervalSec float64
	ReadCount         int
	SkipEdge          bool
	SkipBing          bool
}

type MSRewardsReport struct {
	BeforePoints     string   `json:"before_points,omitempty"`
	AfterPoints      string   `json:"after_points,omitempty"`
	TodayPoints      string   `json:"today_points,omitempty"`
	DailyProgress    string   `json:"daily_progress,omitempty"`
	ExploreProgress  string   `json:"explore_progress,omitempty"`
	BingSearch       string   `json:"bing_search,omitempty"`
	SearchStatus     string   `json:"search_status,omitempty"`
	SearchesCount    int      `json:"searches_executed,omitempty"`
	InteractiveTasks []string `json:"interactive_tasks,omitempty"`
	BingCheckin      string   `json:"bing_checkin,omitempty"`
	BingReadOK       int      `json:"bing_read_ok"`
	BingReadAfter    string   `json:"bing_read_after,omitempty"`
	StoppedReason    string   `json:"stopped_reason,omitempty"`
}

type earnCard struct {
	Text string `json:"text"`
	Href string `json:"href"`
}

type earnSnapshot struct {
	HasCaptcha        bool       `json:"hasCaptcha"`
	NeedLogin         bool       `json:"needLogin"`
	AvailablePoints   string     `json:"availablePoints"`
	TodayPoints       string     `json:"todayPoints"`
	ExploreProgress   string     `json:"exploreProgress"`
	DailyProgress     string     `json:"dailyProgress"`
	ExploreCards      []earnCard `json:"exploreCards"`
	DailyCards        []earnCard `json:"dailyCards"`
	BingSearchCurrent *int       `json:"bingSearchCurrent"`
	BingSearchCap     *int       `json:"bingSearchCap"`
	BreakdownText     string     `json:"breakdownText"`
}

var bingSearchCounterRe = regexp.MustCompile(`必应搜索\s+(\d+)/(\d+)`)

func (r *Runner) ensureEdgeTarget(ctx context.Context) (string, error) {
	if err := r.checkGuard(ctx); err != nil {
		return "", err
	}
	if err := r.backend.LaunchApp(ctx, EdgePkg); err != nil {
		return "", err
	}
	var targets []cdp.Target
	for i := 0; i < 8; i++ {
		if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
			return "", err
		}
		client, err := r.cdpClientFor("")
		if err != nil {
			continue
		}
		list, err := client.ListTargets(ctx)
		if err != nil && isFrozenCDPError(err) {
			if uerr := r.unfreezeTarget(ctx, "", ""); uerr != nil {
				return "", uerr
			}
			continue
		}
		if err == nil && len(list) > 0 {
			targets = list
			break
		}
	}
	var pages []cdp.Target
	for _, t := range targets {
		if t.Type == "page" && strings.HasPrefix(t.URL, "http") {
			pages = append(pages, t)
		}
	}
	if len(pages) == 0 {
		for _, t := range targets {
			if t.Type == "page" {
				pages = append(pages, t)
			}
		}
	}
	if len(pages) == 0 {
		return "", errors.New("Edge CDP page target not available")
	}
	for i := 0; i < len(pages) && i < 3; i++ {
		if strings.Contains(pages[i].URL, "rewards.bing.com/earn") {
			var href string
			if err := r.evalJS(ctx, "", pages[i].ID, "location.href", false, &href); err == nil {
				return pages[i].ID, nil
			}
		}
	}
	if _, err := r.sendCDPCommand(ctx, "", pages[0].ID, "Page.bringToFront", map[string]any{}, 10*time.Second); err != nil {
		return "", err
	}
	return pages[0].ID, nil
}

func (r *Runner) parseEarnSnapshot(ctx context.Context, tid string, openModal bool) (*earnSnapshot, error) {
	if err := r.cdpNavigate(ctx, "", tid, "https://rewards.bing.com/earn"); err != nil {
		return nil, err
	}
	if err := r.sleepCtx(ctx, 3600*time.Millisecond); err != nil {
		return nil, err
	}
	expr := fmt.Sprintf(`
	(async () => {
	  const rawText = document.body ? document.body.innerText : "";
	  const hasCaptcha = /安全验证|滑块验证|拖动滑块|人机验证|verify you are human|are you a robot|human verification|captcha/i.test(rawText.slice(0, 1200));
	  const needLogin = /登录|Sign in/i.test(document.title) || location.hostname.includes("login.live.com");
	  const lines = rawText.split("\n").map(s => s.trim()).filter(Boolean);
	  const availIdx = lines.findIndex(l => /^\d{1,3}(,\d{3})*$/.test(l));
	  const availPts = availIdx >= 0 ? lines[availIdx] : "";
	  const todayIdx = lines.findIndex(l => l === "今日积分");
	  const todayPts = (todayIdx >= 0 && todayIdx + 1 < lines.length) ? lines[todayIdx + 1] : "";
	  const exploreCards = [];
	  const dailyCards = [];
	  for (const a of Array.from(document.querySelectorAll("a"))) {
	    const t = (a.innerText || "").replace(/\s+/g, " ").trim();
	    if (!t || t.length > 220) continue;
	    const href = a.getAttribute("href") || "";
	    if (t.includes("已激活") && !t.includes("已完成") && !t.includes("明天解锁")) {
	      exploreCards.push({text: t, href});
	    } else if (/\+\d+/.test(t) && !t.includes("已完成") && !t.includes("明天解锁") && !t.includes("已激活")) {
	      if (href.includes("bing.com") || href.includes("rewards.bing.com") || href.startsWith("/")) {
	        dailyCards.push({text: t, href});
	      }
	    }
	  }
	  let breakdownText = "";
	  if (%t) {
	    const btns = Array.from(document.querySelectorAll("button, a, [role=button]"));
	    const detailBtn = btns.find(b => b.innerText && b.innerText.includes("积分明细"));
	    if (detailBtn) {
	      detailBtn.click();
	      await new Promise(r => setTimeout(r, 1200));
	      const dialogs = Array.from(document.querySelectorAll("[role=dialog], [aria-modal=true]"));
	      breakdownText = dialogs.map(d => d.innerText).join("\n");
	      const closeBtn = document.querySelector('[role=dialog] button[aria-label*="关闭"], [role=dialog] button[aria-label*="Close"], [role=dialog] button');
	      if (closeBtn) closeBtn.click();
	    }
	  }
	  const idxExplore = lines.findIndex(l => l === "在必应上浏览");
	  const idxDaily = lines.findIndex(l => l === "日常任务");
	  return {
	    hasCaptcha, needLogin, availablePoints: availPts, todayPoints: todayPts,
	    exploreProgress: (idxExplore >= 0 && idxExplore + 1 < lines.length) ? lines[idxExplore + 1] : "",
	    dailyProgress: (idxDaily >= 0 && idxDaily + 1 < lines.length) ? lines[idxDaily + 1] : "",
	    exploreCards, dailyCards, breakdownText
	  };
	})()`, openModal)

	var snap earnSnapshot
	if err := r.evalJS(ctx, "", tid, expr, true, &snap); err != nil {
		return nil, err
	}
	if m := bingSearchCounterRe.FindStringSubmatch(snap.BreakdownText); len(m) == 3 {
		var cur, capV int
		fmt.Sscanf(m[1], "%d", &cur)
		fmt.Sscanf(m[2], "%d", &capV)
		snap.BingSearchCurrent = &cur
		snap.BingSearchCap = &capV
	}
	return &snap, nil
}

func (r *Runner) doBingSearch(ctx context.Context, tid, query string, baseWaitSec float64) error {
	qJSON, _ := json.Marshal(query)
	expr := fmt.Sprintf(`(() => {
	  const q = %s;
	  const input = document.querySelector("#sb_form_q, input[name=q]");
	  const form = document.querySelector("#sb_form, form[action*='/search']");
	  if (input && form) {
	    input.focus(); input.value = q;
	    input.dispatchEvent(new Event("input", {bubbles: true}));
	    form.submit();
	    return "form";
	  }
	  location.href = "https://www.bing.com/search?q=" + encodeURIComponent(q) + "&form=QBRE&pc=EMMX01";
	  return "nav";
	})()`, string(qJSON))
	var mode string
	if err := r.evalJS(ctx, "", tid, expr, false, &mode); err != nil {
		return err
	}
	lo := math.Max(5.5, baseWaitSec-0.3)
	hi := baseWaitSec + 2.6
	if err := r.jitterSleep(ctx, lo, hi); err != nil {
		return err
	}
	var snippet string
	if err := r.evalJS(ctx, "", tid, `(document.body ? document.body.innerText : "").slice(0, 800)`, false, &snippet); err != nil {
		return err
	}
	return CheckCaptcha(snippet)
}

func (r *Runner) buildFreshSearchQueries(ctx context.Context, tid string, count int) ([]string, error) {
	var live []string
	seen := make(map[string]bool)
	if err := r.cdpNavigate(ctx, "", tid, "https://www.bing.com/news"); err == nil {
		if err := r.jitterSleep(ctx, 2.2, 3.0); err != nil {
			return nil, err
		}
		var raw []string
		if err := r.evalJS(ctx, "", tid, `(() => {
		  const out = [];
		  for (const a of Array.from(document.querySelectorAll("a.title, .news-card a, [class*=trending] a, h2 a, h3 a"))) {
		    const t = (a.innerText || "").replace(/\s+/g, " ").trim();
		    if (t.length >= 12 && t.length <= 120) out.push(t);
		    if (out.length >= 40) break;
		  }
		  return out;
		})()`, false, &raw); err != nil && (ctx.Err() != nil || isGuardError(err)) {
			return nil, err
		}
		for _, h := range raw {
			q := CleanHeadlineToQuery(h)
			if q != "" && !seen[strings.ToLower(q)] {
				seen[strings.ToLower(q)] = true
				live = append(live, q)
			}
		}
	} else if ctx.Err() != nil || isGuardError(err) {
		return nil, err
	}
	rand.Shuffle(len(live), func(i, j int) { live[i], live[j] = live[j], live[i] })
	var rest []string
	for _, q := range SearchQueryPool {
		if !seen[strings.ToLower(q)] {
			rest = append(rest, q)
		}
	}
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	combined := append(live, rest...)
	if len(combined) > count {
		combined = combined[:count]
	}
	return combined, nil
}

func (r *Runner) completeDashboardDailySet(ctx context.Context, tid string) ([]string, error) {
	if err := r.cdpNavigate(ctx, "", tid, "https://rewards.bing.com/dashboard"); err != nil {
		return nil, err
	}
	if err := r.sleepCtx(ctx, 3200*time.Millisecond); err != nil {
		return nil, err
	}
	var cards []earnCard
	if err := r.evalJS(ctx, "", tid, `(() => {
	  const res = [];
	  for (const a of Array.from(document.querySelectorAll("a"))) {
	    const href = a.getAttribute("href") || "";
	    const txt = (a.innerText || "").replace(/\s+/g, " ").trim();
	    if (!txt || txt.includes("已完成") || !/\+\d+/.test(txt)) continue;
	    if (href.includes("DailySet") || href.includes("RewardsDailyPoll") || href.includes("REWARDSQUIZ")) {
	      res.push({text: txt.slice(0, 120), href});
	    }
	  }
	  return res;
	})()`, false, &cards); err != nil {
		return nil, err
	}

	var manual []string
	thisOrThatRe := regexp.MustCompile(`(?i)this\s*or\s*that|二选一|\+50`)
	for _, card := range cards {
		if thisOrThatRe.MatchString(card.Text) || strings.Contains(card.Href, "ThisOrThat") {
			manual = append(manual, card.Text)
			continue
		}
		if err := r.cdpNavigate(ctx, "", tid, "https://rewards.bing.com/dashboard"); err != nil {
			return manual, err
		}
		if err := r.sleepCtx(ctx, 2*time.Second); err != nil {
			return manual, err
		}
		hint := strings.Split(card.Text, " ")[0]
		if len([]rune(hint)) > 12 {
			hint = string([]rune(hint)[:12])
		}
		hJSON, _ := json.Marshal(hint)
		var okClick bool
		if err := r.evalJS(ctx, "", tid, fmt.Sprintf(`(() => {
		  const el = Array.from(document.querySelectorAll("a")).find(a => a.innerText && a.innerText.includes(%s));
		  if (el) { el.scrollIntoView({block: "center"}); el.click(); return true; }
		  return false;
		})()`, string(hJSON)), false, &okClick); err != nil {
			return manual, err
		}
		if err := r.sleepCtx(ctx, 3200*time.Millisecond); err != nil {
			return manual, err
		}

		isPoll := strings.Contains(card.Href, "Poll") || strings.Contains(card.Text, "投票") || strings.Contains(strings.ToLower(card.Text), "poll")
		isQuiz := strings.Contains(strings.ToUpper(card.Href), "QUIZ") || strings.Contains(card.Text, "问答") || strings.Contains(card.Text, "测验")

		if isPoll {
			var rect struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
				W float64 `json:"w"`
				H float64 `json:"h"`
			}
			if err := r.evalJS(ctx, "", tid, `(() => {
			  const el = document.querySelector("acf-button-standard.btom_opt, .btom_opts > *, #btoption0");
			  if (!el) return null;
			  el.scrollIntoView({block: "center"});
			  const r = el.getBoundingClientRect();
			  return {x: r.left, y: r.top, w: r.width, h: r.height};
			})()`, false, &rect); err != nil {
				return manual, err
			}
			if rect.W > 0 {
				if err := r.cdpClickRect(ctx, "", tid, rect.X, rect.Y, rect.W, rect.H); err != nil {
					return manual, err
				}
				if err := r.sleepCtx(ctx, 1800*time.Millisecond); err != nil {
					return manual, err
				}
			}
		} else if isQuiz {
			tried := make(map[string]map[string]bool)
			for step := 0; step < 15; step++ {
				var info struct {
					Done    bool   `json:"done"`
					Empty   bool   `json:"empty"`
					QKey    string `json:"qKey"`
					NextBtn *struct {
						X float64 `json:"x"`
						Y float64 `json:"y"`
						W float64 `json:"w"`
						H float64 `json:"h"`
					} `json:"nextBtn"`
					Items []struct {
						Text    string  `json:"text"`
						Correct string  `json:"correctAttr"`
						X       float64 `json:"x"`
						Y       float64 `json:"y"`
						W       float64 `json:"w"`
						H       float64 `json:"h"`
					} `json:"items"`
				}
				if err := r.evalJS(ctx, "", tid, `(() => {
				  const box = document.querySelector(".btom_opts")?.closest("li, div.b_ans") || document.body;
				  const t = (box.innerText || "").slice(0, 600);
				  if (/30\/30|干得好|Great job|You've earned|你已获得|获得 30/i.test(t)) return {done: true};
				  const nextEl = Array.from(document.querySelectorAll("acf-button-standard, button, a")).find(e => {
				    const s = (e.innerText || "").trim();
				    return /^(下一题|下一步|下一个|Next|Next question)$/i.test(s) && e.getBoundingClientRect().width > 0;
				  });
				  if (nextEl) {
				    const r = nextEl.getBoundingClientRect();
				    return {done: false, nextBtn: {x: r.left, y: r.top, w: r.width, h: r.height}};
				  }
				  const opts = Array.from(document.querySelectorAll("acf-button-standard.btom_opt, .btom_opts > *"));
				  if (!opts.length) return {done: false, empty: true};
				  opts[0].scrollIntoView({block: "center"});
				  return {
				    done: false,
				    qKey: t.split("\n").slice(0, 3).join("|"),
				    items: opts.map(e => {
				      const r = e.getBoundingClientRect();
				      return {text: (e.innerText || "").trim(), correctAttr: e.getAttribute("iscorrectoption") || "", x: r.left, y: r.top, w: r.width, h: r.height};
				    })
				  };
				})()`, false, &info); err != nil {
					return manual, err
				}
				if info.Done || info.Empty {
					break
				}
				if info.NextBtn != nil && info.NextBtn.W > 0 {
					if err := r.cdpClickRect(ctx, "", tid, info.NextBtn.X, info.NextBtn.Y, info.NextBtn.W, info.NextBtn.H); err != nil {
						return manual, err
					}
					if err := r.sleepCtx(ctx, 1200*time.Millisecond); err != nil {
						return manual, err
					}
					continue
				}
				if tried[info.QKey] == nil {
					tried[info.QKey] = make(map[string]bool)
				}
				pickIdx := -1
				for i, it := range info.Items {
					if strings.EqualFold(it.Correct, "true") && it.W > 0 {
						pickIdx = i
						break
					}
				}
				if pickIdx < 0 {
					for i, it := range info.Items {
						if !tried[info.QKey][it.Text] && it.W > 0 {
							pickIdx = i
							break
						}
					}
				}
				if pickIdx < 0 && len(info.Items) > 0 {
					pickIdx = 0
				}
				if pickIdx < 0 {
					break
				}
				chosen := info.Items[pickIdx]
				tried[info.QKey][chosen.Text] = true
				if err := r.cdpClickRect(ctx, "", tid, chosen.X, chosen.Y, chosen.W, chosen.H); err != nil {
					return manual, err
				}
				if err := r.sleepCtx(ctx, 1400*time.Millisecond); err != nil {
					return manual, err
				}
			}
		}
	}
	return manual, nil
}

func (r *Runner) findBingTargets(ctx context.Context) (feedTID, rewardsTID string) {
	client, err := r.cdpClientFor(BingPkg)
	if err != nil {
		return "", ""
	}
	targets, err := client.ListTargets(ctx)
	if err != nil {
		return "", ""
	}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if strings.Contains(t.URL, "superapp.msn.com/homepagefeed") {
			feedTID = t.ID
		} else if t.Title == "Rewards" || strings.Contains(t.URL, "miniapps") {
			rewardsTID = t.ID
		}
	}
	return feedTID, rewardsTID
}

func (r *Runner) ensureBingRewardsWebView(ctx context.Context) (string, error) {
	if err := r.checkGuard(ctx); err != nil {
		return "", err
	}
	if err := r.backend.LaunchApp(ctx, BingPkg); err != nil {
		return "", err
	}
	if err := r.sleepCtx(ctx, 1800*time.Millisecond); err != nil {
		return "", err
	}
	_, rTID := r.findBingTargets(ctx)
	if rTID != "" {
		return rTID, nil
	}
	// Prefer UI tree node matching on Bing Home before falling back to hardcoded coords.
	if tapped, err := r.tapNodeByLabels(ctx, "Rewards", "Microsoft Rewards", "积分"); err != nil {
		return "", err
	} else if !tapped {
		if homeTapped, err := r.tapNodeByLabels(ctx, "首页", "Home"); err != nil {
			return "", err
		} else if !homeTapped {
			if err := r.tapImage(ctx, 86, 1515); err != nil {
				return "", err
			}
			if err := r.sleepCtx(ctx, 600*time.Millisecond); err != nil {
				return "", err
			}
			if err := r.tapImage(ctx, 86, 1515); err != nil {
				return "", err
			}
		}
		if err := r.sleepCtx(ctx, 800*time.Millisecond); err != nil {
			return "", err
		}
		if tapped2, err := r.tapNodeByLabels(ctx, "Rewards", "Microsoft Rewards", "积分"); err != nil {
			return "", err
		} else if !tapped2 {
			if err := r.tapImage(ctx, 150, 1270); err != nil {
				return "", err
			}
		}
	}
	for i := 0; i < 8; i++ {
		if err := r.sleepCtx(ctx, 600*time.Millisecond); err != nil {
			return "", err
		}
		_, rTID = r.findBingTargets(ctx)
		if rTID != "" {
			return rTID, nil
		}
	}
	return "", nil
}

type bingRewardsDOM struct {
	Points     string `json:"points"`
	Today      string `json:"todayProgress"`
	ReadToEarn string `json:"readToEarn"`
	Checkin    string `json:"checkinReward"`
}

func (r *Runner) readBingRewardsDOM(ctx context.Context, rTID string, refresh bool) (*bingRewardsDOM, error) {
	if refresh {
		if err := r.jitterSwipe(ctx, 260, 460, 260, 1150, 220, 140, 440); err != nil {
			return nil, err
		}
		if err := r.sleepCtx(ctx, 2200*time.Millisecond); err != nil {
			return nil, err
		}
	}
	var out bingRewardsDOM
	err := r.evalJS(ctx, BingPkg, rTID, `(() => {
	  const txt = (document.body?.innerText || "");
	  const ptsM = txt.match(/([\d,]+)\s*\n\s*积分/);
	  const todayM = txt.match(/今日积分\s*\n\s*(\d+\/\d+)/);
	  const readM = txt.match(/阅读以赚取[\s\S]{0,60}?(\d+(?:\s*\/\s*30)?)/);
	  const checkinM = txt.match(/签入\s*\n\s*(\d+)/);
	  return {
	    points: ptsM ? ptsM[1] : "",
	    todayProgress: todayM ? todayM[1] : "",
	    readToEarn: readM ? readM[1].replace(/\s+/g, "") : "",
	    checkinReward: checkinM ? checkinM[1] : ""
	  };
	})()`, false, &out)
	return &out, err
}

func (r *Runner) runBingCheckinAndRead(ctx context.Context, count int, report *MSRewardsReport) error {
	rTID, err := r.ensureBingRewardsWebView(ctx)
	if err != nil {
		return err
	}
	if rTID == "" {
		return errors.New("Bing WebView CDP unavailable; ensure Agent WebView Debug is enabled for com.microsoft.bing")
	}
	if _, err := r.readBingRewardsDOM(ctx, rTID, true); err != nil {
		return err
	}
	var clicked string
	if err := r.evalJS(ctx, BingPkg, rTID, `(() => {
	  const title = Array.from(document.querySelectorAll(".title, div, span")).find(e => (e.innerText || "").trim() === "签入");
	  let cards = Array.from(document.querySelectorAll(".check-in-card")).filter(c => c.getBoundingClientRect().width > 0);
	  if (!cards.length && title) { title.scrollIntoView({block: "center"}); title.click(); }
	  cards = Array.from(document.querySelectorAll(".check-in-card"));
	  const active = cards.filter(c => !c.querySelector(".inActive"));
	  if (!active.length) return "";
	  const target = active[active.length - 1];
	  target.scrollIntoView({block: "center"});
	  target.click();
	  return (target.innerText || "").replace(/\s+/g, " ").trim();
	})()`, false, &clicked); err != nil {
		return err
	}
	report.BingCheckin = clicked
	if err := r.sleepCtx(ctx, 1800*time.Millisecond); err != nil {
		return err
	}

	domMid, err := r.readBingRewardsDOM(ctx, rTID, false)
	if err != nil {
		return err
	}
	if domMid != nil && IsReadToEarnFull(domMid.ReadToEarn) {
		report.BingReadAfter = domMid.ReadToEarn
		report.AfterPoints = domMid.Points
		r.logf("[Bing] Read-to-Earn already %s, skipping read loop", domMid.ReadToEarn)
		return nil
	}

	// Switch back to Bing Home news feed and read pure-text MSN /ar- articles via Shadow DOM
	if err := r.tapImage(ctx, 86, 1515); err != nil {
		return err
	}
	if err := r.sleepCtx(ctx, 700*time.Millisecond); err != nil {
		return err
	}
	if err := r.tapImage(ctx, 86, 1515); err != nil {
		return err
	}
	if err := r.sleepCtx(ctx, 900*time.Millisecond); err != nil {
		return err
	}
	if err := r.jitterSwipe(ctx, 260, 1350, 260, 320, 220, 140, 440); err != nil {
		return err
	}
	if err := r.sleepCtx(ctx, 800*time.Millisecond); err != nil {
		return err
	}

	fTID, _ := r.findBingTargets(ctx)
	if fTID == "" {
		return errors.New("Bing MSN feed WebView target not found")
	}

	seen := make(map[string]bool)
	maxScrolls := count * 2
	if maxScrolls < 12 {
		maxScrolls = 12
	}
	for s := 0; s < maxScrolls && report.BingReadOK < count; s++ {
		if err := r.checkGuard(ctx); err != nil {
			return err
		}
		var items []struct {
			Key   string `json:"key"`
			Title string `json:"title"`
		}
		if err := r.evalJS(ctx, BingPkg, fTID, `(() => {
		  const out = [];
		  function walk(root, depth) {
		    if (!root || depth > 8) return;
		    for (const el of (root.querySelectorAll ? root.querySelectorAll("*") : [])) {
		      if (el.tagName === "A") {
		        const href = el.getAttribute("href") || "";
		        const cls = (el.className || "");
		        const txt = (el.innerText || "").replace(/\s+/g, " ").trim();
		        if (cls.includes("heading") && href.includes("msn.com/") && href.includes("/ar-") && !href.includes("/vi-") && txt.length > 8) {
		          const key = href.split("?")[0];
		          if (!out.some(x => x.key === key)) out.push({key, title: txt.slice(0, 70)});
		        }
		      }
		      if (el.shadowRoot) walk(el.shadowRoot, depth + 1);
		    }
		  }
		  walk(document, 0);
		  return out;
		})()`, false, &items); err != nil {
			return err
		}

		clickedAny := false
		for _, it := range items {
			if report.BingReadOK >= count {
				break
			}
			if seen[it.Key] {
				continue
			}
			seen[it.Key] = true
			kJSON, _ := json.Marshal(it.Key)
			var ok bool
			if err := r.evalJS(ctx, BingPkg, fTID, fmt.Sprintf(`(() => {
			  const want = %s;
			  let found = null;
			  function walk(root, depth) {
			    if (!root || depth > 8 || found) return;
			    for (const el of (root.querySelectorAll ? root.querySelectorAll("*") : [])) {
			      if (el.tagName === "A" && (el.getAttribute("href") || "").split("?")[0] === want) { found = el; return; }
			      if (el.shadowRoot) walk(el.shadowRoot, depth + 1);
			    }
			  }
			  walk(document, 0);
			  if (!found) return false;
			  found.scrollIntoView({block: "center"});
			  found.click();
			  return true;
			})()`, string(kJSON)), false, &ok); err != nil {
				return err
			}
			if !ok {
				continue
			}
			clickedAny = true
			if err := r.jitterSleep(ctx, 1.5, 2.2); err != nil {
				return err
			}
			if fg, err := r.backend.ForegroundApp(ctx); err == nil && fg.Package != "" && fg.Package != BingPkg && fg.Package != "com.server.auditor.ssh.client" {
				if err := r.backend.KeyEvent(ctx, "BACK"); err != nil {
					return err
				}
				if err := r.sleepCtx(ctx, 600*time.Millisecond); err != nil {
					return err
				}
				if err := r.backend.LaunchApp(ctx, BingPkg); err != nil {
					return err
				}
				continue
			}
			for fl := 0; fl < 3; fl++ {
				if err := r.jitterSwipe(ctx, 260, 1340, 260, 200, 125, 140, 440); err != nil {
					return err
				}
				if err := r.jitterSleep(ctx, 0.22, 0.42); err != nil {
					return err
				}
			}
			if err := r.jitterSleep(ctx, 0.45, 0.90); err != nil {
				return err
			}
			report.BingReadOK++
			r.logf("[Bing] Read article #%d: %s", report.BingReadOK, it.Title)
			if err := r.backend.KeyEvent(ctx, "BACK"); err != nil {
				return err
			}
			if err := r.jitterSleep(ctx, 0.6, 1.1); err != nil {
				return err
			}
		}
		if !clickedAny && report.BingReadOK < count {
			if err := r.jitterSwipe(ctx, 260, 1350, 260, 260, 220, 140, 440); err != nil {
				return err
			}
			if err := r.jitterSleep(ctx, 0.75, 1.15); err != nil {
				return err
			}
		}
	}

	if rTID2, err := r.ensureBingRewardsWebView(ctx); err != nil {
		return err
	} else if rTID2 != "" {
		if domEnd, err := r.readBingRewardsDOM(ctx, rTID2, true); err == nil {
			report.BingReadAfter = domEnd.ReadToEarn
			if domEnd.Points != "" {
				report.AfterPoints = domEnd.Points
			}
		}
	}
	return nil
}

func (r *Runner) RunMSRewards(ctx context.Context, opts MSRewardsOptions) (*MSRewardsReport, error) {
	if opts.SearchIntervalSec <= 0 {
		opts.SearchIntervalSec = 6.0
	}
	if opts.ReadCount <= 0 {
		opts.ReadCount = 10
	}
	rep := &MSRewardsReport{}
	if err := r.checkGuard(ctx); err != nil {
		return rep, err
	}

	if !opts.SkipEdge {
		r.logf("[MS] Starting Edge CDP workflow...")
		tid, err := r.ensureEdgeTarget(ctx)
		if err != nil {
			return rep, err
		}
		before, err := r.parseEarnSnapshot(ctx, tid, true)
		if err != nil {
			return rep, err
		}
		rep.BeforePoints = before.AvailablePoints
		if before.HasCaptcha || before.NeedLogin {
			rep.StoppedReason = "captcha_or_login_required"
			return rep, errors.New(rep.StoppedReason)
		}
		manual, err := r.completeDashboardDailySet(ctx, tid)
		if err != nil {
			return rep, err
		}
		rep.InteractiveTasks = append(rep.InteractiveTasks, manual...)

		interactiveRe := regexp.MustCompile(`(?i)问答|测验|quiz|poll|投票|选择`)
		for _, card := range before.DailyCards {
			if interactiveRe.MatchString(card.Text) {
				rep.InteractiveTasks = append(rep.InteractiveTasks, card.Text)
				continue
			}
			if err := r.cdpNavigate(ctx, "", tid, "https://rewards.bing.com/earn"); err != nil {
				return rep, err
			}
			if err := r.sleepCtx(ctx, 2800*time.Millisecond); err != nil {
				return rep, err
			}
			hint := strings.Split(card.Text, " ")[0]
			if len([]rune(hint)) > 16 {
				hint = string([]rune(hint)[:16])
			}
			hJSON, _ := json.Marshal(hint)
			var ok bool
			if err := r.evalJS(ctx, "", tid, fmt.Sprintf(`(() => {
			  const el = Array.from(document.querySelectorAll("a")).find(a => a.innerText && a.innerText.includes(%s));
			  if (!el) return false;
			  el.scrollIntoView({block: "center"}); el.click(); return true;
			})()`, string(hJSON)), false, &ok); err != nil {
				return rep, err
			}
			if ok {
				if err := r.sleepCtx(ctx, 5*time.Second); err != nil {
					return rep, err
				}
			}
		}

		if len(before.ExploreCards) > 0 {
			for _, card := range before.ExploreCards {
				if err := r.cdpNavigate(ctx, "", tid, resolveExploreURL(card.Href)); err != nil {
					return rep, err
				}
				if err := r.sleepCtx(ctx, 2500*time.Millisecond); err != nil {
					return rep, err
				}
				if err := r.doBingSearch(ctx, tid, MapExploreQuery(card.Text), opts.SearchIntervalSec); err != nil {
					return rep, err
				}
			}
			if retrySnap, err := r.parseEarnSnapshot(ctx, tid, false); err == nil {
				for _, card := range retrySnap.ExploreCards {
					if err := r.cdpNavigate(ctx, "", tid, resolveExploreURL(card.Href)); err != nil {
						return rep, err
					}
					if err := r.sleepCtx(ctx, 2500*time.Millisecond); err != nil {
						return rep, err
					}
					if err := r.doBingSearch(ctx, tid, altExploreQuery(card.Text), opts.SearchIntervalSec); err != nil {
						return rep, err
					}
				}
			} else if ctx.Err() != nil || isGuardError(err) {
				return rep, err
			}
		}

		mid, err := r.parseEarnSnapshot(ctx, tid, true)
		if err != nil {
			return rep, err
		}
		if mid.BingSearchCurrent != nil {
			cur := *mid.BingSearchCurrent
			capV := 100
			if mid.BingSearchCap != nil && *mid.BingSearchCap > 0 {
				capV = *mid.BingSearchCap
			}
			if cur >= capV {
				rep.SearchStatus = "already_completed"
			} else {
				pool, err := r.buildFreshSearchQueries(ctx, tid, 28)
				if err != nil {
					return rep, err
				}
				idx := 0
				rep.SearchStatus = "in_progress"
				for cur < capV && idx < len(pool) {
					rem := capV - cur
					need := (rem + 4) / 5
					batchCap := 7 + rand.Intn(4)
					if need > batchCap {
						need = batchCap
					}
					if need > len(pool)-idx {
						need = len(pool) - idx
					}
					if err := r.cdpNavigate(ctx, "", tid, "https://www.bing.com/"); err != nil {
						return rep, err
					}
					if err := r.sleepCtx(ctx, 2200*time.Millisecond); err != nil {
						return rep, err
					}
					for k := 0; k < need; k++ {
						q := pool[idx]
						idx++
						if err := r.doBingSearch(ctx, tid, q, opts.SearchIntervalSec); err != nil {
							return rep, err
						}
						rep.SearchesCount++
					}
					chk, err := r.parseEarnSnapshot(ctx, tid, true)
					if err != nil {
						if ctx.Err() != nil || isGuardError(err) {
							return rep, err
						}
						rep.SearchStatus = "unknown_counter"
						break
					}
					if chk.BingSearchCurrent == nil {
						rep.SearchStatus = "unknown_counter"
						break
					}
					newCur := *chk.BingSearchCurrent
					mid = chk
					if newCur <= cur {
						rep.SearchStatus = fmt.Sprintf("cooldown_detected_at_%d/%d", newCur, capV)
						break
					}
					cur = newCur
				}
				if cur >= capV {
					rep.SearchStatus = "completed"
				}
			}
		}
		rep.AfterPoints = mid.AvailablePoints
		rep.TodayPoints = mid.TodayPoints
		rep.DailyProgress = mid.DailyProgress
		rep.ExploreProgress = mid.ExploreProgress
		if mid.BingSearchCurrent != nil && mid.BingSearchCap != nil {
			rep.BingSearch = fmt.Sprintf("%d/%d", *mid.BingSearchCurrent, *mid.BingSearchCap)
		}
		r.logf("[MS Edge] Points=%s Today=%s Explore=%s Search=%s", rep.AfterPoints, rep.TodayPoints, rep.ExploreProgress, rep.BingSearch)
	}

	if !opts.SkipBing {
		r.logf("[MS Bing] Starting 0-screenshot Bing App WebView CDP check-in & read...")
		if err := r.runBingCheckinAndRead(ctx, opts.ReadCount, rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// ---------- JD Daily (东东农场 + 天天刮京豆, 0 screenshots) ----------

type JDDailyOptions struct {
	BrowseSec     float64
	MaxBrowse     int
	WaterMode     string // "tasks" (default) | "all" | "fixed"
	MaxWaterRound int
	AllowMature   bool
	SkipBean      bool
}

type JDDailyReport struct {
	WaterBefore   string   `json:"water_before,omitempty"`
	WaterAfter    string   `json:"water_after,omitempty"`
	StageAfter    string   `json:"stage_after,omitempty"`
	BrowseRuns    int      `json:"browse_runs"`
	DrawerClaims  int      `json:"drawer_claims"`
	WaterRounds   int      `json:"water_rounds"`
	WaterStop     string   `json:"water_stop_reason,omitempty"`
	RedPackets    []string `json:"red_packets,omitempty"`
	BeanAction    string   `json:"bean_action,omitempty"`
	BeanWorth     string   `json:"bean_worth_after,omitempty"`
	StoppedReason string   `json:"stopped_reason,omitempty"`
}

func jdDeepLink(rawURL string) string {
	payload, _ := json.Marshal(map[string]string{"category": "jump", "des": "m", "url": rawURL})
	return "openapp.jdmobile://virtual?params=" + url.QueryEscape(string(payload))
}

func (r *Runner) findJDTarget(ctx context.Context, urlSubstr, titleSubstr string) string {
	client, err := r.cdpClientFor(JDPkg)
	if err != nil {
		return ""
	}
	targets, err := client.ListTargets(ctx)
	if err != nil {
		return ""
	}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if (urlSubstr != "" && strings.Contains(t.URL, urlSubstr)) || (titleSubstr != "" && strings.Contains(t.Title, titleSubstr)) {
			return t.ID
		}
	}
	return ""
}

func (r *Runner) ensureJDFarmOpen(ctx context.Context) (string, error) {
	if err := r.checkGuard(ctx); err != nil {
		return "", err
	}
	if err := r.backend.LaunchApp(ctx, JDPkg); err != nil {
		return "", err
	}
	if err := r.sleepCtx(ctx, 1800*time.Millisecond); err != nil {
		return "", err
	}
	if tid := r.findJDTarget(ctx, "42HV4J3Q87B2xFQMJk81PCc1mEs3", "东东农场"); tid != "" {
		return tid, nil
	}
	if err := r.backend.OpenDeepLink(ctx, jdDeepLink(farmURL), JDPkg); err == nil {
		for i := 0; i < 9; i++ {
			if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
				return "", err
			}
			if tid := r.findJDTarget(ctx, "42HV4J3Q87B2xFQMJk81PCc1mEs3", "东东农场"); tid != "" {
				return tid, nil
			}
		}
	}
	for i := 0; i < 5; i++ {
		if fg, err := r.backend.ForegroundApp(ctx); err == nil && fg.Package == JDPkg && strings.Contains(fg.Activity, "MainFrameActivity") {
			break
		}
		if err := r.backend.KeyEvent(ctx, "BACK"); err != nil {
			return "", err
		}
		if err := r.sleepCtx(ctx, 1100*time.Millisecond); err != nil {
			return "", err
		}
		if tid := r.findJDTarget(ctx, "42HV4J3Q87B2xFQMJk81PCc1mEs3", "东东农场"); tid != "" {
			return tid, nil
		}
	}
	if err := r.tapImage(ctx, 155, 1515); err != nil {
		return "", err
	}
	if err := r.sleepCtx(ctx, 800*time.Millisecond); err != nil {
		return "", err
	}
	if err := r.tapImage(ctx, 155, 1515); err != nil {
		return "", err
	}
	if err := r.sleepCtx(ctx, 900*time.Millisecond); err != nil {
		return "", err
	}
	if err := r.tapImage(ctx, 300, 200); err != nil {
		return "", err
	}
	if err := r.sleepCtx(ctx, 1100*time.Millisecond); err != nil {
		return "", err
	}
	if _, err := r.backend.InputText(ctx, "东东农场", "clipboard", true); err != nil {
		return "", err
	}
	for i := 0; i < 12; i++ {
		if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
			return "", err
		}
		if tid := r.findJDTarget(ctx, "42HV4J3Q87B2xFQMJk81PCc1mEs3", "东东农场"); tid != "" {
			return tid, nil
		}
	}
	return "", errors.New("东东农场 WebView target not found")
}

type farmSummary struct {
	Water       string   `json:"water"`
	WaterNum    *int     `json:"waterNum"`
	Stage       string   `json:"stage"`
	RemainPct   *float64 `json:"remainPct"`
	NearHarvest bool     `json:"nearHarvest"`
	NeedPrize   bool     `json:"needPrize"`
	Matured     bool     `json:"matured"`
	Snippet     string   `json:"snippet"`
}

func (r *Runner) readFarmSummary(ctx context.Context, tid string) (*farmSummary, error) {
	var out farmSummary
	err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  const txt = (document.body?.innerText || "");
	  const wm = txt.match(/\b(\d+)g\b/);
	  const stage = (txt.match(/再浇([\d.]+)%([^\n!！]*)/) || [null, null, ""]);
	  const remainPct = stage[1] ? parseFloat(stage[1]) : null;
	  const nearHarvest = (remainPct !== null && remainPct <= 5.0 && /收获|成熟/.test(stage[2] || ""));
	  return {
	    water: wm ? wm[0] : "",
	    waterNum: wm ? parseInt(wm[1], 10) : null,
	    stage: stage[0] ? stage[0].trim() : "",
	    remainPct,
	    nearHarvest,
	    needPrize: /请选择奖品|档位奖励示意图/.test(txt),
	    matured: /恭喜成熟|水果已成熟|去兑换奖品|立即兑换奖品|可兑换奖品/.test(txt),
	    snippet: txt.slice(0, 400)
	  };
	})()`, false, &out)
	return &out, err
}

func (r *Runner) waitFarmReady(ctx context.Context, tid string) (*farmSummary, error) {
	deadline := time.Now().Add(14 * time.Second)
	var last *farmSummary
	for time.Now().Before(deadline) {
		s, err := r.readFarmSummary(ctx, tid)
		if err == nil {
			last = s
			if s.WaterNum != nil || s.NeedPrize || s.Matured || strings.Contains(s.Snippet, "再浇") || strings.Contains(s.Snippet, "浇5次") {
				return s, nil
			}
		} else if ctx.Err() != nil || isGuardError(err) {
			return last, err
		}
		if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
			return last, err
		}
	}
	return last, nil
}

func (r *Runner) dismissFarmPopups(ctx context.Context, tid string) (string, error) {
	var res struct {
		RedPacket string `json:"redPacket"`
		Snippet   string `json:"snippet"`
	}
	if err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  const txt = (document.body?.innerText || "");
	  const redMatch = txt.match(/¥\s*\d+(?:\.\d+)?/);
	  for (const el of Array.from(document.querySelectorAll("div, span, button"))) {
	    const t = (el.innerText || "").trim();
	    const r = el.getBoundingClientRect();
	    if (/^(开心收下|收下了|我知道了|好的|继续浇水|明日再来|明天再来)$/.test(t) && r.width > 30 && r.height > 18) {
	      el.click();
	      return {redPacket: redMatch ? redMatch[0] : "", snippet: txt.slice(0, 300)};
	    }
	  }
	  if (/恭喜中奖|无门槛红包|买买买|升级福利|水滴奖励\+10/.test(txt)) {
	    const closes = Array.from(document.querySelectorAll("[class*=close_icon], [class*=close], [class*=Close], img[src*=close]"))
	      .filter(e => { const r = e.getBoundingClientRect(); return r.width > 12 && r.width < 90 && r.top > 60; });
	    if (closes.length > 0) closes[closes.length - 1].click();
	  }
	  return {redPacket: redMatch ? redMatch[0] : "", snippet: txt.slice(0, 300)};
	})()`, false, &res); err != nil {
		return "", err
	}
	if err := CheckCaptcha(res.Snippet); err != nil {
		return "", err
	}
	return strings.ReplaceAll(res.RedPacket, "\n", ""), nil
}

func (r *Runner) openTaskDrawer(ctx context.Context, tid string) (bool, error) {
	var status string
	if err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  if ((document.body?.innerText || "").includes("做任务 领水滴")) return "already_open";
	  const wrap = document.querySelector(".bottombar_wrap");
	  if (wrap && wrap.children.length >= 3) { wrap.children[2].click(); return "clicked"; }
	  return "";
	})()`, false, &status); err != nil {
		return false, err
	}
	if status != "" {
		if err := r.sleepCtx(ctx, 700*time.Millisecond); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (r *Runner) closeTaskDrawer(ctx context.Context, tid string) error {
	if err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  if (!(document.body?.innerText || "").includes("做任务 领水滴")) return;
	  const closes = Array.from(document.querySelectorAll("[class*=close], [class*=Close], img[src*=close]"))
	    .filter(e => { const r = e.getBoundingClientRect(); return r.width > 12 && r.width < 80 && r.top > 60 && r.top < 360; });
	  if (closes.length > 0) closes[closes.length - 1].click();
	})()`, false, nil); err != nil {
		return err
	}
	return r.sleepCtx(ctx, 500*time.Millisecond)
}

func (r *Runner) claimDrawerRewards(ctx context.Context, tid string) (int, error) {
	var n int
	if err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  let n = 0;
	  const seen = new Set();
	  for (const el of Array.from(document.querySelectorAll("div, span, button"))) {
	    const t = (el.innerText || "").trim();
	    const r = el.getBoundingClientRect();
	    if (/^(去领奖|领奖励|领取)$/.test(t) && r.width > 20 && r.height > 15) {
	      if (el.parentElement && seen.has(el.parentElement)) continue;
	      if (el.parentElement) seen.add(el.parentElement);
	      el.scrollIntoView({block: "center"});
	      el.click();
	      n++;
	    }
	  }
	  return n;
	})()`, false, &n); err != nil {
		return 0, err
	}
	if n > 0 {
		if err := r.sleepCtx(ctx, 700*time.Millisecond); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (r *Runner) returnToFarmActivity(ctx context.Context) (string, error) {
	for attempt := 0; attempt < 6; attempt++ {
		if err := r.checkGuard(ctx); err != nil {
			return "", err
		}
		if fg, err := r.backend.ForegroundApp(ctx); err == nil {
			if fg.Package == JDPkg && strings.Contains(fg.Activity, "BabelActivity") {
				if tid := r.findJDTarget(ctx, "42HV4J3Q87B2xFQMJk81PCc1mEs3", "东东农场"); tid != "" {
					return tid, nil
				}
			}
			if fg.Package != "" && fg.Package != JDPkg && fg.Package != "com.server.auditor.ssh.client" {
				if err := r.backend.LaunchApp(ctx, JDPkg); err != nil {
					return "", err
				}
				if err := r.sleepCtx(ctx, 1300*time.Millisecond); err != nil {
					return "", err
				}
				continue
			}
		}
		if attempt == 1 || attempt == 3 {
			if err := r.tapImage(ctx, 360, 1142); err != nil {
				return "", err
			}
			if err := r.sleepCtx(ctx, 700*time.Millisecond); err != nil {
				return "", err
			}
		}
		if err := r.backend.KeyEvent(ctx, "BACK"); err != nil {
			return "", err
		}
		if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
			return "", err
		}
		if attempt == 2 || attempt == 4 {
			if err := r.tapImage(ctx, 45, 124); err != nil {
				return "", err
			}
			if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
				return "", err
			}
		}
	}
	return r.ensureJDFarmOpen(ctx)
}

var progRe = regexp.MustCompile(`[（(](\d+)/(\d+)[）)]`)

func (r *Runner) RunJDDaily(ctx context.Context, opts JDDailyOptions) (*JDDailyReport, error) {
	if opts.BrowseSec <= 0 {
		opts.BrowseSec = 8.5
	}
	if opts.MaxBrowse <= 0 {
		opts.MaxBrowse = 24
	}
	if opts.WaterMode == "" {
		opts.WaterMode = "tasks"
	}
	if opts.MaxWaterRound <= 0 {
		opts.MaxWaterRound = 30
	}
	rep := &JDDailyReport{}
	if err := r.checkGuard(ctx); err != nil {
		return rep, err
	}

	tid, err := r.ensureJDFarmOpen(ctx)
	if err != nil {
		return rep, err
	}
	before, err := r.waitFarmReady(ctx, tid)
	if err != nil {
		return rep, err
	}
	if before != nil {
		rep.WaterBefore = before.Water
		if before.NeedPrize {
			rep.StoppedReason = "prize_selection_required"
			return rep, errors.New(rep.StoppedReason)
		}
	}
	if _, err := r.dismissFarmPopups(ctx, tid); err != nil {
		return rep, err
	}

	// Collect floating 10g water bubble & left 10g entry
	if err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  const b = document.querySelector(".FixedTaskIcon_fixed_task_img__388lV");
	  if (b) b.click();
	  const left10 = Array.from(document.querySelectorAll(".LeftTaskEntry_task_item__o3Mg-")).find(e => (e.innerText||"").includes("10水滴"));
	  if (left10) left10.click();
	})()`, false, nil); err != nil {
		return rep, err
	}
	if err := r.sleepCtx(ctx, 900*time.Millisecond); err != nil {
		return rep, err
	}
	if _, err := r.dismissFarmPopups(ctx, tid); err != nil {
		return rep, err
	}

	// Daily sign-in (with 已签到 / 明天再来 detection)
	var signOpened bool
	if err := r.evalJS(ctx, JDPkg, tid, `(() => {
	  const w = document.querySelector(".bottombar_wrap");
	  if (w && w.children.length >= 1) { w.children[0].click(); return true; }
	  return false;
	})()`, false, &signOpened); err != nil {
		return rep, err
	}
	if signOpened {
		if err := r.sleepCtx(ctx, 2200*time.Millisecond); err != nil {
			return rep, err
		}
		if client, err := r.cdpClientFor(JDPkg); err == nil {
			targets, _ := client.ListTargets(ctx)
			for _, t := range targets {
				if t.Type == "page" && t.ID != tid {
					var signRes string
					if err := r.evalJS(ctx, JDPkg, t.ID, `(() => {
					  const txt = document.body?.innerText || "";
					  if (/已签到|明天再来/.test(txt)) return "already_signed";
					  const btn = Array.from(document.querySelectorAll("div, span, button")).find(e => /^(点击签到领水滴|立即签到|点击签到|签到领水滴)$/.test((e.innerText||"").trim()) && e.getBoundingClientRect().width > 40);
					  if (btn) { btn.click(); return "clicked:" + btn.innerText.trim(); }
					  return "";
					})()`, false, &signRes); err != nil {
						return rep, err
					}
					if signRes != "" {
						r.logf("[JD SignIn] %s", signRes)
					}
					break
				}
			}
		}
		if err := r.sleepCtx(ctx, 1600*time.Millisecond); err != nil {
			return rep, err
		}
		if err := r.backend.KeyEvent(ctx, "BACK"); err != nil {
			return rep, err
		}
		if err := r.sleepCtx(ctx, 1200*time.Millisecond); err != nil {
			return rep, err
		}
		tid, err = r.ensureJDFarmOpen(ctx)
		if err != nil {
			return rep, err
		}
		if _, err := r.waitFarmReady(ctx, tid); err != nil {
			return rep, err
		}
		if _, err := r.dismissFarmPopups(ctx, tid); err != nil {
			return rep, err
		}
	}

	// Safe 6s browse tasks
	drawerOpen, err := r.openTaskDrawer(ctx, tid)
	if err != nil {
		return rep, err
	}
	if drawerOpen {
		c0, err := r.claimDrawerRewards(ctx, tid)
		if err != nil {
			return rep, err
		}
		rep.DrawerClaims += c0
		tried := make(map[string]int)
		stalled := make(map[string][2]int)
		for run := 0; run < opts.MaxBrowse; run++ {
			if err := r.checkGuard(ctx); err != nil {
				return rep, err
			}
			cn, err := r.claimDrawerRewards(ctx, tid)
			if err != nil {
				return rep, err
			}
			rep.DrawerClaims += cn
			var cand []struct {
				RowText string `json:"rowText"`
			}
			if err := r.evalJS(ctx, JDPkg, tid, `(() => {
			  const seen = new Set(); const rows = [];
			  for (const btn of Array.from(document.querySelectorAll("div, span, button"))) {
			    const bt = (btn.innerText || "").trim();
			    const br = btn.getBoundingClientRect();
			    if (!/^(去完成|逛一逛)$/.test(bt) || br.width < 30 || br.height < 18) continue;
			    let row = btn.parentElement;
			    for (let k = 0; k < 4 && row; k++) {
			      if ((row.innerText || "").length > 15 && row.getBoundingClientRect().width > 240) break;
			      row = row.parentElement;
			    }
			    const rtxt = (row ? row.innerText : bt).replace(/\s+/g, " ").trim();
			    if (seen.has(rtxt)) continue;
			    seen.add(rtxt);
			    rows.push({rowText: rtxt.slice(0, 100)});
			  }
			  return rows;
			})()`, false, &cand); err != nil {
				return rep, err
			}

			chosenText := ""
			chosenKey := ""
			chosenCur := 0
			for _, c := range cand {
				if ForbiddenTaskRe.MatchString(c.RowText) || !SafeBrowseRe.MatchString(c.RowText) {
					continue
				}
				curN, tgtN := 0, 1
				if m := progRe.FindStringSubmatch(c.RowText); len(m) == 3 {
					fmt.Sscanf(m[1], "%d", &curN)
					fmt.Sscanf(m[2], "%d", &tgtN)
				}
				key := progRe.ReplaceAllString(c.RowText, "")
				if len([]rune(key)) > 24 {
					key = string([]rune(key)[:24])
				}
				maxTries := 2
				if tgtN > 1 {
					maxTries = tgtN + 6
				}
				if tried[key] >= maxTries {
					continue
				}
				if st, ok := stalled[key]; ok && st[0] == curN && st[1] >= 3 {
					continue
				}
				chosenText, chosenKey, chosenCur = c.RowText, key, curN
				break
			}
			if chosenText == "" {
				break
			}
			tried[chosenKey]++
			prev := stalled[chosenKey]
			if prev[0] == chosenCur && prev[1] > 0 {
				stalled[chosenKey] = [2]int{chosenCur, prev[1] + 1}
			} else {
				stalled[chosenKey] = [2]int{chosenCur, 1}
			}

			wantPrefix := chosenText
			if len([]rune(wantPrefix)) > 24 {
				wantPrefix = string([]rune(wantPrefix)[:24])
			}
			wJSON, _ := json.Marshal(wantPrefix)
			var clicked bool
			if err := r.evalJS(ctx, JDPkg, tid, fmt.Sprintf(`(() => {
			  const want = %s;
			  for (const btn of Array.from(document.querySelectorAll("div, span, button"))) {
			    const bt = (btn.innerText || "").trim();
			    const br = btn.getBoundingClientRect();
			    if (!/^(去完成|逛一逛)$/.test(bt) || br.width < 30 || br.height < 18) continue;
			    let row = btn.parentElement;
			    for (let k = 0; k < 4 && row; k++) {
			      if ((row.innerText || "").length > 15 && row.getBoundingClientRect().width > 240) break;
			      row = row.parentElement;
			    }
			    if (row && (row.innerText || "").replace(/\s+/g, " ").includes(want)) {
			      btn.scrollIntoView({block: "center"}); btn.click(); return true;
			    }
			  }
			  return false;
			})()`, string(wJSON)), false, &clicked); err != nil {
				return rep, err
			}
			if !clicked {
				break
			}

			targetSec := opts.BrowseSec + rand.Float64()*2.0 - 0.3
			if targetSec < 7.8 {
				targetSec = 7.8
			}
			t1 := 1.8 + rand.Float64()*0.9
			t2 := 2.5 + rand.Float64()*1.1
			rem := targetSec - t1 - t2
			if rem < 1.0 {
				rem = 1.0
			}
			if err := r.sleepCtx(ctx, time.Duration(t1*float64(time.Second))); err != nil {
				return rep, err
			}
			if err := r.jitterSwipe(ctx, 360, 1100, 360, 680, 220, 160, 520); err != nil {
				return rep, err
			}
			if err := r.sleepCtx(ctx, time.Duration(t2*float64(time.Second))); err != nil {
				return rep, err
			}
			if err := r.jitterSwipe(ctx, 360, 1000, 360, 720, 200, 160, 520); err != nil {
				return rep, err
			}
			if err := r.sleepCtx(ctx, time.Duration(rem*float64(time.Second))); err != nil {
				return rep, err
			}

			if err := r.backend.KeyEvent(ctx, "BACK"); err != nil {
				return rep, err
			}
			if err := r.jitterSleep(ctx, 0.85, 1.35); err != nil {
				return rep, err
			}
			tid, err = r.returnToFarmActivity(ctx)
			if err != nil {
				return rep, err
			}
			if _, err := r.openTaskDrawer(ctx, tid); err != nil {
				return rep, err
			}
			cn2, err := r.claimDrawerRewards(ctx, tid)
			if err != nil {
				return rep, err
			}
			rep.DrawerClaims += cn2
			rep.BrowseRuns++
			r.logf("[JD Browse #%d] %s", rep.BrowseRuns, wantPrefix)
		}
		if err := r.closeTaskDrawer(ctx, tid); err != nil {
			return rep, err
		}
	}

	singleWaterClick := func() (string, error) {
		var btnText string
		if err := r.evalJS(ctx, JDPkg, tid, `(() => {
		  const btn = Array.from(document.querySelectorAll("div, span, button")).find(e => /^(浇5次|浇水|浇1次)$/.test((e.innerText||"").trim()) && e.getBoundingClientRect().width > 20 && e.getBoundingClientRect().top > 200);
		  if (!btn) return "";
		  btn.click();
		  return btn.innerText.trim();
		})()`, false, &btnText); err != nil {
			return "", err
		}
		return btnText, nil
	}

	// Watering: supports "tasks" (milestone only), "all" (pour until <10g or harvest), and "fixed" (up to MaxWaterRound clicks).
	switch opts.WaterMode {
	case "tasks":
		for cycle := 0; cycle < 6; cycle++ {
			opened, err := r.openTaskDrawer(ctx, tid)
			if err != nil {
				return rep, err
			}
			if !opened {
				rep.WaterStop = "drawer_unavailable"
				break
			}
			cn, err := r.claimDrawerRewards(ctx, tid)
			if err != nil {
				return rep, err
			}
			rep.DrawerClaims += cn
			var wt struct {
				Found  bool `json:"found"`
				Cur    int  `json:"cur"`
				Target int  `json:"target"`
			}
			if err := r.evalJS(ctx, JDPkg, tid, `(() => {
			  const m = (document.body?.innerText || "").match(/每日累计浇水(\d+)次\s*[（(](\d+)\/(\d+)[）)]/);
			  if (!m) return {found: false};
			  return {found: true, cur: parseInt(m[2], 10), target: parseInt(m[3], 10)};
			})()`, false, &wt); err != nil {
				return rep, err
			}
			if err := r.closeTaskDrawer(ctx, tid); err != nil {
				return rep, err
			}
			if !wt.Found || wt.Target-wt.Cur <= 0 {
				rep.WaterStop = "all_watering_tasks_done"
				break
			}
			needClicks := (wt.Target - wt.Cur + 4) / 5
			if needClicks > opts.MaxWaterRound-rep.WaterRounds {
				needClicks = opts.MaxWaterRound - rep.WaterRounds
			}
			stoppedEarly := false
			for c := 0; c < needClicks; c++ {
				if rp, err := r.dismissFarmPopups(ctx, tid); err != nil {
					return rep, err
				} else if rp != "" {
					rep.RedPackets = append(rep.RedPackets, rp)
				}
				sum, err := r.readFarmSummary(ctx, tid)
				if err != nil {
					return rep, err
				}
				if sum != nil {
					if sum.Matured {
						rep.WaterStop = "tree_matured_ready_to_redeem"
						stoppedEarly = true
						break
					}
					if !opts.AllowMature && sum.NearHarvest {
						rep.WaterStop = "paused_near_harvest_for_user"
						stoppedEarly = true
						break
					}
					if sum.WaterNum != nil && *sum.WaterNum < 10 {
						rep.WaterStop = "water_insufficient"
						stoppedEarly = true
						break
					}
				}
				btnText, err := singleWaterClick()
				if err != nil {
					return rep, err
				}
				if btnText == "" {
					rep.WaterStop = "water_button_not_found"
					stoppedEarly = true
					break
				}
				rep.WaterRounds++
				if err := r.jitterSleep(ctx, 1.5, 2.3); err != nil {
					return rep, err
				}
				if rp, err := r.dismissFarmPopups(ctx, tid); err != nil {
					return rep, err
				} else if rp != "" {
					rep.RedPackets = append(rep.RedPackets, rp)
				}
			}
			if stoppedEarly || rep.WaterRounds >= opts.MaxWaterRound {
				break
			}
		}

	case "all", "fixed":
		stalled := 0
		rep.WaterStop = "completed"
		for idx := 1; idx <= opts.MaxWaterRound; idx++ {
			if rp, err := r.dismissFarmPopups(ctx, tid); err != nil {
				return rep, err
			} else if rp != "" {
				rep.RedPackets = append(rep.RedPackets, rp)
			}
			sum, err := r.readFarmSummary(ctx, tid)
			if err != nil {
				return rep, err
			}
			var beforeWater *int
			if sum != nil {
				beforeWater = sum.WaterNum
				if sum.Matured {
					rep.WaterStop = "tree_matured_ready_to_redeem"
					break
				}
				if !opts.AllowMature && sum.NearHarvest {
					rep.WaterStop = "paused_near_harvest_for_user"
					break
				}
				if opts.WaterMode == "all" && sum.WaterNum != nil && *sum.WaterNum < 10 {
					rep.WaterStop = "water_insufficient"
					break
				}
			}
			btnText, err := singleWaterClick()
			if err != nil {
				return rep, err
			}
			if btnText == "" {
				rep.WaterStop = "water_button_not_found"
				break
			}
			rep.WaterRounds++
			if err := r.jitterSleep(ctx, 1.5, 2.3); err != nil {
				return rep, err
			}
			if rp, err := r.dismissFarmPopups(ctx, tid); err != nil {
				return rep, err
			} else if rp != "" {
				rep.RedPackets = append(rep.RedPackets, rp)
			}
			afterSum, err := r.readFarmSummary(ctx, tid)
			if err != nil {
				return rep, err
			}
			if beforeWater != nil && afterSum != nil && afterSum.WaterNum != nil && *afterSum.WaterNum >= *beforeWater {
				stalled++
				if stalled >= 4 {
					rep.WaterStop = "water_unchanged_or_daily_limit"
					break
				}
			} else {
				stalled = 0
			}
			if opts.WaterMode == "all" && idx%8 == 0 {
				if opened, err := r.openTaskDrawer(ctx, tid); err != nil {
					return rep, err
				} else if opened {
					cn, err := r.claimDrawerRewards(ctx, tid)
					if err != nil {
						return rep, err
					}
					rep.DrawerClaims += cn
					if err := r.closeTaskDrawer(ctx, tid); err != nil {
						return rep, err
					}
				}
			}
		}
	}

	if opened, err := r.openTaskDrawer(ctx, tid); err != nil {
		return rep, err
	} else if opened {
		cn, err := r.claimDrawerRewards(ctx, tid)
		if err != nil {
			return rep, err
		}
		rep.DrawerClaims += cn
		if err := r.closeTaskDrawer(ctx, tid); err != nil {
			return rep, err
		}
	}
	if after, err := r.readFarmSummary(ctx, tid); err == nil && after != nil {
		rep.WaterAfter = after.Water
		rep.StageAfter = after.Stage
	}
	r.logf("[JD Farm] Water %s -> %s (%s), Browse=%d, WaterRounds=%d", rep.WaterBefore, rep.WaterAfter, rep.StageAfter, rep.BrowseRuns, rep.WaterRounds)

	// 天天刮京豆
	if !opts.SkipBean {
		bTID := r.findJDTarget(ctx, "4E1fFrgX1eGtWTc5KBNFzXMrh6xn", "")
		if bTID == "" {
			_ = r.backend.OpenDeepLink(ctx, jdDeepLink(beanURL), JDPkg)
			for i := 0; i < 8; i++ {
				if err := r.sleepCtx(ctx, 1*time.Second); err != nil {
					return rep, err
				}
				if bTID = r.findJDTarget(ctx, "4E1fFrgX1eGtWTc5KBNFzXMrh6xn", ""); bTID != "" {
					break
				}
			}
		}
		if bTID != "" {
			if err := r.sleepCtx(ctx, 1400*time.Millisecond); err != nil {
				return rep, err
			}
			var info struct {
				Text string  `json:"text"`
				X    float64 `json:"x"`
				Y    float64 `json:"y"`
				VW   float64 `json:"vw"`
			}
			if err := r.evalJS(ctx, JDPkg, bTID, `(() => {
			  const btn = Array.from(document.querySelectorAll("div, span, button")).find(e => /^(立即刮卡|明日再来|今日已刮)$/.test((e.innerText||"").trim()) && e.getBoundingClientRect().width > 40);
			  if (!btn) return {text: ""};
			  const r = btn.getBoundingClientRect();
			  return {text: btn.innerText.trim(), x: r.left + r.width/2, y: r.top + r.height/2, vw: window.innerWidth || 393};
			})()`, false, &info); err != nil {
				return rep, err
			}
			if info.Text == "立即刮卡" {
				vw := info.VW
				if vw <= 0 {
					vw = 393
				}
				tx := int(math.Round(info.X * float64(r.imageWidth) / vw))
				ty := int(math.Round(info.Y * float64(r.imageWidth) / vw))
				if ty < 200 || ty > 900 {
					tx, ty = 360, 535
				}
				if err := r.tapImage(ctx, tx, ty); err != nil {
					return rep, err
				}
				rep.BeanAction = "scratched"
				if err := r.sleepCtx(ctx, 2200*time.Millisecond); err != nil {
					return rep, err
				}
			} else if info.Text != "" {
				rep.BeanAction = "already_done:" + info.Text
			} else {
				rep.BeanAction = "no_scratch_button"
			}
			var afterTxt string
			if err := r.evalJS(ctx, JDPkg, bTID, `(document.body?.innerText || "").slice(0, 300)`, false, &afterTxt); err != nil {
				return rep, err
			}
			if m := regexp.MustCompile(`可抵[\d.]+元`).FindString(afterTxt); m != "" {
				rep.BeanWorth = m
			}
			r.logf("[JD Bean] action=%s worth=%s", rep.BeanAction, rep.BeanWorth)
		} else {
			rep.BeanAction = "target_not_found"
		}
	}
	return rep, nil
}

type DailyAllReport struct {
	ElapsedSec float64          `json:"elapsed_sec"`
	MSRewards  *MSRewardsReport `json:"ms_rewards,omitempty"`
	MSError    string           `json:"ms_error,omitempty"`
	JDDaily    *JDDailyReport   `json:"jd_daily,omitempty"`
	JDError    string           `json:"jd_error,omitempty"`
}

func (r *Runner) RunDailyAll(ctx context.Context) (*DailyAllReport, error) {
	t0 := time.Now()
	out := &DailyAllReport{}
	msRep, msErr := r.RunMSRewards(ctx, MSRewardsOptions{SearchIntervalSec: 6.0, ReadCount: 10})
	out.MSRewards = msRep
	if msErr != nil {
		out.MSError = msErr.Error()
		r.logf("[DailyAll] MS Rewards error: %v", msErr)
	}
	var jdErr error
	if ctx.Err() == nil {
		var jdRep *JDDailyReport
		jdRep, jdErr = r.RunJDDaily(ctx, JDDailyOptions{BrowseSec: 8.5, MaxBrowse: 24, WaterMode: "tasks"})
		out.JDDaily = jdRep
		if jdErr != nil {
			out.JDError = jdErr.Error()
			r.logf("[DailyAll] JD Daily error: %v", jdErr)
		}
	} else {
		jdErr = ctx.Err()
		out.JDError = jdErr.Error()
	}
	out.ElapsedSec = math.Round(time.Since(t0).Seconds()*10) / 10
	summary := fmt.Sprintf("耗时 %.1fs", out.ElapsedSec)
	if msRep != nil && msRep.AfterPoints != "" {
		summary += fmt.Sprintf(" | 微软 %s (今日%s)", msRep.AfterPoints, msRep.TodayPoints)
	}
	if out.JDDaily != nil && out.JDDaily.WaterAfter != "" {
		summary += fmt.Sprintf(" | 农场 %s (%s)", out.JDDaily.WaterAfter, out.JDDaily.StageAfter)
	}
	notifTitle := "每日任务完成"
	if msErr != nil && jdErr != nil {
		notifTitle = "每日任务失败"
	} else if msErr != nil || jdErr != nil {
		notifTitle = "每日任务部分失败"
	}
	if r.backend != nil {
		_ = r.backend.PostNotification(ctx, "daily_all", notifTitle, summary)
	}
	if msErr != nil || jdErr != nil {
		var errs []error
		if msErr != nil {
			errs = append(errs, fmt.Errorf("ms_rewards: %w", msErr))
		}
		if jdErr != nil {
			errs = append(errs, fmt.Errorf("jd_daily: %w", jdErr))
		}
		return out, errors.Join(errs...)
	}
	return out, nil
}
