package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Scraper performs BFS web crawl up to a configured depth
type Scraper struct {
	cfg     Config
	storage *Storage
	http    *http.Client
}

func NewScraper(cfg Config, storage *Storage) *Scraper {
	return &Scraper{
		cfg:     cfg,
		storage: storage,
		http:    &http.Client{Timeout: 90 * time.Second},
	}
}

// Run executes a job, blocking until complete. Updates job in place.
func (s *Scraper) Run(ctx context.Context, job *ScrapeJob) {
	job.Status = "running"
	job.UpdatedAt = time.Now().UTC()

	seed, err := url.Parse(job.SeedURL)
	if err != nil {
		job.Status = "failed"
		job.Error = "invalid seed url: " + err.Error()
		return
	}
	seedHost := seed.Host

	type queueItem struct {
		url   string
		depth int
	}

	visited := map[string]bool{}
	queue := []queueItem{{url: job.SeedURL, depth: 0}}

	// Cap total pages to keep demo bounded
	const maxPages = 50

	for len(queue) > 0 && len(job.Pages) < maxPages {
		item := queue[0]
		queue = queue[1:]

		if visited[item.url] {
			continue
		}
		visited[item.url] = true

		page, links, err := s.fetchAndStore(ctx, job, item.url, item.depth)
		if err != nil {
			log.Printf("fetch %s: %v", item.url, err)
			continue
		}
		job.Pages = append(job.Pages, page)
		job.UpdatedAt = time.Now().UTC()

		// Enqueue same-host links if depth permits
		if item.depth < job.Depth {
			for _, link := range links {
				lu, err := url.Parse(link)
				if err != nil {
					continue
				}
				// Stay on the same host; strip fragments
				if lu.Host != seedHost {
					continue
				}
				lu.Fragment = ""
				normalized := lu.String()
				if !visited[normalized] {
					queue = append(queue, queueItem{url: normalized, depth: item.depth + 1})
				}
			}
		}
	}

	job.Status = "done"
	job.UpdatedAt = time.Now().UTC()
}

// fetchAndStore renders one URL, stores output to S3, and returns links for further traversal
func (s *Scraper) fetchAndStore(ctx context.Context, job *ScrapeJob, pageURL string, depth int) (ScrapePage, []string, error) {
	wantScreenshot := job.Format == "screenshot"

	body, _ := json.Marshal(map[string]any{
		"url":        pageURL,
		"screenshot": wantScreenshot,
		"timeout":    30000,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.PlaywrightURL+"/render", bytes.NewReader(body))
	if err != nil {
		return ScrapePage{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return ScrapePage{}, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ScrapePage{}, nil, err
	}
	if resp.StatusCode >= 400 {
		return ScrapePage{}, nil, fmt.Errorf("playwright %d: %s", resp.StatusCode, string(raw))
	}

	var pr PlaywrightResp
	if err := json.Unmarshal(raw, &pr); err != nil {
		return ScrapePage{}, nil, fmt.Errorf("decode playwright: %w", err)
	}

	pageID := uuid.NewString()
	var key, contentType string
	var data []byte

	switch job.Format {
	case "screenshot":
		key = fmt.Sprintf("jobs/%s/pages/%s.png", job.ID, pageID)
		contentType = "image/png"
		data, err = base64.StdEncoding.DecodeString(pr.Screenshot)
		if err != nil {
			return ScrapePage{}, nil, fmt.Errorf("decode screenshot: %w", err)
		}
	case "html":
		key = fmt.Sprintf("jobs/%s/pages/%s.html", job.ID, pageID)
		contentType = "text/html; charset=utf-8"
		data = []byte(pr.HTML)
	default: // "text"
		key = fmt.Sprintf("jobs/%s/pages/%s.txt", job.ID, pageID)
		contentType = "text/plain; charset=utf-8"
		// Always store text alongside, even when format is text
		data = []byte(pr.Text)
	}

	if err := s.storage.Put(ctx, key, data, contentType); err != nil {
		return ScrapePage{}, nil, err
	}

	// For non-text formats, also stash a parallel text version so the
	// AI indexer always has clean text to work with.
	if job.Format != "text" {
		textKey := fmt.Sprintf("jobs/%s/pages/%s.txt", job.ID, pageID)
		_ = s.storage.Put(ctx, textKey, []byte(pr.Text), "text/plain; charset=utf-8")
	}

	page := ScrapePage{
		URL:    pr.URL,
		Title:  strings.TrimSpace(pr.Title),
		S3Key:  key,
		Depth:  depth,
		Format: job.Format,
		Bytes:  len(data),
	}

	return page, pr.Links, nil
}
