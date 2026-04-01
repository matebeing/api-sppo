package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	cachedJSON []byte
	cacheLock  sync.RWMutex
)

func parseBR(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// fetchSPPO usa streaming JSON: lê 1 objeto por vez, transforma e escreve
// direto num buffer. Nunca segura 488k structs na memória.
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

	// Lê o '[' inicial do array
	t, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if delim, ok := t.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("expected '[', got %v", t)
	}

	// Buffer onde montamos o JSON de saída
	var buf bytes.Buffer
	buf.WriteByte('[')

	first := true
	count := 0

	// Processa um objeto por vez — memória constante
	for decoder.More() {
		var rb RawBus
		if err := decoder.Decode(&rb); err != nil {
			continue // Pula registros malformados
		}

		if rb.Ordem == "" || rb.Latitude == "" || rb.Longitude == "" {
			continue
		}

		if !first {
			buf.WriteByte(',')
		}
		first = false

		// Escreve o JSON transformado direto no buffer
		fmt.Fprintf(&buf,
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

	buf.WriteByte(']')

	log.Printf("Processados %d onibus, %d bytes", count, buf.Len())
	return buf.Bytes(), nil
}

func cacheUpdater() {
	for {
		data, err := fetchSPPO()
		if err != nil {
			log.Printf("Erro fetch: %v", err)
		} else {
			cacheLock.Lock()
			cachedJSON = data
			cacheLock.Unlock()
			log.Printf("Cache atualizado: %d bytes", len(data))
		}
		time.Sleep(10 * time.Second)
	}
}

func main() {
	log.Println("Iniciando SPPO Proxy...")

	cachedJSON = []byte("[]")
	go cacheUpdater()

	mux := http.NewServeMux()

	mux.HandleFunc("/buses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		cacheLock.RLock()
		data := cachedJSON
		cacheLock.RUnlock()

		w.Write(data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Escutando em 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, mux))
}
