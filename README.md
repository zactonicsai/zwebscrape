# Web Scraper Console

A complete web-scraping system that:

awslocal s3 ls --recursive scraper-data

- Renders JS-heavy pages with **Playwright** and **clicks every interactive element** (buttons, `[role=button]`, `[onclick]`, submit inputs, `<summary>`)
- Captures both the initial state and one snapshot per click
- Crawls links to a configurable **depth (0–5)**
- Stores raw **HTML**, extracted **text**, and **PNG screenshots** to **S3 (LocalStack)** for every page and every click state
- Persists rich metadata to **Postgres 17** (scans, pages, click states with timestamps and byte counts)
- **Auto-indexes** text and click-state text into **ChromaDB** as soon as a scan completes
- Answers questions with **Ollama** (`llama3.2`) using the indexed context
- Provides a **Tailwind** HTML console with a metadata-rich table, a file browser, and an HTML/text viewer

## Stack

| Service     | What it does                              | Port  |
|-------------|-------------------------------------------|-------|
| `frontend`  | Static Tailwind UI (nginx)                | 8000  |
| `api`       | Go HTTP API (chi router + lib/pq)         | 8080  |
| `playwright`| Node.js Playwright renderer + clicker     | 3000  |
| `postgres`  | Postgres 17 (scans / pages / click_states)| 5432  |
| `localstack`| S3-compatible storage                     | 4566  |
| `chromadb`  | Vector store                              | 8001  |
| `ollama`    | LLM + embeddings                          | 11434 |

All services share an internal Docker network (`scraper-net`).

## Quick start

```bash
docker compose up --build
```

First boot:
- Postgres comes up with a healthcheck; the API waits for it
- LocalStack creates the `scraper-data` S3 bucket via `scripts/init-aws.sh`
- `ollama-init` pulls `llama3.2` and `nomic-embed-text` (a few minutes the first time)
- Open **http://localhost:8000**

## What happens during a scrape

1. **Submit** a URL with depth, format (`all` = HTML + text + screenshot), and "click all buttons" toggle.
2. **API** writes a row to `scans` (Postgres) and dispatches a goroutine.
3. **Scraper** walks the seed and same-host links breadth-first up to depth.
4. For each page, **Playwright** renders it, locates every clickable element, clicks each one in turn, and captures the resulting HTML+text per click. Selectors are deduped, dialogs are auto-dismissed, and the page is restored to the original URL between clicks.
5. **API** stores HTML, text, screenshot to S3, writes a `pages` row, and one `click_states` row per click.
6. When the scan finishes, **auto-index** pulls every text artefact, chunks at ~1500 chars, embeds with `nomic-embed-text`, and writes to ChromaDB. `chunks_indexed` and `indexed_at` are stamped on the scan row.
7. The frontend's **AI agent** panel can now answer questions over the scan.

## API

| Method | Path                                  | Description                                     |
|--------|---------------------------------------|-------------------------------------------------|
| GET    | `/health`                             | Liveness probe                                  |
| POST   | `/api/scrape`                         | Start a scan                                    |
| GET    | `/api/scrapes`                        | List scans (Postgres-backed)                    |
| GET    | `/api/scrapes/{id}`                   | Get scan + pages                                |
| GET    | `/api/scrapes/{id}/pages`             | Just the pages                                  |
| GET    | `/api/scrapes/{id}/page/{pageID}`     | Page + click states                             |
| GET    | `/api/scrapes/{id}/files`             | Every S3 artefact for the scan, classified      |
| GET    | `/api/scrapes/{id}/file?key=...`      | Stream one S3 object (scan-scoped key auth)     |
| POST   | `/api/scrapes/{id}/index`             | Re-index the scan into ChromaDB                 |
| POST   | `/api/query`                          | RAG query (`{prompt, job_id?, top_k?}`)         |

### Sample requests

```bash
# Start a scrape with click-all and full capture
curl -X POST http://localhost:8080/api/scrape \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","depth":1,"format":"all","click_buttons":true}'

# List recent scans
curl -s http://localhost:8080/api/scrapes | jq

# List every file produced by a scan
curl -s http://localhost:8080/api/scrapes/<id>/files | jq

# Fetch the HTML of a specific page
curl -s "http://localhost:8080/api/scrapes/<id>/file?key=scans/<id>/pages/<pageID>.html"

# Ask the agent
curl -X POST http://localhost:8080/api/query \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Summarize this site","top_k":5}'
```

## Database schema (Postgres 17)

```
scans (
  id uuid PK, seed_url, depth, format, click_buttons,
  status, error, page_count, click_count,
  chunks_indexed, indexed_at,
  started_at, completed_at, created_at, updated_at
)

pages (
  id uuid PK, scan_id FK,
  url, title, depth, status_code,
  html_s3_key, text_s3_key, screenshot_s3_key,
  html_bytes, text_bytes, screenshot_bytes,
  click_count, error, scraped_at
)

click_states (
  id uuid PK, page_id FK, scan_id FK,
  step, selector, label,
  html_s3_key, text_s3_key, captured_at
)
```

The schema is auto-applied at API startup via `go:embed` of `migrations.sql`.

## S3 layout

```
scans/<scan_id>/
  pages/
    <page_id>.html
    <page_id>.txt
    <page_id>.png
    <page_id>/clicks/
      001.html
      001.txt
      002.html
      ...
```

## Frontend features

- Scans table with columns: URL, Status, Pages, Clicks, Index (chunk count), Started, Finished (with elapsed duration), Actions
- **Pages** modal: per-page byte counts, click-state count, view buttons for HTML / Text / Screenshot
- **Files** modal: every S3 object grouped by kind (page-html, page-text, page-screenshot, click-html, click-text)
- **File viewer**: HTML renders in a sandboxed iframe with a "Source" toggle; PNGs render inline; text shows pre-formatted
- **AI agent**: ask across all indexed scans or scope to one

## Tests

Unit tests cover the chunker, RAG prompt builder, config loader, and DB-helper logic without requiring a live Postgres:

```bash
cd api
go test -v ./...
```

For full integration testing, just `docker compose up` and exercise the API.

## Notes on safety / scope

- The scraper stays on the **seed host** (no off-domain crawling)
- Total pages per scan are capped at **50**
- Depth is capped at **5** server-side
- Click count per page is capped at **20**
- LocalStack and Postgres credentials are intentionally `test`/`scraper` — do not use in production
- File-fetch endpoint validates the key starts with `scans/<id>/` so users can't read other scans' artefacts

## File map

```
.
├── docker-compose.yml
├── api/                       # Go API (chi + AWS SDK v2 + lib/pq)
│   ├── main.go                # Config + server + router
│   ├── handlers.go            # HTTP handlers (DB-backed)
│   ├── db.go                  # Postgres layer (CRUD for scans/pages/clicks)
│   ├── migrations.sql         # Embedded schema
│   ├── scraper.go             # BFS crawler + auto-index
│   ├── storage.go             # S3 client (Put / Get / List)
│   ├── ai.go                  # ChromaDB + Ollama
│   ├── models.go              # Scan / Page / ClickState / FileEntry
│   ├── api_test.go            # Unit tests
│   ├── go.mod / go.sum
│   └── Dockerfile
├── playwright-service/        # Node.js renderer + clicker
│   ├── server.js              # /render (legacy) + /scrape (clicks all buttons)
│   ├── package.json
│   └── Dockerfile
├── frontend/                  # Tailwind HTML
│   ├── index.html
│   └── Dockerfile
├── scripts/
│   └── init-aws.sh            # LocalStack S3 bucket setup
└── README.md
```
