package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
)

type Config struct {
	ChunkSize int    `json:"chunkSize"`
	Workers   int    `json:"workers"`
	FileName  string `json:"fileName"`
}

func main() {
	config := Config{
		ChunkSize: 4096,
		Workers:   3,
		FileName:  "primary.log",
	}

	file, err := os.Open("config.json")
	if err != nil {
		fmt.Println("Configuration file not found, using default values.")
	} else {
		// Decode the JSON data into a Config struct
		decoder := json.NewDecoder(file)
		err = decoder.Decode(&config)
		if err != nil {
			fmt.Println("Error decoding configuration file:", err)
			return
		}
	}
	defer file.Close()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	availableMemory := m.Sys - m.HeapReleased
	fmt.Printf("Available system memory: %v bytes\n", availableMemory)

	filePath := fmt.Sprintf("log-files/%s", config.FileName)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		log.Fatal(err)
	}

	fileSize := uint64(fileInfo.Size())
	if availableMemory > fileSize {
		fmt.Print("Good to Open")
		//Open the entire file in one go and work on it
	} else {
		file, err := os.Open(filePath)
		if err != nil {
			fmt.Println("Error opening file:", err)
			return
		}
		defer file.Close()
		// Read the file in chunks
		buffer := make([]byte, config.ChunkSize)
		var chunkIndex int

		for {
			n, err := file.Read(buffer)
			if err != nil && err != io.EOF {
				fmt.Println("Error reading file:", err)
				return
			}

			if n == 0 {
				break
			}

			// fmt.Print(string(buffer[:n]))

			logChunk(buffer, config.Workers)

			chunkIndex++
		}

		fmt.Println("Reading the file in chunks.")
	}
}

func logChunk(chunk []byte, workers int) {
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			fmt.Println(string(chunk))
		}()
	}

	wg.Wait()
}
