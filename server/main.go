package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed server.crt
var certPEM []byte

//go:embed server.key
var keyPEM []byte

// ============================================================
// Config with hot reload
// ============================================================

type Config struct {
	Server  ServerConfig  `toml:"server"`
	Network NetworkConfig `toml:"network"`
	Proxy   ProxyConfig   `toml:"proxy"`
	Idle    IdleConfig    `toml:"idle"`
	Auth    AuthConfig    `toml:"auth"`
}

type ServerConfig struct {
	Port     int    `toml:"port"`
	URI      string `toml:"uri"`
	Metrics  bool   `toml:"metrics"`
	LogLevel string `toml:"log_level"`
}

type NetworkConfig struct {
	Iface          string  `toml:"iface"`
	SampleInterval float64 `toml:"sample_interval"`
	SmoothWindow   float64 `toml:"smooth_window"`
}

type ProxyConfig struct {
	Threshold float64 `toml:"threshold"`
	ExitTime  int     `toml:"exit_time"`
	Ratio     float64 `toml:"ratio"`
	MaxSpeed  float64 `toml:"maxspeed"`
	MinSpeed  float64 `toml:"minspeed"`
}

type IdleConfig struct {
	SMin int `toml:"smin"`
	SMax int `toml:"smax"`
	TMin int `toml:"tmin"`
	TMax int `toml:"tmax"`
	PMin int `toml:"pmin"`
	PMax int `toml:"pmax"`
}

type AuthConfig struct {
	Enabled    bool   `toml:"enabled"`
	ClientCert string `toml:"client_cert"`
}

type ConfigManager struct {
	path      string
	config    atomic.Value
	watcher   *fsnotify.Watcher
	hasMinSpeed bool
	hasSmoothWindow bool
}

func NewConfigManager(path string) (*ConfigManager, error) {
	cm := &ConfigManager{path: path}
	if err := cm.load(); err != nil {
		return nil, err
	}
	
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	cm.watcher = watcher
	
	go cm.watch()
	if err := watcher.Add(path); err != nil {
		return nil, err
	}
	
	return cm, nil
}

func (cm *ConfigManager) load() error {
	var cfg Config
	md, err := toml.DecodeFile(cm.path, &cfg)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}
	
	hasMinSpeed := md.IsDefined("proxy", "minspeed")
	hasSmoothWindow := md.IsDefined("network", "smooth_window")
	
	if !hasSmoothWindow {
		cfg.Network.SmoothWindow = 1.5
	}
	
	if err := cm.validate(cfg, hasMinSpeed); err != nil {
		return err
	}
	
	cm.hasMinSpeed = hasMinSpeed
	cm.hasSmoothWindow = hasSmoothWindow
	cm.config.Store(&cfg)
	return nil
}

func (cm *ConfigManager) validate(cfg Config, hasMinSpeed bool) error {
	if cfg.Server.Port <= 0 {
		return fmt.Errorf("server.port is required")
	}
	if cfg.Server.URI == "" {
		return fmt.Errorf("server.uri is required")
	}
	if cfg.Network.Iface == "" {
		return fmt.Errorf("network.iface is required")
	}
	if cfg.Network.SampleInterval <= 0 {
		return fmt.Errorf("network.sample_interval must be > 0")
	}
	if cfg.Proxy.Threshold <= 0 {
		return fmt.Errorf("proxy.threshold must be > 0")
	}
	if cfg.Proxy.ExitTime <= 0 {
		return fmt.Errorf("proxy.exit_time must be > 0")
	}
	if cfg.Proxy.Ratio <= 1 {
		return fmt.Errorf("proxy.ratio must be > 1")
	}
	if cfg.Proxy.MaxSpeed <= 0 {
		return fmt.Errorf("proxy.maxspeed must be > 0")
	}
	if hasMinSpeed && cfg.Proxy.MinSpeed < 0 {
		return fmt.Errorf("proxy.minspeed must be >= 0")
	}
	if hasMinSpeed && cfg.Proxy.MinSpeed > cfg.Proxy.MaxSpeed {
		return fmt.Errorf("proxy.minspeed must be <= proxy.maxspeed")
	}
	if cfg.Idle.SMin <= 0 || cfg.Idle.SMax <= 0 || cfg.Idle.SMin > cfg.Idle.SMax {
		return fmt.Errorf("idle.smin/smax invalid")
	}
	if cfg.Idle.TMin <= 0 || cfg.Idle.TMax <= 0 || cfg.Idle.TMin > cfg.Idle.TMax {
		return fmt.Errorf("idle.tmin/tmax invalid")
	}
	if cfg.Idle.PMin <= 0 || cfg.Idle.PMax <= 0 || cfg.Idle.PMin > cfg.Idle.PMax {
		return fmt.Errorf("idle.pmin/pmax invalid")
	}
	return nil
}

