package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ObservedRequest struct {
	Method             string   `json:"method"`
	URL                string   `json:"url"`
	ResourceType       string   `json:"resource_type,omitempty"`
	Status             int      `json:"status,omitempty"`
	MimeType           string   `json:"mime_type,omitempty"`
	RequestHeaderNames []string `json:"request_header_names,omitempty"`
}

var (
	validAndroidPkgRe = regexp.MustCompile(`^[a-zA-Z0-9._]+$`)
	dumpsysFilterRe   = regexp.MustCompile(`^[0-9a-fA-F]+\s+([^\s]+/[^\s]+)\s+filter\s+[0-9a-fA-F]+`)
	quotedValRe       = regexp.MustCompile(`"([^"]+)"`)
	patternMatcherRe  = regexp.MustCompile(`PatternMatcher\{([A-Z_]+):\s*([^}]+)\}`)

	staticExtRe = regexp.MustCompile(`(?i)\.(js|mjs|css|map|png|jpe?g|gif|webp|avif|svg|ico|woff2?|ttf|otf|eot|mp4|webm|mp3|m4a|m3u8|ts|wasm|pdf|zip)$`)
	analyticsRe = regexp.MustCompile(`(?i)(google-analytics|googletagmanager|doubleclick|googlesyndication|clarity\.ms|sentry\.io|umeng\.com|bugly\.qq|events\.data\.microsoft\.com|browser\.events\.data\.msn\.com|bat\.bing\.com|adservice|doubleverify|crashlytics|appsflyer|adjust\.com|mixpanel|amplitude|segment\.io|telemetry|beacon|metrics\.|/log/collect|/track|/pixel|/stat\b)`)

	sensitiveQueryRe = regexp.MustCompile(`(?i)(token|auth|key|secret|cookie|session|ticket|sig|sign|code|pwd|password|passwd|phone|mobile|email|uid|userid|openid|unionid|csrf|nonce)`)
	riskParamRe      = regexp.MustCompile(`(?i)^(sign|sig|signature|h5st|w_rid|_stk|g_tk|x-s|x-s-common|x-t|x-sign|x-mini-wua|x-sgext|x-sap-ri|x-sap-sec|x-zse-96|x-bogus|a_bogus|msToken|captcha|verify|anti_?bot|risk_?token|nonce)$`)
	riskPathRe       = regexp.MustCompile(`(?i)/(signin|sign[_-]?in|checkin|check[_-]?in|water|watering|collect|claim|redeem|reward|submit|order|pay|payment|checkout|purchase|buy|cart/add|login|logout|oauth|captcha|verify|slider|risk|search)\b`)
	noisySchemeRe    = regexp.MustCompile(`(?i)^(stripesdk|jdanalytics|tencent\d+|wx[0-9a-f]{10,}|wb\d+|alipay\d+|qq\d+|fb\d+|line3rdp|copilotpay-paypal|msauth)$`)
)

