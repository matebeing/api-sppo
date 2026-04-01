package main

import (
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

type Bus struct {
	ID    string  `json:"ordem"`
	Lat   float64 `json:"latitude"`
	Long  float64 `json:"longitude"`
	Time  string  `json:"datahora"`
	Speed float64 `json:"velocidade"`
	Line  string  `json:"linha"`
}

var (
	// Cacheamos os bytes JSON prontos em vez de []Bus — evita re-encoding por request
	cachedJSON []byte
	cacheLock  sync.RWMutex
)

func parseBR(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func fetchSPPO() ([]byte, error) {
	url := fmt.Sprintf("https://dados.mobilidade.rio/gps/sppo?_t=%d", time.Now().UnixNano())

	// Client sem timeout global — usamos só timeout de conexão
	client := &http.Client{Timeout: 90 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status: %d", resp.StatusCode)
	}

	// Lê todo o body de uma vez (evita timeout durante streaming decode)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Decodifica pra filtrar dados inválidos
	var raw []RawBus
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	// Filtra e converte
	buses := make([]Bus, 0, len(raw))
	for _, rb := range raw {
		if rb.Ordem != "" && rb.Latitude != "" && rb.Longitude != "" {
			buses = append(buses, Bus{
				ID:    rb.Ordem,
				Lat:   parseBR(rb.Latitude),
				Long:  parseBR(rb.Longitude),
				Time:  rb.DataHora,
				Speed: parseBR(rb.Velocidade),
				Line:  rb.Linha,
			})
		}
	}

	// Serializa uma vez — esse []byte é o que servimos nos requests
	result, err := json.Marshal(buses)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	// Libera memória dos slices intermediários
	raw = nil
	buses = nil

	return result, nil
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
			log.Printf("Cache OK: %d bytes", len(data))
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

		// Escreve os bytes JSON direto — sem encoding, sem alocação
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
