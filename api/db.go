package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

//go:embed migrations.sql
var migrationsSQL string

// DB wraps a *sql.DB and exposes high-level scan/page/click operations.
type DB struct {
	conn *sql.DB
}

// NewDB opens a connection pool and runs migrations. It retries for ~30s
// to accommodate Postgres being slow to start in compose.
func NewDB(ctx context.Context, dsn string) (*DB, error) {
	var conn *sql.DB
	var err error

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = sql.Open("postgres", dsn)
		if err == nil {
			err = conn.PingContext(ctx)
			if err == nil {
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	conn.SetMaxOpenConns(20)
	conn.SetMaxIdleConns(4)
	conn.SetConnMaxLifetime(time.Hour)

	if _, err := conn.ExecContext(ctx, migrationsSQL); err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &DB{conn: conn}, nil
}

func (d *DB) Close() error { return d.conn.Close() }

// ============================== SCANS ==============================

func (d *DB) CreateScan(ctx context.Context, s *Scan) error {
	_, err := d.conn.ExecContext(ctx, `
		INSERT INTO scans (id, seed_url, depth, format, click_buttons, status, started_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		s.ID, s.SeedURL, s.Depth, s.Format, s.ClickButtons, s.Status,
		s.StartedAt, s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (d *DB) UpdateScanStatus(ctx context.Context, id, status, errMsg string) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET status = $2, error = NULLIF($3,''), updated_at = now()
		WHERE id = $1`, id, status, errMsg)
	return err
}

// SetCurrentURL records what the worker is doing right now, so the UI can
// show "scraping <url>" / "indexing <url>" between progress checkpoints.
func (d *DB) SetCurrentURL(ctx context.Context, id, url string) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET current_url = NULLIF($2,''), updated_at = now()
		WHERE id = $1`, id, url)
	return err
}

// BumpScanProgress is called after each page is persisted during a scrape.
// Updates page_count + click_count atomically so the table always reflects
// the live total.
func (d *DB) BumpScanProgress(ctx context.Context, id string, pageCount, clickCount int) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET page_count = $2, click_count = $3, updated_at = now()
		WHERE id = $1`, id, pageCount, clickCount)
	return err
}

func (d *DB) MarkScanCompleted(ctx context.Context, id string, pageCount, clickCount int, errMsg string) error {
	status := "done"
	if errMsg != "" {
		status = "failed"
	}
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET
			status       = $2,
			error        = NULLIF($3,''),
			page_count   = $4,
			click_count  = $5,
			current_url  = NULL,
			completed_at = now(),
			updated_at   = now()
		WHERE id = $1`, id, status, errMsg, pageCount, clickCount)
	return err
}

// MarkIndexingStart flags the scan as actively indexing into ChromaDB.
func (d *DB) MarkIndexingStart(ctx context.Context, id string) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET indexing_status = 'running', chunks_indexed = 0, updated_at = now()
		WHERE id = $1`, id)
	return err
}

// BumpIndexProgress updates the running chunk total during auto-index.
// Called after each page (not each chunk) to keep update load reasonable.
func (d *DB) BumpIndexProgress(ctx context.Context, id string, chunksTotal int) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET chunks_indexed = $2, updated_at = now()
		WHERE id = $1`, id, chunksTotal)
	return err
}

func (d *DB) MarkScanIndexed(ctx context.Context, id string, chunks int) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET
			chunks_indexed  = $2,
			indexed_at      = now(),
			indexing_status = 'done',
			current_url     = NULL,
			updated_at      = now()
		WHERE id = $1`, id, chunks)
	return err
}

// MarkIndexingFailed records a fatal indexing error.
func (d *DB) MarkIndexingFailed(ctx context.Context, id string, errMsg string) error {
	_, err := d.conn.ExecContext(ctx, `
		UPDATE scans SET
			indexing_status = 'failed',
			error           = NULLIF($2,''),
			current_url     = NULL,
			updated_at      = now()
		WHERE id = $1`, id, errMsg)
	return err
}

