package utils

import (
	"fmt"
	"log"
	"os"
	"time"
)

var outputFile *os.File

func InitialiseOutputFile() {
	currentTime := time.Now()
	outputFileName := currentTime.Format("2006-01-02_15-04-05") + ".log"
	var err error
	outputFile, err = os.Create(fmt.Sprintf("./log-files/%s", outputFileName))
	if err != nil {
		log.Println("Error creating file:", err)
		return
	}
}

func OutputLog(chunk []byte, toFile bool) {
	if toFile {
		_, err := outputFile.Write(chunk)
		if err != nil {
			log.Println("Error writing to file:", err)
			return
		}
	} else {
		log.Println(string(chunk))
	}
}

func CloseOutputFile() {
	if outputFile != nil {
		outputFile.Close()
	}
}

func SplitByteArray(chunk []byte, n int) [][]byte {
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