func (cm *ConfigManager) watch() {
	for {
		select {
		case event, ok := <-cm.watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				log.Println("Config file changed, reloading...")
				if err := cm.load(); err != nil {
					log.Printf("Config reload failed: %v", err)
				} else {
					log.Println("Config reloaded successfully")
				}
			}
		case err, ok := <-cm.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error: %v", err)
		}
	}
}

func (cm *ConfigManager) Get() *Config {
	return cm.config.Load().(*Config)
}

// ============================================================
// Cross-platform traffic collector
// ============================================================

type TrafficCollector interface {
	GetStats(iface string) (rx, tx uint64, err error)
}

type LinuxCollector struct{}

func (c *LinuxCollector) GetStats(iface string) (rx, tx uint64, err error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	prefix := iface + ":"
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		colonIdx := strings.Index(line, ":")
		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 9 {
			return 0, 0, fmt.Errorf("unexpected format for %s", iface)
		}
		rx, err = strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse rx_bytes: %w", err)
		}
		tx, err = strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse tx_bytes: %w", err)
		}
		return rx, tx, nil
	}
	return 0, 0, fmt.Errorf("interface %s not found", iface)
}

type DummyCollector struct {
	rx, tx uint64
}

func (c *DummyCollector) GetStats(iface string) (rx, tx uint64, err error) {
	// Fallback for non-Linux platforms
	return c.rx, c.tx, nil
}

func NewTrafficCollector() TrafficCollector {
	if _, err := os.Stat("/proc/net/dev"); err == nil {
		return &LinuxCollector{}
	}
	return &DummyCollector{}
}

// ============================================================
// Token bucket for rate limiting
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
	
	// Add tokens
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
// Metrics
// ============================================================

var (
	paddingBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "padding_bytes_total",
			Help: "Total padding bytes received",
		},
		[]string{"client"},
	)
	controlMessagesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "control_messages_total",
			Help: "Total control messages sent",
		},
		[]string{"type"},
	)
	clientsConnected = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "clients_connected",
			Help: "Number of connected clients",
		},
	)
	modeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "current_mode",
			Help: "Current operating mode (0=idle, 1=proxy)",
		},
		[]string{},
	)
	targetRateGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "target_rate_bytes_per_second",
			Help: "Current target padding rate",
		},
	)
)

func init() {
	prometheus.MustRegister(paddingBytesTotal, controlMessagesTotal, 
		clientsConnected, modeGauge, targetRateGauge)
}

// ============================================================
// Shared state
// ============================================================

const (
	ModeIdle  = 0
	ModeProxy = 1
)

type ControlMsg struct {
	Mode       string  `json:"mode"`
	TargetRate float64 `json:"target_rate"`
	Timestamp  int64   `json:"ts"`
}

type SharedState struct {
	mu          sync.RWMutex
	mode        int
	targetRate  float64
	padReceived int64
	clients     map[string]*ClientConn
}

type ClientConn struct {
	ID       string
	Conn     *websocket.Conn
	Bucket   *TokenBucket
	SendChan chan []byte
}

func (s *SharedState) GetControlMsg() ControlMsg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	modeStr := "idle"
	if s.mode == ModeProxy {
		modeStr = "proxy"
	}
	return ControlMsg{
		Mode:       modeStr,
		TargetRate: s.targetRate,
		Timestamp:  time.Now().Unix(),
	}
}

func (s *SharedState) Set(mode int, rate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	s.targetRate = rate
	modeGauge.WithLabelValues().Set(float64(mode))
	targetRateGauge.Set(rate)
}

func (s *SharedState) AddReceived(n int64) {
	atomic.AddInt64(&s.padReceived, n)
}

func (s *SharedState) GetReceived() int64 {
	return atomic.LoadInt64(&s.padReceived)
}

func (s *SharedState) RegisterClient(id string, conn *websocket.Conn) *ClientConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	client := &ClientConn{
		ID:       id,
		Conn:     conn,
		Bucket:   NewTokenBucket(100*1024*1024, 10*1024*1024), // 100MB/s burst 10MB
		SendChan: make(chan []byte, 100),
	}
	s.clients[id] = client
	clientsConnected.Set(float64(len(s.clients)))
	return client
}

