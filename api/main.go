package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// Config holds runtime configuration loaded from env vars
type Config struct {
	PlaywrightURL    string
	S3Endpoint       string
	S3Bucket         string
	AWSRegion        string
	ChromaURL        string
	OllamaURL        string
	OllamaModel      string
	OllamaEmbedModel string
	DatabaseURL      string
	AutoIndex        bool
}

func loadConfig() Config {
	autoIdx := strings.ToLower(getEnv("AUTO_INDEX", "true")) == "true"
	return Config{
		PlaywrightURL:    getEnv("PLAYWRIGHT_URL", "http://playwright:3000"),
		S3Endpoint:       getEnv("S3_ENDPOINT", "http://localstack:4566"),
		S3Bucket:         getEnv("S3_BUCKET", "scraper-data"),
		AWSRegion:        getEnv("AWS_REGION", "us-east-1"),
		ChromaURL:        getEnv("CHROMA_URL", "http://chromadb:8000"),
		OllamaURL:        getEnv("OLLAMA_URL", "http://ollama:11434"),
		OllamaModel:      getEnv("OLLAMA_MODEL", "llama3.2"),
		OllamaEmbedModel: getEnv("OLLAMA_EMBED_MODEL", "nomic-embed-text"),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://scraper:scraper@postgres:5432/scraper?sslmode=disable"),
		AutoIndex:        autoIdx,
	}
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Server bundles all dependencies for handlers
type Server struct {
	cfg     Config
	storage *Storage
	ai      *AI
	scraper *Scraper
	db      *DB
}

func newServer(cfg Config) (*Server, error) {
	ctx := context.Background()

	db, err := NewDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	storage, err := NewStorage(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ai := NewAI(cfg)
	scraper := NewScraper(cfg, storage, db, ai)

	return &Server{
		cfg:     cfg,
		storage: storage,
		ai:      ai,
		scraper: scraper,
		db:      db,
	}, nil
}

func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Minute))

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		AllowCredentials: false,
	}))

	r.Get("/health", s.handleHealth)

	r.Route("/api", func(r chi.Router) {
		r.Post("/scrape", s.handleScrape)
		r.Get("/scrapes", s.handleListScrapes)
		r.Get("/scrapes/{id}", s.handleGetScrape)
		r.Get("/scrapes/{id}/pages", s.handleListPages)
		r.Get("/scrapes/{id}/files", s.handleListFiles)
		r.Get("/scrapes/{id}/file", s.handleGetFile) // ?key=...
		r.Get("/scrapes/{id}/page/{pageID}", s.handleGetPageDetail)
		r.Post("/scrapes/{id}/index", s.handleIndexScrape)
		r.Post("/query", s.handleQuery)
	})

	return r
}

func main() {
	cfg := loadConfig()
	srv, err := newServer(cfg)
	if err != nil {
		log.Fatalf("startup failure: %v", err)
	}

	addr := ":8080"
	log.Printf("api listening on %s (auto_index=%v)", addr, cfg.AutoIndex)
	if err := http.ListenAndServe(addr, srv.router()); err != nil {
		log.Fatal(err)
	}
}
