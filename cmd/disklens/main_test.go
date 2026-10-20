package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnalyze(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "movies", "clip.mp4"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := analyze(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalBytes != 8 || result.FileCount != 2 || result.DirCount != 1 {
		t.Fatalf("unexpected totals: %+v", result)
	}
	if len(result.TopDirs) != 1 || result.TopDirs[0].Path != "movies" {
		t.Fatalf("unexpected top dirs: %+v", result.TopDirs)
	}
	if result.Categories[0].Name != "视频" {
		t.Fatalf("expected video category first, got %+v", result.Categories)
	}
	if len(result.TopFiles) != 2 || result.TopFiles[0].Path != "movies/clip.mp4" {
		t.Fatalf("nested files missing from ranking: %+v", result.TopFiles)
	}
}

func TestIsAllowed(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0755); err != nil {
		t.Fatal(err)
	}
	if !pathAllowed(child, []string{root}) {
		t.Fatal("child directory should be allowed")
	}
	if pathAllowed(filepath.Dir(root), []string{root}) {
		t.Fatal("parent directory should not be allowed")
	}
}

func TestAuthorizationAndLinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if pathAllowed(filepath.Join(root, "escape"), []string{root}) {
		t.Fatal("symlink escaped authorization")
	}
	if pathAllowed("/", []string{"/"}) {
		t.Fatal("system root must never be allowed")
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.iso"), make([]byte, 200), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := analyze(root)
	if err != nil {
		t.Fatal(err)
	}
	if data.FileCount != 0 || data.Skipped != 1 {
		t.Fatalf("followed a link: %+v", data)
	}
}

func TestFnOSRootsNeverFallBackToCWD(t *testing.T) {
	t.Setenv("TRIM_APPNAME", "DiskLens")
	t.Setenv("DISKLENS_ROOTS", "/tmp/should-not-override-fnos")
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", "")
	if roots := configuredRoots(); len(roots) != 0 {
		t.Fatalf("unexpected roots: %v", roots)
	}
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", "/vol1/test files:/vol2/photo:/")
	if roots := configuredRoots(); len(roots) != 2 || roots[0] != "/vol1/test files" {
		t.Fatalf("invalid roots: %v", roots)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSharedRootsResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"empty revocation", `{"code":0,"data":{"paths":[]}}`, 200, false},
		{"unauthorized", `{"code":200004}`, 401, true},
		{"malformed", `{}`, 200, true},
		{"denied", `{"code":200003,"data":{}}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing token")
				}
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if request["req"] != "trim.file.getSharedAccessibleFolders" || request["appName"] != "DiskLens" {
					t.Errorf("wrong API request: %v", request)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer mock.Close()
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				r.URL.Host = strings.TrimPrefix(mock.URL, "http://")
				return http.DefaultTransport.RoundTrip(r)
			})}
			paths, err := fetchSharedRoots(context.Background(), client, "test-token", "DiskLens")
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(paths) != 0 {
				t.Fatalf("unexpected paths: %v", paths)
			}
		})
	}
}

func TestCancellationAndTop50(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= 70; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%03d.txt", i)), make([]byte, i), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := analyze(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.TopFiles) != 50 || result.TopFiles[0].Bytes != 70 || result.TopFiles[49].Bytes != 21 {
		t.Fatal("incorrect largest-file ranking")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = analyzeContext(ctx, root, func(progress) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan was not cancelled: %v", err)
	}
}

func TestScanLifecycleAndGateway(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "disk.iso"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	s := &server{allowedRoots: []string{root}}
	handler := s.handler(false)
	for _, input := range []string{`{"root":""}`, `{"root":"/"}`, `{"root":"../"}`} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/api/scan", strings.NewReader(input)))
		if rec.Code < 400 {
			t.Fatalf("accepted invalid root: %s", input)
		}
	}
	body, _ := json.Marshal(map[string]string{"root": root})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/scan", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("start failed: %s", rec.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/scan", nil))
		var job scanJob
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.State == "completed" {
			if job.Result.TotalBytes != 1024 {
				t.Fatal("incorrect result")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck job: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	adminHandler := s.handler(true)
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest("GET", gatewayPrefix+"/api/config", nil)
		if authorized {
			req.Header.Set("X-Trim-Userid", "1000")
			req.Header.Set("X-Trim-Isadmin", "true")
		}
		rec := httptest.NewRecorder()
		adminHandler.ServeHTTP(rec, req)
		if authorized && rec.Code != 200 || !authorized && rec.Code != 403 {
			t.Fatalf("admin boundary: %d", rec.Code)
		}
	}
	s.allowedRoots = nil
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/scan", nil))
	if rec.Code != 403 {
		t.Fatal("revoked result still visible")
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	if !strings.Contains(rec.Body.String(), `"lastScan":null`) {
		t.Fatal("revoked last result still visible")
	}
}
