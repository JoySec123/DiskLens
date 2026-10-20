package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}
func fixtureFile(t *testing.T, root, path, content string) string {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return full
}
func snapshot(t *testing.T, s *store, id string, roots []string, excludes ...string) *scanResult {
	t.Helper()
	result, err := s.analyze(context.Background(), id, scanOptions{Roots: roots, Excludes: excludes}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func api(t *testing.T, s *server, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	s.handler(false).ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: %d, %s", method, path, rec.Code, rec.Body.String())
	}
	return rec
}
func queryPage(t *testing.T, s *store, id string, params url.Values, browse bool) entryPage {
	t.Helper()
	q, err := parseQuery(params, id, browse)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.page(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestIndexFullQueriesAndDrilldown(t *testing.T) {
	s := testStore(t)
	root := t.TempDir()
	for i := 1; i <= 75; i++ {
		fixtureFile(t, root, fmt.Sprintf("deep/nested/%03d.txt", i), strings.Repeat("x", i))
	}
	fixtureFile(t, root, "deep/ignored.tmp", "ignore")
	fixtureFile(t, root, "@recycle/secret.txt", "ignore")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	data := snapshot(t, s, "001", []string{root}, "*.tmp", "@recycle")
	if data.FileCount != 75 || data.TotalBytes != 2850 || data.Skipped != 2 {
		t.Fatalf("incorrect scan: %+v", data)
	}
	page := queryPage(t, s, "001", url.Values{"q": {"001.txt"}}, false)
	if page.Total != 1 || page.Items[0].Bytes != 1 {
		t.Fatalf("full-index search missing non-Top50: %+v", page)
	}
	page = queryPage(t, s, "001", url.Values{"ext": {"TXT"}, "min": {"30"}, "max": {"40"}, "sort": {"size"}, "order": {"asc"}, "pageSize": {"5"}, "page": {"2"}}, false)
	if page.Total != 11 || len(page.Items) != 5 || page.Items[0].Bytes != 35 {
		t.Fatalf("filter/page failure: %+v", page)
	}
	page = queryPage(t, s, "001", url.Values{"path": {root}}, true)
	if page.Total != 2 {
		t.Fatalf("root contents: %+v", page)
	}
	var deep indexedEntry
	for _, e := range page.Items {
		if e.Name == "deep" {
			deep = e
		}
	}
	if deep.Files != 75 || deep.Bytes != 2850 {
		t.Fatalf("recursive directory aggregate: %+v", deep)
	}
	page = queryPage(t, s, "001", url.Values{"path": {filepath.Join(root, "deep")}}, true)
	if page.Total != 1 || page.Items[0].Name != "nested" || page.Items[0].Bytes != 2850 {
		t.Fatalf("drilldown: %+v", page)
	}
	page = queryPage(t, s, "001", url.Values{}, true)
	if page.Total != 1 || page.Items[0].Path != root || page.Items[0].Files != 75 {
		t.Fatalf("root index: %+v", page)
	}
	page = queryPage(t, s, "001", url.Values{"since": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}, false)
	if page.Total != 0 {
		t.Fatal("modified filter failed")
	}
	response := api(t, &server{store: s, allowedRoots: []string{root}}, "GET", "/api/export?id=001", "", 200)
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(response.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(records) != 76 {
		t.Fatalf("full CSV truncated: rows=%d error=%v", len(records), err)
	}
}

func TestMultipleRootsAndExcludeValidation(t *testing.T) {
	root, second := t.TempDir(), t.TempDir()
	fixtureFile(t, root, "a/nested.txt", "123")
	fixtureFile(t, second, "b.txt", "45")
	options, err := validateOptions(scanOptions{Roots: []string{root, filepath.Join(root, "a"), root, second}, Excludes: []string{"*.tmp", "*.tmp"}}, []string{root, second})
	if err != nil || len(options.Roots) != 2 || len(options.Excludes) != 1 {
		t.Fatalf("overlap dedup: %+v %v", options, err)
	}
	s := testStore(t)
	data := snapshot(t, s, "001", options.Roots, options.Excludes...)
	if data.TotalBytes != 5 || data.FileCount != 2 {
		t.Fatalf("double counted roots: %+v", data)
	}
	for _, rules := range [][]string{{"["}, {"../secret"}, {"."}, {"/"}} {
		if _, err := validateOptions(scanOptions{Roots: []string{root}, Excludes: rules}, []string{root}); err == nil {
			t.Fatalf("accepted rule %q", rules)
		}
	}
	if _, err := validateOptions(scanOptions{Roots: []string{second}}, []string{root}); err == nil {
		t.Fatal("unauthorized root accepted")
	}
	if !excluded(root, "foo/bar.tmp", []string{"*.tmp"}) || !excluded(root, "foo/deep/file", []string{"foo"}) || excluded(root, "foobar/file", []string{"foo"}) {
		t.Fatal("exclusions invalid")
	}
}

func TestPersistenceCancellationAndRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixtureFile(t, root, "file.txt", "original")
	snapshot(t, s, "001", []string{root})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.analyze(ctx, "002", scanOptions{Roots: []string{root}}, func(progress) { cancel() }); err == nil {
		t.Fatal("cancelled scan committed")
	}
	list, err := s.history(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("partial snapshot visible: %v %v", list, err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	list, err = s.history(context.Background())
	if err != nil || len(list) != 1 || list[0].FileCount != 1 {
		t.Fatalf("restart lost history: %+v %v", list, err)
	}
	for i := 2; i <= 22; i++ {
		snapshot(t, s, fmt.Sprintf("%03d", i), []string{root})
	}
	if err := s.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err = s.history(context.Background())
	if err != nil || len(list) != 20 || list[0].ID != "022" {
		t.Fatalf("retention: %v %v", len(list), err)
	}
	var orphans int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM entries WHERE scan_id='001'").Scan(&orphans); err != nil || orphans != 0 {
		t.Fatal("retained orphan index")
	}
	if content, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(content) != "original" {
		t.Fatal("source data was modified")
	}
}

func TestComparisonExportAndRevocation(t *testing.T) {
	index := testStore(t)
	root := t.TempDir()
	fixtureFile(t, root, "folder/growing.txt", "abc")
	removed := fixtureFile(t, root, "removed.txt", "abcde")
	fixtureFile(t, root, "=formula.txt", "constant")
	before := snapshot(t, index, "001", []string{root})
	fixtureFile(t, root, "folder/growing.txt", "abcdefghi")
	fixtureFile(t, root, "new.txt", "abcd")
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, index, "002", []string{root})
	s := &server{store: index, allowedRoots: []string{root}, last: after}
	rec := api(t, s, "GET", "/api/compare?before=001&after=002", "", 200)
	var comparison comparison
	if err := json.Unmarshal(rec.Body.Bytes(), &comparison); err != nil {
		t.Fatal(err)
	}
	if comparison.Added != 1 || comparison.Removed != 1 || comparison.Changed != 1 || comparison.DeltaBytes != 5 || len(comparison.Directories) != 1 {
		t.Fatalf("incorrect diff: %+v", comparison)
	}
	if !sameScope(before, after) {
		t.Fatal("same scope rejected")
	}
	snapshot(t, index, "003", []string{root}, "*.tmp")
	api(t, s, "GET", "/api/compare?before=001&after=003", "", 400)
	rec = api(t, s, "GET", "/api/export?id=002&ext=txt", "", 200)
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(rec.Body.String(), "\ufeff")))
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("CSV missing full rows: %v", records)
	}
	var protected bool
	for _, record := range records {
		if record[0] == "'=formula.txt" {
			protected = true
		}
	}
	if !protected {
		t.Fatal("CSV formula injection")
	}
	for _, value := range []string{"=cmd", " +SUM(1)", "@foo", "\tformula", "-123", "\n=cmd"} {
		if !strings.HasPrefix(csvSafe(value), "'") {
			t.Fatalf("unprotected %q", value)
		}
	}
	s.allowedRoots = nil
	for _, endpoint := range []string{"/api/files?id=002", "/api/browse?id=002", "/api/compare?before=001&after=002", "/api/export?id=002"} {
		api(t, s, "GET", endpoint, "", 403)
	}
	api(t, s, "POST", "/api/duplicates", `{"id":"002"}`, 403)
	rec = api(t, s, "GET", "/api/history", "", 200)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatal("revoked history visible")
	}
	rec = api(t, s, "GET", "/api/config", "", 200)
	if !strings.Contains(rec.Body.String(), `"lastScan":null`) {
		t.Fatal("revoked config visible")
	}
}