// DiscoverAndroidEntries inspects an installed Android package's exported DeepLinks,
// custom URL Schemes, and App Links via `dumpsys package <pkg>` (zero hooks, zero root-evasion).
func DiscoverAndroidEntries(ctx context.Context, pkg string, execFn func(context.Context, string, ...string) ([]byte, error)) ([]string, []DeepLinkEntry, error) {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" || !validAndroidPkgRe.MatchString(pkg) {
		return nil, nil, fmt.Errorf("invalid Android package name %q", pkg)
	}
	if execFn == nil {
		execFn = func(c context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(c, name, args...).Output()
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := execFn(dctx, "/system/bin/dumpsys", "package", pkg)
	if err != nil {
		// Fallback to PATH lookup on non-Android hosts/tests.
		out2, err2 := execFn(dctx, "dumpsys", "package", pkg)
		if err2 != nil {
			return nil, nil, fmt.Errorf("dumpsys package %s failed: %w", pkg, err)
		}
		out = out2
	}
	schemes, links := ParseDumpsysPackage(string(out), pkg)
	return schemes, links, nil
}

// ParseDumpsysPackage parses the Activity Resolver Table -> Schemes section from `dumpsys package`.
func ParseDumpsysPackage(raw, pkg string) ([]string, []DeepLinkEntry) {
	lines := strings.Split(raw, "\n")
	inActivityTable := false
	inSchemes := false

	type filterBlock struct {
		component  string
		schemes    []string
		hosts      []string
		paths      []struct{ typ, val string }
		browsable  bool
		isView     bool
		autoVerify bool
	}

	var blocks []filterBlock
	var cur *filterBlock
	flush := func() {
		if cur != nil && len(cur.schemes) > 0 {
			blocks = append(blocks, *cur)
		}
		cur = nil
	}

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}
		// Top-level section transitions (0-indent)
		if !strings.HasPrefix(rawLine, " ") && !strings.HasPrefix(rawLine, "\t") {
			flush()
			if strings.HasPrefix(trimmed, "Activity Resolver Table:") {
				inActivityTable = true
				inSchemes = false
			} else {
				inActivityTable = false
				inSchemes = false
			}
			continue
		}
		if !inActivityTable {
			continue
		}
		// 2-space subsection headers inside Activity Resolver Table
		if strings.HasPrefix(rawLine, "  ") && !strings.HasPrefix(rawLine, "   ") {
			flush()
			inSchemes = trimmed == "Schemes:"
			continue
		}
		if !inSchemes {
			continue
		}
		// New filter entry line (e.g. "a5898e0 com.microsoft.bing/... filter 38d8799")
		if m := dumpsysFilterRe.FindStringSubmatch(trimmed); len(m) == 2 {
			flush()
			cur = &filterBlock{component: m[1]}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "Action:"):
			if strings.Contains(trimmed, "android.intent.action.VIEW") {
				cur.isView = true
			}
		case strings.HasPrefix(trimmed, "Category:"):
			if strings.Contains(trimmed, "android.intent.category.BROWSABLE") {
				cur.browsable = true
			}
		case strings.HasPrefix(trimmed, "Scheme:"):
			if m := quotedValRe.FindStringSubmatch(trimmed); len(m) == 2 {
				cur.schemes = appendUnique(cur.schemes, m[1])
			}
		case strings.HasPrefix(trimmed, "Authority:"):
			if m := quotedValRe.FindStringSubmatch(trimmed); len(m) == 2 {
				cur.hosts = appendUnique(cur.hosts, m[1])
			}
		case strings.HasPrefix(trimmed, "Path:"):
			if pm := patternMatcherRe.FindStringSubmatch(trimmed); len(pm) == 3 {
				cur.paths = append(cur.paths, struct{ typ, val string }{
					typ: strings.TrimSpace(pm[1]),
					val: strings.TrimSpace(pm[2]),
				})
			}
		case strings.HasPrefix(trimmed, "AutoVerify=true"):
			cur.autoVerify = true
		}
	}
	flush()

	schemeSet := make(map[string]bool)
	seenURI := make(map[string]bool)
	var links []DeepLinkEntry

	addLink := func(dl DeepLinkEntry) {
		key := strings.ToLower(dl.URITemplate) + "|" + dl.Component
		if seenURI[key] {
			return
		}
		seenURI[key] = true
		links = append(links, dl)
	}

	for _, b := range blocks {
		if pkg != "" && !strings.HasPrefix(b.component, pkg+"/") {
			continue
		}
		// Prefer VIEW/BROWSABLE filters; keep non-browsable custom schemes if they are app-specific.
		for _, sc := range b.schemes {
			lowSc := strings.ToLower(sc)
			isHTTP := lowSc == "http" || lowSc == "https"
			if !isHTTP && !noisySchemeRe.MatchString(sc) {
				schemeSet[sc] = true
			}
			// Skip generic http:// or https:// catch-all browsers with no Authority.
			if isHTTP && len(b.hosts) == 0 {
				continue
			}
			// Prefer https over duplicate http when both exist in the same filter.
			if lowSc == "http" && containsFold(b.schemes, "https") {
				continue
			}
			if noisySchemeRe.MatchString(sc) {
				continue
			}
			hosts := b.hosts
			if len(hosts) == 0 {
				hosts = []string{""}
			}
			paths := b.paths
			if len(paths) == 0 {
				paths = []struct{ typ, val string }{{}}
			}
			for _, h := range hosts {
				for _, pt := range paths {
					uri := sc + "://" + h
					if pt.val != "" {
						if !strings.HasPrefix(pt.val, "/") && h != "" {
							uri += "/"
						}
						uri += pt.val
						if pt.typ == "PREFIX" && !strings.HasSuffix(uri, "*") {
							uri += "*"
						}
					}
					addLink(DeepLinkEntry{
						Scheme:      sc,
						Host:        h,
						Path:        pt.val,
						PathType:    pt.typ,
						URITemplate: uri,
						Component:   b.component,
						AutoVerify:  b.autoVerify,
						Browsable:   b.browsable || b.isView,
					})
				}
			}
		}
	}

	// Sort: custom schemes and AutoVerify App Links first, then shorter concrete URIs.
	sort.Slice(links, func(i, j int) bool {
		ai, aj := links[i], links[j]
		iCustom := !strings.EqualFold(ai.Scheme, "http") && !strings.EqualFold(ai.Scheme, "https")
		jCustom := !strings.EqualFold(aj.Scheme, "http") && !strings.EqualFold(aj.Scheme, "https")
		if iCustom != jCustom {
			return iCustom
		}
		if ai.AutoVerify != aj.AutoVerify {
			return ai.AutoVerify
		}
		if len(ai.URITemplate) != len(aj.URITemplate) {
			return len(ai.URITemplate) < len(aj.URITemplate)
		}
		return ai.URITemplate < aj.URITemplate
	})
	if len(links) > maxDeepLinksPerProfile {
		links = links[:maxDeepLinksPerProfile]
	}

	schemes := make([]string, 0, len(schemeSet))
	for sc := range schemeSet {
		schemes = append(schemes, sc)
	}
	sort.Strings(schemes)
	return schemes, links
}

