package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var k8ISTLocation *time.Location

func init() {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		k8ISTLocation = time.FixedZone("IST", 5*3600+30*60)
	} else {
		k8ISTLocation = loc
	}
}

// formatK8IstTimestamp formats like "20/04/2026, 20:39:43" (IST).
func formatK8IstTimestamp(t time.Time) string {
	return t.In(k8ISTLocation).Format("02/01/2006, 15:04:05")
}

// K8LogTemplate matches templates/prod_log_k8_logs.json entries.
type K8LogTemplate struct {
	ContainerName string `json:"container_name"`
	NamespaceName string `json:"namespace_name"`
	PluginSource  string `json:"plugin_source"`
	PodName       string `json:"pod_name"`
	Stream        string `json:"stream"`
	Body          string `json:"body"`
}

// K8LogEntry matches logs ingested into ClickHouse (no synthetic HTTP/metrics fields).
type K8LogEntry struct {
	Timestamp     int64  `json:"_timestamp"`
	IstTimestamp  string `json:"ist_timestamp"`
	Message       string `json:"message"`
	TraceID       string `json:"trace_id"`
	SpanID        string `json:"span_id"`
	NamespaceName string `json:"namespace_name"`
	ContainerName string `json:"container_name"`
	PodName       string `json:"pod_name"`
	PluginSource  string `json:"plugin_source"`
	Stream        string `json:"stream"`
	Bytes         int    `json:"bytes"`
}

func readK8TemplatesFromFile(filePath string) ([]K8LogTemplate, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}
	var templates []K8LogTemplate
	if err := json.Unmarshal(data, &templates); err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}
	return templates, nil
}

func fastReplaceK8Message(msg, jobID, threadID, ipAddr string, logTime time.Time) string {
	if idx := strings.Index(msg, "jid="); idx != -1 {
		start := idx + 4
		end := start
		for end < len(msg) && msg[end] != ' ' && msg[end] != ']' && msg[end] != ',' {
			end++
		}
		if end > start {
			msg = msg[:start] + jobID + msg[end:]
		}
	}
	if idx := strings.Index(msg, "tid="); idx != -1 {
		start := idx + 4
		end := start
		for end < len(msg) && msg[end] != ' ' && msg[end] != ']' && msg[end] != ',' {
			end++
		}
		if end > start {
			msg = msg[:start] + threadID + msg[end:]
		}
	}
	if strings.Contains(msg, "2025-03-08T") {
		newTime := logTime.Format("2006-01-02T15:04:05.000Z")
		msg = strings.Replace(msg, "2025-03-08T20:57:48.591Z", newTime, 1)
		msg = strings.Replace(msg, "2025-03-08T20:57:48.497Z", newTime, 1)
		msg = strings.Replace(msg, "2025-03-08T20:57:48.589Z", newTime, 1)
	}
	if strings.Contains(msg, " for ") && strings.Contains(msg, ".253 ") {
		msg = strings.Replace(msg, "20.197.75.253", ipAddr, 1)
	}
	return msg
}

type K8BatchGenerator struct {
	templates []K8LogTemplate
}

func setupK8Generator(logFile string) BatchGenerator {
	templates, err := readK8TemplatesFromFile(logFile)
	if err != nil {
		fmt.Printf("Error reading K8 templates: %v\n", err)
		return nil
	}
	if len(templates) == 0 {
		fmt.Println("No K8 templates found in file")
		return nil
	}
	fmt.Printf("Loaded %d K8 templates from %s\n", len(templates), logFile)
	return &K8BatchGenerator{templates: templates}
}

func (g *K8BatchGenerator) Generate(size int) ([]byte, error) {
	batch := make([]K8LogEntry, size)
	baseTime := time.Now()
	batchUUID := uuid.New().String()

	tplCount := len(g.templates)
	jobCount := len(templatePool.JobIDs)
	threadCount := len(templatePool.ThreadIDs)
	ipCount := len(templatePool.IPAddresses)

	var traceID string

	for i := 0; i < size; i++ {
		tpl := &g.templates[rand.Intn(tplCount)]
		logTime := baseTime.Add(time.Duration(i) * time.Millisecond)

		if i%4 == 0 {
			traceID = GenerateTraceID()
		}

		jobID := templatePool.JobIDs[rand.Intn(jobCount)]
		threadID := templatePool.ThreadIDs[rand.Intn(threadCount)]
		ipAddr := templatePool.IPAddresses[rand.Intn(ipCount)]

		msg := fastReplaceK8Message(tpl.Body, jobID, threadID, ipAddr, logTime)
		msg += " [batch:" + batchUUID + strconv.Itoa(i) + "]"

		batch[i] = K8LogEntry{
			Timestamp:     logTime.Unix() * 1_000_000,
			IstTimestamp:  formatK8IstTimestamp(logTime),
			Message:       msg,
			TraceID:       traceID,
			SpanID:        GenerateSpanID(),
			NamespaceName: tpl.NamespaceName,
			ContainerName: tpl.ContainerName,
			PodName:       tpl.PodName,
			PluginSource:  tpl.PluginSource,
			Stream:        tpl.Stream,
			Bytes:         rand.Intn(30000),
		}
	}

	return marshalBatch(batch)
}
