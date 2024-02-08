package main

import (
	"time"

	http "github.com/ctrlb-hq/demo-log-generator/internal"
)

var startTime1 time.Time

func main() {
	http.StartServer() // Start the server on a separate goroutine
}
