package backend

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPurifyUITreeCardMergeAndTokenBudget(t *testing.T) {
	raw := []RawUINode{
		// Decorative root & off-screen nodes
		{Cls: "android.widget.FrameLayout", Bounds: [4]int{0, 0, 1080, 2400}, Depth: 0},
		{Cls: "android.widget.TextView", Text: "offscreen", Bounds: [4]int{0, -200, 100, -10}, Depth: 2},
		{Cls: "android.view.View", Bounds: [4]int{10, 10, 12, 12}, Flags: "c", Depth: 2}, // too small

		// Search bar
		{Cls: "android.widget.EditText", ID: "com.sankuai.meituan:id/search_edit", Text: "搜索美食、奶茶、超市", Bounds: [4]int{60, 120, 920, 220}, Flags: "ce", Depth: 3},

		// Password field must be masked
		{Cls: "android.widget.EditText", Text: "mySecretPass", Bounds: [4]int{60, 240, 920, 320}, Flags: "ep", Depth: 3},

		// Clickable shop card with child TextViews merged into one line
		{Cls: "android.widget.LinearLayout", Bounds: [4]int{40, 360, 1040, 560}, Flags: "c", Depth: 3},
		{Cls: "android.widget.TextView", Text: "蜜雪冰城（科技园店）", Bounds: [4]int{240, 380, 760, 430}, Depth: 4},
		{Cls: "android.widget.TextView", Text: "4.9分 月售3000+", Bounds: [4]int{240, 440, 680, 480}, Depth: 4},
		{Cls: "android.widget.TextView", Text: "起送¥15 免配送费", Bounds: [4]int{240, 490, 680, 530}, Depth: 4},

		// Duplicate overlapping text right next to each other
		{Cls: "android.widget.TextView", Text: "外卖", Bounds: [4]int{80, 600, 200, 650}, Flags: "c", Depth: 3},
		{Cls: "android.widget.TextView", Text: "外卖", Bounds: [4]int{82, 602, 202, 652}, Depth: 4},
	}

	// Add 200 extra nodes to simulate heavy feed list and verify 300~500 token budget cap
	for i := 0; i < 200; i++ {
		top := 700 + i*20
		if top >= 2380 {
			top = 700 + (i%50)*30
		}
		raw = append(raw, RawUINode{
			Cls:    "android.widget.TextView",
			Text:   fmt.Sprintf("优质推荐商家 #%d 满减优惠", i+1),
			Bounds: [4]int{40, top, 980, top + 28},
			Flags:  "c",
			Depth:  4,
		})
	}

	res := PurifyUITree(raw, "com.sankuai.meituan", 1080, 2400, UITreeOptions{MaxTokens: 450})
	if res.Empty {
		t.Fatal("expected non-empty purified UI tree")
	}
	if !res.Truncated {
		t.Fatal("expected 200-item tree to enforce token truncation")
	}
	if res.ApproxTokens > 460 {
		t.Fatalf("expected compressed output within ~450 tokens, got %d\n%s", res.ApproxTokens, res.Text)
	}
	if strings.Contains(res.Text, "mySecretPass") {
		t.Fatal("password text leaked into UI tree output")
	}
	if !strings.Contains(res.Text, `"蜜雪冰城（科技园店） | 4.9分 月售3000+ | 起送¥15 免配送费"`) {
		t.Fatalf("expected merged shop card label, got:\n%s", res.Text)
	}
	if strings.Count(res.Text, `"外卖"`) != 1 {
		t.Fatalf("expected duplicate '外卖' label to be collapsed, got:\n%s", res.Text)
	}
}

func TestPurifyUITreeEmptyProvidesFallbackHint(t *testing.T) {
	res := PurifyUITree(nil, "com.unity.game", 1080, 2400, UITreeOptions{})
	if !res.Empty || res.NodeCount != 0 || res.FallbackHint == "" {
		t.Fatalf("expected empty flag and fallback hint, got %+v", res)
	}
}

func TestTapPointAndScrollBoundsGeometry(t *testing.T) {
	n := UINode{ID: 1, Bounds: [4]int{100, 500, 300, 620}}
	for i := 0; i < 30; i++ {
		x, y := TapPointForNode(n, 1080, 2400)
		if x < 102 || x > 298 || y < 502 || y > 618 {
			t.Fatalf("tap point (%d,%d) escaped bounds %v", x, y, n.Bounds)
		}
	}

	x1, y1, x2, y2, err := ScrollCoordsForBounds([4]int{0, 400, 1080, 2200}, "down", 0.65)
	if err != nil || y1 <= y2 {
		t.Fatalf("scroll down should swipe bottom->top: (%d,%d)->(%d,%d) err=%v", x1, y1, x2, y2, err)
	}
	x1, y1, x2, y2, err = ScrollCoordsForBounds([4]int{0, 400, 1080, 2200}, "right", 0.5)
	if err != nil || x1 <= x2 {
		t.Fatalf("scroll right should swipe right->left: (%d,%d)->(%d,%d) err=%v", x1, y1, x2, y2, err)
	}
}

func TestMicroKernelLazyLoadIdleExitAndGameGuardKill(t *testing.T) {
	mk := NewMicroKernel(nil, 150*time.Millisecond)
	// Use a pure POSIX shell JSONL echo loop on Linux host to test lifecycle deterministically
	mk.cmdFactory = func(_ bool) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", `while IFS= read -r line; do case "$line" in *ping*) printf '{"ok":true,"pong":true}\n';; *) printf '{"ok":true}\n';; esac; done`)
	}

	if mk.Running() {
		t.Fatal("microkernel must be lazy and NOT running before first call")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := mk.Call(ctx, kernelRequest{Cmd: "ping"})
	if err != nil || !resp.Pong {
		t.Fatalf("ping failed: resp=%+v err=%v", resp, err)
	}
	if !mk.Running() {
		t.Fatal("expected microkernel to be running right after first call")
	}

	// Wait past 150ms idle timeout -> child must auto-exit cleanly
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && mk.Running() {
		time.Sleep(25 * time.Millisecond)
	}
	if mk.Running() {
		t.Fatal("expected microkernel to auto-exit after idle timeout")
	}

	// Second call lazily respawns, and Stop() (simulating sgame foreground) kills immediately
	if _, err := mk.Call(ctx, kernelRequest{Cmd: "ping"}); err != nil {
		t.Fatalf("lazy respawn failed: %v", err)
	}
	if !mk.Running() {
		t.Fatal("expected microkernel running after respawn")
	}
	mk.Stop("com.tencent.tmgp.sgame")
	if mk.Running() {
		t.Fatal("expected Stop to kill microkernel immediately")
	}
}
