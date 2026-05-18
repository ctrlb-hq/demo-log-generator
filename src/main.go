package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	crand "crypto/rand"
	"encoding/hex"

	"github.com/brianvoe/gofakeit"
	"github.com/google/uuid"
)

const (
	metricsUrl     = "http://10.0.0.3:8445/metrics"
	testDuration   = 300 * time.Second
	reportInterval = 5 * time.Second
)

var (
	concurrency      int
	batchSize        int
	baseURL          string
	logFile          string
	orgId            string
	streamName       string
	targetVolumeGB   float64
	useVolumeTarget  bool
	weightConfigPath string
	weightSelection  string
	logsPerMinute    int
	logFormat        string
)

func init() {
	flag.IntVar(&concurrency, "concurrency", 20, "Number of concurrent workers")
	flag.IntVar(&batchSize, "batchSize", 1000, "Number of logs per batch")
	flag.StringVar(&baseURL, "baseURL", "http://10.0.0.3:8445", "Base URL to send logs to")
	flag.StringVar(&logFile, "logFile", "prod_log_k8_logs.json", "Path if the file from which to read logs")
	flag.StringVar(&orgId, "orgId", "ctrlb", "Org ID")
	flag.StringVar(&streamName, "streamName", "test_stream", "Steam Name")
	flag.Float64Var(&targetVolumeGB, "targetVolumeGB", 0, "Target volume in GB (0 = use duration-based)")
	flag.BoolVar(&useVolumeTarget, "useVolumeTarget", false, "Use volume-based termination instead of time-based")
	flag.StringVar(&weightConfigPath, "weightConfig", "", "Path to JSON weight config for category proportions")
	flag.StringVar(&weightSelection, "weightSelection", "quota", "With -weightConfig: quota = running mix tracks weights across all batches; random = i.i.d. weighted draws per log line")
	flag.IntVar(&logsPerMinute, "logsPerMinute", 0, "Cap total log lines per minute across all workers (0 = unlimited)")
	flag.StringVar(&logFormat, "logFormat", "text", "Log format: text (flat log lines) or waf (AWS WAF JSON)")
}

