package internal

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

	"github.com/ctrlb-hq/demo-log-generator/utils"
)

var (
	wgMain            sync.WaitGroup
	wgWorkers         sync.WaitGroup
	stopMainChannel   = make(chan struct{})
	stopWorkerChannel = make(chan struct{})
)

var startTime time.Time
var totalBytesWritten uint64
var stopProcessing bool

func StartServer() {
	http.HandleFunc("/", dashboardHandler)
	http.HandleFunc("/metrics", metricsHandler)
	http.HandleFunc("/start", startHandler)
	http.HandleFunc("/stop", stopHandler)

	log.Println("Server listening on port 8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func startHandler(w http.ResponseWriter, r *http.Request) {
	// Parse the request body to extract input values
	var inputData struct {
		Chunk          int     `json:"chunk"`
		Workers        int     `json:"workers"`
		Delay          float64 `json:"delay"`
		ToFile         bool    `json:"tofile"`
		PrimaryLogFile string  `json:"primaryLogFile"`
	}
	err := json.NewDecoder(r.Body).Decode(&inputData)
	if err != nil {
		log.Println(err)
		http.Error(w, "Failed to parse request body", http.StatusBadRequest)
		return
	}

	log.Println("Start processing with inputs:", inputData)

	// Reset stop channels
	stopMainChannel = make(chan struct{})
	stopWorkerChannel = make(chan struct{})

	wgMain = sync.WaitGroup{}
	wgWorkers = sync.WaitGroup{}

	wgMain.Add(1)

	go readFileSafely(inputData.PrimaryLogFile, inputData.Chunk,
		inputData.Workers, inputData.Delay, inputData.ToFile)

	w.WriteHeader(http.StatusOK)
}

func stopHandler(w http.ResponseWriter, r *http.Request) {

	log.Println("Stop processing command received")
	stopProcessing = true

	close(stopWorkerChannel)
	wgWorkers.Wait()

	log.Println("All Workers Go Routines Stopped")
	// Close the stop channel for the main goroutine
	close(stopMainChannel)
	wgMain.Wait()
	log.Println("Main Read Go Routine Stopped")

	utils.CloseOutputFile()
	log.Println("All Go Routines Stopped Successfully!")

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("All Go Routines Stopped Successfully!"))
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
		<div id="inputs">
    <label for="chunk">Chunk:</label>
    <input type="number" id="chunk" name="chunk (4kb)" value="4096"><br>

    <label for="workers">No. of Workers:</label>
    <input type="number" id="workers" name="workers" value="3"><br>

    <label for="delay">Delay (s):</label>
    <input type="number" id="delay" name="delay" value="0.5"><br>

    <label for="tofile">To File:</label>
    <input type="checkbox" id="tofile" name="tofile"><br> <!-- Assuming default value is true -->

    <label for="primaryLogFile">Primary Log File:</label>
    <input type="text" id="primaryLogFile" name="primaryLogFile" value="part_aa.log"><br>

    <button id="startButton" onclick="startProcessing()">Start</button>
    <button onclick="stopProcessing()">Stop</button>
</div>

		<div id="metrics"></div>
		<script>
			let intervalId;

			function fetchMetrics() {
				fetch('/metrics')
					.then(response => response.json())
					.then(data => {
						document.getElementById('metrics').innerText = JSON.stringify(data, null, 2);
					});
			}

			function startProcessing() {
				// Disable the Start button
				document.getElementById('startButton').disabled = true;

				// Parse values from HTML inputs
    const chunk = parseInt(document.getElementById('chunk').value);
    const workers = parseInt(document.getElementById('workers').value);
    const delay = parseInt(document.getElementById('delay').value);
    const tofile = document.getElementById('tofile').checked;
    const primaryLogFile = document.getElementById('primaryLogFile').value;

    // Create an object with the desired structure
    const inputData = {
        chunk: chunk,
        workers: workers,
        delay: delay,
        tofile: tofile,
        primaryLogFile: primaryLogFile
    };

    // Stringify the object before sending it in the fetch request
    const requestBody = JSON.stringify(inputData);

    // Log the body being sent
    console.log('Request Body:', requestBody);

				// Send input values to server to start processing
				fetch('/start', {
					method: 'POST',
					headers: {
						'Content-Type': 'application/json',
					},
					body: requestBody,
				})
				.then(response => {
					if (response.ok) {
						console.log('Process started.');
						// Disable the Start button when the request is successful
						document.getElementById('startButton').disabled = true;
						// Start fetching metrics
						intervalId = setInterval(fetchMetrics, 1000);
					}
				})
				.catch(error => console.error('Error starting processing:', error));
			}

			function stopProcessing() {
				
				// Send request to server to stop processing
				fetch('/stop')
				.then(response => {
					if (response.ok) {
						console.log('Process stopped.');
						// Enable the Start button when the request is successful
						document.getElementById('startButton').disabled = false;
						clearInterval(intervalId);
					}
				})
				.catch(error => console.error('Error stopping processing:', error));
			}
		</script>
	</body>
	</html>
	`
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, html)
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	metrics := map[string]interface{}{
		"uptime":             time.Since(startTime).String(),
		"bytes_written (GB)": float64(totalBytesWritten) / float64(1000*1000*1000),
	}

	// Return metrics as JSON
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metrics); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func readFileSafely(fileName string, chunkSize int, workers int, delay float64, toFile bool) {
	defer wgMain.Done()
	log.Println("Starting ReadFileSafely Go Routine")
	stopProcessing = false

	if toFile {
		utils.InitialiseOutputFile()
	}

	// Do some computation or tasks in the goroutine
	for {
		select {
		case <-stopMainChannel:
			log.Println("Stopping Main goroutine...")
			return // Exit the goroutine
		default:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)

			availableMemory := m.Sys - m.HeapReleased
			log.Printf("Available system memory: %v bytes\n", availableMemory)

			filePath := fmt.Sprintf("./log-files/%s", fileName)
			fileInfo, err := os.Stat(filePath)
			if err != nil {
				log.Fatal(err)
			}

			fileSize := uint64(fileInfo.Size())

			log.Println("Reading the file in chunks. ", fileSize)
			file, err := os.Open(filePath)
			if err != nil {
				log.Println("Error opening file:", err)
				return
			}
			defer file.Close()

			// Read the file in chunks
			buffer := make([]byte, chunkSize)
			startTime = time.Now()
			totalBytesWritten = 0
			var chunkIndex int
			for {
				n, err := file.Read(buffer)
				if err != nil && err != io.EOF {
					log.Println("Error reading file:", err)
					return
				}
				chunkIndex++
				if n == 0 || stopProcessing {
					break
				}

				wgWorkers.Add(workers)
				for i := 0; i < workers; i++ {
					go func(i int) {
						logChunk(buffer[:n], delay, toFile, i, chunkIndex)
					}(i)
				}
				wgWorkers.Wait()

				totalBytesWritten += uint64(n * workers)
				log.Println("Total Bytes Written: ", totalBytesWritten)
			}
		}
	}

}

func logChunk(chunk []byte, delay float64, toFile bool, workerNumber int, chunkIndex int) {
	defer wgWorkers.Done()
	log.Printf("Starting Wroker %d for chunk index %d\n", workerNumber, chunkIndex)
	for {
		select {
		case <-stopWorkerChannel:
			log.Println("Stopping writing worker ", workerNumber)
			return // Exit the worker goroutine
		default:
			log.Println("Outputting Log from worker ", workerNumber)
			utils.OutputLog(chunk, toFile)
			if delay > 0 {
				time.Sleep(time.Duration(delay) * time.Second)
			}
			log.Printf("Shutting Down Wroker %d for chunk index %d\n", workerNumber, chunkIndex)
			return
		}
	}
}
