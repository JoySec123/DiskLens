package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const version = "0.3.3"
const gatewayPrefix = "/app/DiskLens"

//go:embed web/*
var webFS embed.FS

type server struct {
	allowedRoots    []string
	rootProvider    func(context.Context) ([]string, string, error)
	mu              sync.RWMutex
	job             *scanJob
	last            *scanResult
	cancel          context.CancelFunc
	store           *store
	duplicate       *duplicateJob
	duplicateCancel context.CancelFunc
	workers         sync.WaitGroup
	autoMu          sync.Mutex
	stopping        bool
}

type scanJob struct {
	ID       string      `json:"id"`
	Root     string      `json:"root"`
	Roots    []string    `json:"roots"`
	State    string      `json:"state"`
	Progress progress    `json:"progress"`
	Result   *scanResult `json:"result,omitempty"`
	Error    string      `json:"error,omitempty"`
	Source   string      `json:"source"`
}

func main() {
	directory, err := dataDirectory()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		log.Fatal(err)
	}
	lock, err := os.OpenFile(directory+"/.disklens.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		log.Fatal("DiskLens data directory is already in use")
	}
	index, err := openStore(directory)
	if err != nil {
		log.Fatal(err)
	}
	defer index.db.Close()
	s := &server{rootProvider: resolveRoots, store: index}
	history, err := index.history(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	if len(history) > 0 {
		s.last = history[0]
	}
	socket := os.Getenv("DISKLENS_SOCKET")
	if socket == "" && os.Getenv("TRIM_APPNAME") != "" {
		log.Fatal("fnOS requires DISKLENS_SOCKET")
	}
	var listener net.Listener
	if socket != "" {
		// A stale Unix socket may remain after an unclean shutdown.
		if info, statErr := os.Lstat(socket); statErr == nil {
			if info.Mode()&os.ModeSocket == 0 {
				log.Fatal("socket path is not a socket")
			}
			if conn, dialErr := net.DialTimeout("unix", socket, time.Second); dialErr == nil {
				conn.Close()
				log.Fatal("DiskLens is already running")
			}
			if err := os.Remove(socket); err != nil {
				log.Fatal(err)
			}
		}
		listener, err = net.Listen("unix", socket)
		if err == nil {
			err = os.Chmod(socket, 0660)
		}
	} else {
		listener, err = net.Listen("tcp", "127.0.0.1:"+envOrString("DISKLENS_PORT", "8787"))
	}
	if err != nil {
		log.Fatal(err)
	}
	httpServer := &http.Server{Handler: s.handler(socket != ""), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	s.workers.Add(1)
	go s.scheduler(ctx)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		s.mu.Lock()
		s.stopping = true
		if s.cancel != nil {
			s.cancel()
		}
		if s.duplicateCancel != nil {
			s.duplicateCancel()
		}
		s.mu.Unlock()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	log.Printf("DiskLens %s listening on %s", version, listener.Addr())
	if err = httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
	s.workers.Wait()
}

func (s *server) handler(requireAdmin bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version})
	})
	mux.HandleFunc("/api/config", s.config)
	mux.HandleFunc("/api/scan", s.scan)
	mux.HandleFunc("/api/files", s.entries)
	mux.HandleFunc("/api/browse", s.entries)
	mux.HandleFunc("/api/history", s.history)
	mux.HandleFunc("/api/compare", s.compare)
	mux.HandleFunc("/api/export", s.export)
	mux.HandleFunc("/api/capacity", s.capacities)
	mux.HandleFunc("/api/duplicates", s.duplicates)
	mux.HandleFunc("/api/inspection", s.inspectionAPI)
	mux.HandleFunc("/api/ledger", s.ledgerAPI)
	mux.HandleFunc("/api/alerts", s.alertsAPI)
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if requireAdmin && (r.Header.Get("X-Trim-Userid") == "" || r.Header.Get("X-Trim-Isadmin") != "true") {
			writeError(w, 403, "请使用飞牛管理员账号打开磁盘瞭望")
			return
		}
		if !mutationAllowed(r, requireAdmin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "不允许跨站操作，请重新打开应用后重试", "code": "cross_site_request"})
			return
		}
		if r.URL.Path == gatewayPrefix {
			http.Redirect(w, r, gatewayPrefix+"/", http.StatusTemporaryRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, gatewayPrefix+"/") {
			http.StripPrefix(gatewayPrefix, mux).ServeHTTP(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
	})
}

func (s *server) roots(ctx context.Context) ([]string, string, error) {
	if s.rootProvider != nil {
		return s.rootProvider(ctx)
	}
	return s.allowedRoots, "", nil
}

