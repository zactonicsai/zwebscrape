package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startMockPlaywright returns a server that simulates the playwright /render endpoint
func startMockPlaywright(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/render", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL        string `json:"url"`
			Screenshot bool   `json:"screenshot"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// Return one link only for the seed page so we don't recurse forever
		links := []string{}
		if !strings.Contains(body.URL, "/sub") {
			links = append(links, body.URL+"/sub1")
		}
		resp := map[string]any{
			"url":   body.URL,
			"title": "Mock " + body.URL,
			"html":  "<html><body><h1>" + body.URL + "</h1></body></html>",
			"text":  "page text for " + body.URL,
			"links": links,
		}
		if body.Screenshot {
			resp["screenshot"] = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkAAIAAAoAAv/lxKUAAAAASUVORK5CYII="
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

// startMockChroma simulates only the endpoints we hit
func startMockChroma(t *testing.T) *httptest.Server {
	t.Helper()
	collID := "coll-123"
	mux := http.NewServeMux()
	base := "/api/v2/tenants/default_tenant/databases/default_database"

	mux.HandleFunc(base+"/collections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": collID, "name": "scraper_docs"})
	})
	mux.HandleFunc(base+"/collections/"+collID+"/add", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc(base+"/collections/"+collID+"/query", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"documents": [][]string{{"some retrieved chunk about the page"}},
			"metadatas": [][]map[string]any{{{"url": "https://example.com", "job_id": "j1"}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

// startMockOllama simulates /api/embeddings and /api/generate
func startMockOllama(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string][]float32{
			"embedding": {0.1, 0.2, 0.3, 0.4},
		})
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"response": "This is a mocked summary answer.",
		})
	})
	return httptest.NewServer(mux)
}

// startMockS3 implements just enough S3 surface for PutObject/GetObject
// (LocalStack speaks real S3, but tests should not need a container).
func startMockS3(t *testing.T) *httptest.Server {
	t.Helper()
	store := map[string][]byte{}
	contentTypes := map[string]string{}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path looks like /<bucket>/<key...>
		// CreateBucket is PUT on /<bucket>; PutObject is PUT on /<bucket>/<key>; GetObject is GET on /<bucket>/<key>
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) < 1 || parts[0] == "" {
			w.WriteHeader(200)
			return
		}

		switch r.Method {
		case http.MethodPut:
			if len(parts) == 1 {
				// Create bucket
				w.WriteHeader(200)
				return
			}
			body, _ := io.ReadAll(r.Body)
			store[parts[1]] = body
			contentTypes[parts[1]] = r.Header.Get("Content-Type")
			w.Header().Set("ETag", `"abc"`)
			w.WriteHeader(200)
		case http.MethodGet:
			if len(parts) < 2 {
				w.WriteHeader(404)
				return
			}
			data, ok := store[parts[1]]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if ct := contentTypes[parts[1]]; ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			_, _ = w.Write(data)
		default:
			w.WriteHeader(405)
		}
	}))
}

// buildTestServer wires the API to all four mocks
func buildTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	pw := startMockPlaywright(t)
	chroma := startMockChroma(t)
	ollama := startMockOllama(t)
	s3srv := startMockS3(t)

	cfg := Config{
		PlaywrightURL:    pw.URL,
		S3Endpoint:       s3srv.URL,
		S3Bucket:         "scraper-data",
		AWSRegion:        "us-east-1",
		ChromaURL:        chroma.URL,
		OllamaURL:        ollama.URL,
		OllamaModel:      "llama3.2",
		OllamaEmbedModel: "nomic-embed-text",
	}

	srv, err := newServer(cfg)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	cleanup := func() {
		pw.Close()
		chroma.Close()
		ollama.Close()
		s3srv.Close()
	}
	return srv, cleanup
}

func TestHealth(t *testing.T) {
	srv, cleanup := buildTestServer(t)
	defer cleanup()

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestScrapeValidation(t *testing.T) {
	srv, cleanup := buildTestServer(t)
	defer cleanup()

	cases := []struct {
		body string
		code int
	}{
		{`{}`, 400},
		{`{"url":"not-a-url"}`, 400},
		{`{"url":"https://example.com","format":"bogus"}`, 400},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/api/scrape", strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.router().ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("body %s: want %d got %d (%s)", c.body, c.code, w.Code, w.Body.String())
		}
	}
}

// waitForJob polls until the job is done or the deadline passes
func waitForJob(t *testing.T, srv *Server, id string, timeout time.Duration) *ScrapeJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		srv.mu.RLock()
		j := srv.jobs[id]
		srv.mu.RUnlock()
		if j != nil && (j.Status == "done" || j.Status == "failed") {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish within %s", id, timeout)
	return nil
}

func TestScrapeFlowEndToEnd(t *testing.T) {
	srv, cleanup := buildTestServer(t)
	defer cleanup()

	// 1. POST a scrape
	body := bytes.NewBufferString(`{"url":"https://example.com","depth":1,"format":"text"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/scrape", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("scrape: want 202 got %d (%s)", w.Code, w.Body.String())
	}
	var job ScrapeJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Fatal("missing job id")
	}

	// 2. Wait for it to finish
	done := waitForJob(t, srv, job.ID, 10*time.Second)
	if done.Status != "done" {
		t.Fatalf("job ended in status %s: %s", done.Status, done.Error)
	}
	if len(done.Pages) == 0 {
		t.Fatal("no pages were scraped")
	}

	// 3. Fetch one stored page from S3 via the API
	pageKey := done.Pages[0].S3Key
	url := fmt.Sprintf("/api/scrapes/%s/page?key=%s", done.ID, pageKey)
	r = httptest.NewRequest(http.MethodGet, url, nil)
	w = httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("get page: want 200 got %d (%s)", w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Fatal("page body was empty")
	}

	// 4. Index into ChromaDB
	r = httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/scrapes/%s/index", done.ID), nil)
	w = httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("index: want 200 got %d (%s)", w.Code, w.Body.String())
	}

	// 5. Query the agent
	q := bytes.NewBufferString(`{"prompt":"summarize this site"}`)
	r = httptest.NewRequest(http.MethodPost, "/api/query", q)
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("query: want 200 got %d (%s)", w.Code, w.Body.String())
	}
	var qr QueryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Answer == "" {
		t.Fatal("expected non-empty answer")
	}
}

func TestChunkText(t *testing.T) {
	chunks := ChunkText("hello world this is a test of chunking some text", 10)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c == "" {
			t.Fatal("empty chunk produced")
		}
	}

	if got := ChunkText("", 10); got != nil {
		t.Errorf("empty input should produce nil, got %v", got)
	}
	if got := ChunkText("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("small input: got %v", got)
	}
}

func TestStoragePutGet(t *testing.T) {
	srv, cleanup := buildTestServer(t)
	defer cleanup()

	ctx := context.Background()
	if err := srv.storage.Put(ctx, "test/hello.txt", []byte("hi"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ct, err := srv.storage.Get(ctx, "test/hello.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("got %q want %q", got, "hi")
	}
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("got content-type %q", ct)
	}
}

func TestPageAuthorizationCheck(t *testing.T) {
	srv, cleanup := buildTestServer(t)
	defer cleanup()

	// Try to fetch a key that doesn't belong to a job we own
	r := httptest.NewRequest(http.MethodGet,
		"/api/scrapes/nonexistent/page?key=jobs/other/pages/x.txt", nil)
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d", w.Code)
	}
}
