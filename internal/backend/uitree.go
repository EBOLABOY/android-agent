package backend

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultUIMaxDepth  = 36
	DefaultUIMaxNodes  = 1200
	DefaultUIMaxTokens = 450
	maxEmittedUINodes  = 75
	maxLabelRunes      = 48
)

// RawUINode mirrors a compact node emitted by AgentKernel's AccessibilityNodeInfo walker.
type RawUINode struct {
	Cls    string `json:"cls"`
	Pkg    string `json:"pkg,omitempty"`
	ID     string `json:"id,omitempty"`
	Text   string `json:"t,omitempty"`
	Desc   string `json:"d,omitempty"`
	Bounds [4]int `json:"b"` // [left, top, right, bottom]
	Flags  string `json:"f,omitempty"`
	Depth  int    `json:"dp"`
}

// UITreeOptions tunes traversal bounds and LLM prompt token budget.
type UITreeOptions struct {
	MaxDepth  int `json:"max_depth,omitempty"`
	MaxNodes  int `json:"max_nodes,omitempty"`
	MaxTokens int `json:"max_tokens,omitempty"`
}

// UINode represents a purified, numbered UI element cached in Go for id-based interaction.
type UINode struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Label      string `json:"label,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
	Bounds     [4]int `json:"bounds"`
	Clickable  bool   `json:"clickable,omitempty"`
	Scrollable bool   `json:"scrollable,omitempty"`
	Editable   bool   `json:"editable,omitempty"`
	Checkable  bool   `json:"checkable,omitempty"`
	Checked    bool   `json:"checked,omitempty"`
	Selected   bool   `json:"selected,omitempty"`
}

// UITreeResult is returned to MCP / Brain clients after purification and token budgeting.
type UITreeResult struct {
	Package      string   `json:"package"`
	ScreenWidth  int      `json:"screen_width,omitempty"`
	ScreenHeight int      `json:"screen_height,omitempty"`
	Text         string   `json:"text"`
	Nodes        []UINode `json:"nodes,omitempty"`
	NodeCount    int      `json:"node_count"`
	RawCount     int      `json:"raw_count"`
	Truncated    bool     `json:"truncated,omitempty"`
	Empty        bool     `json:"empty,omitempty"`
	ElapsedMs    int64    `json:"elapsed_ms"`
	ApproxTokens int      `json:"approx_tokens"`
	FallbackHint string   `json:"fallback_hint,omitempty"`
}

func (o UITreeOptions) normalized() UITreeOptions {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultUIMaxDepth
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = DefaultUIMaxNodes
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = DefaultUIMaxTokens
	}
	return o
}

// PurifyUITree filters invisible/decorative nodes, merges card labels, deduplicates overlaps,
// numbers surviving interactive elements [1..N], and enforces the LLM token budget.
func PurifyUITree(raw []RawUINode, pkg string, screenW, screenH int, opts UITreeOptions) *UITreeResult {
	opts = opts.normalized()
	if screenW <= 0 {
		screenW = 1080
	}
	if screenH <= 0 {
		screenH = 2400
	}
	// Defensive orientation auto-swap if caller passed physical portrait bounds while screen is landscape.
	maxX, maxY := 0, 0
	for _, rn := range raw {
		if rn.Bounds[2] > maxX {
			maxX = rn.Bounds[2]
		}
		if rn.Bounds[3] > maxY {
			maxY = rn.Bounds[3]
		}
	}
	if maxX > screenW && maxX <= screenH && maxY <= screenW {
		screenW, screenH = screenH, screenW
	}

	// Step 1: Filter valid geometry
	valid := make([]RawUINode, 0, len(raw))
	for _, rn := range raw {
		b := rn.Bounds
		w := b[2] - b[0]
		h := b[3] - b[1]
		if w < 4 || h < 4 {
			continue
		}
		if b[2] <= 0 || b[0] >= screenW || b[3] <= 0 || b[1] >= screenH {
			continue
		}
		// Clamp bounds inside visible screen area
		if b[0] < 0 {
			b[0] = 0
		}
		if b[1] < 0 {
			b[1] = 0
		}
		if b[2] > screenW {
			b[2] = screenW
		}
		if b[3] > screenH {
			b[3] = screenH
		}
		rn.Bounds = b
		if pkg == "" && rn.Pkg != "" {
			pkg = rn.Pkg
		}
		valid = append(valid, rn)
	}

	consumed := make([]bool, len(valid))
	candidates := make([]UINode, 0, len(valid))

	for i, rn := range valid {
		if consumed[i] {
			continue
		}
		flags := rn.Flags
		isPass := strings.ContainsRune(flags, 'p')
		isClick := strings.ContainsRune(flags, 'c')
		isLong := strings.ContainsRune(flags, 'l')
		isScroll := strings.ContainsRune(flags, 's')
		isEdit := strings.ContainsRune(flags, 'e')
		isCheck := strings.ContainsRune(flags, 'k')
		isChecked := strings.ContainsRune(flags, 'x')
		isSel := strings.ContainsRune(flags, 'S')

		label := cleanLabel(rn.Text, rn.Desc, isPass)

		// Merge child text/descriptions into empty clickable card containers (e.g. Meituan shop cards)
		if (isClick || isLong) && !isScroll && !isEdit && label == "" {
			var merged []string
			seenPiece := make(map[string]struct{})
			for j := i + 1; j < len(valid) && len(merged) < 3; j++ {
				if consumed[j] {
					continue
				}
				child := valid[j]
				if child.Depth <= rn.Depth {
					break
				}
				if !rectContains(rn.Bounds, child.Bounds) {
					continue
				}
				cFlags := child.Flags
				if strings.ContainsRune(cFlags, 'c') || strings.ContainsRune(cFlags, 'e') || strings.ContainsRune(cFlags, 's') {
					continue
				}
				cl := cleanLabel(child.Text, child.Desc, strings.ContainsRune(cFlags, 'p'))
				if cl == "" {
					continue
				}
				consumed[j] = true
				if _, ok := seenPiece[cl]; !ok {
					seenPiece[cl] = struct{}{}
					merged = append(merged, cl)
				}
			}
			if len(merged) > 0 {
				label = strings.Join(merged, " | ")
			}
		}

		interactive := isClick || isLong || isScroll || isEdit || isCheck
		if !interactive && label == "" {
			continue
		}
		if label == "" && !isScroll && !isEdit {
			// Skip unlabelled generic View/ImageView with clickable/focusable noise unless ID gives signal
			resShort := shortResourceID(rn.ID)
			if resShort == "" || (!isClick && !isCheck) {
				continue
			}
			label = "#" + resShort
		}

		label = truncateRunes(label, maxLabelRunes)
		node := UINode{
			Type:       shortClassName(rn.Cls),
			Label:      label,
			ResourceID: shortResourceID(rn.ID),
			Bounds:     rn.Bounds,
			Clickable:  isClick || isLong,
			Scrollable: isScroll,
			Editable:   isEdit,
			Checkable:  isCheck,
			Checked:    isChecked,
			Selected:   isSel,
		}
		candidates = append(candidates, node)
	}

	// Step 2: Deduplicate overlapping elements with identical label & near-identical center
	deduped := make([]UINode, 0, len(candidates))
	for _, c := range candidates {
		dupIdx := -1
		cx, cy := rectCenter(c.Bounds)
		for idx, existing := range deduped {
			if existing.Label != "" && existing.Label == c.Label {
				ex, ey := rectCenter(existing.Bounds)
				if absInt(cx-ex) <= 12 && absInt(cy-ey) <= 12 {
					dupIdx = idx
					break
				}
			}
		}
		if dupIdx >= 0 {
			// Keep whichever is richer interactively
			if (c.Clickable || c.Editable) && !(deduped[dupIdx].Clickable || deduped[dupIdx].Editable) {
				deduped[dupIdx] = c
			}
			continue
		}
		deduped = append(deduped, c)
	}

	// Step 3: Priority-bucketed packing (top/bottom bars, inputs, primary CTAs protected) into token budget,
	// then restore original visual top-to-bottom order before assigning IDs [1..N].
	if pkg == "" {
		pkg = "unknown"
	}
	headerPlaceholder := fmt.Sprintf("App: %s (%dx%d, 0ms, 999 nodes, ~999 tok)", pkg, screenW, screenH)
	usedTokens := estimateTokens(headerPlaceholder)

	type indexedNode struct {
		origIdx int
		tier    int
		tokens  int
		node    UINode
	}
	var buckets [3][]indexedNode
	for i, n := range deduped {
		tier := classifyUINodeTier(n, screenH)
		sample := n
		sample.ID = len(deduped)
		if sample.ID > maxEmittedUINodes {
			sample.ID = maxEmittedUINodes
		}
		tok := estimateTokens(formatNodeLine(sample)) + 1
		buckets[tier] = append(buckets[tier], indexedNode{
			origIdx: i,
			tier:    tier,
			tokens:  tok,
			node:    n,
		})
	}

	var chosen []indexedNode
	truncated := false
	hasNonEdgeBody := len(buckets[1]) > 0
	tier0Cap := opts.MaxTokens
	if hasNonEdgeBody {
		tier0Cap = usedTokens + int(math.Ceil(float64(opts.MaxTokens-usedTokens)*0.60))
		if tier0Cap <= usedTokens {
			tier0Cap = opts.MaxTokens
		}
	}

	for tier := 0; tier < 3; tier++ {
		for _, item := range buckets[tier] {
			if len(chosen) >= maxEmittedUINodes {
				truncated = true
				break
			}
			limit := opts.MaxTokens
			if tier == 0 {
				limit = tier0Cap
			}
			if len(chosen) > 0 && usedTokens+item.tokens > limit {
				truncated = true
				continue
			}
			usedTokens += item.tokens
			chosen = append(chosen, item)
		}
	}

	// Preserve visual order top-to-bottom after priority selection.
	sort.Slice(chosen, func(i, j int) bool {
		return chosen[i].origIdx < chosen[j].origIdx
	})

	lines := make([]string, 0, len(chosen))
	finalNodes := make([]UINode, 0, len(chosen))
	actualTok := estimateTokens(headerPlaceholder)
	for i, item := range chosen {
		n := item.node
		n.ID = i + 1
		line := formatNodeLine(n)
		actualTok += estimateTokens(line) + 1
		finalNodes = append(finalNodes, n)
		lines = append(lines, line)
	}
	usedTokens = actualTok

	header := fmt.Sprintf("App: %s (%dx%d, %d nodes, ~%d tok)", pkg, screenW, screenH, len(finalNodes), usedTokens)
	if truncated {
		header += " [truncated]"
	}
	var sb strings.Builder
	sb.WriteString(header)
	for _, line := range lines {
		sb.WriteByte('\n')
		sb.WriteString(line)
	}
	text := sb.String()
	if len(finalNodes) == 0 {
		return &UITreeResult{
			Package:      pkg,
			ScreenWidth:  screenW,
			ScreenHeight: screenH,
			Text:         text,
			NodeCount:    0,
			RawCount:     len(raw),
			Empty:        true,
			ApproxTokens: estimateTokens(text),
			FallbackHint: "UI tree is empty (WebView, Canvas, or protected surface); use cdp_* for WebViews or phone_screenshot for visual fallback.",
		}
	}

	return &UITreeResult{
		Package:      pkg,
		ScreenWidth:  screenW,
		ScreenHeight: screenH,
		Text:         text,
		Nodes:        finalNodes,
		NodeCount:    len(finalNodes),
		RawCount:     len(raw),
		Truncated:    truncated,
		ApproxTokens: estimateTokens(text),
	}
}

// TapPointForNode returns the center coordinates of node.Bounds plus small bounded jitter.
func TapPointForNode(n UINode, screenW, screenH int) (int, int) {
	l, t, r, b := n.Bounds[0], n.Bounds[1], n.Bounds[2], n.Bounds[3]
	cx := (l + r) / 2
	cy := (t + b) / 2
	w := r - l
	h := b - t
	jxMax := minInt(12, w/6)
	jyMax := minInt(12, h/6)
	if jxMax > 0 {
		cx += rand.Intn(2*jxMax+1) - jxMax
	}
	if jyMax > 0 {
		cy += rand.Intn(2*jyMax+1) - jyMax
	}
	if r-l >= 4 {
		cx = clampInt(cx, l+2, r-2)
	}
	if b-t >= 4 {
		cy = clampInt(cy, t+2, b-2)
	}
	if screenW > 0 {
		cx = clampInt(cx, 1, screenW-2)
	}
	if screenH > 0 {
		cy = clampInt(cy, 1, screenH-2)
	}
	return cx, cy
}

// ScrollCoordsForBounds computes swipe start/end coordinates inside bounds for a content scroll direction.
// direction="down" scrolls content downward (finger drags from bottom toward top).
func ScrollCoordsForBounds(bounds [4]int, direction string, distance float64) (x1, y1, x2, y2 int, err error) {
	l, t, r, b := bounds[0], bounds[1], bounds[2], bounds[3]
	w := r - l
	h := b - t
	if w < 10 || h < 10 {
		return 0, 0, 0, 0, fmt.Errorf("container bounds too small to scroll: %v", bounds)
	}
	if distance <= 0 || distance > 0.95 {
		distance = 0.65
	}
	cx := (l + r) / 2
	cy := (t + b) / 2

	dir := strings.ToLower(strings.TrimSpace(direction))
	if dir == "" {
		dir = "down"
	}
	switch dir {
	case "down":
		span := int(math.Round(float64(h) * distance / 2))
		x1 = cx + randOffset(minInt(18, w/10))
		x2 = cx + randOffset(minInt(18, w/10))
		y1 = clampInt(cy+span, t+12, b-12)
		y2 = clampInt(cy-span, t+12, b-12)
	case "up":
		span := int(math.Round(float64(h) * distance / 2))
		x1 = cx + randOffset(minInt(18, w/10))
		x2 = cx + randOffset(minInt(18, w/10))
		y1 = clampInt(cy-span, t+12, b-12)
		y2 = clampInt(cy+span, t+12, b-12)
	case "right":
		span := int(math.Round(float64(w) * distance / 2))
		y1 = cy + randOffset(minInt(18, h/10))
		y2 = cy + randOffset(minInt(18, h/10))
		x1 = clampInt(cx+span, l+12, r-12)
		x2 = clampInt(cx-span, l+12, r-12)
	case "left":
		span := int(math.Round(float64(w) * distance / 2))
		y1 = cy + randOffset(minInt(18, h/10))
		y2 = cy + randOffset(minInt(18, h/10))
		x1 = clampInt(cx-span, l+12, r-12)
		x2 = clampInt(cx+span, l+12, r-12)
	default:
		return 0, 0, 0, 0, fmt.Errorf("invalid scroll direction %q (use up, down, left, right)", direction)
	}
	return x1, y1, x2, y2, nil
}

var primaryCTAKeywords = []string{
	"结算", "去结算", "加入购物车", "选规格", "立即购买",
	"搜索", "确认", "提交", "下单", "支付", "确定", "登录", "下一步",
}

func classifyUINodeTier(n UINode, screenH int) int {
	_, cy := rectCenter(n.Bounds)
	isTopBar := screenH > 0 && cy <= int(float64(screenH)*0.15)
	isBottomBar := screenH > 0 && cy >= int(float64(screenH)*0.85)
	if n.Editable {
		return 0
	}
	if n.Clickable && (isTopBar || isBottomBar) {
		return 0
	}
	if n.Clickable && n.Label != "" {
		for _, kw := range primaryCTAKeywords {
			if strings.Contains(n.Label, kw) {
				return 0
			}
		}
	}
	if n.Clickable || n.Scrollable || n.Checkable {
		return 1
	}
	return 2
}

func formatNodeLine(n UINode) string {
	var props []string
	if n.Clickable {
		props = append(props, "click")
	}
	if n.Editable {
		props = append(props, "edit")
	}
	if n.Scrollable {
		props = append(props, "scroll")
	}
	if n.Checkable {
		if n.Checked {
			props = append(props, "checked")
		} else {
			props = append(props, "check")
		}
	}
	if n.Selected {
		props = append(props, "sel")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d] %s", n.ID, n.Type)
	if n.Label != "" {
		clean := strings.ReplaceAll(n.Label, `"`, `'`)
		fmt.Fprintf(&sb, " %q", clean)
	}
	if len(props) > 0 {
		fmt.Fprintf(&sb, " (%s)", strings.Join(props, ","))
	}
	return sb.String()
}

