package main

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed server.crt
var certPEM []byte

// ============================================================
// Token bucket for smooth rate limiting
// ============================================================

type TokenBucket struct {
	tokens    float64
	capacity  float64
	rate      float64
	lastTime  time.Time
	mu        sync.Mutex
}

func NewTokenBucket(rate, capacity float64) *TokenBucket {
	return &TokenBucket{
		tokens:   capacity,
		capacity: capacity,
		rate:     rate,
		lastTime: time.Now(),
	}
}

func (tb *TokenBucket) Take(n float64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	
	now := time.Now()
	elapsed := now.Sub(tb.lastTime).Seconds()
	tb.lastTime = now
	
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	
	if tb.tokens >= n {
		tb.tokens -= n
		return true
	}
	return false
}

// ============================================================
// Buffer pool
// ============================================================

var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 32*1024) // 32KB chunks
	},
}

func getBuffer() []byte {
	return bufferPool.Get().([]byte)
}

func putBuffer(b []byte) {
	if cap(b) == 32*1024 {
		bufferPool.Put(b)
	}
}

// ============================================================
// Control message
// ============================================================

type ControlMsg struct {
	Mode       string  `json:"mode"`
	TargetRate float64 `json:"target_rate"`
	Timestamp  int64   `json:"ts"`
}

// ============================================================
// Client
// ============================================================

type Client struct {
	addr       string
	uri        string
	reconnMin  int
	reconnMax  int
	usemTLS    bool
	
	wsConn     *websocket.Conn
	dialer     *websocket.Dialer
	
	currentMode     string
	targetRate      float64
	rateMu          sync.RWMutex
	
	bucket     *TokenBucket
	stopCh     chan struct{}
	stopped    atomic.Bool
	
	totalBytes atomic.Int64
}

func NewClient(addr, uri string, reconnMin, reconnMax int, usemTLS bool) *Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	
	tlsConfig := &tls.Config{
		RootCAs:    pool,
		ServerName: "Common Name",
	}
	
	if usemTLS {
		// Load client certificate
		cert, err := tls.LoadX509KeyPair("client.crt", "client.key")
		if err != nil {
			log.Fatalf("Failed to load client cert: %v", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	
	return &Client{
		addr:      addr,
		uri:       uri,
		reconnMin: reconnMin,
		reconnMax: reconnMax,
		usemTLS:   usemTLS,
		dialer: &websocket.Dialer{
			TLSClientConfig: tlsConfig,
			ReadBufferSize:  1024,
			WriteBufferSize: 32 * 1024,
		},
		bucket: NewTokenBucket(100*1024*1024, 10*1024*1024), // 100MB/s burst 10MB
		stopCh: make(chan struct{}),
	}
}

func (c *Client) Run() {
	log.Printf("pad-client started  target=wss://%s%s  reconnect=%d-%ds  mTLS=%v",
		c.addr, c.uri, c.reconnMin, c.reconnMax, c.usemTLS)
	
	for !c.stopped.Load() {
		c.connectAndServe()
		
		if c.stopped.Load() {
			break
		}
		
		delay := mrand.Intn(c.reconnMax-c.reconnMin+1) + c.reconnMin
		log.Printf("reconnecting in %ds...", delay)
		
		select {
		case <-c.stopCh:
			return
		case <-time.After(time.Duration(delay) * time.Second):
		}
	}
}

func (c *Client) Stop() {
	c.stopped.Store(true)
	close(c.stopCh)
	if c.wsConn != nil {
		c.wsConn.Close()
	}
}

func (c *Client) connectAndServe() {
	url := fmt.Sprintf("wss://%s%s", c.addr, c.uri)
	
	log.Printf("connecting to %s...", url)
	conn, _, err := c.dialer.Dial(url, nil)
	if err != nil {
		log.Printf("connection failed: %v", err)
		return
	}
	
	c.wsConn = conn
	defer func() {
		conn.Close()
		c.wsConn = nil
	}()
	
	log.Printf("connected")
	
	// Start sender goroutine
	done := make(chan struct{})
	go c.sender(done)
	
	// Read control messages
	for {
		select {
		case <-c.stopCh:
			return
		case <-done:
			return
		default:
		}
		
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("server closed connection")
			} else {
				log.Printf("read error: %v", err)
			}
			return
		}
		
		if msgType == websocket.TextMessage {
			var msg ControlMsg
			if err := json.Unmarshal(data, &msg); err == nil {
				c.handleControl(msg)
			}
		}
	}
}

