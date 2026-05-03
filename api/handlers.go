package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/scrape  { url, depth, format }
func (s *Server) handleScrape(w http.ResponseWriter, r *http.Request) {
	var req ScrapeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeErr(w, http.StatusBadRequest, "url is required")
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeErr(w, http.StatusBadRequest, "url must start with http:// or https://")
		return
	}
	if req.Depth < 0 {
		req.Depth = 0
	}
	if req.Depth > 5 {
		req.Depth = 5 // safety cap
	}
	if req.Format == "" {
		req.Format = "text"
	}
	switch req.Format {
	case "text", "html", "screenshot":
	default:
		writeErr(w, http.StatusBadRequest, "format must be text|html|screenshot")
		return
	}

	now := time.Now().UTC()
	job := &ScrapeJob{
		ID:        uuid.NewString(),
		SeedURL:   req.URL,
		Depth:     req.Depth,
		Format:    req.Format,
		Status:    "pending",
		CreatedAt: now,
		UpdatedAt: now,
	}

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	// Run async; the caller polls /api/scrapes/{id}
	go func(jobID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		s.mu.RLock()
		j := s.jobs[jobID]
		s.mu.RUnlock()
		if j == nil {
			return
		}
		s.scraper.Run(ctx, j)
	}(job.ID)

	writeJSON(w, http.StatusAccepted, job)
}

// GET /api/scrapes
func (s *Server) handleListScrapes(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*ScrapeJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/scrapes/{id}
func (s *Server) handleGetScrape(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	job, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// GET /api/scrapes/{id}/page?key=...
// Streams the stored object straight from S3 with its content-type.
func (s *Server) handleGetPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	// Make sure the key actually belongs to this job to prevent path tricks
	if !strings.HasPrefix(key, fmt.Sprintf("jobs/%s/", id)) {
		writeErr(w, http.StatusForbidden, "key does not belong to this job")
		return
	}

	data, ct, err := s.storage.Get(r.Context(), key)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// POST /api/scrapes/{id}/index
// Pulls every page's text from S3, chunks it, and indexes into ChromaDB.
func (s *Server) handleIndexScrape(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	job, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if job.Status != "done" {
		writeErr(w, http.StatusConflict, "job not finished yet")
		return
	}

	indexed := 0
	for _, p := range job.Pages {
		// Always read the .txt sibling so we index clean text regardless
		// of what format the user asked for.
		textKey := strings.TrimSuffix(p.S3Key, ".png")
		textKey = strings.TrimSuffix(textKey, ".html")
		if !strings.HasSuffix(textKey, ".txt") {
			textKey = textKey + ".txt"
		}

		data, _, err := s.storage.Get(r.Context(), textKey)
		if err != nil {
			continue
		}
		text := string(data)
		chunks := ChunkText(text, 1500)

		for i, ch := range chunks {
			docID := fmt.Sprintf("%s::%s::%d", job.ID, p.URL, i)
			meta := map[string]string{
				"job_id": job.ID,
				"url":    p.URL,
				"title":  p.Title,
				"chunk":  fmt.Sprintf("%d", i),
			}
			if err := s.ai.IndexDoc(r.Context(), docID, ch, meta); err != nil {
				writeErr(w, http.StatusBadGateway, "index error: "+err.Error())
				return
			}
			indexed++
		}
	}

	s.mu.Lock()
	job.Indexed = true
	job.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":         job.ID,
		"chunks_indexed": indexed,
	})
}

// POST /api/query  { prompt, job_id?, top_k? }
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}

	docs, metas, err := s.ai.QueryChunks(r.Context(), req.Prompt, req.TopK, req.JobID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	prompt := BuildRAGPrompt(req.Prompt, docs)
	answer, err := s.ai.Generate(r.Context(), prompt)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	sources := make([]string, 0, len(metas))
	seen := map[string]bool{}
	for _, m := range metas {
		if u, ok := m["url"].(string); ok && !seen[u] {
			sources = append(sources, u)
			seen[u] = true
		}
	}

	writeJSON(w, http.StatusOK, QueryResponse{Answer: answer, Sources: sources})
}
