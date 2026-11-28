package discover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxDeepLinksPerProfile    = 60
	maxEndpointsPerBucket     = 40
	maxOpenAPISpecsPerProfile = 8
	maxFallbacksPerProfile    = 20
)

var validProfileKeyRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,120}$`)

type DeepLinkEntry struct {
	Scheme      string `json:"scheme"`
	Host        string `json:"host,omitempty"`
	Path        string `json:"path,omitempty"`
	PathType    string `json:"path_type,omitempty"`
	URITemplate string `json:"uri_template"`
	Component   string `json:"component,omitempty"`
	AutoVerify  bool   `json:"auto_verify,omitempty"`
	Browsable   bool   `json:"browsable,omitempty"`
}

type DiscoveredEndpoint struct {
	Method      string   `json:"method"`
	Host        string   `json:"host"`
	Path        string   `json:"path"`
	URLTemplate string   `json:"url_template"`
	QueryKeys   []string `json:"query_keys,omitempty"`
	Status      int      `json:"status,omitempty"`
	MimeType    string   `json:"mime_type,omitempty"`
	Mode        string   `json:"mode"` // "fast_read" | "ui_required"
	Reason      string   `json:"reason,omitempty"`
	RiskSignals []string `json:"risk_signals,omitempty"`
	ObservedAt  string   `json:"observed_at,omitempty"`
}

type OpenAPISummary struct {
	SpecURL   string   `json:"spec_url"`
	Kind      string   `json:"kind"` // "openapi" | "mcp" | "webmcp"
	Title     string   `json:"title,omitempty"`
	Version   string   `json:"version,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

type FallbackNote struct {
	Target     string `json:"target"`
	Reason     string `json:"reason"`
	Status     int    `json:"status,omitempty"`
	RecordedAt string `json:"recorded_at"`
}

type AppProfile struct {
	Package         string               `json:"package"`
	UpdatedAt       string               `json:"updated_at"`
	Schemes         []string             `json:"schemes,omitempty"`
	DeepLinks       []DeepLinkEntry      `json:"deeplinks,omitempty"`
	ReadAPIs        []DiscoveredEndpoint `json:"read_apis,omitempty"`
	RiskActions     []DiscoveredEndpoint `json:"risk_actions,omitempty"`
	OpenAPISpecs    []OpenAPISummary     `json:"openapi_specs,omitempty"`
	FallbackHistory []FallbackNote       `json:"fallback_history,omitempty"`
}

type ProfileSummary struct {
	Package        string `json:"package"`
	UpdatedAt      string `json:"updated_at"`
	SchemesCount   int    `json:"schemes_count"`
	DeepLinksCount int    `json:"deeplinks_count"`
	ReadAPIsCount  int    `json:"read_apis_count"`
	RiskCount      int    `json:"risk_actions_count"`
	SpecsCount     int    `json:"openapi_specs_count"`
	FallbacksCount int    `json:"fallbacks_count"`
}

// ProfileStore persists auto-discovered App capability profiles as 0600 JSON files.
type ProfileStore struct {
	mu  sync.RWMutex
	dir string
}

func NewProfileStore(dir string) *ProfileStore {
	return &ProfileStore{dir: strings.TrimSpace(dir)}
}

func ValidateProfileKey(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" || !validProfileKeyRe.MatchString(k) || strings.Contains(k, "..") {
		return "", fmt.Errorf("invalid profile package/key %q", key)
	}
	return k, nil
}

func (s *ProfileStore) filePath(key string) (string, error) {
	if s == nil || s.dir == "" {
		return "", errors.New("profile store directory is not configured")
	}
	k, err := ValidateProfileKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, k+".json"), nil
}

func (s *ProfileStore) Get(pkg string) (*AppProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(pkg)
}

func (s *ProfileStore) getLocked(pkg string) (*AppProfile, error) {
	path, err := s.filePath(pkg)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p AppProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", pkg, err)
	}
	return &p, nil
}

func (s *ProfileStore) Save(p *AppProfile) error {
	if p == nil {
		return errors.New("nil profile")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(p)
}

func (s *ProfileStore) saveLocked(p *AppProfile) error {
	path, err := s.filePath(p.Package)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	p.UpdatedAt = time.Now().Format(time.RFC3339)
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *ProfileStore) Delete(pkg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.filePath(pkg)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *ProfileStore) List() ([]ProfileSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s == nil || s.dir == "" {
		return []ProfileSummary{}, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ProfileSummary{}, nil
		}
		return nil, err
	}
	out := make([]ProfileSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var p AppProfile
		if err := json.Unmarshal(raw, &p); err != nil || p.Package == "" {
			continue
		}
		out = append(out, ProfileSummary{
			Package:        p.Package,
			UpdatedAt:      p.UpdatedAt,
			SchemesCount:   len(p.Schemes),
			DeepLinksCount: len(p.DeepLinks),
			ReadAPIsCount:  len(p.ReadAPIs),
			RiskCount:      len(p.RiskActions),
			SpecsCount:     len(p.OpenAPISpecs),
			FallbacksCount: len(p.FallbackHistory),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].Package < out[j].Package
	})
	return out, nil
}

func (s *ProfileStore) loadOrInitLocked(pkg string) (*AppProfile, error) {
	k, err := ValidateProfileKey(pkg)
	if err != nil {
		return nil, err
	}
	p, err := s.getLocked(k)
	if err != nil {
		return nil, err
	}
	if p == nil {
		p = &AppProfile{Package: k}
	}
	return p, nil
}

