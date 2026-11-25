package workflows

import (
	"strings"
	"testing"
)

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
