package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	metricsUrl = "http://10.0.0.3:8445/metrics" // Metrics endpoint (add this to your server)
	// logFile    = "prod_log_k8_logs.json"        // Source log file for templates

	// Benchmark configuration
	testDuration   = 300 * time.Second // Fixed duration test
	reportInterval = 5 * time.Second   // How often to print stats
)

var (
	concurrency     int
	batchSize       int
	baseURL         string
	logFile         string
	orgId           string
	streamName      string
	targetVolumeGB  float64
	useVolumeTarget bool
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
}

type LogEntry struct {
	ClusterName   string `json:"cluster_name"`
	ContainerName string `json:"container_name"`
	CtrlbIsource  string `json:"ctrlb_isource"`
	CtrlbLogLevel string `json:"ctrlb_log_level"`
	Message       string `json:"message"`
	NamespaceName string `json:"namespace_name"`
	PluginSource  string `json:"plugin_source"`
	PodName       string `json:"pod_name"`
	Stream        string `json:"stream"`
	Timestamp     int64  `json:"timestamp"`
	IstTimestamp  string `json:"ist_timestamp"`
}

// Metrics collector
type BenchmarkMetrics struct {
	totalRequests      int64
	totalLogs          int64
	totalBytes         int64
	successfulRequests int64
	failedRequests     int64
	targetBytes        int64
}

// Manual Connection Pool
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

	// Pre-create connections
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

	// Set TCP keep-alive
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
		// No connections available, try to create a new one if under limit
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

		// Wait for a connection to become available
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
		// Connection returned to pool
	default:
		// Pool is full, close the connection
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

// HTTP client using manual connection pool
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
	defer c.pool.Put(conn)

	// Build HTTP request
	request := fmt.Sprintf(
		"POST %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Type: application/json\r\n"+
			"Content-Length: %d\r\n"+
			"Authorization: %s\r\n"+
			"User-Agent: %s\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n",
		path,
		c.host,
		len(data),
		c.authHeader,
		c.userAgent,
	)

	// Send request
	totalBytes := int64(len(request) + len(data))

	// Set write deadline
	conn.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

	_, err = conn.conn.Write([]byte(request))
	if err != nil {
		return false, totalBytes, err
	}

	_, err = conn.conn.Write(data)
	if err != nil {
		return false, totalBytes, err
	}

	// Read response
	conn.conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	reader := bufio.NewReader(conn.conn)

	// Read status line
	statusLine, isPrefix, err := reader.ReadLine()
	if err != nil {
		return false, totalBytes, err
	}

	// Handle long status lines (unlikely but defensive)
	for isPrefix {
		var moreLine []byte
		moreLine, isPrefix, err = reader.ReadLine()
		if err != nil {
			return false, totalBytes, err
		}
		statusLine = append(statusLine, moreLine...)
	}

	// Parse status code
	parts := strings.Split(string(statusLine), " ")
	if len(parts) < 2 {
		return false, totalBytes, fmt.Errorf("invalid status line: %s", string(statusLine))
	}

	statusCode := parts[1]
	success := statusCode == "200"

	// Read headers to find content length
	contentLength := 0
	for {
		line, isPrefix, err := reader.ReadLine()
		if err != nil {
			return false, totalBytes, err
		}

		// Handle long header lines
		for isPrefix {
			var moreLine []byte
			moreLine, isPrefix, err = reader.ReadLine()
			if err != nil {
				return false, totalBytes, err
			}
			line = append(line, moreLine...)
		}

		if len(line) == 0 {
			// Empty line indicates end of headers
			break
		}

		lineStr := string(line)
		if strings.HasPrefix(strings.ToLower(lineStr), "content-length:") {
			lengthStr := strings.TrimSpace(lineStr[15:])
			contentLength, _ = strconv.Atoi(lengthStr)
		}
	}

	// Read body (we need to consume it to keep connection alive)
	if contentLength > 0 {
		body := make([]byte, contentLength)
		_, err = io.ReadFull(reader, body)
		if err != nil {
			return false, totalBytes, err
		}
	}

	return success, totalBytes, nil
}

func (c *PooledHTTPClient) Close() {
	c.pool.Close()
}

func (c *PooledHTTPClient) Stats() (int64, int64) {
	return c.pool.Stats()
}

