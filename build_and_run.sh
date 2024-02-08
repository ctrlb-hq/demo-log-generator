docker build -t log-generator:latest .
docker run -p 8080:8080 -v ./log-files:/root/log-files log-generator
