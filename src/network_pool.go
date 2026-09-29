package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- Connection pool ---

type Connection struct {
	conn     net.Conn
	lastUsed time.Time
	inUse    bool
	id       int
}

type ConnectionPool struct {
	connections []*Connection
	available   chan *Connection
	host        string
	port        string
	mu          sync.Mutex
	maxConns    int
	created     int64
	reused      int64
}

func NewConnectionPool(targetURL string, maxConns int) (*ConnectionPool, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	pool := &ConnectionPool{
		connections: make([]*Connection, 0, maxConns),
		available:   make(chan *Connection, maxConns),
		host:        host,
		port:        port,
		maxConns:    maxConns,
	}

	for i := 0; i < maxConns; i++ {
		conn, err := pool.createConnection(i)
		if err != nil {
			fmt.Printf("Warning: Failed to create connection %d: %v\n", i, err)
			continue
		}
		pool.connections = append(pool.connections, conn)
		pool.available <- conn
	}

	fmt.Printf("Created connection pool with %d connections to %s:%s\n", len(pool.connections), host, port)
	return pool, nil
}

func (p *ConnectionPool) createConnection(id int) (*Connection, error) {
	address := net.JoinHostPort(p.host, p.port)
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true)
	}

	return &Connection{
		conn:     conn,
		lastUsed: time.Now(),
		inUse:    false,
		id:       id,
	}, nil
}

func (p *ConnectionPool) Get() (*Connection, error) {
	select {
	case conn := <-p.available:
		conn.inUse = true
		conn.lastUsed = time.Now()
		atomic.AddInt64(&p.reused, 1)
		return conn, nil
	default:
		p.mu.Lock()
		defer p.mu.Unlock()

		if len(p.connections) < p.maxConns {
			conn, err := p.createConnection(len(p.connections))
			if err != nil {
				return nil, err
			}
			p.connections = append(p.connections, conn)
			conn.inUse = true
			atomic.AddInt64(&p.created, 1)
			return conn, nil
		}

		conn := <-p.available
		conn.inUse = true
		conn.lastUsed = time.Now()
		atomic.AddInt64(&p.reused, 1)
		return conn, nil
	}
}

func (p *ConnectionPool) Put(conn *Connection) {
	conn.inUse = false
	select {
	case p.available <- conn:
	default:
		conn.conn.Close()
	}
}

func (p *ConnectionPool) Close() {
	close(p.available)
	for _, conn := range p.connections {
		conn.conn.Close()
	}
}

func (p *ConnectionPool) Stats() (int64, int64) {
	return atomic.LoadInt64(&p.created), atomic.LoadInt64(&p.reused)
}

// --- Pooled HTTP client ---

type PooledHTTPClient struct {
	pool       *ConnectionPool
	host       string
	userAgent  string
	authHeader string
}

func NewPooledHTTPClient(targetURL string, maxConns int) (*PooledHTTPClient, error) {
	pool, err := NewConnectionPool(targetURL, maxConns)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse(targetURL)
	host := u.Host

	return &PooledHTTPClient{
		pool:       pool,
		host:       host,
		userAgent:  "load-generator/1.0",
		authHeader: "Basic YWRtaW46YWRtaW4=",
	}, nil
}

func (c *PooledHTTPClient) Post(path string, data []byte) (bool, int64, error) {
	conn, err := c.pool.Get()
	if err != nil {
		return false, 0, err
	}

	// Remove defer - we'll handle connection return manually
	// defer c.pool.Put(conn)

	// Test if connection is still alive before using it
	conn.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	one := make([]byte, 1)
	_, err = conn.conn.Read(one)
	conn.conn.SetReadDeadline(time.Time{}) // Reset deadline

	if err == nil {
		// If we can read data immediately, server probably closed connection
		conn.conn.Close()
		// Create new connection
		conn, err = c.pool.createConnection(conn.id)
		if err != nil {
			return false, 0, fmt.Errorf("failed to create new connection: %w", err)
		}
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		// If it's not a timeout error, connection is broken
		conn.conn.Close()
		conn, err = c.pool.createConnection(conn.id)
		if err != nil {
			return false, 0, fmt.Errorf("failed to create new connection: %w", err)
		}
	}
	// If it's a timeout error, connection is probably good to use

	request := fmt.Sprintf(
		"POST %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Type: application/json\r\n"+
			"Content-Length: %d\r\n"+
			"Authorization: %s\r\n"+
			"User-Agent: %s\r\n"+
			"Connection: keep-alive\r\n"+
			"Keep-Alive: timeout=300\r\n"+ // Tell server we want 5min timeout
			"\r\n",
		path, c.host, len(data), c.authHeader, c.userAgent,
	)

	totalBytes := int64(len(request) + len(data))

	conn.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = conn.conn.Write([]byte(request))
	if err != nil {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("write request failed: %w", err)
	}

	_, err = conn.conn.Write(data)
	if err != nil {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("write data failed: %w", err)
	}

	conn.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn.conn)

	statusLine, isPrefix, err := reader.ReadLine()
	if err != nil {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("read status failed: %w", err)
	}

	for isPrefix {
		var moreLine []byte
		moreLine, isPrefix, err = reader.ReadLine()
		if err != nil {
			conn.conn.Close()
			return false, totalBytes, err
		}
		statusLine = append(statusLine, moreLine...)
	}

	parts := strings.Split(string(statusLine), " ")
	if len(parts) < 2 {
		conn.conn.Close()
		return false, totalBytes, fmt.Errorf("invalid status line: %s", string(statusLine))
	}

	success := len(parts[1]) == 3 && parts[1][0] == '2'

	contentLength := 0
	for {
		line, isPrefix, err := reader.ReadLine()
		if err != nil {
			conn.conn.Close()
			return false, totalBytes, err
		}

		for isPrefix {
			var moreLine []byte
			moreLine, isPrefix, err = reader.ReadLine()
			if err != nil {
				conn.conn.Close()
				return false, totalBytes, err
			}
			line = append(line, moreLine...)
		}

		if len(line) == 0 {
			break
		}

		lineStr := string(line)
		if strings.HasPrefix(strings.ToLower(lineStr), "content-length:") {
			lengthStr := strings.TrimSpace(lineStr[15:])
			contentLength, _ = strconv.Atoi(lengthStr)
		}
	}

	if contentLength > 0 {
		body := make([]byte, contentLength)
		_, err = io.ReadFull(reader, body)
		if err != nil {
			conn.conn.Close()
			return false, totalBytes, err
		}
	}

	// Only put connection back in pool if everything succeeded
	if success && err == nil {
		c.pool.Put(conn)
	} else {
		conn.conn.Close()
	}

	return success, totalBytes, nil
}

func (c *PooledHTTPClient) Close() {
	c.pool.Close()
}

func (c *PooledHTTPClient) Stats() (int64, int64) {
	return c.pool.Stats()
}

func sendBatchData(client *PooledHTTPClient, data []byte, workerID int) (bool, int64) {
	endpoint := "_json_evolving_old"
	if useNDJSON {
		endpoint = "_ndjson_evolving"
	}
	path := fmt.Sprintf("/%s/%s/%s", orgId, streamName, endpoint)
	success, bytesProcessed, err := client.Post(path, data)
	if err != nil {
		fmt.Printf("Worker %d: Error sending request: %v\n", workerID, err)
		return false, bytesProcessed
	}
	if !success {
		fmt.Printf("Worker %d: Non-OK HTTP status\n", workerID)
		return false, bytesProcessed
	}
	return true, bytesProcessed
}
