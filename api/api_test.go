package main

import (
	"strings"
	"testing"
)

// ChunkText should split long text into roughly size-bounded chunks
// at word boundaries, never silently dropping content.
func TestChunkText(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		size    int
		minLen  int
		maxLen  int
		minN    int
	}{
		{"empty", "", 100, 0, 0, 0},
		{"shorter than chunk size", "hello world", 100, 11, 11, 1},
		{"exact size", strings.Repeat("a", 100), 100, 100, 100, 1},
		{"two chunks", strings.Repeat("word ", 60), 100, 1, 105, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(tc.input, tc.size)
			if len(chunks) < tc.minN {
				t.Fatalf("expected >= %d chunks, got %d", tc.minN, len(chunks))
			}
			for i, c := range chunks {
				if len(c) < tc.minLen {
					t.Errorf("chunk %d shorter than minLen: %q (len %d)", i, c, len(c))
				}
				if tc.maxLen > 0 && len(c) > tc.maxLen+50 {
					t.Errorf("chunk %d longer than maxLen: %d > %d", i, len(c), tc.maxLen+50)
				}
			}
			// Reassembly: concatenated chunks should contain all original
			// non-whitespace tokens.
			joined := strings.Join(chunks, " ")
			tokens := strings.Fields(tc.input)
			for _, tok := range tokens {
				if !strings.Contains(joined, tok) {
					t.Errorf("token %q lost in chunking", tok)
				}
			}
		})
	}
}

// BuildRAGPrompt should embed every context block in numbered form,
// quote the user question verbatim, and not strip context.
func TestBuildRAGPrompt(t *testing.T) {
	q := "What is X?"
	contexts := []string{
		"X is the first letter of foo.",
		"X is also the second letter of bar.",
	}
	got := BuildRAGPrompt(q, contexts)

	if !strings.Contains(got, q) {
		t.Errorf("prompt missing question: %q", got)
	}
	for i, c := range contexts {
		if !strings.Contains(got, c) {
			t.Errorf("context %d missing from prompt", i)
		}
	}
	if !strings.Contains(got, "[1]") || !strings.Contains(got, "[2]") {
		t.Errorf("expected numbered context markers in prompt, got: %q", got)
	}
}

// nilIfEmpty maps "" to nil and any non-empty string to itself.
// This is what the DB layer relies on for nullable text columns.
func TestNilIfEmpty(t *testing.T) {
	if v := nilIfEmpty(""); v != nil {
		t.Errorf("expected nil for empty string, got %v", v)
	}
	if v := nilIfEmpty("hello"); v != "hello" {
		t.Errorf(`expected "hello", got %v`, v)
	}
}

// loadConfig should pick up env vars when set and use defaults otherwise.
func TestLoadConfig(t *testing.T) {
	t.Setenv("OLLAMA_MODEL", "test-model")
	t.Setenv("AUTO_INDEX", "false")

	cfg := loadConfig()
	if cfg.OllamaModel != "test-model" {
		t.Errorf("expected OLLAMA_MODEL override, got %q", cfg.OllamaModel)
	}
	if cfg.AutoIndex {
		t.Error("expected AUTO_INDEX=false to disable auto-index")
	}
	if cfg.S3Bucket == "" {
		t.Error("expected S3_BUCKET default")
	}
}
