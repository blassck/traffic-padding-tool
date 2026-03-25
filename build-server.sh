cd /root
mkdir -p pad-server-src
cat > pad-server-src/main.go << 'EOF'
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	port       = flag.Int("port", 8443, "Server port")
	uri        = flag.String("uri", "/pad", "WebSocket URI")
	metrics    = flag.Bool("metrics", true, "Enable Prometheus metrics")
	certFile   = flag.String("cert", "server.crt", "TLS certificate")
	keyFile    = flag.String("key", "server.key", "TLS key")
)

var (
	paddingBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "padding_bytes_total",
			Help: "Total padding bytes sent",
		},
		[]string{"client"},
	)
	clientsConnected = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "clients_connected",
			Help: "Number of connected clients",
		},
	)
)

func init() {
	prometheus.MustRegister(paddingBytes)
	prometheus.MustRegister(clientsConnected)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Client struct {
	conn   *websocket.Conn
	id     string
	send   chan []byte
	mu     sync.Mutex
}

var clients = make(map[string]*Client)
var clientsMu sync.RWMutex

func generateClientID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	clientID := generateClientID()
	client := &Client{
		conn: conn,
		id:   clientID,
		send: make(chan []byte, 256),
	}

	clientsMu.Lock()
	clients[clientID] = client
	clientsMu.Unlock()
	clientsConnected.Inc()

	log.Printf("Client %s connected from %s", clientID, r.RemoteAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.writePump()
	go client.readPump(ctx)

	<-ctx.Done()

	clientsMu.Lock()
	delete(clients, clientID)
	clientsMu.Unlock()
	clientsConnected.Dec()
	conn.Close()
	log.Printf("Client %s disconnected", clientID)
}

func (c *Client) readPump(ctx context.Context) {
	defer func() {
		cancel := ctx.Value("cancel").(context.CancelFunc)
		cancel()
	}()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}
		paddingBytes.WithLabelValues(c.id).Add(float64(len(message)))
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(5 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.WriteMessage(websocket.TextMessage, message)

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func main() {
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc(*uri, handleWebSocket)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/", healthHandler)
	
	if *metrics {
		mux.Handle("/metrics", promhttp.Handler())
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("Failed to load certificates: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	}

	server := &http.Server{
		Addr:      fmt.Sprintf(":%d", *port),
		Handler:   mux,
		TLSConfig: tlsConfig,
	}

	go func() {
		log.Printf("Server starting on :%d", *port)
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
EOF

cat > pad-server-src/go.mod << 'EOF'
module pad-server

go 1.19

require (
	github.com/gorilla/websocket v1.5.0
	github.com/prometheus/client_golang v1.17.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/prometheus/client_model v0.4.1-0.20230718164431-9a2bf3000d16 // indirect
	github.com/prometheus/common v0.44.0 // indirect
	github.com/prometheus/procfs v0.11.1 // indirect
	golang.org/x/sys v0.11.0 // indirect
	google.golang.org/protobuf v1.31.0 // indirect
)
EOF

cd pad-server-src
go mod tidy
go build -o /root/pad-server .
echo "Build complete"
