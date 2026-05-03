# Web Scraper Console

A simple but complete web-scraping system that:

- Renders JS-heavy pages with **Playwright**
- Crawls links to a configurable **depth (0–5)**
- Stores output (raw text, HTML, or screenshot) in **S3 (LocalStack)**
- Indexes scraped text into **ChromaDB** for retrieval
- Answers questions with **Ollama** (`llama3.2`) using the indexed context
- Provides a **Tailwind** HTML console for everything

## Stack

| Service     | What it does                       | Port |
|-------------|------------------------------------|------|
| `frontend`  | Static Tailwind UI (nginx)         | 8000 |
| `api`       | Go HTTP API (chi router)           | 8080 |
| `playwright`| Node.js Playwright renderer        | 3000 |
| `localstack`| S3-compatible storage              | 4566 |
| `chromadb`  | Vector store                       | 8001 |
| `ollama`    | LLM + embeddings                   | 11434|

All services share an internal Docker network (`scraper-net`).

## Quick start

```bash
docker compose up --build
```

First boot:
- LocalStack creates the `scraper-data` S3 bucket via `scripts/init-aws.sh`
- `ollama-init` pulls `llama3.2` and `nomic-embed-text` (this can take a few minutes)
- Open **http://localhost:8000**

## Usage

1. Enter a URL, pick a depth (0 = seed only, 1 = seed + linked pages, ...) and a format
2. Click **Start scrape** — the job runs asynchronously
3. The table refreshes every 4 s; click **View** to see captured pages
4. Click **AI agent** on a finished job to index its text into ChromaDB
5. Use the **AI agent** panel to ask questions; answers are grounded in retrieved chunks

## API

| Method | Path                          | Description                         |
|--------|-------------------------------|-------------------------------------|
| GET    | `/health`                     | Liveness probe                      |
| POST   | `/api/scrape`                 | Start a scrape job                  |
| GET    | `/api/scrapes`                | List all jobs                       |
| GET    | `/api/scrapes/{id}`           | Get one job                         |
| GET    | `/api/scrapes/{id}/page?key=` | Stream a page artifact from S3      |
| POST   | `/api/scrapes/{id}/index`     | Index job text into ChromaDB        |
| POST   | `/api/query`                  | RAG query (ChromaDB + Ollama)       |

### Sample requests

```bash
# Start a scrape
curl -X POST http://localhost:8080/api/scrape \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","depth":1,"format":"text"}'

# Ask the agent
curl -X POST http://localhost:8080/api/query \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Summarize what this site is about","top_k":5}'
```

## Tests

The Go test suite mocks Playwright, ChromaDB, Ollama, and S3 with `httptest`,
so no Docker is needed to run them:

```bash
cd api
go test -v ./...
```

The end-to-end test exercises the full flow: scrape → S3 store → page fetch
→ Chroma index → RAG query.

## Notes on safety / scope

- The scraper stays on the **seed host** (no off-domain crawling)
- Total pages per job are capped at **50**
- Depth is capped at **5** server-side
- LocalStack credentials are intentionally `test`/`test` — do not use in production
- Job state is in-memory; restarting the API loses the job list (artifacts in S3 persist)

## File map

```
.
├── docker-compose.yml
├── api/                  # Go API (chi + AWS SDK v2)
│   ├── main.go
│   ├── handlers.go
│   ├── scraper.go
│   ├── storage.go
│   ├── ai.go
│   ├── models.go
│   └── api_test.go
├── playwright-service/   # Node.js renderer
│   ├── server.js
│   └── package.json
├── frontend/             # Tailwind HTML
│   └── index.html
└── scripts/
    └── init-aws.sh       # LocalStack S3 bucket setup
```
"# zwebscrape" 