func main() {
	flag.Parse()

	// Read template logs
	templateLogs, err := readLogsFromFile(logFile)
	if err != nil {
		fmt.Printf("Error reading logs: %v\n", err)
		return
	}

	if len(templateLogs) == 0 {
		fmt.Println("No logs found in the file")
		return
	}

	fmt.Printf("Loaded %d template logs\n", len(templateLogs))

	// For generating random log data
	rand.Seed(time.Now().UnixNano())

	// Metrics
	metrics := &BenchmarkMetrics{}

	// Create stop channel for graceful shutdown
	stopCh := make(chan struct{})

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down benchmark...")
		close(stopCh)
	}()

	// Create pooled HTTP client
	maxConnections := concurrency // One connection per worker initially

	client, err := NewPooledHTTPClient(baseURL, maxConnections)
	if err != nil {
		fmt.Printf("Error creating HTTP client: %v\n", err)
		return
	}
	defer client.Close()

	// Start the workers
	var wg sync.WaitGroup
	startTime := time.Now()

	// Set termination criteria
	var endTime time.Time
	if useVolumeTarget && targetVolumeGB > 0 {
		metrics.targetBytes = int64(targetVolumeGB * 1024 * 1024 * 1024) // Convert GB to bytes
		endTime = startTime.Add(24 * time.Hour)                          // Safety net - max 24 hours
		fmt.Printf("Target: %.2f GB (%.0f bytes)\n", targetVolumeGB, float64(metrics.targetBytes))
	} else {
		endTime = startTime.Add(testDuration)
		fmt.Printf("Duration-based test: %v\n", testDuration)
	}

	// Start the metrics reporter
	go reportMetrics(metrics, client, startTime, stopCh)

	// Start the workers
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			runWorker(client, templateLogs, metrics, stopCh, endTime, workerID)
		}(i)
	}

	// Wait for completion
	wg.Wait()

	// Final report
	elapsed := time.Since(startTime).Seconds()
	printMetrics(metrics, client, elapsed)
}

func runWorker(client *PooledHTTPClient, templateLogs []LogEntry, metrics *BenchmarkMetrics,
	stopCh chan struct{}, endTime time.Time, workerID int) {

	for {
		select {
		case <-stopCh:
			return
		default:
			if useVolumeTarget && targetVolumeGB > 0 {
				// Volume-based termination
				if atomic.LoadInt64(&metrics.totalBytes) >= metrics.targetBytes {
					return
				}
				// Also check time safety net
				if time.Now().After(endTime) {
					fmt.Printf("Worker %d: Safety timeout reached\n", workerID)
					return
				}
			} else {
				// Time-based termination (original behavior)
				if time.Now().After(endTime) {
					return
				}
			}

			// Generate a batch of logs by randomly selecting and modifying template logs
			batch := generateLogBatch(templateLogs, batchSize)

			// Send the batch
			success, bytesProcessed := sendLogBatch(client, batch, workerID)

			// Update metrics
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

func generateLogBatch(templateLogs []LogEntry, size int) []LogEntry {
	batch := make([]LogEntry, size)

	uuid := uuid.New().String()
	for i := 0; i < size; i++ {
		// Pick a random template log
		template := templateLogs[rand.Intn(len(templateLogs))]

		// Create a modified copy with unique timestamp and some random fields
		log := LogEntry{
			ClusterName:   template.ClusterName + ":: [" + orgId + "]",
			ContainerName: template.ContainerName + "-" + strconv.Itoa(rand.Intn(1000)),
			CtrlbIsource:  template.CtrlbIsource,
			CtrlbLogLevel: template.CtrlbLogLevel,
			Message:       template.Message + " - " + " [benchmark:" + strconv.FormatInt(time.Now().UnixNano(), 10) + "::" + strconv.Itoa(i) + "]" + " IDER: " + uuid,
			NamespaceName: template.NamespaceName,
			PluginSource:  template.PluginSource,
			PodName:       template.PodName + "-" + strconv.Itoa(rand.Intn(100)),
			Stream:        template.Stream + ":: [" + streamName + "]",
			Timestamp:     time.Now().Unix(),
			IstTimestamp:  time.Now().Format(time.RFC3339),
		}

		batch[i] = log
	}

	return batch
}

func sendLogBatch(client *PooledHTTPClient, logs []LogEntry, workerID int) (bool, int64) {
	data, err := json.Marshal(logs)
	if err != nil {
		fmt.Printf("Worker %d: Unable to marshal payload: %v\n", workerID, err)
		return false, 0
	}

	path := fmt.Sprintf("/%s/%s/_json", orgId, streamName)
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
			// Calculate interval metrics
			intervalElapsed := t.Sub(lastTime).Seconds()
			totalBytes := atomic.LoadInt64(&metrics.totalBytes)
			totalLogs := atomic.LoadInt64(&metrics.totalLogs)
			intervalBytes := totalBytes - lastBytes
			intervalLogs := totalLogs - lastLogs

			// Calculate rates
			mbPerSecInterval := float64(intervalBytes) / intervalElapsed / (1024 * 1024)
			logsPerSecInterval := float64(intervalLogs) / intervalElapsed

			// Print current rate
			fmt.Printf("[%s] Current Rate: %.2f MB/s | Instantaneous log rate: %.2f MB/s | ",
				t.Format("15:04:05"),
				mbPerSecInterval, logsPerSecInterval)

			// Update for next interval
			lastBytes = totalBytes
			lastLogs = totalLogs
			lastTime = t

			// Also print cumulative metrics
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

	// Calculate per-core metrics (estimation)
	numCores := runtime.NumCPU()
	mbPerSecPerCore := mbPerSec / float64(numCores)

	// Get connection pool stats
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
	// Read file content
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	// Parse JSON
	var logs []LogEntry
	err = json.Unmarshal(data, &logs)
	if err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}

	return logs, nil
}
