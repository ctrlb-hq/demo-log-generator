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

	"github.com/brianvoe/gofakeit"
	"github.com/google/uuid"
)

// --- Domain types ---

type LogEntry struct {
	Timestamp int64  `json:"_timestamp,omitempty"`
	Log       string `json:"log,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

type TemplateEntry struct {
	Log      string `json:"log"`
	Filename string `json:"filename"`
	Category string `json:"category,omitempty"`
}

type WeightConfig struct {
	Categories map[string]float64 `json:"categories"`
}

type WeightedLogSelector struct {
	categories   []string
	templates    map[string][]TemplateEntry
	weights      []float64
	totalWeight  float64
	cumulWeights []float64
	quotaCredits []float64
	quotaMu      sync.Mutex
	selection    string
}

type BatchGenerator interface {
	Generate(size int) ([]byte, error)
}

// --- Pre-generated template pool ---

// Pre-generated realistic values (initialized once)
type FastTemplatePool struct {
	JobIDs       []string
	ExecutionIDs []string
	ThreadIDs    []string
	ProcessIDs   []int
	PodSuffixes  []string
	IPAddresses  []string
	ResourceURIs []string
}

var templatePool *FastTemplatePool

var beaconAdj = []string{
	"objective", "determined", "focused", "stoic", "admiring", "practical", "serene", "eager",
	"bold", "clever", "brave", "lucid", "inventive", "polished", "resolute", "spirited",
}
var beaconSurname = []string{
	"wilbur", "turing", "curie", "lovelace", "babbage", "hopper", "einstein", "newton",
	"feynman", "noether", "bohr", "tesla", "franklin", "pasteur", "maxwell", "darwin",
}

var templateIPs = []string{
	"20.197.75.253", "10.42.3.17", "203.0.113.55", "172.16.0.42", "198.51.100.23",
}

func initTemplatePool() {
	templatePool = &FastTemplatePool{
		JobIDs:       make([]string, 10000),
		ExecutionIDs: make([]string, 5000),
		ThreadIDs:    make([]string, 300),
		ProcessIDs:   make([]int, 100),
		PodSuffixes:  make([]string, 50),
		IPAddresses:  make([]string, 20000),
		ResourceURIs: make([]string, 2000),
	}

	// Pre-generate all values once
	for i := range templatePool.JobIDs {
		templatePool.JobIDs[i] = generateHex(24)
	}
	for i := range templatePool.ExecutionIDs {
		templatePool.ExecutionIDs[i] = generateHex(8)
	}
	for i := range templatePool.ThreadIDs {
		templatePool.ThreadIDs[i] = generateThreadID()
	}
	for i := range templatePool.ProcessIDs {
		templatePool.ProcessIDs[i] = rand.Intn(5000) + 1
	}
	for i := range templatePool.PodSuffixes {
		templatePool.PodSuffixes[i] = generateHex(10) + "-" + generateHex(5)
	}
	for i := range templatePool.IPAddresses {
		templatePool.IPAddresses[i] = fmt.Sprintf("%d.%d.%d.%d",
			rand.Intn(255)+1, rand.Intn(255), rand.Intn(255), rand.Intn(255))
	}
	for i := range templatePool.ResourceURIs {
		templatePool.ResourceURIs[i] = generateRealisticURI()
	}
}

func generateRealisticURI() string {
	endpoints := []string{
		"/api/v1/users", "/api/v1/orders", "/api/v1/products", "/api/v1/tracking",
		"/api/v1/shipments", "/api/v1/customers", "/api/v2/orders", "/api/v2/tracking",
		"/dashboard", "/login", "/admin", "/reports", "/health", "/metrics",
		"/assets/js", "/assets/css", "/static/img",
	}

	base := endpoints[rand.Intn(len(endpoints))]

	// Add some variation
	switch rand.Intn(4) {
	case 0:
		return base
	case 1:
		return base + "/" + strconv.Itoa(rand.Intn(10000))
	case 2:
		return base + "?id=" + strconv.Itoa(rand.Intn(1000))
	default:
		return base + "/" + gofakeit.Word()
	}
}

func generateHex(length int) string {
	const chars = "0123456789abcdef"
	result := make([]byte, length)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

func generateThreadID() string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	length := rand.Intn(4) + 3
	result := make([]byte, length)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

func randDockerishName() string {
	return beaconAdj[rand.Intn(len(beaconAdj))] + "_" + beaconSurname[rand.Intn(len(beaconSurname))]
}

// --- File loading and weight config ---

func readLogsFromFile(filePath string) ([]LogEntry, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	var logs []LogEntry
	err = json.Unmarshal(data, &logs)
	if err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}

	return logs, nil
}

func readTemplatesFromFile(filePath string) ([]TemplateEntry, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	var templates []TemplateEntry
	if err := json.Unmarshal(data, &templates); err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}

	return templates, nil
}

func loadWeightConfig(path string) (*WeightConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading weight config: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("error parsing weight config: %w", err)
	}

	config := &WeightConfig{Categories: make(map[string]float64)}
	if catData, ok := raw["categories"]; ok {
		if err := json.Unmarshal(catData, &config.Categories); err != nil {
			return nil, fmt.Errorf("error parsing categories: %w", err)
		}
	}

	return config, nil
}

// --- Selectors ---

func NewWeightedLogSelector(templates []TemplateEntry, config *WeightConfig, selection string) *WeightedLogSelector {
	grouped := make(map[string][]TemplateEntry)
	for _, t := range templates {
		cat := t.Category
		if cat == "" {
			cat = "default"
		}
		grouped[cat] = append(grouped[cat], t)
	}

	var catNames []string
	for cat, weight := range config.Categories {
		if weight <= 0 {
			continue
		}
		if len(grouped[cat]) == 0 {
			fmt.Printf("  [warn] category '%s' (weight %.1f) has no matching templates, skipping\n", cat, weight)
			continue
		}
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)

	if len(catNames) == 0 {
		fmt.Println("  [error] no categories with templates and positive weight")
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
			fmt.Printf("  [warn] %d templates with category '%s' have no weight in config, will not be selected\n", len(entries), cat)
		}
	}

	mode := strings.ToLower(strings.TrimSpace(selection))
	if mode != "quota" && mode != "random" {
		fmt.Printf("  [warn] unknown weightSelection %q, using quota\n", selection)
		mode = "quota"
	}

	fmt.Printf("\nWeighted log selector initialized (%d categories, %.1f total weight, mode=%s):\n", len(catNames), totalWeight, mode)
	for i, cat := range catNames {
		pct := (weights[i] / totalWeight) * 100
		fmt.Printf("  %-28s %5.1f%% (%d templates)\n", cat, pct, len(grouped[cat]))
	}
	fmt.Println()

	quotaCredits := make([]float64, len(catNames))
	return &WeightedLogSelector{
		categories:   catNames,
		templates:    grouped,
		weights:      weights,
		totalWeight:  totalWeight,
		cumulWeights: cumul,
		quotaCredits: quotaCredits,
		selection:    mode,
	}
}

func NewUniformSelector(logs []LogEntry) *WeightedLogSelector {
	all := make([]TemplateEntry, len(logs))
	for i, l := range logs {
		all[i] = TemplateEntry{Log: l.Log, Filename: l.Filename, Category: "default"}
	}
	return &WeightedLogSelector{
		categories:   []string{"default"},
		templates:    map[string][]TemplateEntry{"default": all},
		weights:      []float64{1.0},
		totalWeight:  1.0,
		cumulWeights: []float64{1.0},
		quotaCredits: []float64{0},
		selection:    "random",
	}
}

func (s *WeightedLogSelector) Select() TemplateEntry {
	if len(s.categories) == 0 {
		return TemplateEntry{}
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

	// random (i.i.d.) weighted selection
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

// --- Text batch generator ---

type TextBatchGenerator struct {
	selector *WeightedLogSelector
}

func (g *TextBatchGenerator) Generate(size int) ([]byte, error) {
	batch := generateFastRealisticBatch(g.selector, size)
	return json.Marshal(batch)
}

func setupTextGenerator(logFile, weightConfigPath, weightSelection string) BatchGenerator {
	var selector *WeightedLogSelector
	if weightConfigPath != "" {
		templates, err := readTemplatesFromFile(logFile)
		if err != nil {
			fmt.Printf("Error reading template logs: %v\n", err)
			return nil
		}
		if len(templates) == 0 {
			fmt.Println("No logs found in the file")
			return nil
		}
		config, err := loadWeightConfig(weightConfigPath)
		if err != nil {
			fmt.Printf("Error loading weight config: %v\n", err)
			return nil
		}
		selector = NewWeightedLogSelector(templates, config, weightSelection)
		if selector == nil {
			return nil
		}
		fmt.Printf("Loaded %d template logs with weighted selection (%s) from %s\n", len(templates), weightSelection, weightConfigPath)
	} else {
		templateLogs, err := readLogsFromFile(logFile)
		if err != nil {
			fmt.Printf("Error reading logs: %v\n", err)
			return nil
		}
		if len(templateLogs) == 0 {
			fmt.Println("No logs found in the file")
			return nil
		}
		selector = NewUniformSelector(templateLogs)
		fmt.Printf("Loaded %d template logs with uniform selection\n", len(templateLogs))
	}
	return &TextBatchGenerator{selector: selector}
}

func generateFastRealisticBatch(selector *WeightedLogSelector, size int) []LogEntry {
	batch := make([]LogEntry, size)
	batchUUID := uuid.New().String()
	baseTime := time.Now()
	for i := 0; i < size; i++ {
		template := selector.Select()

		jobID := templatePool.JobIDs[rand.Intn(len(templatePool.JobIDs))]
		execID := templatePool.ExecutionIDs[rand.Intn(len(templatePool.ExecutionIDs))]
		threadID := templatePool.ThreadIDs[rand.Intn(len(templatePool.ThreadIDs))]
		processID := templatePool.ProcessIDs[rand.Intn(len(templatePool.ProcessIDs))]
		ipAddr := templatePool.IPAddresses[rand.Intn(len(templatePool.IPAddresses))]

		logTime := baseTime.Add(time.Duration(i) * time.Millisecond)
		timestamp := logTime.Unix()

		logField := ""
		if template.Log != "" {
			logField = fastReplaceMessage(template.Log, jobID, execID, threadID, processID, ipAddr, logTime) + " [batch:" + batchUUID + strconv.Itoa(i) + "]"
		}

		batch[i] = LogEntry{
			Timestamp: timestamp * 1000000,
			Log:       logField,
			Filename:  template.Filename,
		}
	}

	return batch
}

func fastReplaceMessage(msg, jobID, execID, threadID string, processID int, ipAddr string, logTime time.Time) string {
	if strings.Contains(msg, "jid=") {
		if idx := strings.Index(msg, "jid="); idx != -1 {
			start := idx + 4
			end := start
			for end < len(msg) && (msg[end] != ' ' && msg[end] != ']' && msg[end] != ',') {
				end++
			}
			if end > start {
				msg = msg[:start] + jobID + msg[end:]
			}
		}
	}

	if strings.Contains(msg, "tid=") {
		if idx := strings.Index(msg, "tid="); idx != -1 {
			start := idx + 4
			end := start
			for end < len(msg) && (msg[end] != ' ' && msg[end] != ']' && msg[end] != ',') {
				end++
			}
			if end > start {
				msg = msg[:start] + threadID + msg[end:]
			}
		}
	}

	if idx := strings.Index(msg, "2025-03-08T"); idx != -1 {
		newTime := logTime.Format("2006-01-02T15:04:05.000Z")
		zIdx := strings.Index(msg[idx:], "Z")
		if zIdx != -1 {
			msg = msg[:idx] + newTime + msg[idx+zIdx+1:]
		}
	}

	for _, tip := range templateIPs {
		if strings.Contains(msg, tip) {
			msg = strings.Replace(msg, tip, ipAddr, 1)
			break
		}
	}

	if len(msg) > 0 && msg[len(msg)-1] != ' ' {
		msg += " "
	}
	msg += "beacon_name=" + randDockerishName()

	return msg
}

func generateFastPodName(template, suffix string) string {
	if strings.Contains(template, "-") {
		lastDash := strings.LastIndex(template, "-")
		if lastDash > 0 {
			secondLastDash := strings.LastIndex(template[:lastDash], "-")
			if secondLastDash > 0 {
				return template[:secondLastDash] + "-" + suffix
			}
		}
	}
	return template + "-" + suffix
}

// --- Apache log helpers ---

const (
	ApacheCommonLog = "%s - %s [%s] \"%s %s %s\" %d %d"
	Apache          = "02/Jan/2006:15:04:05 -0700"
	ApacheErrorLog  = "[%s] [%s:%s] [pid %d:tid %d] [client %s:%d] %s"
	ApacheError     = "Mon Jan 02 15:04:05 2006"
)

// func RandResourceURI() string {
// 	var uri string
// 	num := gofakeit.Number(1, 4)
// 	for i := 0; i < num; i++ {
// 		uri += "/" + url.QueryEscape(gofakeit.BS())
// 	}
// 	uri = strings.ToLower(uri)
// 	return uri
// }

func RandResourceURI() string {
	return templatePool.ResourceURIs[rand.Intn(len(templatePool.ResourceURIs))]
}

// RandAuthUserID generates a random auth user id
func RandAuthUserID() string {
	candidates := []string{"-", strings.ToLower(gofakeit.Username())}
	return candidates[rand.Intn(2)]
}

// RandHTTPVersion returns a random http version
func RandHTTPVersion() string {
	versions := []string{"HTTP/1.0", "HTTP/1.1", "HTTP/2.0"}
	return versions[rand.Intn(3)]
}

func NewApacheCommonLog() string {
	t := time.Now()
	return fmt.Sprintf(
		ApacheCommonLog,
		gofakeit.IPv4Address(),
		RandAuthUserID(),
		t.Format(Apache),
		gofakeit.HTTPMethod(),
		RandResourceURI(),
		RandHTTPVersion(),
		gofakeit.StatusCode(),
		gofakeit.Number(0, 30000),
	)
}

func NewApacheErrorLog() string {
	t := time.Now()
	return fmt.Sprintf(
		ApacheErrorLog,
		t.Format(ApacheError),
		gofakeit.Word(),
		gofakeit.LogLevel("apache"),
		gofakeit.Number(1, 10000),
		gofakeit.Number(1, 10000),
		gofakeit.IPv4Address(),
		gofakeit.Number(1, 65535),
		gofakeit.HackerPhrase(),
	)
}

func GenerateTraceID() string {
	return fmt.Sprintf("%016x%016x", rand.Uint64(), rand.Uint64())
}

func GenerateSpanID() string {
	return fmt.Sprintf("%016x", rand.Uint64())
}
