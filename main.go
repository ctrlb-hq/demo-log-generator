package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
)

const (
	chunkSize   = 4096 // Adjust this based on your needs
	concurrency = 3    // Number of goroutines to run in parallel for writing
)

func main() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	availableMemory := m.Sys - m.HeapReleased
	fmt.Printf("Available system memory: %v bytes\n", availableMemory)

	fileInfo, err := os.Stat("primary.log")
	if err != nil {
		log.Fatal(err)
	}

	fileSize := uint64(fileInfo.Size())
	if availableMemory > fileSize {
		fmt.Print("Good to Open")
		//Open the entire file in one go and work on it
	} else {
		file, err := os.Open("primary.log")
		if err != nil {
			fmt.Println("Error opening file:", err)
			return
		}
		defer file.Close()
		// Read the file in chunks
		buffer := make([]byte, chunkSize)
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

			fmt.Print(string(buffer[:n]))
			// Write the chunk to the same output file
			logChunk(buffer)

			chunkIndex++
		}

		fmt.Println("Reading the file in chunks.")
	}
}

func logChunk(chunk []byte) {
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()

			log.Println(string(chunk))
		}()
	}

	wg.Wait()
}
