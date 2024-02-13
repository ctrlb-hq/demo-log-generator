docker build -t log-generator:latest .
docker run --log-driver=fluentd --log-opt tag="docker.{{.ID}}" -p 8080:8080 -v ./log-files:/root/log-files log-generator