type LogEntry struct {
	Timestamp int64  `json:"_timestamp,omitempty"`
	Log       string `json:"log,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

type BenchmarkMetrics struct {
	totalRequests      int64
	totalLogs          int64
	totalBytes         int64
	successfulRequests int64
	failedRequests     int64
	targetBytes        int64
}

type BatchRateLimiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
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

// Connection Pool (keeping your existing implementation)
type Connection struct {
	conn     net.Conn
	lastUsed time.Time
	inUse    bool
	id       int
}

type ConnectionPool struct {
	connections []*Connection
	available   chan *Connection
	host        string
	port        string
	mu          sync.Mutex
	maxConns    int
	created     int64
	reused      int64
}

func NewConnectionPool(targetURL string, maxConns int) (*ConnectionPool, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	pool := &ConnectionPool{
		connections: make([]*Connection, 0, maxConns),
		available:   make(chan *Connection, maxConns),
		host:        host,
		port:        port,
		maxConns:    maxConns,
	}

	for i := 0; i < maxConns; i++ {
		conn, err := pool.createConnection(i)
		if err != nil {
			fmt.Printf("Warning: Failed to create connection %d: %v\n", i, err)
			continue
		}
		pool.connections = append(pool.connections, conn)
		pool.available <- conn
	}

	fmt.Printf("Created connection pool with %d connections to %s:%s\n", len(pool.connections), host, port)
	return pool, nil
}

func (p *ConnectionPool) createConnection(id int) (*Connection, error) {
	address := net.JoinHostPort(p.host, p.port)
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true)
	}

	return &Connection{
		conn:     conn,
		lastUsed: time.Now(),
		inUse:    false,
		id:       id,
	}, nil
}

func (p *ConnectionPool) Get() (*Connection, error) {
	select {
	case conn := <-p.available:
		conn.inUse = true
		conn.lastUsed = time.Now()
		atomic.AddInt64(&p.reused, 1)
		return conn, nil
	default:
		p.mu.Lock()
		defer p.mu.Unlock()

		if len(p.connections) < p.maxConns {
			conn, err := p.createConnection(len(p.connections))
			if err != nil {
				return nil, err
			}
			p.connections = append(p.connections, conn)
			conn.inUse = true
			atomic.AddInt64(&p.created, 1)
			return conn, nil
		}

		conn := <-p.available
		conn.inUse = true
		conn.lastUsed = time.Now()
		atomic.AddInt64(&p.reused, 1)
		return conn, nil
	}
}

func (p *ConnectionPool) Put(conn *Connection) {
	conn.inUse = false
	select {
	case p.available <- conn:
	default:
		conn.conn.Close()
	}
}

func (p *ConnectionPool) Close() {
	close(p.available)
	for _, conn := range p.connections {
		conn.conn.Close()
	}
}

func (p *ConnectionPool) Stats() (int64, int64) {
	return atomic.LoadInt64(&p.created), atomic.LoadInt64(&p.reused)
}

type PooledHTTPClient struct {
	pool       *ConnectionPool
	host       string
	userAgent  string
	authHeader string
}

func NewPooledHTTPClient(targetURL string, maxConns int) (*PooledHTTPClient, error) {
	pool, err := NewConnectionPool(targetURL, maxConns)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse(targetURL)
	host := u.Host

	return &PooledHTTPClient{
		pool:       pool,
		host:       host,
		userAgent:  "load-generator/1.0",
		authHeader: "Basic YWRtaW46YWRtaW4=",
	}, nil
}

func (c *PooledHTTPClient) Post(path string, data []byte) (bool, int64, error) {
	conn, err := c.pool.Get()
	if err != nil {
		return false, 0, err
	}

	// Remove defer - we'll handle connection return manually
	// defer c.pool.Put(conn)

	// Test if connection is still alive before using it
	conn.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	one := make([]byte, 1)
	_, err = conn.conn.Read(one)
	conn.conn.SetReadDeadline(time.Time{}) // Reset deadline

	if err == nil {
		// If we can read data immediately, server probably closed connection
		conn.conn.Close()
		// Create new connection
		conn, err = c.pool.createConnection(conn.id)
		if err != nil {
			return false, 0, fmt.Errorf("failed to create new connection: %w", err)
		}
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		// If it's not a timeout error, connection is broken
		conn.conn.Close()
		conn, err = c.pool.createConnection(conn.id)
		if err != nil {
			return false, 0, fmt.Errorf("failed to create new connection: %w", err)
		}
	}
	// If it's a timeout error, connection is probably good to use

	request := fmt.Sprintf(
		"POST %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Type: application/json\r\n"+
			"Content-Length: %d\r\n"+
			"Authorization: %s\r\n"+
			"User-Agent: %s\r\n"+
			"Connection: keep-alive\r\n"+
			"Keep-Alive: timeout=300\r\n"+ // Tell server we want 5min timeout
			"\r\n",
		path, c.host, len(data), c.authHeader, c.userAgent,
	)

	totalBytes := int64(len(request) + len(data))

	conn.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = conn.conn.Write([]byte(request))
	if err != nil {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("write request failed: %w", err)
	}

	_, err = conn.conn.Write(data)
	if err != nil {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("write data failed: %w", err)
	}

	conn.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn.conn)

	statusLine, isPrefix, err := reader.ReadLine()
	if err != nil {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("read status failed: %w", err)
	}

	for isPrefix {
		var moreLine []byte
		moreLine, isPrefix, err = reader.ReadLine()
		if err != nil {
			conn.conn.Close()
			return false, totalBytes, err
		}
		statusLine = append(statusLine, moreLine...)
	}

	parts := strings.Split(string(statusLine), " ")
	if len(parts) < 2 {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("invalid status line: %s", string(statusLine))
	}

	success := parts[1] == "200"

	contentLength := 0
	for {
		line, isPrefix, err := reader.ReadLine()
		if err != nil {
			conn.conn.Close()
			return false, totalBytes, err
		}

		for isPrefix {
			var moreLine []byte
			moreLine, isPrefix, err = reader.ReadLine()
			if err != nil {
				conn.conn.Close()
				return false, totalBytes, err
			}
			line = append(line, moreLine...)
		}

		if len(line) == 0 {
			break
		}

		lineStr := string(line)
		if strings.HasPrefix(strings.ToLower(lineStr), "content-length:") {
			lengthStr := strings.TrimSpace(lineStr[15:])
			contentLength, _ = strconv.Atoi(lengthStr)
		}
	}

	if contentLength > 0 {
		body := make([]byte, contentLength)
		_, err = io.ReadFull(reader, body)
		if err != nil {
			conn.conn.Close()
			return false, totalBytes, err
		}
	}

	// Only put connection back in pool if everything succeeded
	if success && err == nil {
		c.pool.Put(conn)
	} else {
		conn.conn.Close()
	}

	return success, totalBytes, nil
}

func (c *PooledHTTPClient) Close() {
	c.pool.Close()
}

func (c *PooledHTTPClient) Stats() (int64, int64) {
	return c.pool.Stats()
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

var templateIPs = []string{
	"20.197.75.253", "10.42.3.17", "203.0.113.55", "172.16.0.42", "198.51.100.23",
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

func main() {
	flag.Parse()

	initTemplatePool()

	rand.Seed(time.Now().UnixNano())

	var generator BatchGenerator

	if logFormat == "waf" {
		if weightConfigPath == "" {
			fmt.Println("WAF mode requires -weightConfig")
			return
		}
		wafTemplates, err := readWAFTemplatesFromFile(logFile)
		if err != nil {
			fmt.Printf("Error reading WAF templates: %v\n", err)
			return
		}
		if len(wafTemplates) == 0 {
			fmt.Println("No WAF templates found in file")
			return
		}
		config, err := loadWeightConfig(weightConfigPath)
		if err != nil {
			fmt.Printf("Error loading weight config: %v\n", err)
			return
		}
		wafSel := NewWAFSelector(wafTemplates, config, weightSelection)
		if wafSel == nil {
			return
		}
		generator = &WAFBatchGenerator{selector: wafSel}
		fmt.Printf("Loaded %d WAF templates with weighted selection (%s)\n", len(wafTemplates), weightSelection)
	} else {
		var selector *WeightedLogSelector
		if weightConfigPath != "" {
			templates, err := readTemplatesFromFile(logFile)
			if err != nil {
				fmt.Printf("Error reading template logs: %v\n", err)
				return
			}
			if len(templates) == 0 {
				fmt.Println("No logs found in the file")
				return
			}
			config, err := loadWeightConfig(weightConfigPath)
			if err != nil {
				fmt.Printf("Error loading weight config: %v\n", err)
				return
			}
			selector = NewWeightedLogSelector(templates, config, weightSelection)
			if selector == nil {
				return
			}
			fmt.Printf("Loaded %d template logs with weighted selection (%s) from %s\n", len(templates), weightSelection, weightConfigPath)
		} else {
			templateLogs, err := readLogsFromFile(logFile)
			if err != nil {
				fmt.Printf("Error reading logs: %v\n", err)
				return
			}
			if len(templateLogs) == 0 {
				fmt.Println("No logs found in the file")
				return
			}
			selector = NewUniformSelector(templateLogs)
			fmt.Printf("Loaded %d template logs with uniform selection\n", len(templateLogs))
		}
		generator = &TextBatchGenerator{selector: selector}
	}

	if batchSize <= 0 {
		fmt.Println("batchSize must be > 0")
		return
	}
	if concurrency <= 0 {
		fmt.Println("concurrency must be > 0")
		return
	}
	if logsPerMinute < 0 {
		fmt.Println("logsPerMinute must be >= 0")
		return
	}

	var logRateLimiter *BatchRateLimiter
	if logsPerMinute > 0 {
		if batchSize > logsPerMinute {
			fmt.Printf("batchSize (%d) must be <= logsPerMinute (%d) when rate limiting is enabled\n", batchSize, logsPerMinute)
			return
		}
		logRateLimiter = NewBatchRateLimiter(logsPerMinute, batchSize)
		fmt.Printf("Rate limit: ~%d log lines/minute across all workers (%d logs every %s)\n",
			logsPerMinute, batchSize, logRateLimiter.interval)
	}

	metrics := &BenchmarkMetrics{}
	stopCh := make(chan struct{})
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down benchmark...")
		close(stopCh)
		workerCancel()
	}()

	maxConnections := concurrency
	client, err := NewPooledHTTPClient(baseURL, maxConnections)
	if err != nil {
		fmt.Printf("Error creating HTTP client: %v\n", err)
		return
	}
	defer client.Close()

	var wg sync.WaitGroup
	startTime := time.Now()

	var endTime time.Time
	if useVolumeTarget && targetVolumeGB > 0 {
		metrics.targetBytes = int64(targetVolumeGB * 1024 * 1024 * 1024)
		endTime = startTime.Add(24 * time.Hour)
		fmt.Printf("Target: %.2f GB (%.0f bytes)\n", targetVolumeGB, float64(metrics.targetBytes))
	} else {
		endTime = startTime.Add(testDuration)
		fmt.Printf("Duration-based test: %v\n", testDuration)
	}

	go reportMetrics(metrics, client, startTime, stopCh)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			runWorker(workerCtx, client, generator, logRateLimiter, metrics, stopCh, endTime, workerID)
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime).Seconds()
	printMetrics(metrics, client, elapsed)
}

func runWorker(ctx context.Context, client *PooledHTTPClient, generator BatchGenerator,
	logRateLimiter *BatchRateLimiter, metrics *BenchmarkMetrics,
	stopCh chan struct{}, endTime time.Time, workerID int) {

	for {
		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		default:
			if useVolumeTarget && targetVolumeGB > 0 {
				if atomic.LoadInt64(&metrics.totalBytes) >= metrics.targetBytes {
					return
				}
				if time.Now().After(endTime) {
					return
				}
			} else {
				if time.Now().After(endTime) {
					return
				}
			}

			if err := waitLogBatchQuota(ctx, logRateLimiter, batchSize); err != nil {
				return
			}

			data, err := generator.Generate(batchSize)
			if err != nil {
				fmt.Printf("Worker %d: Generate error: %v\n", workerID, err)
				continue
			}
			//print the data
			// fmt.Printf("Worker %d: Data: %s\n", workerID, string(data))
			success, bytesProcessed := sendBatchData(client, data, workerID)

			atomic.AddInt64(&metrics.totalRequests, 1)
			if success {
				atomic.AddInt64(&metrics.successfulRequests, 1)
				atomic.AddInt64(&metrics.totalLogs, int64(batchSize))
				atomic.AddInt64(&metrics.totalBytes, bytesProcessed)
			} else {
				atomic.AddInt64(&metrics.failedRequests, 1)
			}
		}
	}
}

func NewBatchRateLimiter(logsPerMinute, batchSize int) *BatchRateLimiter {
	interval := time.Duration(float64(time.Minute) * float64(batchSize) / float64(logsPerMinute))
	if interval < time.Nanosecond {
		interval = time.Nanosecond
	}
	return &BatchRateLimiter{
		next:     time.Now(),
		interval: interval,
	}
}

func waitLogBatchQuota(ctx context.Context, lim *BatchRateLimiter, n int) error {
	if lim == nil || n <= 0 {
		return nil
	}
	return lim.Wait(ctx)
}

func (l *BatchRateLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	scheduled := l.next
	if scheduled.Before(now) {
		scheduled = now
	}
	l.next = scheduled.Add(l.interval)
	l.mu.Unlock()

	delay := time.Until(scheduled)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// --- WAF log generation ---

var wafCountries = []struct {
	code   string
	region string
	weight float64
}{
	{"IN", "KA", 50}, {"IN", "MH", 20}, {"IN", "DL", 10},
	{"US", "VA", 8}, {"SG", "SG", 5}, {"AE", "DU", 4}, {"GB", "LND", 3},
}

var wafASNs = []string{
	"AS9829", "AS55836", "AS24560", "AS45609", "AS18209",
	"AS16509", "AS14618", "AS13335", "AS15169", "AS20473",
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

func randomizeWAFEntry(entry map[string]interface{}, ts int64, ip, country, region string) {
	entry["_timestamp"] = ts

	if req, ok := entry["httpRequest"].(map[string]interface{}); ok {
		req["clientIp"] = ip
		req["country"] = country
		req["requestId"] = uuid.New().String()

		if headers, ok := req["headers"].([]interface{}); ok {
			for _, h := range headers {
				hdr, ok := h.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := hdr["name"].(string)
				switch name {
				case "sessionid", "session_id":
					hdr["value"] = uuid.New().String()
				case "deviceuid", "device_uid":
					hdr["value"] = uuid.New().String()
				case "requestid", "request_id":
					hdr["value"] = uuid.New().String()
				case "authorization":
					hdr["value"] = "Bearer eyJ" + randomHex(32) + "." + randomHex(48) + "." + randomHex(24)
				case "user-agent":
					hdr["value"] = wafUserAgents[rand.Intn(len(wafUserAgents))]
				case "app_version", "appversion":
					hdr["value"] = wafAppVersions[rand.Intn(len(wafAppVersions))]
				case "store_id", "storeid":
					hdr["value"] = fmt.Sprintf("%d", rand.Intn(9000)+1000)
				case "advertiser_id":
					hdr["value"] = uuid.New().String()
				case "sentry-trace":
					hdr["value"] = randomHex(16) + "-" + randomHex(8) + "-1"
				}
			}
		}
	}

	entry["ja3Fingerprint"] = randomHex(16)
	entry["ja4Fingerprint"] = "t13d1516h2_" + randomHex(6) + "_" + randomHex(6)
	entry["clientAsn"] = wafASNs[rand.Intn(len(wafASNs))]

	geoLabels := []interface{}{
		map[string]interface{}{"name": "awswaf:clientip:geo:country:" + country},
		map[string]interface{}{"name": "awswaf:clientip:geo:region:" + region},
	}
	entry["labels"] = geoLabels

	if rateRules, ok := entry["rateBasedRuleList"].([]interface{}); ok {
		for _, rr := range rateRules {
			rule, ok := rr.(map[string]interface{})
			if !ok {
				continue
			}
			if _, hasLimit := rule["limitValue"]; hasLimit {
				rule["limitValue"] = ip
			}
			if customs, ok := rule["customValues"].([]interface{}); ok {
				for _, cv := range customs {
					if cval, ok := cv.(map[string]interface{}); ok {
						cval["value"] = randomHex(12)
					}
				}
			}
		}
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
		tsMicros := logTime.UnixMicro()
		ip := templatePool.IPAddresses[rand.Intn(len(templatePool.IPAddresses))]
		country, region := pickWeightedCountry()

		randomizeWAFEntry(entry, tsMicros, ip, country, region)
		batch[i] = entry
	}
	return json.Marshal(batch)
}

type TextBatchGenerator struct {
	selector *WeightedLogSelector
}

func (g *TextBatchGenerator) Generate(size int) ([]byte, error) {
	batch := generateFastRealisticBatch(g.selector, size)
	return json.Marshal(batch)
}

func sendBatchData(client *PooledHTTPClient, data []byte, workerID int) (bool, int64) {
	path := fmt.Sprintf("/%s/%s/_json_evolving", orgId, streamName)
	success, bytesProcessed, err := client.Post(path, data)
	if err != nil {
		fmt.Printf("Worker %d: Error sending request: %v\n", workerID, err)
		return false, bytesProcessed
	}
	if !success {
		fmt.Printf("Worker %d: Non-OK HTTP status\n", workerID)
		return false, bytesProcessed
	}
	return true, bytesProcessed
}

func reportMetrics(metrics *BenchmarkMetrics, client *PooledHTTPClient, startTime time.Time, stopCh chan struct{}) {
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	var lastBytes int64
	var lastLogs int64
	var lastTime time.Time = startTime

	for {
		select {
		case <-stopCh:
			return
		case t := <-ticker.C:
			intervalElapsed := t.Sub(lastTime).Seconds()
			totalBytes := atomic.LoadInt64(&metrics.totalBytes)
			totalLogs := atomic.LoadInt64(&metrics.totalLogs)
			intervalBytes := totalBytes - lastBytes
			intervalLogs := totalLogs - lastLogs

			mbPerSecInterval := float64(intervalBytes) / intervalElapsed / (1024 * 1024)
			logsPerSecInterval := float64(intervalLogs) / intervalElapsed

			fmt.Printf("[%s] Current Rate: %.2f MB/s | %.0f logs/s | ",
				t.Format("15:04:05"), mbPerSecInterval, logsPerSecInterval)

			lastBytes = totalBytes
			lastLogs = totalLogs
			lastTime = t

			printMetrics(metrics, client, time.Since(startTime).Seconds())
		}
	}
}

func printMetrics(metrics *BenchmarkMetrics, client *PooledHTTPClient, elapsedSeconds float64) {
	totalRequests := atomic.LoadInt64(&metrics.totalRequests)
	totalLogs := atomic.LoadInt64(&metrics.totalLogs)
	totalBytes := atomic.LoadInt64(&metrics.totalBytes)
	successRate := float64(atomic.LoadInt64(&metrics.successfulRequests)) / float64(totalRequests) * 100

	mbProcessed := float64(totalBytes) / (1024 * 1024)
	mbPerSec := mbProcessed / elapsedSeconds
	logsPerSec := float64(totalLogs) / elapsedSeconds

	numCores := runtime.NumCPU()
	mbPerSecPerCore := mbPerSec / float64(numCores)

	created, reused := client.Stats()

	progressStr := ""
	if useVolumeTarget && targetVolumeGB > 0 {
		progressPct := (float64(totalBytes) / float64(metrics.targetBytes)) * 100
		remainingGB := (float64(metrics.targetBytes - totalBytes)) / (1024 * 1024 * 1024)
		progressStr = fmt.Sprintf(" | Progress: %.1f%% (%.2f GB remaining)", progressPct, remainingGB)
	}

	fmt.Printf("Total Logs Sent: %d | Total: %.2f MB @ %.2f MB/s (%.2f MB/s/core est.) | %.2f cummulative logs/sec | Success: %.2f%% | Conns: %d created, %d reused%s\n",
		totalLogs,
		mbProcessed,
		mbPerSec,
		mbPerSecPerCore,
		logsPerSec,
		successRate,
		created,
		reused,
		progressStr)

}

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

func randDockerishName() string {
	return beaconAdj[rand.Intn(len(beaconAdj))] + "_" + beaconSurname[rand.Intn(len(beaconSurname))]
}
