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

// Scraper performs BFS web crawl up to a configured depth.
// It calls the playwright service's /scrape endpoint, which renders the page,
// clicks every interactive element, and returns the resulting state(s).
type Scraper struct {
	cfg     Config
	storage *Storage
	db      *DB
	ai      *AI
	http    *http.Client
}

func NewScraper(cfg Config, storage *Storage, db *DB, ai *AI) *Scraper {
	return &Scraper{
		cfg:     cfg,
		storage: storage,
		db:      db,
		ai:      ai,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Run executes a scan. Updates the DB row in place. With AUTO_INDEX=true (the
// default) each page is indexed into ChromaDB immediately after it's stored,
// so the chunks_indexed counter ticks up in lockstep with page_count.
func (s *Scraper) Run(ctx context.Context, scan *Scan) {
	_ = s.db.UpdateScanStatus(ctx, scan.ID, "running", "")
	if s.cfg.AutoIndex {
		_ = s.db.MarkIndexingStart(ctx, scan.ID)
	}

	seed, err := url.Parse(scan.SeedURL)
	if err != nil {
		_ = s.db.MarkScanCompleted(ctx, scan.ID, 0, 0, "invalid seed url: "+err.Error())
		return
	}
	seedHost := seed.Host

	type queueItem struct {
		url   string
		depth int
	}

	visited := map[string]bool{}
	queue := []queueItem{{url: scan.SeedURL, depth: 0}}

	const maxPages = 50
	pageCount := 0
	clickCount := 0
	totalChunks := 0

	for len(queue) > 0 && pageCount < maxPages {
		item := queue[0]
		queue = queue[1:]

		if visited[item.url] {
			continue
		}
		visited[item.url] = true

		// Surface "currently scraping <url>" before the fetch starts
		_ = s.db.SetCurrentURL(ctx, scan.ID, item.url)

		page, clicks, links, err := s.fetchAndStorePage(ctx, scan, item.url, item.depth)
		if err != nil {
			log.Printf("scan %s: fetch %s: %v", scan.ID, item.url, err)
			continue
		}
		pageCount++
		clickCount += len(clicks)

		// Live progress: bump page_count + click_count after EVERY page
		if err := s.db.BumpScanProgress(ctx, scan.ID, pageCount, clickCount); err != nil {
			log.Printf("scan %s: bump progress: %v", scan.ID, err)
		}

		// Index this page (and its click-state texts) RIGHT NOW, so chunks_indexed
		// ticks up alongside page_count rather than waiting for the whole scan.
		if s.cfg.AutoIndex {
			n, ierr := s.indexPage(ctx, scan.ID, page, clicks)
			if ierr != nil {
				log.Printf("scan %s: index page %s: %v", scan.ID, page.URL, ierr)
			}
			totalChunks += n
			if err := s.db.BumpIndexProgress(ctx, scan.ID, totalChunks); err != nil {
				log.Printf("scan %s: bump index progress: %v", scan.ID, err)
			}
		}

		// Enqueue same-host links if depth permits
		if item.depth < scan.Depth {
			for _, link := range links {
				lu, err := url.Parse(link)
				if err != nil || lu.Host != seedHost {
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

	if err := s.db.MarkScanCompleted(ctx, scan.ID, pageCount, clickCount, ""); err != nil {
		log.Printf("scan %s: mark completed: %v", scan.ID, err)
	}

	// Stamp the final indexing total + indexed_at when inline indexing was on
	if s.cfg.AutoIndex {
		if err := s.db.MarkScanIndexed(ctx, scan.ID, totalChunks); err != nil {
			log.Printf("scan %s: mark indexed: %v", scan.ID, err)
		}
		log.Printf("scan %s: inline-indexed %d chunks across %d pages", scan.ID, totalChunks, pageCount)
	}
}

// fetchAndStorePage renders one URL via playwright, persists everything to S3
// and Postgres, and returns the persisted *Page, the persisted ClickStates,
// and the links discovered for further BFS traversal.
func (s *Scraper) fetchAndStorePage(ctx context.Context, scan *Scan, pageURL string, depth int) (*Page, []ClickState, []string, error) {
	body, _ := json.Marshal(map[string]any{
		"url":           pageURL,
		"screenshot":    true,
		"click_buttons": scan.ClickButtons,
		"max_clicks":    20,
		"timeout":       45000,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.PlaywrightURL+"/scrape", bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, nil, fmt.Errorf("playwright %d: %s", resp.StatusCode, string(raw))
	}

	var pr PlaywrightResp
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, nil, nil, fmt.Errorf("decode playwright: %w", err)
	}
	if pr.Error != "" {
		return nil, nil, nil, fmt.Errorf("playwright: %s", pr.Error)
	}

	pageID := uuid.NewString()
	page := &Page{
		ID:         pageID,
		ScanID:     scan.ID,
		URL:        pr.URL,
		Title:      strings.TrimSpace(pr.Title),
		Depth:      depth,
		StatusCode: 200,
		ClickCount: len(pr.Clicks),
		ScrapedAt:  time.Now().UTC(),
	}

	// Always store HTML
	if len(pr.HTML) > 0 {
		key := fmt.Sprintf("scans/%s/pages/%s.html", scan.ID, pageID)
		if err := s.storage.Put(ctx, key, []byte(pr.HTML), "text/html; charset=utf-8"); err == nil {
			page.HTMLS3Key = key
			page.HTMLBytes = int64(len(pr.HTML))
		}
	}
	// Always store text
	if len(pr.Text) > 0 {
		key := fmt.Sprintf("scans/%s/pages/%s.txt", scan.ID, pageID)
		if err := s.storage.Put(ctx, key, []byte(pr.Text), "text/plain; charset=utf-8"); err == nil {
			page.TextS3Key = key
			page.TextBytes = int64(len(pr.Text))
		}
	}
	// Screenshot when format permits (default "all" includes it)
	if pr.Screenshot != "" && (scan.Format == "all" || scan.Format == "screenshot") {
		if data, derr := base64.StdEncoding.DecodeString(pr.Screenshot); derr == nil {
			key := fmt.Sprintf("scans/%s/pages/%s.png", scan.ID, pageID)
			if err := s.storage.Put(ctx, key, data, "image/png"); err == nil {
				page.ScreenshotS3Key = key
				page.ScreenshotBytes = int64(len(data))
			}
		}
	}

	if err := s.db.InsertPage(ctx, page); err != nil {
		return nil, nil, nil, fmt.Errorf("insert page: %w", err)
	}

	// Persist each click state and collect them for inline indexing
	persisted := make([]ClickState, 0, len(pr.Clicks))
	for _, c := range pr.Clicks {
		clickID := uuid.NewString()
		cs := ClickState{
			ID:         clickID,
			PageID:     pageID,
			ScanID:     scan.ID,
			Step:       c.Step,
			Selector:   c.Selector,
			Label:      c.Label,
			CapturedAt: time.Now().UTC(),
		}
		if c.HTML != "" {
			key := fmt.Sprintf("scans/%s/pages/%s/clicks/%03d.html", scan.ID, pageID, c.Step)
			if err := s.storage.Put(ctx, key, []byte(c.HTML), "text/html; charset=utf-8"); err == nil {
				cs.HTMLS3Key = key
			}
		}
		if c.Text != "" {
			key := fmt.Sprintf("scans/%s/pages/%s/clicks/%03d.txt", scan.ID, pageID, c.Step)
			if err := s.storage.Put(ctx, key, []byte(c.Text), "text/plain; charset=utf-8"); err == nil {
				cs.TextS3Key = key
			}
		}
		if err := s.db.InsertClickState(ctx, &cs); err != nil {
			log.Printf("scan %s: insert click state: %v", scan.ID, err)
			continue
		}
		persisted = append(persisted, cs)
	}

	return page, persisted, pr.Links, nil
}

// indexPage chunks the freshly-stored page text + each click-state's text and
// pushes them into ChromaDB. Returns the number of chunks indexed.
// Called inline from Run() after each page is stored.
func (s *Scraper) indexPage(ctx context.Context, scanID string, page *Page, clicks []ClickState) (int, error) {
	indexed := 0

	// Page text
	if page.TextS3Key != "" {
		data, _, err := s.storage.Get(ctx, page.TextS3Key)
		if err == nil {
			chunks := ChunkText(string(data), 1500)
			for i, ch := range chunks {
				docID := fmt.Sprintf("%s::%s::%d", scanID, page.ID, i)
				meta := map[string]string{
					"scan_id": scanID,
					"page_id": page.ID,
					"url":     page.URL,
					"title":   page.Title,
					"chunk":   fmt.Sprintf("%d", i),
					"kind":    "page",
				}
				if err := s.ai.IndexDoc(ctx, docID, ch, meta); err != nil {
					log.Printf("scan %s: index page chunk: %v", scanID, err)
					continue
				}
				indexed++
			}
		}
	}

	// Click-state texts
	for _, c := range clicks {
		if c.TextS3Key == "" {
			continue
		}
		cdata, _, err := s.storage.Get(ctx, c.TextS3Key)
		if err != nil {
			continue
		}
		chunks := ChunkText(string(cdata), 1500)
		for i, ch := range chunks {
			docID := fmt.Sprintf("%s::%s::click-%d::%d", scanID, page.ID, c.Step, i)
			meta := map[string]string{
				"scan_id":  scanID,
				"page_id":  page.ID,
				"click_id": c.ID,
				"url":      page.URL,
				"title":    page.Title + " (after click: " + c.Label + ")",
				"chunk":    fmt.Sprintf("%d", i),
				"kind":     "click",
			}
			if err := s.ai.IndexDoc(ctx, docID, ch, meta); err != nil {
				log.Printf("scan %s: index click chunk: %v", scanID, err)
				continue
			}
			indexed++
		}
	}

	return indexed, nil
}

// autoIndex pulls every page's text + every click-state's text from S3, chunks
// it, and indexes it into ChromaDB. Updates the scan row with the chunk count
// after every page so the UI's "Indexed N chunks" badge ticks up live.
func (s *Scraper) autoIndex(ctx context.Context, scanID string) {
	if err := s.db.MarkIndexingStart(ctx, scanID); err != nil {
		log.Printf("scan %s: mark indexing start: %v", scanID, err)
	}

	pages, err := s.db.ListPages(ctx, scanID)
	if err != nil {
		log.Printf("scan %s: list pages: %v", scanID, err)
		_ = s.db.MarkIndexingFailed(ctx, scanID, "list pages: "+err.Error())
		return
	}

	indexed := 0
	for _, p := range pages {
		// Tell the UI which URL is currently being indexed
		_ = s.db.SetCurrentURL(ctx, scanID, p.URL)

		if p.TextS3Key != "" {
			data, _, err := s.storage.Get(ctx, p.TextS3Key)
			if err == nil {
				chunks := ChunkText(string(data), 1500)
				for i, ch := range chunks {
					docID := fmt.Sprintf("%s::%s::%d", scanID, p.ID, i)
					meta := map[string]string{
						"scan_id": scanID,
						"page_id": p.ID,
						"url":     p.URL,
						"title":   p.Title,
						"chunk":   fmt.Sprintf("%d", i),
						"kind":    "page",
					}
					if err := s.ai.IndexDoc(ctx, docID, ch, meta); err != nil {
						log.Printf("scan %s: index page chunk: %v", scanID, err)
						continue
					}
					indexed++
				}
			}
		}

		// Also index click-state text so queries can match clicked-state content
		clicks, _ := s.db.ListClicksForPage(ctx, p.ID)
		for _, c := range clicks {
			if c.TextS3Key == "" {
				continue
			}
			cdata, _, err := s.storage.Get(ctx, c.TextS3Key)
			if err != nil {
				continue
			}
			chunks := ChunkText(string(cdata), 1500)
			for i, ch := range chunks {
				docID := fmt.Sprintf("%s::%s::click-%d::%d", scanID, p.ID, c.Step, i)
				meta := map[string]string{
					"scan_id":  scanID,
					"page_id":  p.ID,
					"click_id": c.ID,
					"url":      p.URL,
					"title":    p.Title + " (after click: " + c.Label + ")",
					"chunk":    fmt.Sprintf("%d", i),
					"kind":     "click",
				}
				if err := s.ai.IndexDoc(ctx, docID, ch, meta); err != nil {
					log.Printf("scan %s: index click chunk: %v", scanID, err)
					continue
				}
				indexed++
			}
		}

		// Live progress checkpoint after each page (not each chunk to avoid
		// hammering the DB)
		if err := s.db.BumpIndexProgress(ctx, scanID, indexed); err != nil {
			log.Printf("scan %s: bump index progress: %v", scanID, err)
		}
	}

	if err := s.db.MarkScanIndexed(ctx, scanID, indexed); err != nil {
		log.Printf("scan %s: mark indexed: %v", scanID, err)
	}
	log.Printf("scan %s: auto-indexed %d chunks", scanID, indexed)
}