func cleanLabel(text, desc string, isPassword bool) string {
	if isPassword {
		return "***"
	}
	t := collapseSpace(text)
	d := collapseSpace(desc)
	if t != "" && d != "" && t != d {
		if strings.Contains(d, t) {
			return d
		}
		if strings.Contains(t, d) {
			return t
		}
		return t + " / " + d
	}
	if t != "" {
		return t
	}
	return d
}

func collapseSpace(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func shortClassName(cls string) string {
	low := strings.ToLower(cls)
	switch {
	case strings.Contains(low, "edittext"):
		return "Input"
	case strings.Contains(low, "checkbox") || strings.Contains(low, "switch") || strings.Contains(low, "toggle"):
		return "Switch"
	case strings.Contains(low, "radiobutton"):
		return "Radio"
	case strings.Contains(low, "button"):
		return "Btn"
	case strings.Contains(low, "recyclerview") || strings.Contains(low, "listview") || strings.Contains(low, "scrollview") || strings.Contains(low, "viewpager"):
		return "List"
	case strings.Contains(low, "webview"):
		return "Web"
	case strings.Contains(low, "image"):
		return "Img"
	case strings.Contains(low, "textview"):
		return "Text"
	default:
		return "Item"
	}
}

func shortResourceID(id string) string {
	if id == "" {
		return ""
	}
	if idx := strings.Index(id, ":id/"); idx >= 0 {
		return id[idx+4:]
	}
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		return id[idx+1:]
	}
	return id
}

func rectContains(outer, inner [4]int) bool {
	return inner[0] >= outer[0]-4 && inner[1] >= outer[1]-4 && inner[2] <= outer[2]+4 && inner[3] <= outer[3]+4
}

func rectCenter(b [4]int) (int, int) {
	return (b[0] + b[2]) / 2, (b[1] + b[3]) / 2
}

func estimateTokens(s string) int {
	total := 0.0
	for _, r := range s {
		if r > 127 && !unicode.IsSpace(r) {
			total += 1.0
		} else {
			total += 0.35
		}
	}
	return int(math.Ceil(total))
}

func randOffset(max int) int {
	if max <= 0 {
		return 0
	}
	return rand.Intn(2*max+1) - max
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if lo > hi {
		return v
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
