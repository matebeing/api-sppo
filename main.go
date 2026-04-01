package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/valyala/fasthttp"
)

// RawBus is the format from the SPPO API
type RawBus struct {
	Ordem      string `json:"ordem"`
	Latitude   string `json:"latitude"`
	Longitude  string `json:"longitude"`
	DataHora   string `json:"datahora"`
	Velocidade string `json:"velocidade"`
	Linha      string `json:"linha"`
}

// Bus is the filtered/optimized format for the PWA
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
	client    = &fasthttp.Client{
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
)

func parseBR(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func fetchSPPO() ([]Bus, error) {
	url := "https://dados.mobilidade.rio/gps/sppo"
	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI(url)
	req.Header.SetMethod("GET")
	// Evita cache da prefeitura
	req.SetRequestURI(fmt.Sprintf("%s?_t=%d", url, time.Now().UnixNano()))

	if err := client.Do(req, res); err != nil {
		return nil, err
	}

	if res.StatusCode() != 200 {
		return nil, fmt.Errorf("status code error: %d", res.StatusCode())
	}

	var raw []RawBus
	if err := json.Unmarshal(res.Body(), &raw); err != nil {
		return nil, err
	}

	buses := make([]Bus, 0, len(raw))
	for _, rb := range raw {
		// Só adiciona se tiver o mínimo necessário
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
	// Loop de 7 segundos para bater o intervalo de 5-10 solicitado
	ticker := time.NewTicker(7 * time.Second)
	for range ticker.C {
		data, err := fetchSPPO()
		if err != nil {
			log.Printf("Erro ao buscar SPPO: %v", err)
			continue
		}

		cacheLock.Lock()
		cache = data
		cacheLock.Unlock()
		log.Printf("Cache atualizado: %d ônibus", len(data))
	}
}

func main() {
	app := fiber.New(fiber.Config{
		AppName: "SPPO Proxy RioNoPonto",
	})

	app.Use(logger.New())
	app.Use(cors.New())

	// Inicializa o primeiro fetch
	initial, err := fetchSPPO()
	if err == nil {
		cache = initial
	} else {
		log.Printf("Erro no fetch inicial: %v", err)
	}

	// Inicia o atualizador de cache em background
	go cacheUpdater()

	app.Get("/buses", func(c *fiber.Ctx) error {
		cacheLock.RLock()
		data := cache
		cacheLock.RUnlock()

		return c.JSON(data)
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.SendString("OK")
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Fatal(app.Listen(":" + port))
}