func (c *Client) handleControl(msg ControlMsg) {
	c.rateMu.Lock()
	oldMode := c.currentMode
	c.currentMode = msg.Mode
	c.targetRate = msg.TargetRate
	c.rateMu.Unlock()
	
	if oldMode != msg.Mode {
		log.Printf("control: mode=%s  target_rate=%.0f KB/s", msg.Mode, msg.TargetRate/1024)
	}
	
	// Update token bucket rate
	c.bucket.rate = msg.TargetRate
	if c.bucket.capacity < msg.TargetRate {
		c.bucket.capacity = msg.TargetRate
	}
}

func (c *Client) sender(done chan struct{}) {
	defer close(done)
	
	ticker := time.NewTicker(10 * time.Millisecond) // 100Hz for smooth traffic
	defer ticker.Stop()
	
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}
		
		c.rateMu.RLock()
		targetRate := c.targetRate
		mode := c.currentMode
		c.rateMu.RUnlock()
		
		if targetRate <= 0 || mode != "proxy" {
			continue
		}
		
		// Calculate bytes to send this tick (10ms = 0.01s)
		bytesNeeded := targetRate * 0.01
		
		// ±10% jitter for more realistic traffic
		bytesNeeded = bytesNeeded * (0.9 + mrand.Float64()*0.2)
		
		// Use token bucket for smooth rate limiting
		if !c.bucket.Take(bytesNeeded) {
			continue
		}
		
		// Build data from pool
		sendSize := int(bytesNeeded)
		if sendSize > 32*1024 {
			sendSize = 32 * 1024
		}
		
		buf := getBuffer()
		mrand.Read(buf[:sendSize])
		
		if c.wsConn == nil {
			putBuffer(buf)
			return
		}
		
		c.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := c.wsConn.WriteMessage(websocket.BinaryMessage, buf[:sendSize])
		putBuffer(buf)
		
		if err != nil {
			log.Printf("write error: %v", err)
			return
		}
		
		c.totalBytes.Add(int64(sendSize))
	}
}

func (c *Client) Stats() (bytes int64, rate float64) {
	return c.totalBytes.Load(), c.targetRate
}

// ============================================================
// Main
// ============================================================

func main() {
	addr := flag.String("addr", "", "server address (host:port)")
	uri := flag.String("uri", "", "server URI path (e.g. /abc/def)")
	reconnMin := flag.Int("reconnect-min", 0, "min reconnect delay (seconds)")
	reconnMax := flag.Int("reconnect-max", 0, "max reconnect delay (seconds)")
	mTLS := flag.Bool("mtls", false, "use mutual TLS")
	flag.Parse()

	var missing []string
	if *addr == "" {
		missing = append(missing, "-addr")
	}
	if *uri == "" {
		missing = append(missing, "-uri")
	}
	if *reconnMin == 0 {
		missing = append(missing, "-reconnect-min")
	}
	if *reconnMax == 0 {
		missing = append(missing, "-reconnect-max")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "error: missing required parameters: %v\n", missing)
		flag.Usage()
		os.Exit(1)
	}
	if *reconnMin <= 0 || *reconnMax < *reconnMin {
		fmt.Fprintln(os.Stderr, "error: invalid reconnect range")
		os.Exit(1)
	}

	client := NewClient(*addr, *uri, *reconnMin, *reconnMax, *mTLS)
	
	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	
	go func() {
		<-sigCh
		log.Println("shutting down...")
		client.Stop()
	}()
	
	// Stats reporter
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				bytes, rate := client.Stats()
				log.Printf("stats: total=%.2f MB  current_rate=%.0f KB/s",
					float64(bytes)/(1024*1024), rate/1024)
			case <-client.stopCh:
				return
			}
		}
	}()
	
	client.Run()
	
	bytes, _ := client.Stats()
	log.Printf("total bytes sent: %.2f MB", float64(bytes)/(1024*1024))
}
