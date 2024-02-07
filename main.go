package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

type Config struct {
	ChunkSize int    `json:"chunkSize"`
	Workers   int    `json:"workers"`
	FileName  string `json:"fileName"`
	Split     bool   `json:"split"`
	Delay     int    `json:"delay"`
}

var startTime time.Time
var totalBytesWritten uint64
var totalBytesWritten_instant uint64
var throughput float64

func main() {
	startTime = time.Now()

	config := Config{
		ChunkSize: 409,
		Workers:   4,
		FileName:  "part_ab.log",
		Split:     false,
		Delay:     0,
	}

	file, err := os.Open("config.json")
	if err != nil {
		log.Println("Configuration file not found, using default values.")
	} else {
		// Decode the JSON data into a Config struct
		decoder := json.NewDecoder(file)
		err = decoder.Decode(&config)
		if err != nil {
			log.Println("Error decoding configuration file:", err)
			return
		}
	}
	defer file.Close()

	go startServer() // Start the server on a separate goroutine

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	availableMemory := m.Sys - m.HeapReleased
	log.Printf("Available system memory: %v bytes\n", availableMemory)

	filePath := fmt.Sprintf("log-files/%s", config.FileName)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		log.Fatal(err)
	}

	fileSize := uint64(fileInfo.Size())
	if availableMemory > fileSize {
		log.Print("Good to Open")
		//Open the entire file in one go and work on it
	} else {
		log.Println("Reading the file in chunks.")
		file, err := os.Open(filePath)
		if err != nil {
			log.Println("Error opening file:", err)
			return
		}
		defer file.Close()
		// Read the file in chunks
		buffer := make([]byte, config.ChunkSize)
		var chunkIndex int

		done := make(chan struct{})

		// Start a goroutine to periodically print the total amount of data written
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(time.Second):
					throughput = float64(totalBytesWritten_instant) / float64(1024*1024*1024)
					totalBytesWritten_instant = 0
				}
			}
		}()

		for {
			n, err := file.Read(buffer)
			if err != nil && err != io.EOF {
				log.Println("Error reading file:", err)
				return
			}

			if n == 0 {
				break
			}

			// fmt.Print(string(buffer[:n]))
			if config.Split {
				totalBytesWritten += uint64(n)
				totalBytesWritten_instant += uint64(n)
			} else {
				totalBytesWritten += uint64(n * config.Workers)
				totalBytesWritten_instant += uint64(n * config.Workers)
			}

			logChunk(buffer, config.Workers, config.Split, config.Delay)

			chunkIndex++
		}
	}
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func logChunk(chunk []byte, workers int, split bool, delay int) {
	var wg sync.WaitGroup
	wg.Add(workers)

	if split {
		splitBytes := splitByteArray(chunk, workers)

		for i := 0; i < workers; i++ {
			go func(i int) {
				defer wg.Done()
				if delay > 0 {
					time.Sleep(time.Duration(delay) * time.Second)
				}
				fmt.Println(string(splitBytes[i]))
			}(i)
		}
	} else {
		for i := 0; i < workers; i++ {
			go func() {
				defer wg.Done()
				if delay > 0 {
					time.Sleep(time.Duration(delay) * time.Second)
				}
				fmt.Println(string(chunk))
			}()
		}
	}

	wg.Wait()
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	html := `
	<!DOCTYPE html>
	<html lang="en">
	<head>
		<meta charset="UTF-8">
		<meta name="viewport" content="width=device-width, initial-scale=1.0">
		<title>Dashboard</title>
	</head>
	<body>
		<h1>Performance Dashboard</h1>
		<div id="metrics"></div>
		<script>
			setInterval(fetchMetrics, 2000);
			
			function fetchMetrics() {
				fetch('/metrics')
					.then(response => response.json())
					.then(data => {
						document.getElementById('metrics').innerText = JSON.stringify(data, null, 2);
					});
			}
		</script>
	</body>
	</html>
	`
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, html)
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	// Simulate fetching performance metrics
	metrics := map[string]interface{}{
		"uptime":             time.Since(startTime).String(),
		"bytes_written (GB)": float64(totalBytesWritten) / float64(1024*1024*1024),
		"throughput (GB/s)":  throughput,
	}

	// Return metrics as JSON
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metrics); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func startServer() {
	http.HandleFunc("/", dashboardHandler)
	http.HandleFunc("/metrics", metricsHandler)

	log.Println("Server listening on port 8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func splitByteArray(chunk []byte, n int) [][]byte {
	// Calculate size of each smaller array
	size := len(chunk) / n

	// Split the chunk into n smaller arrays
	var smallerArrays [][]byte
	for i := 0; i < n; i++ {
		startIndex := i * size
		endIndex := (i + 1) * size
		if i == n-1 { // Last smaller array may have fewer elements
			endIndex = len(chunk)
		}
		smallerArrays = append(smallerArrays, chunk[startIndex:endIndex])
	}

	return smallerArrays
}
