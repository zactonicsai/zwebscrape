package main

import "time"

// ScrapeRequest is the body for POST /api/scrape
type ScrapeRequest struct {
	URL    string `json:"url"`
	Depth  int    `json:"depth"`
	Format string `json:"format"` // "text", "html", or "screenshot"
}

// ScrapeJob represents one crawl operation
type ScrapeJob struct {
	ID        string       `json:"id"`
	SeedURL   string       `json:"seed_url"`
	Depth     int          `json:"depth"`
	Format    string       `json:"format"`
	Status    string       `json:"status"` // pending|running|done|failed
	Error     string       `json:"error,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Pages     []ScrapePage `json:"pages"`
	Indexed   bool         `json:"indexed"`
}

// ScrapePage is a single page captured in a job
type ScrapePage struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	S3Key   string `json:"s3_key"`
	Depth   int    `json:"depth"`
	Format  string `json:"format"`
	Bytes   int    `json:"bytes"`
}

// QueryRequest is the body for POST /api/query
type QueryRequest struct {
	JobID  string `json:"job_id,omitempty"` // optional: scope to one job
	Prompt string `json:"prompt"`
	TopK   int    `json:"top_k,omitempty"`
}

// QueryResponse from POST /api/query
type QueryResponse struct {
	Answer  string   `json:"answer"`
	Sources []string `json:"sources"`
}

// PlaywrightResp shape from the playwright service
type PlaywrightResp struct {
	URL        string   `json:"url"`
	Title      string   `json:"title"`
	HTML       string   `json:"html"`
	Text       string   `json:"text"`
	Links      []string `json:"links"`
	Screenshot string   `json:"screenshot,omitempty"` // base64 png
	Error      string   `json:"error,omitempty"`
}
