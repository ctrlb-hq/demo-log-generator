package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"

	"github.com/google/uuid"
)

// --- WAF log generation ---

type WAFTemplate struct {
	Category string
	Data     map[string]interface{}
}

type WAFSelector struct {
	categories   []string
	templates    map[string][]WAFTemplate
	weights      []float64
	totalWeight  float64
	cumulWeights []float64
	quotaCredits []float64
	quotaMu      sync.Mutex
	selection    string
}

func setupWAFGenerator(logFile, weightConfigPath, weightSelection string) BatchGenerator {
	if weightConfigPath == "" {
		fmt.Println("WAF mode requires -weightConfig")
		return nil
	}
	wafTemplates, err := readWAFTemplatesFromFile(logFile)
	if err != nil {
		fmt.Printf("Error reading WAF templates: %v\n", err)
		return nil
	}
	if len(wafTemplates) == 0 {
		fmt.Println("No WAF templates found in file")
		return nil
	}
	config, err := loadWeightConfig(weightConfigPath)
	if err != nil {
		fmt.Printf("Error loading weight config: %v\n", err)
		return nil
	}
	wafSel := NewWAFSelector(wafTemplates, config, weightSelection)
	if wafSel == nil {
		return nil
	}
	fmt.Printf("Loaded %d WAF templates with weighted selection (%s)\n", len(wafTemplates), weightSelection)
	return &WAFBatchGenerator{selector: wafSel}
}

var wafCountries = []struct {
	code   string
	region string
	weight float64
}{
	{"IN", "KA", 50}, {"IN", "MH", 20}, {"IN", "DL", 10},
	{"US", "VA", 8}, {"SG", "SG", 5}, {"AE", "DU", 4}, {"GB", "LND", 3},
}

var wafUserAgents = []string{
	"okhttp/4.12.0", "okhttp/4.11.0",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
	"Mozilla/5.0 (Linux; Android 14; SM-S921B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
}

var wafAppVersions = []string{"26.4.7", "26.4.6", "26.3.5", "26.3.4", "26.2.1"}

func readWAFTemplatesFromFile(path string) ([]WAFTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading WAF template file: %w", err)
	}

	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("error parsing WAF template JSON: %w", err)
	}

	templates := make([]WAFTemplate, 0, len(raw))
	for _, obj := range raw {
		cat, _ := obj["_category"].(string)
		if cat == "" {
			cat = "default"
		}
		delete(obj, "_category")
		templates = append(templates, WAFTemplate{Category: cat, Data: obj})
	}
	return templates, nil
}

func NewWAFSelector(templates []WAFTemplate, config *WeightConfig, selection string) *WAFSelector {
	grouped := make(map[string][]WAFTemplate)
	for _, t := range templates {
		grouped[t.Category] = append(grouped[t.Category], t)
	}

	var catNames []string
	for cat, weight := range config.Categories {
		if weight <= 0 {
			continue
		}
		if len(grouped[cat]) == 0 {
			fmt.Printf("  [warn] category '%s' (weight %.1f) has no matching WAF templates, skipping\n", cat, weight)
			continue
		}
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)

	if len(catNames) == 0 {
		fmt.Println("  [error] no WAF categories with templates and positive weight")
		return nil
	}

	weights := make([]float64, len(catNames))
	var totalWeight float64
	for i, cat := range catNames {
		weights[i] = config.Categories[cat]
		totalWeight += weights[i]
	}

	cumul := make([]float64, len(catNames))
	var run float64
	for i, w := range weights {
		run += w
		cumul[i] = run
	}

	for cat, entries := range grouped {
		if _, ok := config.Categories[cat]; !ok {
			fmt.Printf("  [warn] %d WAF templates with category '%s' have no weight in config\n", len(entries), cat)
		}
	}

	mode := strings.ToLower(strings.TrimSpace(selection))
	if mode != "quota" && mode != "random" {
		mode = "quota"
	}

	fmt.Printf("\nWAF log selector initialized (%d categories, %.1f total weight, mode=%s):\n", len(catNames), totalWeight, mode)
	for i, cat := range catNames {
		pct := (weights[i] / totalWeight) * 100
		fmt.Printf("  %-28s %5.1f%% (%d templates)\n", cat, pct, len(grouped[cat]))
	}
	fmt.Println()

	return &WAFSelector{
		categories:   catNames,
		templates:    grouped,
		weights:      weights,
		totalWeight:  totalWeight,
		cumulWeights: cumul,
		quotaCredits: make([]float64, len(catNames)),
		selection:    mode,
	}
}

