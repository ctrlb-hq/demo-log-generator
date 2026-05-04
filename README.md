# log-load-generator
Load Generator for Logs written in Go

# How to Run
```
GOOS=linux GOARCH=amd64 go build -o build/benchmark ./src/main.go
```

```
./build/benchmark --concurrency=20 --batchSize=100 --baseURL=http://10.0.0.3:8445
```

# SIEM Auth Log Demo
```
go build -o build/load-gen ./src/
```

```
./build/load-gen -baseURL=http://staging.ctrlb.dev:8080 -orgId=api/default -streamName=ing_test -useVolumeTarget=false -concurrency=1 -batchSize=10 --logFile=auth_logs.json -weightConfig auth_log_weights.json -logsPerMinute=1000
```