func (s *SharedState) UnregisterClient(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, id)
	clientsConnected.Set(float64(len(s.clients)))
}

func (s *SharedState) BroadcastControl(msg ControlMsg) {
	s.mu.RLock()
	clients := make([]*ClientConn, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()
	
	data, _ := json.Marshal(msg)
	for _, c := range clients {
		select {
		case c.SendChan <- data:
		default:
			// Channel full, skip this client
		}
	}
}

// ============================================================
// Sampler with hot config reload
// ============================================================

type Sampler struct {
	cm         *ConfigManager
	state      *SharedState
	collector  TrafficCollector

	ewmaAlpha float64

	prevRx   uint64
	prevTx   uint64
	prevPad  int64
	prevTime time.Time

	smoothIn  float64
	smoothOut float64
	smoothPad float64

	belowSince    time.Time
	belowTracking bool
}

func (s *Sampler) Run() {
	for {
		cfg := s.cm.Get()
		interval := time.Duration(cfg.Network.SampleInterval * float64(time.Second))
		
		if s.ewmaAlpha == 0 {
			s.ewmaAlpha = 1.0 - math.Exp(-cfg.Network.SampleInterval/cfg.Network.SmoothWindow)
			log.Printf("sampler: ewma alpha=%.4f", s.ewmaAlpha)
			
			rx, tx, err := s.collector.GetStats(cfg.Network.Iface)
			if err != nil {
				log.Printf("sampler: initial read failed: %v", err)
				time.Sleep(interval)
				continue
			}
			s.prevRx = rx
			s.prevTx = tx
			s.prevPad = s.state.GetReceived()
			s.prevTime = time.Now()
		}
		
		time.Sleep(interval)
		s.sample()
	}
}

func (s *Sampler) sample() {
	cfg := s.cm.Get()
	hasMinSpeed := s.cm.hasMinSpeed
	
	now := time.Now()
	rx, tx, err := s.collector.GetStats(cfg.Network.Iface)
	if err != nil {
		if cfg.Server.LogLevel == "debug" {
			log.Printf("sampler: read error: %v", err)
		}
		return
	}
	pad := s.state.GetReceived()

	dt := now.Sub(s.prevTime).Seconds()
	if dt <= 0 {
		return
	}

	rawIn := float64(rx-s.prevRx) / dt
	rawOut := float64(tx-s.prevTx) / dt
	rawPad := float64(pad-s.prevPad) / dt

	if rx < s.prevRx {
		rawIn = 0
	}
	if tx < s.prevTx {
		rawOut = 0
	}
	if pad < s.prevPad {
		rawPad = 0
	}

	s.prevRx = rx
	s.prevTx = tx
	s.prevPad = pad
	s.prevTime = now

	a := s.ewmaAlpha
	if s.smoothIn == 0 {
		s.smoothIn = rawIn
		s.smoothOut = rawOut
		s.smoothPad = rawPad
	} else {
		s.smoothIn = a*rawIn + (1-a)*s.smoothIn
		s.smoothOut = a*rawOut + (1-a)*s.smoothOut
		s.smoothPad = a*rawPad + (1-a)*s.smoothPad
	}

	organicIn := s.smoothIn - s.smoothPad
	if organicIn < 0 {
		organicIn = 0
	}

	thresholdBps := cfg.Proxy.Threshold * 1024
	maxBps := cfg.Proxy.MaxSpeed * 1024
	minBps := float64(0)
	if hasMinSpeed {
		minBps = cfg.Proxy.MinSpeed * 1024
	}

	currentMsg := s.state.GetControlMsg()

	if organicIn > thresholdBps {
		s.belowTracking = false

		targetInbound := organicIn * cfg.Proxy.Ratio
		paddingNeeded := targetInbound - organicIn
		if paddingNeeded < minBps {
			paddingNeeded = minBps
		}
		if paddingNeeded > maxBps {
			paddingNeeded = maxBps
		}
		if paddingNeeded < 0 {
			paddingNeeded = 0
		}

		if currentMsg.Mode != "proxy" {
			log.Printf("sampler: → PROXY mode  organic_in=%.0f KB/s  target_pad=%.0f KB/s",
				organicIn/1024, paddingNeeded/1024)
			controlMessagesTotal.WithLabelValues("mode_change").Inc()
		}
		s.state.Set(ModeProxy, paddingNeeded)
	} else {
		if currentMsg.Mode == "proxy" {
			if !s.belowTracking {
				s.belowTracking = true
				s.belowSince = now
			}
			elapsed := now.Sub(s.belowSince).Seconds()
			if elapsed >= float64(cfg.Proxy.ExitTime) {
				log.Printf("sampler: → IDLE mode  (below threshold for %.0fs)", elapsed)
				s.belowTracking = false
				s.state.Set(ModeIdle, 0)
				controlMessagesTotal.WithLabelValues("mode_change").Inc()
			}
		} else {
			s.belowTracking = false
			s.state.Set(ModeIdle, 0)
		}
	}

	// Broadcast control to all clients
	msg := s.state.GetControlMsg()
	s.state.BroadcastControl(msg)

	if cfg.Server.LogLevel == "debug" {
		log.Printf("sampler: [%s]  in=%.0f KB/s  organic_in=%.0f KB/s  pad_in=%.0f KB/s  target_rate=%.0f KB/s",
			msg.Mode, s.smoothIn/1024, organicIn/1024, s.smoothPad/1024, msg.TargetRate/1024)
	}
}

// ============================================================
// WebSocket handler
// ============================================================

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type handler struct {
	cm    *ConfigManager
	state *SharedState
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	case "/metrics":
		promhttp.Handler().ServeHTTP(w, r)
	case h.cm.Get().Server.URI:
		h.handleWebSocket(w, r)
	default:
		replyNotFound(w)
	}
}

func replyNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   "Endpoint does not exist.",
	})
}

func (h *handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	cfg := h.cm.Get()
	
	// Check client cert if auth enabled
	if cfg.Auth.Enabled && cfg.Auth.ClientCert != "" {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "Client certificate required", http.StatusUnauthorized)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	clientID := r.RemoteAddr
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		clientID = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	
	client := h.state.RegisterClient(clientID, conn)
	defer h.state.UnregisterClient(clientID)
	
	log.Printf("client connected: %s", clientID)
	
	// Start goroutines for read and write
	done := make(chan struct{})
	
	// Writer goroutine
	go func() {
		defer close(done)
		for {
			select {
			case data := <-client.SendChan:
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					return
				}
				controlMessagesTotal.WithLabelValues("sent").Inc()
			case <-done:
				return
			}
		}
	}()
	
	// Reader goroutine - receives padding data from client
	go func() {
		defer close(done)
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				log.Printf("client disconnected: %s", clientID)
				return
			}
			
			if msgType == websocket.BinaryMessage {
				n := len(data)
				h.state.AddReceived(int64(n))
				paddingBytesTotal.WithLabelValues(clientID).Add(float64(n))
				
				if cfg.Server.LogLevel == "debug" {
					log.Printf("received %d bytes from %s", n, clientID)
				}
			}
		}
	}()
	
	// Send initial control message
	msg := h.state.GetControlMsg()
	data, _ := json.Marshal(msg)
	client.SendChan <- data
	
	<-done
	log.Printf("client disconnected: %s", clientID)
}

// ============================================================
// Main
// ============================================================

func main() {
	configPath := flag.String("c", "", "path to config file (TOML)")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: -c config_path is required")
		os.Exit(1)
	}

	cm, err := NewConfigManager(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg := cm.Get()
	state := &SharedState{
		mode:    ModeIdle,
		clients: make(map[string]*ClientConn),
	}

	sampler := &Sampler{
		cm:        cm,
		state:     state,
		collector: NewTrafficCollector(),
	}
	go sampler.Run()

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Fatalf("failed to load embedded cert: %v", err)
	}

	var tlsConfig *tls.Config
	if cfg.Auth.Enabled && cfg.Auth.ClientCert != "" {
		caCert, err := os.ReadFile(cfg.Auth.ClientCert)
		if err != nil {
			log.Fatalf("failed to read client CA: %v", err)
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caCert)
		
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    caPool,
		}
	} else {
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	h := &handler{cm: cm, state: state}

	srv := &http.Server{
		Addr:      fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:   h,
		TLSConfig: tlsConfig,
	}

	log.Printf("pad-server starting on :%d", cfg.Server.Port)
	log.Printf("  uri=%s  iface=%s  metrics=%v", cfg.Server.URI, cfg.Network.Iface, cfg.Server.Metrics)
	log.Printf("  proxy: threshold=%.0fKB/s  exit=%ds  ratio=%.1f  max=%.0fKB/s",
		cfg.Proxy.Threshold, cfg.Proxy.ExitTime, cfg.Proxy.Ratio, cfg.Proxy.MaxSpeed)
	log.Printf("  auth: enabled=%v", cfg.Auth.Enabled)

	log.Fatal(srv.ListenAndServeTLS("", ""))
}