func (s *server) config(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	roots, notice, err := s.roots(r.Context())
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	if roots == nil {
		roots = []string{}
	}
	displayed := make([]string, 0, len(roots))
	for _, root := range roots {
		canonical, err := filepath.EvalSymlinks(root)
		if err == nil {
			displayed = append(displayed, canonical)
		} else {
			displayed = append(displayed, root)
		}
	}
	displayed = normalizeRoots(displayed)
	s.mu.RLock()
	last := s.last
	s.mu.RUnlock()
	if last != nil && !resultAllowed(last, roots) {
		last = nil
	}
	defaults := scanOptions{}
	if last != nil {
		defaults.Roots = resultRoots(last)
		for _, rule := range last.Excludes {
			if s.store == nil || rule != s.store.directory {
				defaults.Excludes = append(defaults.Excludes, rule)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"allowedRoots": displayed, "notice": notice, "version": version, "lastScan": last, "scanDefaults": defaults})
}

func (s *server) scan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	roots, _, err := s.roots(r.Context())
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		if s.cancel != nil && s.job != nil && s.job.State == "running" {
			s.job.State = "cancelling"
			s.cancel()
		}
		job := s.jobCopyLocked()
		s.mu.Unlock()
		if job != nil && !jobAllowed(job, roots) {
			writeError(w, 403, "扫描目录的授权已失效")
			return
		}
		writeJSON(w, 200, job)
		return
	}
	if r.Method == http.MethodGet {
		s.mu.Lock()
		job := s.jobCopyLocked()
		if job != nil && !jobAllowed(job, roots) {
			if s.cancel != nil {
				s.cancel()
			}
			s.mu.Unlock()
			writeError(w, 403, "扫描目录的授权已失效，请刷新授权目录")
			return
		}
		s.mu.Unlock()
		writeJSON(w, 200, job)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	var input struct {
		Root string `json:"root"`
		scanOptions
	}
	if err := decodeBody(w, r, &input); err != nil {
		writeError(w, 400, "扫描请求格式错误")
		return
	}
	if len(input.Roots) == 0 && input.Root != "" {
		input.Roots = []string{input.Root}
	}
	options, err := validateOptions(input.scanOptions, roots)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	settings := defaultInspection()
	if s.store != nil {
		settings, err = s.store.inspection(r.Context())
		if err != nil {
			writeError(w, 500, "扫描设置读取失败")
			return
		}
	}
	job, err := s.launchScan(options, settings, fmt.Sprint(time.Now().UnixNano()), "manual")
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *server) jobCopyLocked() *scanJob {
	if s.job == nil {
		return nil
	}
	job := *s.job
	return &job
}

func jobAllowed(job *scanJob, roots []string) bool {
	return resultAllowed(&scanResult{Root: job.Root, Roots: job.Roots}, roots)
}
func (s *server) taskBusyLocked() bool {
	return s.job != nil && (s.job.State == "running" || s.job.State == "cancelling") || s.duplicate != nil && (s.duplicate.State == "running" || s.duplicate.State == "cancelling")
}

func (s *server) runScan(ctx context.Context, cancel context.CancelFunc, options scanOptions, id string) {
	defer s.workers.Done()
	defer cancel()
	checked := time.Now()
	report := func(p progress) {
		if time.Since(checked) > 2*time.Second {
			roots, _, err := s.roots(ctx)
			if err != nil || !resultAllowed(&scanResult{Roots: options.Roots}, roots) {
				cancel()
			}
			checked = time.Now()
		}
		s.mu.Lock()
		s.job.Progress = p
		s.mu.Unlock()
	}
	var result *scanResult
	var err error
	if s.store != nil {
		result, err = s.store.analyze(ctx, id, options, report)
	} else {
		result, err = analyzeOptions(ctx, options, report, nil)
	}
	if err != nil {
		log.Printf("scan %s: %v", id, err)
	}
	if s.store != nil {
		if _, saveErr := s.store.db.Exec("UPDATE inspection_runs SET state=? WHERE scan_id=?", inspectionState(err), id); saveErr != nil {
			log.Printf("record scheduled result: %v", saveErr)
		}
	}
	s.mu.Lock()
	switch {
	case errors.Is(err, context.Canceled):
		s.job.State = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		s.job.State, s.job.Error = "error", fmt.Sprintf("扫描超过 %d 分钟，可调整巡检设置中的最长时间", options.timeoutMinutes)
	case err != nil:
		s.job.State, s.job.Error = "error", "目录读取失败，请检查目录权限及存储状态"
	default:
		s.job.State, s.job.Result, s.last = "completed", result, result
		s.job.Progress = result.progress
	}
	s.cancel = nil
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func envOrString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
