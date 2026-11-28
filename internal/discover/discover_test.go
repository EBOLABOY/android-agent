package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const sampleDumpsysBing = `Activity Resolver Table:
  Full MIME Types:
      image/*:
        c0be0c2 com.microsoft.bing/com.microsoft.sapphire.features.settings.SharedImageActivity filter 795fed3
          Action: "android.intent.action.SEND"
  Schemes:
      stripesdk:
        fc6a73c com.microsoft.bing/com.stripe.android.payments.StripeBrowserProxyReturnActivity filter 2f1acc5
          Action: "android.intent.action.VIEW"
          Category: "android.intent.category.BROWSABLE"
          Scheme: "stripesdk"
          Authority: "payment_return_url": -1
      bing:
        a5898e0 com.microsoft.bing/com.microsoft.sapphire.app.main.SapphireMainActivity filter 38d8799
          Action: "android.intent.action.VIEW"
          Category: "android.intent.category.DEFAULT"
          Category: "android.intent.category.BROWSABLE"
          Scheme: "bing"
          Scheme: "bingnews"
      http:
        a5898e0 com.microsoft.bing/com.microsoft.sapphire.app.main.SapphireMainActivity filter be98a5e
          Action: "android.intent.action.VIEW"
          Category: "android.intent.category.BROWSABLE"
          Scheme: "https"
          Scheme: "http"
          Authority: "www.bing.com": -1
          Path: "PatternMatcher{LITERAL: /sapphireredirect}"
          AutoVerify=true
        914a3f8 com.microsoft.bing/com.microsoft.sapphire.app.browser.IntentDispatchActivity filter b1e3637
          Action: "android.intent.action.VIEW"
          Category: "android.intent.category.BROWSABLE"
          Scheme: "https"
          Scheme: "http"

Receiver Resolver Table:
  Schemes:
      ignored:
        deadbeef com.microsoft.bing/.IgnoredReceiver filter 1234
          Scheme: "ignored"
`

func TestParseDumpsysPackageExtractsCleanDeepLinksAndFiltersNoise(t *testing.T) {
	schemes, links := ParseDumpsysPackage(sampleDumpsysBing, "com.microsoft.bing")
	if len(schemes) != 2 || schemes[0] != "bing" || schemes[1] != "bingnews" {
		t.Fatalf("unexpected schemes: %+v", schemes)
	}
	foundCustom := false
	foundAppLink := false
	for _, dl := range links {
		if dl.Scheme == "stripesdk" || dl.Scheme == "ignored" {
			t.Fatalf("noisy or receiver scheme leaked: %+v", dl)
		}
		if dl.URITemplate == "https://" || dl.URITemplate == "http://" {
			t.Fatalf("generic catch-all http link should be skipped: %+v", dl)
		}
		if dl.URITemplate == "bing://" {
			foundCustom = true
		}
		if dl.URITemplate == "https://www.bing.com/sapphireredirect" && dl.AutoVerify {
			foundAppLink = true
		}
	}
	if !foundCustom || !foundAppLink {
		t.Fatalf("expected custom bing:// and AutoVerify App Link, got %+v", links)
	}
}

func TestClassifyObservedNetworkSanitizesAndSplitsFastReadVsRisk(t *testing.T) {
	recs := []ObservedRequest{
		{
			Method:       "GET",
			URL:          "https://rewards.bing.com/api/getuserinfo?type=1&token=SECRET_XYZ&page=2",
			ResourceType: "Fetch",
			Status:       200,
			MimeType:     "application/json",
		},
		{
			Method:             "GET",
			URL:                "https://api.m.jd.com/client.action?functionId=farm_info&h5st=2026_signed_blob",
			ResourceType:       "XHR",
			Status:             200,
			MimeType:           "application/json",
			RequestHeaderNames: []string{"accept", "x-mini-wua"},
		},
		{
			Method:       "POST",
			URL:          "https://rewards.bing.com/api/reportactivity",
			ResourceType: "Fetch",
			Status:       200,
			MimeType:     "application/json",
		},
		{
			Method:       "GET",
			URL:          "https://rewards.bing.com/api/checkin?day=6",
			ResourceType: "Fetch",
			Status:       200,
			MimeType:     "application/json",
		},
		{
			Method:       "GET",
			URL:          "https://events.data.microsoft.com/OneCollector/1.0/",
			ResourceType: "XHR",
			Status:       200,
			MimeType:     "application/json",
		},
		{
			Method:       "GET",
			URL:          "https://cdn.example.com/static/app.bundle.js",
			ResourceType: "Script",
			Status:       200,
			MimeType:     "application/javascript",
		},
	}

	reads, risks, skipped := ClassifyObservedNetwork(recs)
	if skipped != 2 {
		t.Fatalf("expected 2 skipped (telemetry + js), got %d", skipped)
	}
	if len(reads) != 1 {
		t.Fatalf("expected 1 clean fast_read endpoint, got %+v", reads)
	}
	if strings.Contains(reads[0].URLTemplate, "SECRET_XYZ") || !strings.Contains(reads[0].URLTemplate, "token={redacted}") {
		t.Fatalf("expected token to be redacted in URLTemplate, got %q", reads[0].URLTemplate)
	}
	if reads[0].Mode != "fast_read" {
		t.Fatalf("expected fast_read mode, got %+v", reads[0])
	}
	if len(risks) != 3 {
		t.Fatalf("expected 3 ui_required risk endpoints (signed h5st, POST write, /checkin path), got %+v", risks)
	}
}