// ClassifyObservedNetwork filters static assets & telemetry, sanitizes all secrets/tokens,
// and partitions observed XHR/Fetch JSON endpoints into `fast_read` vs `ui_required` (risk-gated).
func ClassifyObservedNetwork(records []ObservedRequest) (readAPIs []DiscoveredEndpoint, riskActions []DiscoveredEndpoint, skipped int) {
	nowStr := time.Now().Format(time.RFC3339)
	readMap := make(map[string]DiscoveredEndpoint)
	riskMap := make(map[string]DiscoveredEndpoint)

	for _, rec := range records {
		u, err := url.Parse(strings.TrimSpace(rec.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			skipped++
			continue
		}
		if staticExtRe.MatchString(u.Path) || analyticsRe.MatchString(rec.URL) {
			skipped++
			continue
		}
		rt := strings.ToLower(strings.TrimSpace(rec.ResourceType))
		mime := strings.ToLower(strings.TrimSpace(rec.MimeType))
		isAPIResource := rt == "xhr" || rt == "fetch" || strings.Contains(mime, "json") || strings.Contains(mime, "graphql")
		if !isAPIResource {
			skipped++
			continue
		}

		method := strings.ToUpper(strings.TrimSpace(rec.Method))
		if method == "" {
			method = "GET"
		}
		qKeys, sanitizedURL, paramRisks := sanitizeURLAndDetectRisk(u)
		var signals []string
		signals = append(signals, paramRisks...)
		for _, hdr := range rec.RequestHeaderNames {
			h := strings.ToLower(strings.TrimSpace(hdr))
			if riskParamRe.MatchString(h) {
				signals = appendUnique(signals, "header:"+h)
			}
		}
		if m := riskPathRe.FindStringSubmatch(u.Path); len(m) >= 2 {
			signals = appendUnique(signals, "path:"+strings.ToLower(m[1]))
		}

		ep := DiscoveredEndpoint{
			Method:      method,
			Host:        strings.ToLower(u.Host),
			Path:        u.Path,
			URLTemplate: sanitizedURL,
			QueryKeys:   qKeys,
			Status:      rec.Status,
			MimeType:    mime,
			RiskSignals: signals,
			ObservedAt:  nowStr,
		}
		key := method + "|" + ep.Host + "|" + ep.Path

		switch {
		case method != "GET":
			ep.Mode = "ui_required"
			ep.Reason = fmt.Sprintf("%s write/mutation endpoint -> keep on UI/CDP mode", method)
			riskMap[key] = ep
		case rec.Status == 401 || rec.Status == 403 || rec.Status == 429:
			ep.Mode = "ui_required"
			ep.Reason = fmt.Sprintf("HTTP %d auth/risk gate -> keep on UI/CDP mode", rec.Status)
			riskMap[key] = ep
		case len(signals) > 0:
			ep.Mode = "ui_required"
			ep.Reason = "signature/risk-gated endpoint (" + strings.Join(signals, ", ") + ") -> keep on UI/CDP mode"
			riskMap[key] = ep
		case rec.Status >= 200 && rec.Status < 300:
			ep.Mode = "fast_read"
			ep.Reason = "clean same-origin GET JSON read endpoint"
			readMap[key] = ep
		default:
			skipped++
		}
	}

	readAPIs = mapValuesSorted(readMap)
	riskActions = mapValuesSorted(riskMap)
	return readAPIs, riskActions, skipped
}

func sanitizeURLAndDetectRisk(u *url.URL) (keys []string, templateURL string, riskSignals []string) {
	q := u.Query()
	keys = make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		lowK := strings.ToLower(strings.TrimSpace(k))
		if riskParamRe.MatchString(lowK) {
			riskSignals = appendUnique(riskSignals, "param:"+lowK)
		}
		if sensitiveQueryRe.MatchString(lowK) {
			parts = append(parts, url.QueryEscape(k)+"={redacted}")
		} else {
			parts = append(parts, url.QueryEscape(k)+"={"+k+"}")
		}
	}
	base := fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path)
	if len(parts) > 0 {
		base += "?" + strings.Join(parts, "&")
	}
	return keys, base, riskSignals
}

