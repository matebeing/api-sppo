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

// ── HTTP Transport persistente — reutiliza TCP/TLS entre fetches ─────
var transport = &http.Transport{
	MaxIdleConns:        5,
	MaxIdleConnsPerHost: 5,
	IdleConnTimeout:     120 * time.Second,
	DisableKeepAlives:   false,
	// Buffers maiores para ler o body rápido
	WriteBufferSize:     64 * 1024,
	ReadBufferSize:      64 * 1024,
}

var persistentClient = &http.Client{
	Timeout:   90 * time.Second,
	Transport: transport,
}

// ── WebSocket (stdlib puro, zero deps) ───────────────────────────────

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsClient, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "not a websocket request", 400)
		return nil, fmt.Errorf("missing key")
	}

	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-5AB9DC11D85A"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijack not supported")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	bufrw.Flush()

	return &wsClient{conn: conn}, nil
}

func (c *wsClient) writeFrame(opcode byte, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

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
		case 0x8:
			return
		case 0x9:
			c.writeFrame(0xA, nil)
		}
	}
}

// Broadcast ASSÍNCRONO — não bloqueia o fetch loop
func broadcastAsync(data []byte) {
	clientsMu.Lock()
	// Snapshot dos clientes atuais
	snapshot := make([]*wsClient, 0, len(clients))
	for c := range clients {
		snapshot = append(snapshot, c)
	}
	clientsMu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	// Envia em paralelo para todos os clientes
	var wg sync.WaitGroup
	for _, c := range snapshot {
		wg.Add(1)
		go func(client *wsClient) {
			defer wg.Done()
			if err := client.writeFrame(0x2, data); err != nil {
				clientsMu.Lock()
				delete(clients, client)
				clientsMu.Unlock()
				client.conn.Close()
			}
		}(c)
	}
	wg.Wait()

	log.Printf("WS broadcast: %d clientes", len(snapshot))
}

// ── Fetch SPPO ───────────────────────────────────────────────────────

func fetchSPPO() ([]byte, int, error) {
	url := fmt.Sprintf("https://dados.mobilidade.rio/gps/sppo?_t=%d", time.Now().UnixNano())

	resp, err := persistentClient.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, 0, fmt.Errorf("status: %d", resp.StatusCode)
	}

	decoder := json.NewDecoder(resp.Body)

	t, err := decoder.Token()
	if err != nil {
		return nil, 0, fmt.Errorf("token: %w", err)
	}
	if delim, ok := t.(json.Delim); !ok || delim != '[' {
		return nil, 0, fmt.Errorf("expected '[', got %v", t)
	}

	var buf bytes.Buffer
	buf.Grow(6 * 1024 * 1024) // Pre-aloca ~6MB para o gzip
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

		fmt.Fprintf(gz,
			`{"ordem":"%s","latitude":%s,"longitude":%s,"datahora":"%s","velocidade":%s,"linha":"%s"}`,
			rb.Ordem,
			strings.ReplaceAll(rb.Latitude, ",", "."),
			strings.ReplaceAll(rb.Longitude, ",", "."),
			rb.DataHora,
			strings.ReplaceAll(rb.Velocidade, ",", "."),
			rb.Linha,
		)
		count++
	}

	gz.Write([]byte("]"))
	gz.Close()

	return buf.Bytes(), count, nil
}

// ── Cache Updater — fetch contínuo com intervalo mínimo de 5s ────────

const minInterval = 5 * time.Second

func cacheUpdater() {
	for {
		start := time.Now()

		data, count, err := fetchSPPO()
		fetchDuration := time.Since(start)

		if err != nil {
			log.Printf("Erro fetch (%.1fs): %v", fetchDuration.Seconds(), err)
			// Em caso de erro, espera 3s antes de tentar de novo
			time.Sleep(3 * time.Second)
			continue
		}

		// 1. Atualiza o cache IMEDIATAMENTE
		cacheLock.Lock()
		cachedGzip = data
		cacheLock.Unlock()

		// 2. Broadcast assíncrono para WebSocket clients (não bloqueia)
		go broadcastAsync(data)

		// 3. GC e stats
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Printf("Ciclo: %d onibus | fetch: %.1fs | gzip: %d bytes | RAM: %.1f MB",
			count, fetchDuration.Seconds(), len(data), float64(m.Alloc)/1024/1024)

		// 4. Espera o mínimo de 5s desde o início do fetch
		//    Se o fetch demorou 8s, começa o próximo imediatamente
		//    Se demorou 2s, espera mais 3s (total = 5s)
		elapsed := time.Since(start)
		if elapsed < minInterval {
			time.Sleep(minInterval - elapsed)
		}
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

	// HTTP endpoint (fallback)
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

		clientsMu.Lock()
		clients[client] = true
		clientsMu.Unlock()

		// Envia dados atuais imediatamente na conexão
		cacheLock.RLock()
		data := cachedGzip
		cacheLock.RUnlock()
		if data != nil {
			client.writeFrame(0x2, data)
		}

		client.readLoop()
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		cacheLock.RLock()
		cacheSize := len(cachedGzip)
		cacheLock.RUnlock()

		clientsMu.Lock()
		n := len(clients)
		clientsMu.Unlock()

		w.Write([]byte(fmt.Sprintf("OK | cache: %d bytes | ws: %d clients", cacheSize, n)))
	})

	handler := corsMiddleware(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Escutando em 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, handler))
}
