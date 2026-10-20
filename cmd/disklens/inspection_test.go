package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func saveTestInspection(t *testing.T, index *store, settings inspectionSettings) {
	t.Helper()
	raw, _ := json.Marshal(settings)
	if _, err := index.db.Exec("INSERT INTO preferences(key,value) VALUES('inspection',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestInspectionValidationPersistenceAndRedaction(t *testing.T) {
	index, root := testStore(t), t.TempDir()
	settings := defaultInspection()
	if settings.Enabled {
		t.Fatal("schedule must be opt-in")
	}
	settings.Enabled, settings.Roots = true, []string{root}
	settings.Excludes = []string{"*.tmp"}
	valid, err := validateInspection(settings, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*inspectionSettings){
		"clock":           func(s *inspectionSettings) { s.At = "25:00" },
		"ambiguous clock": func(s *inspectionSettings) { s.At = "3:00" },
		"zone":            func(s *inspectionSettings) { s.Timezone = "Invalid/Zone" },
		"local":           func(s *inspectionSettings) { s.Timezone = "Local" },
		"roots":           func(s *inspectionSettings) { s.Roots = nil },
		"rate":            func(s *inspectionSettings) { s.EntriesPerSecond = 1 },
		"duration":        func(s *inspectionSettings) { s.TimeoutMinutes = 1441 },
		"growth":          func(s *inspectionSettings) { s.GrowthBytes = 0 },
		"capacity":        func(s *inspectionSettings) { s.FreePercent = 100 },
	} {
		t.Run(name, func(t *testing.T) {
			input := valid
			change(&input)
			if _, err := validateInspection(input, []string{root}); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
	s := &server{store: index, allowedRoots: []string{root}}
	raw, _ := json.Marshal(valid)
	api(t, s, "POST", "/api/inspection", string(raw), 200)
	loaded, err := index.inspection(context.Background())
	if err != nil || !loaded.Enabled || loaded.Roots[0] != valid.Roots[0] {
		t.Fatalf("settings not saved: %+v %v", loaded, err)
	}
	s.job = &scanJob{State: "running"}
	api(t, s, "POST", "/api/inspection", string(raw), 409)
	s.job, s.allowedRoots = nil, nil
	response := api(t, s, "GET", "/api/inspection", "", 200)
	if strings.Contains(response.Body.String(), root) || !strings.Contains(response.Body.String(), `"scopeValid":false`) {
		t.Fatal("revoked settings exposed")
	}
}

func TestScheduleSlots(t *testing.T) {
	for _, test := range []struct{ at, zone, now, want string }{
		{"03:00", "UTC", "2026-10-19T03:12:00Z", "UTC:2026-10-19"},
		{"23:30", "UTC", "2026-10-20T00:10:00Z", "UTC:2026-10-19"},
		{"03:00", "Asia/Shanghai", "2026-10-18T19:10:00Z", "Asia/Shanghai:2026-10-19"},
		{"01:30", "America/New_York", "2026-11-01T05:40:00Z", "America/New_York:2026-11-01"},
		{"01:30", "America/New_York", "2026-11-01T06:40:00Z", "America/New_York:2026-11-01"},
	} {
		now, _ := time.Parse(time.RFC3339, test.now)
		_, key, err := scheduleSlot(inspectionSettings{At: test.at, Timezone: test.zone}, now)
		if err != nil || key != test.want {
			t.Fatalf("slot %s: %s %v", test.now, key, err)
		}
	}
}

func TestScheduleExactlyOnceBusyRestartAndRevocation(t *testing.T) {
	index, root := testStore(t), t.TempDir()
	fixtureFile(t, root, "file.txt", "content")
	settings := defaultInspection()
	settings.Enabled, settings.Roots, settings.Timezone, settings.At = true, []string{root}, "UTC", "03:00"
	saveTestInspection(t, index, settings)
	now := time.Date(2026, 10, 19, 3, 5, 0, 0, time.UTC)
	s := &server{store: index, allowedRoots: []string{root}, job: &scanJob{State: "running"}}
	if err := s.scheduleTick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var count int
	index.db.QueryRow("SELECT COUNT(*) FROM inspection_runs").Scan(&count)
	if count != 0 {
		t.Fatal("busy scan consumed schedule")
	}
	s.job = nil
	var callers sync.WaitGroup
	for i := 0; i < 3; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if err := s.scheduleTick(context.Background(), now); err != nil {
				t.Error(err)
			}
		}()
	}
	callers.Wait()
	s.workers.Wait()
	if s.job == nil || s.job.Source != "schedule" || s.job.State != "completed" {
		t.Fatalf("scheduled scan: %+v", s.job)
	}
	restarted := &server{store: index, allowedRoots: []string{root}}
	if err := restarted.scheduleTick(context.Background(), now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	restarted.workers.Wait()
	index.db.QueryRow("SELECT COUNT(*) FROM scans").Scan(&count)
	if count != 1 || restarted.job != nil {
		t.Fatal("same-day restart duplicated scan")
	}
	if err := restarted.scheduleTick(context.Background(), now.AddDate(0, 0, 1).Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if restarted.job != nil {
		t.Fatal("missed window replayed")
	}
	restarted.allowedRoots = nil
	if err := restarted.scheduleTick(context.Background(), now.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	var state string
	index.db.QueryRow("SELECT state FROM inspection_runs ORDER BY attempted_at DESC LIMIT 1").Scan(&state)
	if state != "authorization_failed" || restarted.job != nil {
		t.Fatal("revoked roots scanned")
	}
	restarted.allowedRoots = []string{root}
	if err := restarted.scheduleTick(context.Background(), now.AddDate(0, 0, 3)); err != nil {
		t.Fatal(err)
	}
	restarted.workers.Wait()
	index.db.QueryRow("SELECT COUNT(*) FROM scans").Scan(&count)
	if count != 2 {
		t.Fatalf("next day not scanned: %d", count)
	}
}

func TestLedgerOutlivesIndexAndIncludesSmallDirectories(t *testing.T) {
	index, root := testStore(t), t.TempDir()
	for i := 0; i < 25; i++ {
		fixtureFile(t, root, fmt.Sprintf("dir%02d/file.txt", i), strings.Repeat("a", i+1))
	}
	fixtureFile(t, root, "loose.txt", "a")
	for i := 1; i <= 22; i++ {
		if i == 22 {
			fixtureFile(t, root, "dir00/file.txt", "ab")
			fixtureFile(t, root, "loose.txt", "abc")
		}
		snapshot(t, index, fmt.Sprintf("%03d", i), []string{root})
	}
	var remaining int
	index.db.QueryRow("SELECT COUNT(*) FROM entries WHERE scan_id='001'").Scan(&remaining)
	if remaining != 0 {
		t.Fatal("old full index not pruned")
	}
	list, err := index.summaries(context.Background())
	if err != nil || len(list) != 22 {
		t.Fatalf("ledger retention: %d %v", len(list), err)
	}
	s := &server{store: index, allowedRoots: []string{root}}
	response := api(t, s, "GET", "/api/ledger?before=001&after=022", "", 200)
	var diff struct {
		Delta       int64          `json:"deltaBytes"`
		Directories []rollupChange `json:"directories"`
		Partial     bool           `json:"partial"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Delta != 3 || len(diff.Directories) != 2 || diff.Partial {
		t.Fatalf("incorrect ledger: %+v", diff)
	}
	var total int64
	for _, item := range diff.Directories {
		total += item.Delta
	}
	if total != diff.Delta {
		t.Fatal("directory/root-file totals do not reconcile")
	}
	snapshot(t, index, "023", []string{root}, "*.tmp")
	api(t, s, "GET", "/api/ledger?before=001&after=023", "", 400)
	api(t, s, "GET", "/api/ledger?before=022&after=001", "", 400)
	s.allowedRoots = nil
	response = api(t, s, "GET", "/api/ledger", "", 200)
	if strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatal("revoked ledger visible")
	}
	api(t, s, "GET", "/api/ledger?before=001&after=022", "", 404)
}

func TestCoverageAndThrottledCancellation(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "excluded.tmp", "excluded")
	fixtureFile(t, root, "actual.txt", "actual")
	if err := os.Symlink(filepath.Join(root, "actual.txt"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	result, err := analyzeOptions(context.Background(), scanOptions{Roots: []string{root}, Excludes: []string{"*.tmp"}}, nil, nil)
	if err != nil || result.Excluded != 1 || result.Symlinks != 1 || result.ReadErrors != 0 || result.Skipped != 2 {
		t.Fatalf("coverage: %+v %v", result, err)
	}
	index := testStore(t)
	for i := 0; i < 100; i++ {
		fixtureFile(t, root, fmt.Sprintf("%03d.txt", i), "x")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = index.analyze(ctx, "cancelled", scanOptions{Roots: []string{root}, entryRate: 32}, nil)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("throttled cancellation: %v", err)
	}
	list, err := index.summaries(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatal("cancelled scan published summary")
	}
}

func TestGrowthAlertsThresholdsAndRevocation(t *testing.T) {
	index, root := testStore(t), t.TempDir()
	fixtureFile(t, root, "data.txt", "1234")
	settings := defaultInspection()
	settings.GrowthBytes, settings.GrowthPercent, settings.FreeEnabled = 2, 20, false
	options := scanOptions{Roots: []string{root}, inspection: &settings}
	if _, err := index.analyze(context.Background(), "001", options, nil); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, root, "data.txt", "12345678")
	if _, err := index.analyze(context.Background(), "002", options, nil); err != nil {
		t.Fatal(err)
	}
	s := &server{store: index, allowedRoots: []string{root}}
	response := api(t, s, "GET", "/api/alerts", "", 200)
	var alerts struct {
		Items  []ledgerAlert `json:"items"`
		Unread int           `json:"unread"`
	}
	json.Unmarshal(response.Body.Bytes(), &alerts)
	if len(alerts.Items) != 1 || alerts.Unread != 1 || alerts.Items[0].Kind != "growth" {
		t.Fatalf("growth alert: %s", response.Body.String())
	}
	id := alerts.Items[0].ID
	api(t, s, "POST", "/api/alerts", fmt.Sprintf(`{"id":%d}`, id), 200)
	response = api(t, s, "GET", "/api/alerts", "", 200)
	if !strings.Contains(response.Body.String(), `"unread":0`) {
		t.Fatal("ack not saved")
	}
	fixtureFile(t, root, "data.txt", "123456789")
	if _, err := index.analyze(context.Background(), "003", options, nil); err != nil {
		t.Fatal(err)
	}
	response = api(t, s, "GET", "/api/alerts", "", 200)
	json.Unmarshal(response.Body.Bytes(), &alerts)
	if len(alerts.Items) != 1 {
		t.Fatal("small growth generated alert")
	}
	s.allowedRoots = nil
	response = api(t, s, "GET", "/api/alerts", "", 200)
	if strings.Contains(response.Body.String(), "基准扫描") || !strings.Contains(response.Body.String(), `"unread":0`) {
		t.Fatal("revoked alert visible")
	}
	api(t, s, "POST", "/api/alerts", fmt.Sprintf(`{"id":%d}`, id), 404)
}

func TestCapacityTransitionsIncompleteGrowthAndRetention(t *testing.T) {
	index, root := testStore(t), t.TempDir()
	settings := defaultInspection()
	settings.GrowthBytes, settings.GrowthPercent = 1, 1
	base := time.Now().Add(-time.Hour)
	add := func(id string, available uint64, size int64, readErrors int, at time.Time) {
		t.Helper()
		tx, err := index.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		result := &scanResult{ID: id, Root: root, Roots: []string{root}, ScannedAt: at, CoverageVersion: 1, progress: progress{TotalBytes: size, ReadErrors: readErrors}}
		err = recordSummary(context.Background(), tx, result, []*capacity{{ID: "disk", Roots: []string{root}, Total: 100, Available: available}}, nil, &settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	add("001", 30, 1, 0, base)
	add("002", 5, 100, 1, base.Add(time.Minute))
	add("003", 4, 200, 0, base.Add(2*time.Minute))
	add("004", 40, 200, 0, base.Add(3*time.Minute))
	add("005", 5, 200, 0, base.Add(4*time.Minute))
	var capacityCount, growthCount int
	index.db.QueryRow("SELECT COUNT(*) FROM alerts WHERE kind LIKE 'capacity:%'").Scan(&capacityCount)
	index.db.QueryRow("SELECT COUNT(*) FROM alerts WHERE kind='growth'").Scan(&growthCount)
	if capacityCount != 2 || growthCount != 0 {
		t.Fatalf("transition counts: capacity=%d growth=%d", capacityCount, growthCount)
	}
	add("expired", 1, 1, 0, time.Now().Add(-366*24*time.Hour))
	var expired int
	index.db.QueryRow("SELECT COUNT(*) FROM summaries WHERE id='expired'").Scan(&expired)
	if expired != 0 {
		t.Fatal("expired summary retained")
	}
	index.db.QueryRow("SELECT COUNT(*) FROM alerts WHERE summary_id='expired'").Scan(&expired)
	if expired != 0 {
		t.Fatal("expired alerts not cascaded")
	}
}

func expireSummary(t *testing.T, index *store, id string) {
	t.Helper()
	var raw string
	if err := index.db.QueryRow("SELECT result FROM summaries WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var summary scanSummary
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		t.Fatal(err)
	}
	summary.ScannedAt = time.Now().Add(-366 * 24 * time.Hour)
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.Exec("UPDATE summaries SET scanned_at=?, result=? WHERE id=?", summary.ScannedAt.UTC().Format(ledgerTimeLayout), string(data), id); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerRetentionWithoutNewScan(t *testing.T) {
	for _, mode := range []string{"maintenance", "startup"} {
		t.Run(mode, func(t *testing.T) {
			index, root := testStore(t), t.TempDir()
			file := fixtureFile(t, root, "keep.txt", "source file stays untouched")
			snapshot(t, index, "old", []string{root})
			expireSummary(t, index, "old")
			if _, err := index.db.Exec("INSERT INTO alerts(summary_id,kind,message) VALUES('old','test','expired alert')"); err != nil {
				t.Fatal(err)
			}
			if _, err := index.db.Exec("INSERT INTO inspection_runs(day,attempted_at,state,scan_id) VALUES('old',?,'completed','old')", time.Now().Add(-366*24*time.Hour).UTC().Format(ledgerTimeLayout)); err != nil {
				t.Fatal(err)
			}
			saveTestInspection(t, index, defaultInspection())
			s := &server{store: index, allowedRoots: []string{root}}
			if list, err := index.summaries(context.Background()); err != nil || len(list) != 0 {
				t.Fatalf("expired ledger visible without new scan: %d %v", len(list), err)
			}
			if response := api(t, s, "GET", "/api/alerts", "", 200); strings.Contains(response.Body.String(), "expired alert") {
				t.Fatal("expired alert visible")
			}
			if mode == "maintenance" {
				s.job = &scanJob{State: "running"}
				if done, err := s.maintainLedger(context.Background(), time.Now()); done || err != nil {
					t.Fatalf("maintenance did not defer: %v %v", done, err)
				}
				var count int
				if err := index.db.QueryRow("SELECT COUNT(*) FROM summaries").Scan(&count); err != nil || count != 1 {
					t.Fatalf("busy maintenance changed data: %d %v", count, err)
				}
				s.job = nil
				if done, err := s.maintainLedger(context.Background(), time.Now()); !done || err != nil {
					t.Fatalf("idle maintenance failed: %v %v", done, err)
				}
			} else {
				if err := index.db.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				index, err = openStore(index.directory)
				if err != nil {
					t.Fatal(err)
				}
				defer index.db.Close()
			}
			for _, table := range []string{"summaries", "rollups", "alerts", "inspection_runs"} {
				var count int
				if err := index.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s retained: %d %v", table, count, err)
				}
			}
			if history, err := index.history(context.Background()); err != nil || len(history) != 1 {
				t.Fatalf("independent full-index history changed: %d %v", len(history), err)
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "source file stays untouched" {
				t.Fatalf("source changed: %s %v", data, err)
			}
			if _, err := index.inspection(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExpiredSummaryNotUsedForGrowth(t *testing.T) {
	index, root := testStore(t), t.TempDir()
	fixtureFile(t, root, "file.txt", "x")
	snapshot(t, index, "old", []string{root})
	expireSummary(t, index, "old")
	fixtureFile(t, root, "file.txt", strings.Repeat("x", 100))
	settings := defaultInspection()
	settings.FreeEnabled, settings.GrowthBytes, settings.GrowthPercent = false, 1, 1
	if _, err := index.analyze(context.Background(), "new", scanOptions{Roots: []string{root}, inspection: &settings}, nil); err != nil {
		t.Fatal(err)
	}
	var alerts int
	if err := index.db.QueryRow("SELECT COUNT(*) FROM alerts WHERE kind='growth'").Scan(&alerts); err != nil || alerts != 0 {
		t.Fatalf("expired baseline generated growth alert: %d %v", alerts, err)
	}
}
