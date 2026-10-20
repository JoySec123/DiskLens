package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMutationOriginBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin, site, marker string
		gateway, allow                   bool
	}{
		{"fnos HTTP port stripped", "nas.local", "http://nas.local:5666", "", "1", true, true},
		{"fnos HTTPS port stripped", "nas.local", "https://nas.local:5667", "same-origin", "1", true, true},
		{"fnos IPv6 port stripped", "[::1]", "http://[::1]:5666", "", "1", true, true},
		{"fnos custom domain", "NAS.example", "https://nas.example:8443", "", "1", true, true},
		{"fnos full Host", "nas.local:5666", "http://nas.local:5666", "", "1", true, true},
		{"fnos conflicting explicit port", "nas.local:5666", "http://nas.local:9000", "", "1", true, false},
		{"fnos missing marker", "nas.local", "http://nas.local:5666", "", "", true, false},
		{"fnos wrong marker", "nas.local", "http://nas.local:5666", "", "true", true, false},
		{"fnos no Origin with marker", "nas.local", "", "", "1", true, true},
		{"fnos no Origin without marker", "nas.local", "", "", "", true, false},
		{"fnos unrelated site", "nas.local", "https://other.example", "", "1", true, false},
		{"fnos sibling subdomain", "nas.local", "http://other.nas.local:5666", "", "1", true, false},
		{"fnos spoofed fetch metadata", "nas.local", "https://other.example", "same-origin", "1", true, false},
		{"cross-site metadata", "nas.local", "http://nas.local:5666", "cross-site", "1", true, false},
		{"same-site is not same-origin", "nas.local", "http://nas.local:9000", "same-site", "1", true, false},
		{"unknown metadata", "nas.local", "http://nas.local:5666", "invalid", "1", true, false},
		{"direct exact Host", "localhost:8787", "http://localhost:8787", "", "", false, true},
		{"direct wrong port", "localhost:8787", "http://localhost:9000", "", "1", false, false},
		{"direct missing port", "localhost", "http://localhost:9000", "", "1", false, false},
		{"direct no Origin", "localhost:8787", "", "", "", false, true},
		{"null Origin", "nas.local", "null", "", "1", true, false},
		{"relative Origin", "nas.local", "//nas.local", "", "1", true, false},
		{"Origin userinfo", "nas.local", "http://other.example@nas.local", "", "1", true, false},
		{"Origin path", "nas.local", "http://nas.local/path", "", "1", true, false},
		{"Origin query", "nas.local", "http://nas.local?query", "", "1", true, false},
		{"Origin fragment", "nas.local", "http://nas.local#fragment", "", "1", true, false},
		{"Origin list", "nas.local", "http://nas.local https://other.example", "", "1", true, false},
		{"invalid Origin port", "nas.local", "http://nas.local:bad", "", "1", true, false},
		{"non-http Origin", "nas.local", "ftp://nas.local", "", "1", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"POST", "DELETE", "PUT", "PATCH"} {
				req := httptest.NewRequest(method, "http://"+tc.host+"/api/health", nil)
				req.Header.Set("Origin", tc.origin)
				req.Header.Set("Sec-Fetch-Site", tc.site)
				req.Header.Set("X-DiskLens-Request", tc.marker)
				// Untrusted forwarded headers must never override the origin check.
				req.Header.Set("X-Forwarded-Host", "other.example")
				req.Header.Set("X-Forwarded-Proto", "https")
				if got := mutationAllowed(req, tc.gateway); got != tc.allow {
					t.Errorf("%s allowed=%v, want %v", method, got, tc.allow)
				}
			}
		})
	}
}

func TestGatewayMutationScan(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "sample.txt", "sample")
	s := &server{allowedRoots: []string{root}, store: testStore(t)}
	handler := s.handler(true)
	body, _ := json.Marshal(map[string]string{"root": root})
	request := func(method, path, marker string, admin bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://nas.local"+gatewayPrefix+path, strings.NewReader(string(body)))
		r.Header.Set("Origin", "http://nas.local:5666")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-DiskLens-Request", marker)
		if admin {
			r.Header.Set("X-Trim-Userid", "1000")
			r.Header.Set("X-Trim-Isadmin", "true")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}
	if rec := request("POST", "/api/scan", "1", false); rec.Code != 403 {
		t.Fatalf("marker bypassed administrator check: %d", rec.Code)
	}
	for _, method := range []string{"POST", "DELETE"} {
		if rec := request(method, "/api/scan", "", true); rec.Code != 403 || !strings.Contains(rec.Body.String(), "cross_site_request") {
			t.Fatalf("missing marker accepted for %s: %s", method, rec.Body.String())
		}
	}
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		rec := request(method, "/api/health", "", true)
		if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Headers") != "" {
			t.Fatalf("safe request failed or CORS enabled: %d %v", rec.Code, rec.Header())
		}
	}
	if rec := request("POST", "/api/scan", "1", true); rec.Code != http.StatusAccepted {
		t.Fatalf("gateway scan rejected: %s", rec.Body.String())
	}
	s.workers.Wait()
	s.mu.RLock()
	job := s.jobCopyLocked()
	s.mu.RUnlock()
	if job == nil || job.State != "completed" || job.Result.FileCount != 1 {
		t.Fatalf("gateway scan did not finish: %+v", job)
	}
	if rec := request("DELETE", "/api/scan", "1", true); rec.Code != 200 {
		t.Fatalf("gateway stop rejected: %s", rec.Body.String())
	}
}
