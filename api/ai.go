package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AI handles ChromaDB indexing and Ollama-based summarization
type AI struct {
	cfg          Config
	http         *http.Client
	collectionID string // cached after first creation
}

func NewAI(cfg Config) *AI {
	return &AI{
		cfg:  cfg,
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

// chromaBase returns the v2 base URL with the default tenant/database
func (a *AI) chromaBase() string {
	return strings.TrimRight(a.cfg.ChromaURL, "/") +
		"/api/v2/tenants/default_tenant/databases/default_database"
}

// ensureCollection creates "scraper_docs" if needed and caches its id
func (a *AI) ensureCollection(ctx context.Context) (string, error) {
	if a.collectionID != "" {
		return a.collectionID, nil
	}

	body, _ := json.Marshal(map[string]any{
		"name":          "scraper_docs",
		"get_or_create": true,
		"metadata":      map[string]string{"purpose": "web_scraper_rag"},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.chromaBase()+"/collections", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chroma create collection: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("chroma %d: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode chroma: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("chroma returned no id: %s", string(raw))
	}
	a.collectionID = out.ID
	return a.collectionID, nil
}

// Embed gets a vector for one piece of text from Ollama
func (a *AI) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  a.cfg.OllamaEmbedModel,
		"prompt": text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.cfg.OllamaURL, "/")+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ollama embed %d: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding from ollama")
	}
	return out.Embedding, nil
}

// IndexDoc adds a chunk of text to ChromaDB with metadata
func (a *AI) IndexDoc(ctx context.Context, id, text string, meta map[string]string) error {
	collID, err := a.ensureCollection(ctx)
	if err != nil {
		return err
	}

	emb, err := a.Embed(ctx, text)
	if err != nil {
		return err
	}

	metaIface := make(map[string]any, len(meta))
	for k, v := range meta {
		metaIface[k] = v
	}

	body, _ := json.Marshal(map[string]any{
		"ids":        []string{id},
		"documents":  []string{text},
		"embeddings": [][]float32{emb},
		"metadatas":  []map[string]any{metaIface},
	})

	url := a.chromaBase() + "/collections/" + collID + "/add"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("chroma add: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("chroma add %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// QueryChunks retrieves the top-K most similar text chunks for a prompt
func (a *AI) QueryChunks(ctx context.Context, prompt string, topK int, jobFilter string) ([]string, []map[string]any, error) {
	collID, err := a.ensureCollection(ctx)
	if err != nil {
		return nil, nil, err
	}
	if topK <= 0 {
		topK = 5
	}

	emb, err := a.Embed(ctx, prompt)
	if err != nil {
		return nil, nil, err
	}

	payload := map[string]any{
		"query_embeddings": [][]float32{emb},
		"n_results":        topK,
	}
	if jobFilter != "" {
		payload["where"] = map[string]any{"scan_id": jobFilter}
	}

	body, _ := json.Marshal(payload)

	url := a.chromaBase() + "/collections/" + collID + "/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("chroma query: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("chroma query %d: %s", resp.StatusCode, string(raw))
	}

	// Chroma returns nested arrays: documents[[...]]
	var out struct {
		Documents [][]string           `json:"documents"`
		Metadatas [][]map[string]any   `json:"metadatas"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, err
	}

	var docs []string
	var metas []map[string]any
	if len(out.Documents) > 0 {
		docs = out.Documents[0]
	}
	if len(out.Metadatas) > 0 {
		metas = out.Metadatas[0]
	}
	return docs, metas, nil
}

// Generate sends a prompt to Ollama and returns the answer
func (a *AI) Generate(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  a.cfg.OllamaModel,
		"prompt": prompt,
		"stream": false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.cfg.OllamaURL, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama generate: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("ollama generate %d: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Response), nil
}

// ChunkText splits a long string into ~size-character chunks at word boundaries
func ChunkText(s string, size int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if size <= 0 {
		size = 1500
	}
	if len(s) <= size {
		return []string{s}
	}

	var chunks []string
	for len(s) > 0 {
		if len(s) <= size {
			chunks = append(chunks, s)
			break
		}
		// Try to break on a space near the size boundary
		cut := size
		if idx := strings.LastIndex(s[:cut], " "); idx > size/2 {
			cut = idx
		}
		chunks = append(chunks, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	return chunks
}

// BuildRAGPrompt formats retrieved context plus the user question with rules
func BuildRAGPrompt(question string, contexts []string) string {
	var b strings.Builder
	b.WriteString("You are a helpful research assistant analyzing scraped web content.\n")
	b.WriteString("Rules:\n")
	b.WriteString("1. Answer only from the provided context.\n")
	b.WriteString("2. If the context does not contain the answer, say so clearly.\n")
	b.WriteString("3. Be concise and cite which context block(s) you used by number.\n\n")
	b.WriteString("=== CONTEXT ===\n")
	for i, c := range contexts {
		fmt.Fprintf(&b, "[%d] %s\n\n", i+1, c)
	}
	b.WriteString("=== QUESTION ===\n")
	b.WriteString(question)
	b.WriteString("\n\n=== ANSWER ===\n")
	return b.String()
}
