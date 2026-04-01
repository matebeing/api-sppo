package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RawBus struct {
	Ordem      string `json:"ordem"`
	Latitude   string `json:"latitude"`
	Longitude  string `json:"longitude"`
	DataHora   string `json:"datahora"`
	Velocidade string `json:"velocidade"`
	Linha      string `json:"linha"`
}

// ── Cache ────────────────────────────────────────────────────────────
var (
	cachedGzip []byte
	cacheLock  sync.RWMutex
)

// ── WebSocket Clients ────────────────────────────────────────────────
type wsClient struct {
	conn net.Conn
	mu   sync.Mutex
}

var (
	clients   = make(map[*wsClient]bool)
	clientsMu sync.Mutex
)

// ── Helpers ──────────────────────────────────────────────────────────

func parseBR(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// ── WebSocket (stdlib puro, zero deps) ───────────────────────────────

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsClient, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "not a websocket request", 400)
		return nil, fmt.Errorf("missing key")
	}

	// Calcula o accept key conforme RFC 6455
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-5AB9DC11D85A"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	// Hijack: toma controle da conexão TCP
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijack not supported")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	// Envia resposta de upgrade
	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	bufrw.Flush()

	return &wsClient{conn: conn}, nil
}

// Escreve um frame WebSocket (server→client, sem mask)
func (c *wsClient) writeFrame(opcode byte, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

	// Header: FIN=1 + opcode
	header := []byte{0x80 | opcode}

	length := len(data)
	switch {
	case length < 126:
		header = append(header, byte(length))
	case length < 65536:
		header = append(header, 126)
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(length))
		header = append(header, b...)
	default:
		header = append(header, 127)
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(length))
		header = append(header, b...)
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(data)
	return err
}

// Loop de leitura — detecta desconexão e responde pings
func (c *wsClient) readLoop() {
	defer func() {
		clientsMu.Lock()
		delete(clients, c)
		clientsMu.Unlock()
		c.conn.Close()
		log.Println("WS cliente desconectou")
	}()

	buf := make([]byte, 512)
	for {
		c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		n, err := c.conn.Read(buf)
		if err != nil {
			return
		}
		if n < 2 {
			continue
		}

		opcode := buf[0] & 0x0F

		switch opcode {
		case 0x8: // Close
			return
		case 0x9: // Ping → responde Pong
			c.writeFrame(0xA, nil)
		}
	}
}

// Envia dados gzip para todos os clientes conectados
func broadcast(data []byte) {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	for c := range clients {
		if err := c.writeFrame(0x2, data); err != nil { // 0x2 = binary frame
			c.conn.Close()
			delete(clients, c)
			log.Println("WS cliente removido (write error)")
		}
	}

	log.Printf("WS broadcast: %d clientes", len(clients))
}

// ── Fetch SPPO ───────────────────────────────────────────────────────

func fetchSPPO() ([]byte, error) {
	url := fmt.Sprintf("https://dados.mobilidade.rio/gps/sppo?_t=%d", time.Now().UnixNano())
	client := &http.Client{Timeout: 90 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("status: %d", resp.StatusCode)
	}

	decoder := json.NewDecoder(resp.Body)

	t, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if delim, ok := t.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("expected '[', got %v", t)
	}

	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	gz.Write([]byte("["))

	first := true
	count := 0

	for decoder.More() {
		var rb RawBus
		if err := decoder.Decode(&rb); err != nil {
			continue
		}
		if rb.Ordem == "" || rb.Latitude == "" || rb.Longitude == "" {
			continue
		}

		if !first {
			gz.Write([]byte(","))
		}
		first = false

		entry := fmt.Sprintf(
			`{"ordem":"%s","latitude":%s,"longitude":%s,"datahora":"%s","velocidade":%s,"linha":"%s"}`,
			rb.Ordem,
			strings.ReplaceAll(rb.Latitude, ",", "."),
			strings.ReplaceAll(rb.Longitude, ",", "."),
			rb.DataHora,
			strings.ReplaceAll(rb.Velocidade, ",", "."),
			rb.Linha,
		)
		gz.Write([]byte(entry))
		count++
	}

	gz.Write([]byte("]"))
	gz.Close()

	log.Printf("Fetch: %d onibus, gzip: %d bytes", count, buf.Len())
	return buf.Bytes(), nil
}

// ── Cache Updater ────────────────────────────────────────────────────

func cacheUpdater() {
	for {
		data, err := fetchSPPO()
		if err != nil {
			log.Printf("Erro fetch: %v", err)
		} else {
			cacheLock.Lock()
			cachedGzip = data
			cacheLock.Unlock()

			// Broadcast para todos os WebSocket clients
			broadcast(data)

			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("Cache OK: %d bytes | RAM: %.1f MB", len(data), float64(m.Alloc)/1024/1024)
		}
		time.Sleep(10 * time.Second)
	}
}

// ── CORS Middleware ──────────────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Main ─────────────────────────────────────────────────────────────

func main() {
	log.Println("Iniciando SPPO Proxy (HTTP + WebSocket)...")

	cachedGzip = nil
	go cacheUpdater()

	mux := http.NewServeMux()

	// HTTP endpoint (fallback / compatibilidade)
	mux.HandleFunc("/buses", func(w http.ResponseWriter, r *http.Request) {
		cacheLock.RLock()
		data := cachedGzip
		cacheLock.RUnlock()

		if data == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Write(data)
	})

	// WebSocket endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		client, err := wsUpgrade(w, r)
		if err != nil {
			log.Printf("WS upgrade erro: %v", err)
			return
		}
		log.Println("WS novo cliente conectado")

		// Registra o cliente
		clientsMu.Lock()
		clients[client] = true
		clientsMu.Unlock()

		// Envia dados atuais imediatamente
		cacheLock.RLock()
		data := cachedGzip
		cacheLock.RUnlock()

		if data != nil {
			client.writeFrame(0x2, data) // binary frame
		}

		// Bloqueia nessa goroutine lendo do cliente (detecta desconexão)
		client.readLoop()
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		clientsMu.Lock()
		n := len(clients)
		clientsMu.Unlock()
		w.Write([]byte(fmt.Sprintf("OK | %d ws clients", n)))
	})

	handler := corsMiddleware(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Escutando em 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, handler))
}
