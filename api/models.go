package main

import "time"

// ScrapeRequest is the body for POST /api/scrape
type ScrapeRequest struct {
	URL          string `json:"url"`
	Depth        int    `json:"depth"`
	Format       string `json:"format"`        // "all" stores html+text+screenshot, or "text"/"html"/"screenshot"
	ClickButtons *bool  `json:"click_buttons"` // default true
}

// Scan is one crawl operation, persisted to Postgres.
type Scan struct {
	ID             string     `json:"id"`
	SeedURL        string     `json:"seed_url"`
	Depth          int        `json:"depth"`
	Format         string     `json:"format"`
	ClickButtons   bool       `json:"click_buttons"`
	Status         string     `json:"status"` // pending | running | done | failed
	Error          string     `json:"error,omitempty"`
	PageCount      int        `json:"page_count"`
	ClickCount     int        `json:"click_count"`
	ChunksIndexed  int        `json:"chunks_indexed"`
	IndexedAt      *time.Time `json:"indexed_at,omitempty"`
	CurrentURL     string     `json:"current_url,omitempty"`     // live: URL being scraped/indexed
	IndexingStatus string     `json:"indexing_status,omitempty"` // "" | running | done | failed
	StartedAt      time.Time  `json:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// Hydrated by GET /api/scrapes/{id} — not in the row itself
	Pages []Page `json:"pages,omitempty"`
}

// Page represents a single rendered URL, with its three S3 artefacts.
type Page struct {
	ID              string    `json:"id"`
	ScanID          string    `json:"scan_id"`
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	Depth           int       `json:"depth"`
	StatusCode      int       `json:"status_code"`
	HTMLS3Key       string    `json:"html_s3_key,omitempty"`
	TextS3Key       string    `json:"text_s3_key,omitempty"`
	ScreenshotS3Key string    `json:"screenshot_s3_key,omitempty"`
	HTMLBytes       int64     `json:"html_bytes"`
	TextBytes       int64     `json:"text_bytes"`
	ScreenshotBytes int64     `json:"screenshot_bytes"`
	ClickCount      int       `json:"click_count"`
	Error           string    `json:"error,omitempty"`
	ScrapedAt       time.Time `json:"scraped_at"`

	// Hydrated by GET /api/scrapes/{id}/pages/{pageID} — not in the row
	Clicks []ClickState `json:"clicks,omitempty"`
}

// ClickState is the result of clicking one button on a page: the post-click
// HTML and text are stored in S3 and pointed to here.
type ClickState struct {
	ID         string    `json:"id"`
	PageID     string    `json:"page_id"`
	ScanID     string    `json:"scan_id"`
	Step       int       `json:"step"`
	Selector   string    `json:"selector"`
	Label      string    `json:"label,omitempty"`
	HTMLS3Key  string    `json:"html_s3_key,omitempty"`
	TextS3Key  string    `json:"text_s3_key,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
}

// FileEntry describes one artefact in S3 (used by /api/scrapes/{id}/files).
type FileEntry struct {
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Kind        string `json:"kind"` // page-html | page-text | page-screenshot | click-html | click-text
	URL         string `json:"url,omitempty"`
	Step        int    `json:"step,omitempty"`
	PageID      string `json:"page_id,omitempty"`
	ClickID     string `json:"click_id,omitempty"`
}

// QueryRequest is the body for POST /api/query
type QueryRequest struct {
	JobID  string `json:"job_id,omitempty"` // optional: scope to one scan
	Prompt string `json:"prompt"`
	TopK   int    `json:"top_k,omitempty"`
}

// QueryResponse from POST /api/query
type QueryResponse struct {
	Answer  string   `json:"answer"`
	Sources []string `json:"sources"`
}

// PlaywrightResp is the response from the playwright /scrape endpoint.
// It now includes click states as well.
type PlaywrightResp struct {
	URL        string             `json:"url"`
	Title      string             `json:"title"`
	HTML       string             `json:"html"`
	Text       string             `json:"text"`
	Links      []string           `json:"links"`
	Screenshot string             `json:"screenshot,omitempty"` // base64 png
	Clicks     []PlaywrightClick  `json:"clicks,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// PlaywrightClick is one post-click capture from the renderer.
type PlaywrightClick struct {
	Step     int    `json:"step"`
	Selector string `json:"selector"`
	Label    string `json:"label"`
	HTML     string `json:"html"`
	Text     string `json:"text"`
}