func TestDuplicateVerificationAndHardlinks(t *testing.T) {
	index := testStore(t)
	root := t.TempDir()
	original := fixtureFile(t, root, "a.bin", "identical payload")
	fixtureFile(t, root, "nested/b.bin", "identical payload")
	fixtureFile(t, root, "c.bin", "different payload")
	if err := os.Link(original, filepath.Join(root, "hardlink.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, root, "empty.txt", "")
	data := snapshot(t, index, "001", []string{root})
	s := &server{store: index, allowedRoots: []string{root}}
	job := &duplicateJob{ID: "dupes1", ScanID: data.ID, State: "running"}
	budget := &readBudget{ctx: context.Background(), rate: 1 << 40, report: func(int64) error { return nil }}
	if err := s.detectDuplicates(context.Background(), data, job, 1, budget, func(bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if job.Groups != 1 || job.LogicalDuplicateBytes != 17 || job.Skipped != 1 {
		t.Fatalf("duplicates: %+v", job)
	}
	var count int
	if err := index.db.QueryRow("SELECT COUNT(*) FROM duplicate_members WHERE run_id='dupes1'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate membership: %d %v", count, err)
	}
	job.State = "completed"
	s.duplicate = job
	api(t, s, "GET", "/api/duplicates?pageSize=1", "", 200)
	rec := api(t, s, "GET", "/api/duplicates?group=1&pageSize=1&page=2", "", 200)
	if !strings.Contains(rec.Body.String(), `"total":2`) {
		t.Fatal("duplicate member pagination missing")
	}
	s.allowedRoots = nil
	api(t, s, "GET", "/api/duplicates", "", 403)
}

func TestChangedFilesLinksAndCancellation(t *testing.T) {
	index := testStore(t)
	root := t.TempDir()
	file := fixtureFile(t, root, "a.bin", "before")
	snapshot(t, index, "001", []string{root})
	page := queryPage(t, index, "001", url.Values{}, false)
	entry := page.Items[0]
	fixtureFile(t, root, "a.bin", "after-file-length")
	if f, _, err := openCandidate(entry); err == nil {
		f.Close()
		t.Fatal("accepted changed metadata")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", file); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openCandidate(entry); err == nil {
		f.Close()
		t.Fatal("followed replaced symlink")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(file, 0600); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openCandidate(entry); err == nil {
		f.Close()
		t.Fatal("accepted FIFO")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget := &readBudget{ctx: ctx, rate: 1, report: func(int64) error { return nil }}
	started := time.Now()
	if err := budget.consume(1024); err != context.Canceled || time.Since(started) > time.Second {
		t.Fatal("throttle cancellation blocked")
	}
}

func TestQueryValidation(t *testing.T) {
	for _, params := range []url.Values{{"page": {"0"}}, {"pageSize": {"9999"}}, {"min": {"-1"}}, {"max": {"1"}, "min": {"2"}}, {"sort": {"bytes; DROP TABLE scans"}}, {"order": {"bogus"}}, {"since": {"garbage"}}, {"since": {"2026-02-01T00:00:00Z"}, "until": {"2026-01-01T00:00:00Z"}}} {
		if _, err := parseQuery(params, "001", false); err == nil {
			t.Fatalf("accepted invalid query: %v", params)
		}
	}
}

func TestDuplicateLifecycleCancelAndConflict(t *testing.T) {
	index := testStore(t)
	root := t.TempDir()
	fixtureFile(t, root, "a.bin", strings.Repeat("x", 1<<20))
	fixtureFile(t, root, "b.bin", strings.Repeat("x", 1<<20))
	data := snapshot(t, index, "001", []string{root})
	s := &server{store: index, allowedRoots: []string{root}, last: data}
	api(t, s, "POST", "/api/duplicates", `{"id":"001","minBytes":1,"rateMiB":1}`, 202)
	body, _ := json.Marshal(scanOptions{Roots: []string{root}})
	api(t, s, "POST", "/api/scan", string(body), 409)
	api(t, s, "GET", "/api/duplicates", "", 200)
	api(t, s, "DELETE", "/api/duplicates", "", 200)
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("duplicate cancellation did not finish")
	}
	s.mu.RLock()
	job := *s.duplicate
	s.mu.RUnlock()
	if job.State != "cancelled" {
		t.Fatalf("cancel state: %+v", job)
	}
	rec := api(t, s, "GET", "/api/duplicates", "", 200)
	if strings.Contains(rec.Body.String(), `"groups":[`) {
		t.Fatal("cancelled duplicate results published")
	}
	api(t, s, "POST", "/api/scan", string(body), 202)
	s.workers.Wait()
	rec = api(t, s, "GET", "/api/scan", "", 200)
	var scan scanJob
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatal(err)
	}
	if scan.State != "completed" || scan.Result.ID == "" {
		t.Fatalf("indexed async scan failed: %+v", scan)
	}
}

func TestCrossSiteMutationAndDataPath(t *testing.T) {
	index := testStore(t)
	root := t.TempDir()
	s := &server{store: index, allowedRoots: []string{root}}
	body, _ := json.Marshal(scanOptions{Roots: []string{root}})
	request := httptest.NewRequest("POST", "http://disklens.local/api/scan", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://unrelated.example")
	rec := httptest.NewRecorder()
	s.handler(false).ServeHTTP(rec, request)
	if rec.Code != 403 {
		t.Fatal("accepted cross-site scan")
	}
	request = httptest.NewRequest("POST", "/api/scan", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	s.handler(false).ServeHTTP(rec, request)
	if rec.Code != 400 {
		t.Fatal("accepted non-JSON content type")
	}
	parent := t.TempDir()
	actual := filepath.Join(parent, "actual?data")
	link := filepath.Join(parent, "link")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	db, err := openStore(link)
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	canonical, _ := filepath.EvalSymlinks(actual)
	if db.directory != canonical {
		t.Fatalf("data exclusion not canonical: %s", db.directory)
	}
	canonicalParent, _ := filepath.EvalSymlinks(parent)
	if !excluded(canonicalParent, "actual?data/index.sqlite", []string{db.directory}) {
		t.Fatal("self-index exclusion failed")
	}
}

func TestLargeIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("filesystem integration")
	}
	root := t.TempDir()
	index := testStore(t)
	for i := 0; i < 5000; i++ {
		fixtureFile(t, root, fmt.Sprintf("group-%02d/file-%05d.txt", i%50, i), "12345")
	}
	data := snapshot(t, index, "001", []string{root})
	if data.FileCount != 5000 || data.TotalBytes != 25000 {
		t.Fatalf("large scan: %+v", data)
	}
	page := queryPage(t, index, "001", url.Values{"page": {"100"}, "sort": {"path"}, "order": {"asc"}}, false)
	if page.Total != 5000 || len(page.Items) != 50 {
		t.Fatalf("last page incomplete: %+v", page)
	}
}
