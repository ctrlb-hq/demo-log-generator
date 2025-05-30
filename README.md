# log-load-generator
Load Generator for Logs written in Go

# How to Run
```
GOOS=linux GOARCH=amd64 go build -o build/benchmark ./src/main.go
```

```
./build/benchmark --concurrency=20 --batchSize=100 --baseURL=http://10.0.0.3:8445
```
