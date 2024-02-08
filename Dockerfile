FROM golang:1.20.1 as builder

WORKDIR /app

COPY . ./
RUN go mod download

RUN GO111MODULE=on CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o demo-log-generator


FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/demo-log-generator .

EXPOSE 8080

ENTRYPOINT ["./demo-log-generator"]