func (d *DB) GetScan(ctx context.Context, id string) (*Scan, error) {
	row := d.conn.QueryRowContext(ctx, `
		SELECT id, seed_url, depth, format, click_buttons, status, COALESCE(error,''),
		       page_count, click_count, chunks_indexed, indexed_at,
		       COALESCE(current_url,''), COALESCE(indexing_status,''),
		       started_at, completed_at, created_at, updated_at
		FROM scans WHERE id = $1`, id)
	s := &Scan{}
	var indexedAt, completedAt sql.NullTime
	if err := row.Scan(&s.ID, &s.SeedURL, &s.Depth, &s.Format, &s.ClickButtons, &s.Status, &s.Error,
		&s.PageCount, &s.ClickCount, &s.ChunksIndexed, &indexedAt,
		&s.CurrentURL, &s.IndexingStatus,
		&s.StartedAt, &completedAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	if indexedAt.Valid {
		t := indexedAt.Time
		s.IndexedAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		s.CompletedAt = &t
	}
	return s, nil
}

func (d *DB) ListScans(ctx context.Context, limit int) ([]*Scan, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.conn.QueryContext(ctx, `
		SELECT id, seed_url, depth, format, click_buttons, status, COALESCE(error,''),
		       page_count, click_count, chunks_indexed, indexed_at,
		       COALESCE(current_url,''), COALESCE(indexing_status,''),
		       started_at, completed_at, created_at, updated_at
		FROM scans
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Scan, 0)
	for rows.Next() {
		s := &Scan{}
		var indexedAt, completedAt sql.NullTime
		if err := rows.Scan(&s.ID, &s.SeedURL, &s.Depth, &s.Format, &s.ClickButtons, &s.Status, &s.Error,
			&s.PageCount, &s.ClickCount, &s.ChunksIndexed, &indexedAt,
			&s.CurrentURL, &s.IndexingStatus,
			&s.StartedAt, &completedAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		if indexedAt.Valid {
			t := indexedAt.Time
			s.IndexedAt = &t
		}
		if completedAt.Valid {
			t := completedAt.Time
			s.CompletedAt = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ============================== PAGES ==============================

func (d *DB) InsertPage(ctx context.Context, p *Page) error {
	_, err := d.conn.ExecContext(ctx, `
		INSERT INTO pages (id, scan_id, url, title, depth, status_code,
		                   html_s3_key, text_s3_key, screenshot_s3_key,
		                   html_bytes, text_bytes, screenshot_bytes,
		                   click_count, error, scraped_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),$15)`,
		p.ID, p.ScanID, p.URL, p.Title, p.Depth, p.StatusCode,
		nilIfEmpty(p.HTMLS3Key), nilIfEmpty(p.TextS3Key), nilIfEmpty(p.ScreenshotS3Key),
		p.HTMLBytes, p.TextBytes, p.ScreenshotBytes,
		p.ClickCount, p.Error, p.ScrapedAt,
	)
	return err
}

func (d *DB) ListPages(ctx context.Context, scanID string) ([]Page, error) {
	rows, err := d.conn.QueryContext(ctx, `
		SELECT id, scan_id, url, COALESCE(title,''), depth, COALESCE(status_code,0),
		       COALESCE(html_s3_key,''), COALESCE(text_s3_key,''), COALESCE(screenshot_s3_key,''),
		       html_bytes, text_bytes, screenshot_bytes, click_count,
		       COALESCE(error,''), scraped_at
		FROM pages WHERE scan_id = $1 ORDER BY scraped_at`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Page, 0)
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.ID, &p.ScanID, &p.URL, &p.Title, &p.Depth, &p.StatusCode,
			&p.HTMLS3Key, &p.TextS3Key, &p.ScreenshotS3Key,
			&p.HTMLBytes, &p.TextBytes, &p.ScreenshotBytes, &p.ClickCount,
			&p.Error, &p.ScrapedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetPage(ctx context.Context, id string) (*Page, error) {
	row := d.conn.QueryRowContext(ctx, `
		SELECT id, scan_id, url, COALESCE(title,''), depth, COALESCE(status_code,0),
		       COALESCE(html_s3_key,''), COALESCE(text_s3_key,''), COALESCE(screenshot_s3_key,''),
		       html_bytes, text_bytes, screenshot_bytes, click_count,
		       COALESCE(error,''), scraped_at
		FROM pages WHERE id = $1`, id)
	var p Page
	if err := row.Scan(&p.ID, &p.ScanID, &p.URL, &p.Title, &p.Depth, &p.StatusCode,
		&p.HTMLS3Key, &p.TextS3Key, &p.ScreenshotS3Key,
		&p.HTMLBytes, &p.TextBytes, &p.ScreenshotBytes, &p.ClickCount,
		&p.Error, &p.ScrapedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// ============================== CLICK STATES ==============================

func (d *DB) InsertClickState(ctx context.Context, c *ClickState) error {
	_, err := d.conn.ExecContext(ctx, `
		INSERT INTO click_states (id, page_id, scan_id, step, selector, label, html_s3_key, text_s3_key, captured_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9)`,
		c.ID, c.PageID, c.ScanID, c.Step, c.Selector, c.Label,
		nilIfEmpty(c.HTMLS3Key), nilIfEmpty(c.TextS3Key), c.CapturedAt,
	)
	return err
}

func (d *DB) ListClicksForPage(ctx context.Context, pageID string) ([]ClickState, error) {
	rows, err := d.conn.QueryContext(ctx, `
		SELECT id, page_id, scan_id, step, selector, COALESCE(label,''),
		       COALESCE(html_s3_key,''), COALESCE(text_s3_key,''), captured_at
		FROM click_states WHERE page_id = $1 ORDER BY step`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ClickState, 0)
	for rows.Next() {
		var c ClickState
		if err := rows.Scan(&c.ID, &c.PageID, &c.ScanID, &c.Step, &c.Selector, &c.Label,
			&c.HTMLS3Key, &c.TextS3Key, &c.CapturedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) ListClicksForScan(ctx context.Context, scanID string) ([]ClickState, error) {
	rows, err := d.conn.QueryContext(ctx, `
		SELECT id, page_id, scan_id, step, selector, COALESCE(label,''),
		       COALESCE(html_s3_key,''), COALESCE(text_s3_key,''), captured_at
		FROM click_states WHERE scan_id = $1 ORDER BY page_id, step`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ClickState, 0)
	for rows.Next() {
		var c ClickState
		if err := rows.Scan(&c.ID, &c.PageID, &c.ScanID, &c.Step, &c.Selector, &c.Label,
			&c.HTMLS3Key, &c.TextS3Key, &c.CapturedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
