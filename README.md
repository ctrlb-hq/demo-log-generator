# log-load-generator
Load Generator for Logs written in Go

## Build
```
go build -o build/load-gen ./src/
```

Cross-compile for Linux:
```
GOOS=linux GOARCH=amd64 go build -o build/load-gen ./src/main.go
```

## AWS WAF Log Mode
Generate realistic AWS WAF v2 logs and send them to a ctrlb stream:
```
./build/load-gen \
  -logFormat=waf \
  -logFile=templates/waf_logs.json \
  -weightConfig=templates/waf_log_weights.json \
  -baseURL=http://staging.ctrlb.dev:8080 \
  -orgId=api/default \
  -streamName=waf_stream \
  -concurrency=2 \
  -batchSize=50 \
  -logsPerMinute=1000
```

## Text Log Mode (default)
Send flat text-based log lines (k8s, custom formats):
```
./build/load-gen \
  -baseURL=http://10.0.0.3:8445 \
  -logFile=templates/prod_log_k8_logs.json \
  -concurrency=20 \
  -batchSize=100
```

With weighted category selection:
```
./build/load-gen \
  -logFile=my_templates.json \
  -weightConfig=my_weights.json \
  -baseURL=http://staging.ctrlb.dev:8080 \
  -orgId=api/default \
  -streamName=ing_test \
  -concurrency=1 \
  -batchSize=10 \
  -logsPerMinute=1000
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-logFormat` | `text` | `text` (flat log lines) or `waf` (AWS WAF JSON) |
| `-logFile` | `templates/prod_log_k8_logs.json` | Path to template JSON file |
| `-weightConfig` | (none) | Path to category weight config (required for `waf` mode) |
| `-weightSelection` | `quota` | `quota` (steady mix) or `random` (i.i.d. draws) |
| `-baseURL` | `http://10.0.0.3:8445` | Target ingest endpoint |
| `-orgId` | `ctrlb` | Organization ID |
| `-streamName` | `test_stream` | Stream/dataset name |
| `-concurrency` | `20` | Number of concurrent workers |
| `-batchSize` | `1000` | Logs per batch |
| `-logsPerMinute` | `0` | Rate limit (0 = unlimited) |
| `-targetVolumeGB` | `0` | Volume target in GB |
| `-useVolumeTarget` | `false` | Use volume-based instead of time-based termination |
