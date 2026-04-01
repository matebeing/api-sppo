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

// ── HTTP Transport otimizado ─────────────────────────────────────────
// Keep-alive persistente, buffers grandes, conexão reutilizada
var transport = &http.Transport{
	MaxIdleConns:        10,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     300 * time.Second,
	DisableKeepAlives:   false,
	WriteBufferSize:     128 * 1024,
	ReadBufferSize:      128 * 1024,
	ForceAttemptHTTP2:   true,
}

var persistentClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: transport,
}

// ── WebSocket ────────────────────────────────────────────────────────

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

	// TCP tuning na conexão WebSocket
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetWriteBuffer(256 * 1024)
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

	c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))

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

	// Envia header + payload em uma única syscall (Writev)
	full := append(header, data...)
	_, err := c.conn.Write(full)
	return err
}

func (c *wsClient) readLoop() {
	defer func() {
		clientsMu.Lock()
		delete(clients, c)
		clientsMu.Unlock()
		c.conn.Close()
	}()

	buf := make([]byte, 512)
	for {
		c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := c.conn.Read(buf)
		if err != nil {
			return
		}
		if n < 2 {
			continue
		}
		opcode := buf[0] & 0x0F
		if opcode == 0x8 {
			return
		}
		if opcode == 0x9 {
			c.writeFrame(0xA, nil) // Pong
		}
	}
}

// Broadcast paralelo para todos os WS clients
func broadcastAsync(data []byte) {
	clientsMu.Lock()
	snapshot := make([]*wsClient, 0, len(clients))
	for c := range clients {
		snapshot = append(snapshot, c)
	}
	clientsMu.Unlock()

	if len(snapshot) == 0 {
		return
	}

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
}

// ── Fetch SPPO (streaming, zero alocação extra) ─────────────────────

func fetchSPPO(pipeline int) ([]byte, int, time.Duration, error) {
	start := time.Now()
	url := fmt.Sprintf("https://dados.mobilidade.rio/gps/sppo?_t=%d", time.Now().UnixNano())

	resp, err := persistentClient.Get(url)
	if err != nil {
		return nil, 0, time.Since(start), fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, 0, time.Since(start), fmt.Errorf("status: %d", resp.StatusCode)
	}

	decoder := json.NewDecoder(resp.Body)
	t, err := decoder.Token()
	if err != nil {
		return nil, 0, time.Since(start), fmt.Errorf("token: %w", err)
	}
	if delim, ok := t.(json.Delim); !ok || delim != '[' {
		return nil, 0, time.Since(start), fmt.Errorf("expected '['")
	}

	var buf bytes.Buffer
	buf.Grow(6 * 1024 * 1024)
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

	return buf.Bytes(), count, time.Since(start), nil
}

// ── Pipeline Duplo — 2 goroutines buscando continuamente ─────────────
// Pipeline 0:  [====fetch====]              [====fetch====]
// Pipeline 1:       [====fetch====]              [====fetch====]
// Cache:       ─────^update──^update────────^update──^update─────
//
// Resultado: cache atualiza com o DOBRO da frequência

func cacheUpdater() {
	const numPipelines = 2

	for i := 0; i < numPipelines; i++ {
		go func(id int) {
			// Staggers os pipelines pra não baterem juntos na API
			time.Sleep(time.Duration(id) * 3 * time.Second)

			for {
				data, count, duration, err := fetchSPPO(id)

				if err != nil {
					log.Printf("[P%d] Erro (%.1fs): %v", id, duration.Seconds(), err)
					time.Sleep(1 * time.Second) // Retry rápido em erro
					continue
				}

				// Atualiza cache IMEDIATAMENTE
				cacheLock.Lock()
				cachedGzip = data
				cacheLock.Unlock()

				// Push para WS clients em paralelo (não bloqueia próximo fetch)
				go broadcastAsync(data)

				log.Printf("[P%d] %d onibus | %.1fs | %d bytes",
					id, count, duration.Seconds(), len(data))

				// ZERO sleep — começa próximo fetch imediatamente
				// O "intervalo" é o tempo que o fetch demora (~7-10s)
				// Com 2 pipelines, cache atualiza a cada ~3-5s
			}
		}(i)
	}

	// GC periódico suave (a cada 30s, sem travar o fetch)
	go func() {
		for {
			time.Sleep(30 * time.Second)
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("RAM: %.1f MB | GC cycles: %d", float64(m.Alloc)/1024/1024, m.NumGC)
		}
	}()

	select {} // Bloqueia forever
}

// ── CORS ─────────────────────────────────────────────────────────────

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
	log.Println("SPPO Proxy v3 — dual pipeline, zero delay")

	cachedGzip = nil
	go cacheUpdater()

	mux := http.NewServeMux()

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
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		client, err := wsUpgrade(w, r)
		if err != nil {
			log.Printf("WS upgrade erro: %v", err)
			return
		}
		log.Println("WS cliente conectou")

		clientsMu.Lock()
		clients[client] = true
		clientsMu.Unlock()

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
		w.Write([]byte(fmt.Sprintf("OK | cache: %d bytes | ws: %d", cacheSize, n)))
	})

	handler := corsMiddleware(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Porta: %s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, handler))
}
