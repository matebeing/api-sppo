package main

import (
	"encoding/json"
	"fmt"
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
	cache     []Bus
	cacheLock sync.RWMutex
	httpClient = &http.Client{
		Timeout: 15 * time.Second,
	}
)

func parseBR(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func fetchSPPO() ([]Bus, error) {
	url := fmt.Sprintf("https://dados.mobilidade.rio/gps/sppo?_t=%d", time.Now().UnixNano())

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status code: %d", resp.StatusCode)
	}

	var raw []RawBus
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
	}

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

	return buses, nil
}

func cacheUpdater() {
	for {
		data, err := fetchSPPO()
		if err != nil {
			log.Printf("Erro fetch: %v", err)
		} else {
			cacheLock.Lock()
			cache = data
			cacheLock.Unlock()
			log.Printf("Cache: %d onibus", len(data))
		}
		time.Sleep(7 * time.Second)
	}
}

func main() {
	log.Println("Iniciando SPPO Proxy...")

	cache = []Bus{}
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
		data := cache
		cacheLock.RUnlock()

		json.NewEncoder(w).Encode(data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Escutando em 0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, mux))
}