func (s *WAFSelector) Select() WAFTemplate {
	if len(s.categories) == 0 {
		return WAFTemplate{}
	}
	if s.selection == "quota" && s.totalWeight > 0 {
		s.quotaMu.Lock()
		for i := range s.categories {
			s.quotaCredits[i] += s.weights[i]
		}
		best := 0
		for i := 1; i < len(s.categories); i++ {
			if s.quotaCredits[i] > s.quotaCredits[best] {
				best = i
			}
		}
		s.quotaCredits[best] -= s.totalWeight
		cat := s.categories[best]
		s.quotaMu.Unlock()
		entries := s.templates[cat]
		return entries[rand.Intn(len(entries))]
	}

	r := rand.Float64() * s.totalWeight
	lo, hi := 0, len(s.cumulWeights)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if s.cumulWeights[mid] < r {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	cat := s.categories[lo]
	entries := s.templates[cat]
	return entries[rand.Intn(len(entries))]
}

func deepCopyMap(src map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(src)
	var dst map[string]interface{}
	json.Unmarshal(data, &dst)
	return dst
}

func randomHex(n int) string {
	b := make([]byte, n)
	crand.Read(b)
	return hex.EncodeToString(b)
}

func pickWeightedCountry() (code, region string) {
	var total float64
	for _, c := range wafCountries {
		total += c.weight
	}
	r := rand.Float64() * total
	var cum float64
	for _, c := range wafCountries {
		cum += c.weight
		if r <= cum {
			return c.code, c.region
		}
	}
	last := wafCountries[len(wafCountries)-1]
	return last.code, last.region
}

// generateAWSRequestId returns a base64-style id resembling AWS CloudFront request IDs
// (e.g. "PJX1dqcttWETHgAm2SCxHeAPwywtTd7BSO7_kUeUodGRac__7PNzUA=="): 40 random bytes
// encoded with URL-safe base64, producing a 56-char string ending in "==".
func generateAWSRequestId() string {
	b := make([]byte, 40)
	crand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

func randomizeWAFEntry(entry map[string]interface{}, logTime time.Time, ip, country, region string) {
	// Top-level millisecond timestamp + date partitions.
	entry["timestamp"] = logTime.UnixMilli()
	entry["year"] = logTime.Year()
	entry["month"] = int(logTime.Month())
	entry["day"] = logTime.Day()
	entry["hour"] = logTime.Hour()

	// Per-request randomized identifiers (re-used across mirrored header pairs so
	// camelCase and snake_case stay consistent within one record).
	sessionID := strings.ReplaceAll(uuid.New().String(), "-", "")
	deviceUID := randomHex(8) // 16 hex chars, like the sample's "f886ae0599b64f19"
	reqHeaderID := strings.ReplaceAll(uuid.New().String(), "-", "")
	storeID := uuid.New().String()
	storeEta := rand.Intn(13) + 3
	appVer := wafAppVersions[rand.Intn(len(wafAppVersions))]

	if req, ok := entry["httprequest"].(map[string]interface{}); ok {
		req["clientip"] = ip
		req["country"] = country
		req["requestid"] = generateAWSRequestId()

		if headers, ok := req["headers"].([]interface{}); ok {
			for _, h := range headers {
				hdr, ok := h.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := hdr["name"].(string)
				switch name {
				case "sessionid", "session_id":
					hdr["value"] = sessionID
				case "deviceuid", "device_uid":
					hdr["value"] = deviceUID
				case "requestid", "request_id":
					hdr["value"] = reqHeaderID
				case "storeid", "store_id", "store_ids":
					hdr["value"] = storeID
				case "store_etas":
					hdr["value"] = fmt.Sprintf("{\"%s\": %d}", storeID, storeEta)
				case "user-agent":
					hdr["value"] = wafUserAgents[rand.Intn(len(wafUserAgents))]
				case "app_version", "appversion":
					hdr["value"] = appVer
				case "user_gppo":
					hdr["value"] = strconv.Itoa(rand.Intn(5000))
				case "authorization":
					hdr["value"] = "Bearer eyJ" + randomHex(32) + "." + randomHex(48) + "." + randomHex(24)
				case "sentry-trace":
					hdr["value"] = randomHex(16) + "-" + randomHex(8) + "-1"
					// Booleans / blanks in the sample are preserved as-is from the template
					// (isinternaluser=false, is_new_font=true, bundle_version="").
				}
			}
		}
	}

	entry["ja3fingerprint"] = randomHex(16)

	// Sample label format is "awswaf:clientip:geo:region:IN-WB" + country-level label.
	entry["labels"] = []interface{}{
		map[string]interface{}{"name": "awswaf:clientip:geo:region:" + country + "-" + region},
		map[string]interface{}{"name": "awswaf:clientip:geo:country:" + country},
	}
}

type WAFBatchGenerator struct {
	selector *WAFSelector
}

func (g *WAFBatchGenerator) Generate(size int) ([]byte, error) {
	batch := make([]map[string]interface{}, size)
	baseTime := time.Now()
	for i := 0; i < size; i++ {
		tmpl := g.selector.Select()
		entry := deepCopyMap(tmpl.Data)

		logTime := baseTime.Add(time.Duration(i) * time.Millisecond)
		ip := templatePool.IPAddresses[rand.Intn(len(templatePool.IPAddresses))]
		country, region := pickWeightedCountry()

		randomizeWAFEntry(entry, logTime, ip, country, region)
		batch[i] = entry
	}
	return marshalBatch(batch)
}