func mapValuesSorted(m map[string]DiscoveredEndpoint) []DiscoveredEndpoint {
	out := make([]DiscoveredEndpoint, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// CheckFastReadURL validates that a URL is safe for strictly read-only same-origin in-page GET fetch.
func CheckFastReadURL(rawURL, pageURL string) (string, error) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return "", errors.New("cdp_fast_read requires a valid http(s) URL")
	}
	if isBlockedMetadataHost(target.Hostname()) {
		return "", errors.New("blocked metadata host")
	}
	if m := riskPathRe.FindStringSubmatch(target.Path); len(m) >= 2 {
		return "", fmt.Errorf("path %q looks like a write/risk-gated action (%s); use normal CDP/UI mode instead", target.Path, m[1])
	}
	for k := range target.Query() {
		if riskParamRe.MatchString(strings.ToLower(strings.TrimSpace(k))) {
			return "", fmt.Errorf("query parameter %q looks signed/risk-gated; use normal CDP/UI mode instead", k)
		}
	}
	if strings.TrimSpace(pageURL) != "" {
		if pu, err := url.Parse(strings.TrimSpace(pageURL)); err == nil && pu.Host != "" {
			if !strings.EqualFold(pu.Hostname(), target.Hostname()) && !sameSiteSuffix(pu.Hostname(), target.Hostname()) {
				return "", fmt.Errorf("cross-origin fetch blocked (page=%s, target=%s); navigate or use UI/CDP mode", pu.Host, target.Host)
			}
		}
	}
	return target.String(), nil
}

func sameSiteSuffix(a, b string) bool {
	pa := strings.Split(strings.ToLower(strings.TrimSpace(a)), ".")
	pb := strings.Split(strings.ToLower(strings.TrimSpace(b)), ".")
	if len(pa) < 2 || len(pb) < 2 {
		return false
	}
	return pa[len(pa)-1] == pb[len(pb)-1] && pa[len(pa)-2] == pb[len(pb)-2]
}

func isBlockedMetadataHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "metadata.google.internal" || h == "metadata" || h == "169.254.169.254" || h == "169.254.170.2" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLinkLocalUnicast() {
		return true
	}
	return false
}