func (s *ProfileStore) MergeDeepLinks(pkg string, schemes []string, links []DeepLinkEntry) (*AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadOrInitLocked(pkg)
	if err != nil {
		return nil, err
	}
	schemeSet := make(map[string]bool)
	for _, sc := range append(p.Schemes, schemes...) {
		sc = strings.TrimSpace(sc)
		if sc != "" {
			schemeSet[sc] = true
		}
	}
	p.Schemes = make([]string, 0, len(schemeSet))
	for sc := range schemeSet {
		p.Schemes = append(p.Schemes, sc)
	}
	sort.Strings(p.Schemes)

	seen := make(map[string]bool)
	merged := make([]DeepLinkEntry, 0, len(p.DeepLinks)+len(links))
	for _, dl := range append(links, p.DeepLinks...) {
		key := strings.ToLower(dl.URITemplate) + "|" + dl.Component
		if dl.URITemplate == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, dl)
		if len(merged) >= maxDeepLinksPerProfile {
			break
		}
	}
	p.DeepLinks = merged
	if err := s.saveLocked(p); err != nil {
		return nil, err
	}
	return p, nil
}

func mergeEndpointSlices(existing, incoming []DiscoveredEndpoint, limit int) []DiscoveredEndpoint {
	byKey := make(map[string]DiscoveredEndpoint, len(existing)+len(incoming))
	order := make([]string, 0, len(existing)+len(incoming))
	add := func(ep DiscoveredEndpoint) {
		k := strings.ToUpper(ep.Method) + "|" + strings.ToLower(ep.Host) + "|" + ep.Path
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = ep
	}
	for _, ep := range existing {
		add(ep)
	}
	for _, ep := range incoming {
		add(ep)
	}
	out := make([]DiscoveredEndpoint, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (s *ProfileStore) MergeEndpoints(pkg string, readAPIs, riskActions []DiscoveredEndpoint) (*AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadOrInitLocked(pkg)
	if err != nil {
		return nil, err
	}
	// If an endpoint moved into riskActions, remove it from ReadAPIs so agent never retries a risk-gated path.
	riskKeys := make(map[string]bool, len(riskActions))
	for _, ep := range riskActions {
		riskKeys[strings.ToLower(ep.Host)+"|"+ep.Path] = true
	}
	if len(riskKeys) > 0 && len(p.ReadAPIs) > 0 {
		filtered := p.ReadAPIs[:0]
		for _, ep := range p.ReadAPIs {
			if !riskKeys[strings.ToLower(ep.Host)+"|"+ep.Path] {
				filtered = append(filtered, ep)
			}
		}
		p.ReadAPIs = filtered
	}
	p.ReadAPIs = mergeEndpointSlices(p.ReadAPIs, readAPIs, maxEndpointsPerBucket)
	p.RiskActions = mergeEndpointSlices(p.RiskActions, riskActions, maxEndpointsPerBucket)
	if err := s.saveLocked(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *ProfileStore) MergeOpenAPI(pkg string, spec OpenAPISummary) (*AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadOrInitLocked(pkg)
	if err != nil {
		return nil, err
	}
	spec.UpdatedAt = time.Now().Format(time.RFC3339)
	replaced := false
	for i := range p.OpenAPISpecs {
		if strings.EqualFold(p.OpenAPISpecs[i].SpecURL, spec.SpecURL) && p.OpenAPISpecs[i].Kind == spec.Kind {
			p.OpenAPISpecs[i] = spec
			replaced = true
			break
		}
	}
	if !replaced {
		p.OpenAPISpecs = append(p.OpenAPISpecs, spec)
		if len(p.OpenAPISpecs) > maxOpenAPISpecsPerProfile {
			p.OpenAPISpecs = p.OpenAPISpecs[len(p.OpenAPISpecs)-maxOpenAPISpecsPerProfile:]
		}
	}
	if err := s.saveLocked(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *ProfileStore) RecordFallback(pkg, target, reason string, status int) (*AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadOrInitLocked(pkg)
	if err != nil {
		return nil, err
	}
	note := FallbackNote{
		Target:     strings.TrimSpace(target),
		Reason:     strings.TrimSpace(reason),
		Status:     status,
		RecordedAt: time.Now().Format(time.RFC3339),
	}
	p.FallbackHistory = append(p.FallbackHistory, note)
	if len(p.FallbackHistory) > maxFallbacksPerProfile {
		p.FallbackHistory = p.FallbackHistory[len(p.FallbackHistory)-maxFallbacksPerProfile:]
	}
	// Also demote matching endpoint from ReadAPIs -> RiskActions so future runs go straight to UI/CDP.
	if target != "" {
		var demoted []DiscoveredEndpoint
		kept := make([]DiscoveredEndpoint, 0, len(p.ReadAPIs))
		for _, ep := range p.ReadAPIs {
			if strings.Contains(target, ep.Path) && (ep.Host == "" || strings.Contains(strings.ToLower(target), strings.ToLower(ep.Host))) {
				ep.Mode = "ui_required"
				ep.Reason = "runtime fallback: " + note.Reason
				ep.Status = status
				ep.ObservedAt = note.RecordedAt
				demoted = append(demoted, ep)
				continue
			}
			kept = append(kept, ep)
		}
		if len(demoted) > 0 {
			p.ReadAPIs = kept
			p.RiskActions = mergeEndpointSlices(p.RiskActions, demoted, maxEndpointsPerBucket)
		}
	}
	if err := s.saveLocked(p); err != nil {
		return nil, err
	}
	return p, nil
}
