package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type store struct {
	db        *sql.DB
	directory string
}

func openStore(directory string) (*store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	directory = canonical
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: filepath.Join(directory, "index.sqlite")}
	params := url.Values{"_pragma": []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"}}
	dsn.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &store{db: db, directory: directory}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS scans (id TEXT PRIMARY KEY, result TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entries (
 scan_id TEXT NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
 path TEXT NOT NULL, root TEXT NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL,
 kind TEXT NOT NULL, bytes INTEGER NOT NULL, files INTEGER NOT NULL,
 modified INTEGER NOT NULL, extension TEXT NOT NULL, category TEXT NOT NULL, depth INTEGER NOT NULL,
 PRIMARY KEY(scan_id,path)
);
CREATE INDEX IF NOT EXISTS entries_parent ON entries(scan_id,parent);
CREATE INDEX IF NOT EXISTS entries_size ON entries(scan_id,kind,bytes);
CREATE INDEX IF NOT EXISTS entries_depth ON entries(scan_id,depth,kind);
CREATE INDEX IF NOT EXISTS entries_modified ON entries(scan_id,kind,modified);
CREATE TABLE IF NOT EXISTS duplicate_hashes (
 run_id TEXT NOT NULL,path TEXT NOT NULL,root TEXT NOT NULL,bytes INTEGER NOT NULL,modified INTEGER NOT NULL,
 digest TEXT NOT NULL,device_key TEXT NOT NULL,PRIMARY KEY(run_id,path),UNIQUE(run_id,device_key)
);
CREATE INDEX IF NOT EXISTS duplicate_digest ON duplicate_hashes(run_id,digest,bytes);
CREATE TABLE IF NOT EXISTS duplicate_members (
 run_id TEXT NOT NULL,group_id INTEGER NOT NULL,path TEXT NOT NULL,bytes INTEGER NOT NULL,
 PRIMARY KEY(run_id,group_id,path)
);
CREATE TABLE IF NOT EXISTS preferences (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS inspection_runs (
 day TEXT PRIMARY KEY, attempted_at TEXT NOT NULL, state TEXT NOT NULL, scan_id TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS summaries (
 id TEXT PRIMARY KEY, scope TEXT NOT NULL, scanned_at TEXT NOT NULL, result TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS summaries_scope ON summaries(scope,scanned_at);
CREATE TABLE IF NOT EXISTS rollups (
 summary_id TEXT NOT NULL REFERENCES summaries(id) ON DELETE CASCADE,
 path TEXT NOT NULL, kind TEXT NOT NULL, bytes INTEGER NOT NULL, files INTEGER NOT NULL,
 PRIMARY KEY(summary_id,path,kind)
);
CREATE TABLE IF NOT EXISTS alerts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, summary_id TEXT NOT NULL REFERENCES summaries(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, message TEXT NOT NULL, acknowledged INTEGER NOT NULL DEFAULT 0,
 UNIQUE(summary_id,kind)
);
PRAGMA user_version=2;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(filepath.Join(directory, "index.sqlite"), 0600); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("UPDATE inspection_runs SET state='interrupted' WHERE state='running'"); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.pruneLedger(context.Background(), time.Now()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) history(ctx context.Context) ([]*scanResult, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT result FROM scans ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []*scanResult{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var result scanResult
		if err := json.Unmarshal([]byte(data), &result); err != nil {
			return nil, err
		}
		list = append(list, &result)
	}
	return list, rows.Err()
}

func (s *store) get(ctx context.Context, id string) (*scanResult, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT result FROM scans WHERE id=?", id).Scan(&raw); err != nil {
		return nil, err
	}
	var result scanResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *store) analyze(ctx context.Context, id string, options scanOptions, report func(progress)) (*scanResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO scans(id,result) VALUES(?,?)", id, "{}"); err != nil {
		return nil, err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO entries(scan_id,path,root,parent,name,kind,bytes,files,modified,extension,category,depth) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer insert.Close()
	maxDepth := 0
	result, err := analyzeOptions(ctx, options, report, func(e indexedEntry) error {
		if e.Depth > maxDepth {
			maxDepth = e.Depth
		}
		_, err := insert.ExecContext(ctx, id, e.Path, e.Root, e.Parent, e.Name, e.Kind, e.Bytes, e.Files, e.Modified, e.Extension, e.Category, e.Depth)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Bottom-up aggregation keeps a full directory tree on disk, not in RAM.
	for depth := maxDepth; depth >= 0; depth-- {
		_, err = tx.ExecContext(ctx, `UPDATE entries AS parent SET
 bytes=COALESCE((SELECT SUM(child.bytes) FROM entries AS child WHERE child.scan_id=parent.scan_id AND child.parent=parent.path),0),
 files=COALESCE((SELECT SUM(child.files) FROM entries AS child WHERE child.scan_id=parent.scan_id AND child.parent=parent.path),0)
 WHERE scan_id=? AND kind='directory' AND depth=?`, id, depth)
		if err != nil {
			return nil, err
		}
	}
	result.ID = id
	result.DurationMs = time.Since(result.ScannedAt).Milliseconds()
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE scans SET result=? WHERE id=?", string(raw), id); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if options.authorize != nil {
		if err := options.authorize(ctx); err != nil {
			return nil, err
		}
	}
	volumes, unavailable := readCapacities(options.Roots)
	if err := recordSummary(ctx, tx, result, volumes, unavailable, options.inspection); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM scans WHERE id NOT IN (SELECT id FROM scans ORDER BY id DESC LIMIT 20)"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *store) prune(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM scans WHERE id NOT IN (SELECT id FROM scans ORDER BY id DESC LIMIT 20)")
	return err
}

func dataDirectory() (string, error) {
	if value := os.Getenv("DISKLENS_DATA_DIR"); value != "" {
		return filepath.Abs(value)
	}
	if value := os.Getenv("TRIM_PKGVAR"); value != "" {
		return filepath.Join(value, "disklens"), nil
	}
	if os.Getenv("TRIM_APPNAME") != "" {
		return "", fmt.Errorf("fnOS runtime data directory is missing")
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "DiskLens"), nil
}
