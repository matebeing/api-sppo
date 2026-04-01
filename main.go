package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

var (
	cachedGzip []byte
	cacheLock  sync.RWMutex
)

func parseBR(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

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

	// Escreve direto num gzip writer → buffer comprimido
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

func cacheUpdater() {
	for {
		data, err := fetchSPPO()
		if err != nil {
			log.Printf("Erro fetch: %v", err)
		} else {
			cacheLock.Lock()
			cachedGzip = data
			cacheLock.Unlock()

			// Força GC após trocar o cache pra liberar memória
			runtime.GC()

			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("Cache OK: %d bytes gzip | RAM: %.1f MB", len(data), float64(m.Alloc)/1024/1024)
		}
		time.Sleep(10 * time.Second)
	}
}

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

func main() {
	log.Println("Iniciando SPPO Proxy...")

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
		w.Write(data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	handler := corsMiddleware(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Escutando em 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, handler))
}