func TestProfileStorePersistMergeAndFallbackDemotion(t *testing.T) {
	store := NewProfileStore(filepath.Join(t.TempDir(), "app_profiles"))
	if _, err := store.MergeDeepLinks("com.example.shop", []string{"shop"}, []DeepLinkEntry{
		{Scheme: "shop", URITemplate: "shop://orders", Browsable: true},
	}); err != nil {
		t.Fatalf("MergeDeepLinks: %v", err)
	}
	if _, err := store.MergeEndpoints("com.example.shop", []DiscoveredEndpoint{
		{Method: "GET", Host: "api.shop.example", Path: "/v1/orders", URLTemplate: "https://api.shop.example/v1/orders", Mode: "fast_read", Status: 200},
	}, nil); err != nil {
		t.Fatalf("MergeEndpoints: %v", err)
	}

	// Simulate runtime 403 -> RecordFallback must demote /v1/orders from ReadAPIs -> RiskActions!
	prof, err := store.RecordFallback("com.example.shop", "https://api.shop.example/v1/orders", "HTTP 403 risk gate", 403)
	if err != nil {
		t.Fatalf("RecordFallback: %v", err)
	}
	if len(prof.ReadAPIs) != 0 || len(prof.RiskActions) != 1 || len(prof.FallbackHistory) != 1 {
		t.Fatalf("expected /v1/orders demoted to RiskActions, got reads=%+v risks=%+v fallbacks=%+v", prof.ReadAPIs, prof.RiskActions, prof.FallbackHistory)
	}

	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].Package != "com.example.shop" {
		t.Fatalf("unexpected profile list: %+v err=%v", list, err)
	}
	if _, err := store.Get("../escape"); err == nil {
		t.Fatal("expected path traversal key to be rejected")
	}
}

func TestCheckFastReadURLAndDiscoverWebOpenAPI(t *testing.T) {
	if _, err := CheckFastReadURL("https://rewards.bing.com/api/points", "https://rewards.bing.com/earn"); err != nil {
		t.Fatalf("expected same-origin clean read URL allowed, got %v", err)
	}
	if _, err := CheckFastReadURL("https://evil.example.com/api/points", "https://rewards.bing.com/earn"); err == nil {
		t.Fatal("expected cross-origin fetch to be blocked")
	}
	if _, err := CheckFastReadURL("https://rewards.bing.com/api/checkin", "https://rewards.bing.com/earn"); err == nil {
		t.Fatal("expected risk path /checkin to be blocked from fast_read")
	}
	if _, err := CheckFastReadURL("http://169.254.169.254/latest/meta-data", ""); err == nil {
		t.Fatal("expected cloud metadata IP to be blocked")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"openapi":"3.0.0","info":{"title":"PT Agent","version":"1.0"},"paths":{"/api/search":{"get":{"summary":"Search torrents"}},"/api/download":{"post":{"summary":"Push download"}}}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	spec, err := DiscoverWebOpenAPI(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("DiscoverWebOpenAPI: %v", err)
	}
	if spec.Kind != "openapi" || spec.Title != "PT Agent" || len(spec.Endpoints) != 2 {
		t.Fatalf("unexpected OpenAPI summary: %+v", spec)
	}
}