// DiscoverWebOpenAPI probes standard read-only OpenAPI / Swagger / MCP discovery endpoints on baseURL.
func DiscoverWebOpenAPI(ctx context.Context, rawBaseURL string, client *http.Client) (*OpenAPISummary, error) {
	u, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid base_url %q", rawBaseURL)
	}
	if isBlockedMetadataHost(u.Hostname()) {
		return nil, errors.New("blocked link-local/metadata host")
	}
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second}
	}
	origin := u.Scheme + "://" + u.Host
	var candidates []string
	if strings.HasSuffix(strings.ToLower(u.Path), ".json") || strings.Contains(strings.ToLower(u.Path), "openapi") || strings.Contains(strings.ToLower(u.Path), "swagger") {
		candidates = append(candidates, u.String())
	} else {
		base := strings.TrimRight(u.String(), "/")
		candidates = append(candidates,
			base+"/openapi.json",
			origin+"/openapi.json",
			origin+"/.well-known/mcp.json",
			origin+"/swagger.json",
			origin+"/v3/api-docs",
		)
	}

	seen := make(map[string]bool)
	for _, cand := range candidates {
		if seen[cand] {
			continue
		}
		seen[cand] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cand, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		resp.Body.Close()
		if rerr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || len(body) == 0 {
			continue
		}
		if summary, ok := parseDiscoveryJSON(cand, body); ok {
			return summary, nil
		}
	}
	return nil, fmt.Errorf("no OpenAPI/Swagger/MCP discovery document found at %s", rawBaseURL)
}

func parseDiscoveryJSON(specURL string, body []byte) (*OpenAPISummary, bool) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	// OpenAPI 3.x or Swagger 2.0
	if paths, ok := raw["paths"].(map[string]any); ok && len(paths) > 0 {
		out := &OpenAPISummary{SpecURL: specURL, Kind: "openapi"}
		if info, ok := raw["info"].(map[string]any); ok {
			out.Title, _ = info["title"].(string)
			out.Version, _ = info["version"].(string)
		}
		pathKeys := make([]string, 0, len(paths))
		for p := range paths {
			pathKeys = append(pathKeys, p)
		}
		sort.Strings(pathKeys)
		for _, p := range pathKeys {
			methods, ok := paths[p].(map[string]any)
			if !ok {
				continue
			}
			mKeys := make([]string, 0, len(methods))
			for m := range methods {
				up := strings.ToUpper(m)
				if up == "GET" || up == "POST" || up == "PUT" || up == "DELETE" || up == "PATCH" {
					mKeys = append(mKeys, m)
				}
			}
			sort.Strings(mKeys)
			for _, m := range mKeys {
				desc := ""
				if op, ok := methods[m].(map[string]any); ok {
					if s, ok := op["summary"].(string); ok && strings.TrimSpace(s) != "" {
						desc = strings.TrimSpace(s)
					} else if s, ok := op["operationId"].(string); ok && strings.TrimSpace(s) != "" {
						desc = strings.TrimSpace(s)
					}
				}
				line := fmt.Sprintf("%s %s", strings.ToUpper(m), p)
				if desc != "" {
					line += " - " + desc
				}
				out.Endpoints = append(out.Endpoints, line)
				if len(out.Endpoints) >= 40 {
					return out, true
				}
			}
		}
		return out, len(out.Endpoints) > 0
	}
	// MCP /.well-known/mcp.json manifest
	if tools, ok := raw["tools"].([]any); ok && len(tools) > 0 {
		out := &OpenAPISummary{SpecURL: specURL, Kind: "mcp"}
		out.Title, _ = raw["name"].(string)
		out.Version, _ = raw["version"].(string)
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tm["name"].(string)
			desc, _ := tm["description"].(string)
			if name == "" {
				continue
			}
			line := name
			if desc != "" {
				line += " - " + desc
			}
			out.Endpoints = append(out.Endpoints, line)
			if len(out.Endpoints) >= 40 {
				break
			}
		}
		return out, len(out.Endpoints) > 0
	}
	return nil, false
}

func appendUnique(list []string, item string) []string {
	item = strings.TrimSpace(item)
	if item == "" {
		return list
	}
	for _, existing := range list {
		if existing == item {
			return list
		}
	}
	return append(list, item)
}

func containsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}
