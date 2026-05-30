package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
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
	flag.StringVar(&logFile, "logFile", "templates/prod_log_k8_logs.json", "Path if the file from which to read logs")
	flag.StringVar(&orgId, "orgId", "ctrlb", "Org ID")
	flag.StringVar(&streamName, "streamName", "test_stream", "Steam Name")
	flag.Float64Var(&targetVolumeGB, "targetVolumeGB", 0, "Target volume in GB (0 = use duration-based)")
	flag.BoolVar(&useVolumeTarget, "useVolumeTarget", false, "Use volume-based termination instead of time-based")
	flag.StringVar(&weightConfigPath, "weightConfig", "", "Path to JSON weight config for category proportions")
	flag.StringVar(&weightSelection, "weightSelection", "quota", "With -weightConfig: quota = running mix tracks weights across all batches; random = i.i.d. weighted draws per log line")
	flag.IntVar(&logsPerMinute, "logsPerMinute", 0, "Cap total log lines per minute across all workers (0 = unlimited)")
	flag.StringVar(&logFormat, "logFormat", "text", "Log format: text (flat log lines), k8 (K8/Fluent Bit JSON), or waf (AWS WAF JSON)")
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

func main() {
	flag.Parse()

	initTemplatePool()
	rand.Seed(time.Now().UnixNano())

	var generator BatchGenerator
	switch logFormat {
	case "waf":
		generator = setupWAFGenerator(logFile, weightConfigPath, weightSelection)
	case "k8":
		generator = setupK8Generator(logFile)
	default:
		generator = setupTextGenerator(logFile, weightConfigPath, weightSelection)
	}
	if generator == nil {
		return
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
