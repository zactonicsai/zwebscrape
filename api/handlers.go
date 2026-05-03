package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

// POST /api/scrape  { url, depth, format, click_buttons }
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
		req.Depth = 5
	}
	if req.Format == "" {
		req.Format = "all"
	}
	switch req.Format {
	case "all", "text", "html", "screenshot":
	default:
		writeErr(w, http.StatusBadRequest, "format must be all|text|html|screenshot")
		return
	}

	clickButtons := true
	if req.ClickButtons != nil {
		clickButtons = *req.ClickButtons
	}

	now := time.Now().UTC()
	scan := &Scan{
		ID:           uuid.NewString(),
		SeedURL:      req.URL,
		Depth:        req.Depth,
		Format:       req.Format,
		ClickButtons: clickButtons,
		Status:       "pending",
		StartedAt:    now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.db.CreateScan(r.Context(), scan); err != nil {
		writeErr(w, http.StatusInternalServerError, "create scan: "+err.Error())
		return
	}

	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		fresh, err := s.db.GetScan(ctx, id)
		if err != nil {
			return
		}
		s.scraper.Run(ctx, fresh)
	}(scan.ID)

	writeJSON(w, http.StatusAccepted, scan)
}

// GET /api/scrapes
func (s *Server) handleListScrapes(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	scans, err := s.db.ListScans(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, scans)
}

// GET /api/scrapes/{id} — scan + pages
func (s *Server) handleGetScrape(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	scan, err := s.db.GetScan(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "scan not found")
		return
	}
	pages, err := s.db.ListPages(r.Context(), id)
	if err == nil {
		scan.Pages = pages
	}
	writeJSON(w, http.StatusOK, scan)
}

// GET /api/scrapes/{id}/pages — just the pages
func (s *Server) handleListPages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	pages, err := s.db.ListPages(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pages)
}

// GET /api/scrapes/{id}/page/{pageID} — page + click states
func (s *Server) handleGetPageDetail(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	page, err := s.db.GetPage(r.Context(), pageID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	clicks, _ := s.db.ListClicksForPage(r.Context(), pageID)
	page.Clicks = clicks
	writeJSON(w, http.StatusOK, page)
}

// GET /api/scrapes/{id}/files — every S3 artefact for this scan, classified
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	pages, err := s.db.ListPages(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	pageByID := map[string]Page{}
	for _, p := range pages {
		pageByID[p.ID] = p
	}

	clicks, _ := s.db.ListClicksForScan(r.Context(), id)

	out := make([]FileEntry, 0)

	for _, p := range pages {
		if p.HTMLS3Key != "" {
			out = append(out, FileEntry{
				Key: p.HTMLS3Key, Size: p.HTMLBytes, ContentType: "text/html",
				Kind: "page-html", URL: p.URL, PageID: p.ID,
			})
		}
		if p.TextS3Key != "" {
			out = append(out, FileEntry{
				Key: p.TextS3Key, Size: p.TextBytes, ContentType: "text/plain",
				Kind: "page-text", URL: p.URL, PageID: p.ID,
			})
		}
		if p.ScreenshotS3Key != "" {
			out = append(out, FileEntry{
				Key: p.ScreenshotS3Key, Size: p.ScreenshotBytes, ContentType: "image/png",
				Kind: "page-screenshot", URL: p.URL, PageID: p.ID,
			})
		}
	}

	for _, c := range clicks {
		pg := pageByID[c.PageID]
		if c.HTMLS3Key != "" {
			out = append(out, FileEntry{
				Key: c.HTMLS3Key, ContentType: "text/html",
				Kind: "click-html", URL: pg.URL, Step: c.Step,
				PageID: c.PageID, ClickID: c.ID,
			})
		}
		if c.TextS3Key != "" {
			out = append(out, FileEntry{
				Key: c.TextS3Key, ContentType: "text/plain",
				Kind: "click-text", URL: pg.URL, Step: c.Step,
				PageID: c.PageID, ClickID: c.ID,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scan_id": id,
		"count":   len(out),
		"files":   out,
	})
}

// GET /api/scrapes/{id}/file?key=... — stream an S3 object
// Allowed keys are scoped to scans/{id}/...
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	if !strings.HasPrefix(key, fmt.Sprintf("scans/%s/", id)) {
		writeErr(w, http.StatusForbidden, "key does not belong to this scan")
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
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// POST /api/scrapes/{id}/index — manual re-index (auto-index already runs)
func (s *Server) handleIndexScrape(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	scan, err := s.db.GetScan(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "scan not found")
		return
	}
	if scan.Status != "done" {
		writeErr(w, http.StatusConflict, "scan not finished yet")
		return
	}

	go s.scraper.autoIndex(context.Background(), id)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"scan_id": id,
		"status":  "indexing",
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
