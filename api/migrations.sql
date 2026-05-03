-- Schema for the web scraper. Loaded once at API startup.
-- All tables are idempotent (CREATE IF NOT EXISTS) so the API can run them
-- on every boot without breaking.

CREATE TABLE IF NOT EXISTS scans (
    id              UUID PRIMARY KEY,
    seed_url        TEXT NOT NULL,
    depth           INT  NOT NULL DEFAULT 0,
    format          TEXT NOT NULL DEFAULT 'all',
    click_buttons   BOOLEAN NOT NULL DEFAULT TRUE,
    status          TEXT NOT NULL DEFAULT 'pending',  -- pending | running | done | failed
    error           TEXT,
    page_count      INT  NOT NULL DEFAULT 0,
    click_count     INT  NOT NULL DEFAULT 0,
    chunks_indexed  INT  NOT NULL DEFAULT 0,
    indexed_at      TIMESTAMPTZ,
    current_url     TEXT,                             -- url currently being scraped/indexed
    indexing_status TEXT,                             -- null | running | done | failed
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotent migrations for older databases that predate these columns
ALTER TABLE scans ADD COLUMN IF NOT EXISTS current_url     TEXT;
ALTER TABLE scans ADD COLUMN IF NOT EXISTS indexing_status TEXT;

CREATE INDEX IF NOT EXISTS scans_status_idx     ON scans (status);
CREATE INDEX IF NOT EXISTS scans_created_at_idx ON scans (created_at DESC);

CREATE TABLE IF NOT EXISTS pages (
    id                UUID PRIMARY KEY,
    scan_id           UUID NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
    url               TEXT NOT NULL,
    title             TEXT,
    depth             INT  NOT NULL DEFAULT 0,
    status_code       INT,
    html_s3_key       TEXT,         -- raw HTML in S3
    text_s3_key       TEXT,         -- extracted plain text in S3
    screenshot_s3_key TEXT,         -- PNG screenshot in S3
    html_bytes        BIGINT NOT NULL DEFAULT 0,
    text_bytes        BIGINT NOT NULL DEFAULT 0,
    screenshot_bytes  BIGINT NOT NULL DEFAULT 0,
    click_count       INT NOT NULL DEFAULT 0,
    error             TEXT,
    scraped_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS pages_scan_id_idx ON pages (scan_id);
CREATE INDEX IF NOT EXISTS pages_url_idx     ON pages (url);

-- Each page may produce multiple "click states" — one per button clicked.
-- The state captured AFTER the click is stored as HTML+text in S3.
CREATE TABLE IF NOT EXISTS click_states (
    id            UUID PRIMARY KEY,
    page_id       UUID NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    scan_id       UUID NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
    step          INT  NOT NULL,
    selector      TEXT NOT NULL,    -- CSS-ish selector identifying what was clicked
    label         TEXT,             -- visible button label, if any
    html_s3_key   TEXT,
    text_s3_key   TEXT,
    captured_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS click_states_page_id_idx ON click_states (page_id);
CREATE INDEX IF NOT EXISTS click_states_scan_id_idx ON click_states (scan_id);
