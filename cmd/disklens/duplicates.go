package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type duplicateJob struct {
	ID                    string `json:"id"`
	ScanID                string `json:"scanId"`
	State                 string `json:"state"`
	Phase                 string `json:"phase"`
	Candidates            int    `json:"candidates"`
	Processed             int    `json:"processed"`
	Skipped               int    `json:"skipped"`
	ReadBytes             int64  `json:"readBytes"`
	Groups                int    `json:"groups"`
	LogicalDuplicateBytes int64  `json:"logicalDuplicateBytes"`
	CurrentPath           string `json:"currentPath"`
	Error                 string `json:"error,omitempty"`
	RateMiB               int64  `json:"rateMiB"`
}
type duplicateGroup struct {
	ID    int      `json:"id"`
	Count int      `json:"count"`
	Bytes int64    `json:"bytes"`
	Paths []string `json:"paths"`
}

func (s *server) duplicates(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" && r.Method != "DELETE" {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	if r.Method == "POST" {
		var input struct {
			ID       string `json:"id"`
			MinBytes int64  `json:"minBytes"`
			RateMiB  int64  `json:"rateMiB"`
		}
		if err := decodeBody(w, r, &input); err != nil {
			writeError(w, 400, "重复检测请求格式错误")
			return
		}
		if input.MinBytes < 0 || input.RateMiB < 0 || input.RateMiB > 256 {
			writeError(w, 400, "大小或限速参数无效")
			return
		}
		if input.MinBytes == 0 {
			input.MinBytes = 1
		}
		if input.RateMiB == 0 {
			input.RateMiB = 32
		}
		result, ok := s.authorizedSnapshot(w, r, input.ID)
		if !ok {
			return
		}
		s.mu.Lock()
		if s.taskBusyLocked() {
			s.mu.Unlock()
			writeError(w, 409, "已有扫描或重复检测任务正在运行")
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		job := duplicateJob{ID: fmt.Sprint(time.Now().UnixNano()), ScanID: input.ID, State: "running", Phase: "hashing", RateMiB: input.RateMiB}
		stored := job
		s.duplicate = &stored
		s.duplicateCancel = cancel
		s.workers.Add(1)
		s.mu.Unlock()
		go s.runDuplicates(ctx, cancel, result, job, input.MinBytes)
		writeJSON(w, 202, job)
		return
	}
	s.mu.RLock()
	var job *duplicateJob
	if s.duplicate != nil {
		copy := *s.duplicate
		job = &copy
	}
	s.mu.RUnlock()
	if job == nil {
		writeJSON(w, 200, nil)
		return
	}
	_, ok := s.authorizedSnapshot(w, r, job.ScanID)
	if !ok {
		s.mu.Lock()
		if s.duplicateCancel != nil {
			s.duplicateCancel()
		}
		s.mu.Unlock()
		return
	}
	if r.Method == "DELETE" {
		s.mu.Lock()
		if s.duplicateCancel != nil && s.duplicate.State == "running" {
			s.duplicate.State = "cancelling"
			s.duplicateCancel()
		}
		copy := *s.duplicate
		s.mu.Unlock()
		writeJSON(w, 200, copy)
		return
	}
	if job.State != "completed" {
		writeJSON(w, 200, map[string]any{"job": job})
		return
	}
	q, err := parseQuery(r.URL.Query(), job.ScanID, false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if raw := r.URL.Query().Get("group"); raw != "" {
		group, err := strconv.Atoi(raw)
		if err != nil || group < 1 {
			writeError(w, 400, "重复分组无效")
			return
		}
		var total int
		if err = s.store.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM duplicate_members WHERE run_id=? AND group_id=?", job.ID, group).Scan(&total); err != nil {
			writeError(w, 500, "无法读取重复结果")
			return
		}
		rows, err := s.store.db.QueryContext(r.Context(), "SELECT path FROM duplicate_members WHERE run_id=? AND group_id=? ORDER BY path LIMIT ? OFFSET ?", job.ID, group, q.size, (q.page-1)*q.size)
		if err != nil {
			writeError(w, 500, "无法读取重复结果")
			return
		}
		defer rows.Close()
		paths := []string{}
		for rows.Next() {
			var path string
			if err = rows.Scan(&path); err != nil {
				break
			}
			paths = append(paths, path)
		}
		if err != nil || rows.Err() != nil {
			writeError(w, 500, "无法读取重复结果")
			return
		}
		writeJSON(w, 200, map[string]any{"job": job, "paths": paths, "total": total, "page": q.page, "pageSize": q.size})
		return
	}
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT group_id,COUNT(*),MAX(bytes) FROM duplicate_members WHERE run_id=? GROUP BY group_id HAVING COUNT(*)>1 ORDER BY (COUNT(*)-1)*MAX(bytes) DESC,group_id LIMIT ? OFFSET ?`, job.ID, q.size, (q.page-1)*q.size)
	if err != nil {
		writeError(w, 500, "无法读取重复结果")
		return
	}
	groups := []duplicateGroup{}
	for rows.Next() {
		var group duplicateGroup
		if err = rows.Scan(&group.ID, &group.Count, &group.Bytes); err != nil {
			break
		}
		groups = append(groups, group)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeError(w, 500, "无法读取重复结果")
		return
	}
	for i := range groups {
		paths, err := s.store.db.QueryContext(r.Context(), "SELECT path FROM duplicate_members WHERE run_id=? AND group_id=? ORDER BY path LIMIT 3", job.ID, groups[i].ID)
		if err != nil {
			writeError(w, 500, "无法读取重复结果")
			return
		}
		groups[i].Paths = []string{}
		for paths.Next() {
			var path string
			if err = paths.Scan(&path); err != nil {
				break
			}
			groups[i].Paths = append(groups[i].Paths, path)
		}
		if err == nil {
			err = paths.Err()
		}
		paths.Close()
		if err != nil {
			writeError(w, 500, "无法读取重复结果")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"job": job, "groups": groups, "page": q.page, "pageSize": q.size, "total": job.Groups})
}

type readBudget struct {
	ctx    context.Context
	rate   int64
	read   int64
	next   time.Time
	report func(int64) error
}

func (b *readBudget) consume(n int) error {
	b.read += int64(n)
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if err := b.report(b.read); err != nil {
		return err
	}
	now := time.Now()
	if b.next.Before(now) {
		b.next = now
	}
	b.next = b.next.Add(time.Duration(float64(n) / float64(b.rate) * float64(time.Second)))
	wait := time.Until(b.next)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return b.ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// Open within an OS-enforced root; reject links, special files and changed metadata.
func openCandidate(e indexedEntry) (*os.File, os.FileInfo, error) {
	root, err := os.OpenRoot(e.Root)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(e.Root, e.Path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, nil, fmt.Errorf("outside root")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("symbolic link")
		}
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !matchesEntry(info, e) {
		f.Close()
		return nil, nil, fmt.Errorf("file changed")
	}
	return f, info, nil
}
func matchesEntry(info os.FileInfo, e indexedEntry) bool {
	return info != nil && info.Mode().IsRegular() && info.Size() == e.Bytes && info.ModTime().UnixNano() == e.Modified
}
func stillCurrent(f *os.File, before os.FileInfo, e indexedEntry) bool {
	after, err := f.Stat()
	if err != nil || !matchesEntry(after, e) || !os.SameFile(before, after) {
		return false
	}
	check, current, err := openCandidate(e)
	if err != nil {
		return false
	}
	check.Close()
	return os.SameFile(before, current)
}

func digestFile(f *os.File, e indexedEntry, info os.FileInfo, budget *readBudget) (string, error) {
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	var read int64
	for {
		n, err := f.Read(buffer)
		if n > 0 {
			read += int64(n)
			if read > e.Bytes {
				return "", fmt.Errorf("file grew during reading")
			}
			hash.Write(buffer[:n])
			if err := budget.consume(n); err != nil {
				return "", err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if !stillCurrent(f, info, e) {
		return "", fmt.Errorf("file changed during reading")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func equalFiles(a, b indexedEntry, budget *readBudget) (bool, error) {
	left, li, err := openCandidate(a)
	if err != nil {
		return false, err
	}
	defer left.Close()
	right, ri, err := openCandidate(b)
	if err != nil {
		return false, err
	}
	defer right.Close()
	if os.SameFile(li, ri) {
		return false, fmt.Errorf("hard link alias")
	}
	lbuf, rbuf := make([]byte, 256<<10), make([]byte, 256<<10)
	for {
		ln, le := io.ReadFull(left, lbuf)
		rn, re := io.ReadFull(right, rbuf)
		if err := budget.consume(ln + rn); err != nil {
			return false, err
		}
		if le != nil && !errors.Is(le, io.EOF) && !errors.Is(le, io.ErrUnexpectedEOF) {
			return false, le
		}
		if re != nil && !errors.Is(re, io.EOF) && !errors.Is(re, io.ErrUnexpectedEOF) {
			return false, re
		}
		if ln != rn || !bytes.Equal(lbuf[:ln], rbuf[:rn]) {
			return false, nil
		}
		if le != nil || re != nil {
			break
		}
	}
	if !stillCurrent(left, li, a) || !stillCurrent(right, ri, b) {
		return false, fmt.Errorf("file changed during comparison")
	}
	return true, nil
}

func (s *server) runDuplicates(ctx context.Context, cancel context.CancelFunc, result *scanResult, job duplicateJob, minBytes int64) {
	defer s.workers.Done()
	defer cancel()
	lastReport, checked := time.Time{}, time.Time{}
	publish := func(force bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(checked) > 2*time.Second {
			roots, _, err := s.roots(ctx)
			if err != nil || !resultAllowed(result, roots) {
				return fmt.Errorf("目录授权已失效或暂时无法确认")
			}
			checked = time.Now()
		}
		if force || time.Since(lastReport) > 200*time.Millisecond {
			s.mu.Lock()
			copy := job
			if s.duplicate.State == "cancelling" {
				copy.State = "cancelling"
			}
			s.duplicate = &copy
			s.mu.Unlock()
			lastReport = time.Now()
		}
		return nil
	}
	budget := &readBudget{ctx: ctx, rate: job.RateMiB * 1024 * 1024, report: func(n int64) error { job.ReadBytes = n; return publish(false) }}
	err := s.detectDuplicates(ctx, result, &job, minBytes, budget, publish)
	s.mu.Lock()
	switch {
	case errors.Is(err, context.Canceled) || s.duplicate.State == "cancelling":
		job.State = "cancelled"
	case err != nil:
		job.State = "error"
		job.Error = "重复检测失败，请检查授权、存储状态并重试"
		log.Printf("duplicate scan: %v", err)
	default:
		job.State = "completed"
	}
	job.CurrentPath = ""
	s.duplicate = &job
	s.duplicateCancel = nil
	s.mu.Unlock()
}

func (s *server) detectDuplicates(ctx context.Context, result *scanResult, job *duplicateJob, minBytes int64, budget *readBudget, publish func(bool) error) error {
	if err := publish(true); err != nil {
		return err
	}
	if _, err := s.store.db.ExecContext(ctx, "DELETE FROM duplicate_hashes; DELETE FROM duplicate_members"); err != nil {
		return err
	}
	where := `scan_id=? AND kind='file' AND bytes>=? AND bytes IN (SELECT bytes FROM entries WHERE scan_id=? AND kind='file' AND bytes>=? GROUP BY bytes HAVING COUNT(*)>1)`
	args := []any{result.ID, minBytes, result.ID, minBytes}
	if err := s.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM entries WHERE "+where, args...).Scan(&job.Candidates); err != nil {
		return err
	}
	rows, err := s.store.db.QueryContext(ctx, "SELECT "+entryColumns+" FROM entries WHERE "+where+" ORDER BY bytes,path", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		e, readErr := readEntry(rows)
		if readErr != nil {
			rows.Close()
			return readErr
		}
		job.CurrentPath = e.Path
		if err = publish(false); err != nil {
			rows.Close()
			return err
		}
		f, info, openErr := openCandidate(e)
		if openErr != nil {
			job.Skipped++
			job.Processed++
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			f.Close()
			job.Skipped++
			job.Processed++
			continue
		}
		key := fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
		var exists int
		err = s.store.db.QueryRowContext(ctx, "SELECT 1 FROM duplicate_hashes WHERE run_id=? AND device_key=?", job.ID, key).Scan(&exists)
		if err == nil {
			f.Close()
			job.Skipped++
			job.Processed++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			f.Close()
			rows.Close()
			return err
		}
		digest, hashErr := digestFile(f, e, info, budget)
		f.Close()
		if hashErr != nil {
			if err = publish(false); err != nil {
				rows.Close()
				return err
			}
			job.Skipped++
			job.Processed++
			continue
		}
		_, err = s.store.db.ExecContext(ctx, "INSERT INTO duplicate_hashes(run_id,path,root,bytes,modified,digest,device_key) VALUES(?,?,?,?,?,?,?)", job.ID, e.Path, e.Root, e.Bytes, e.Modified, digest, key)
		if err != nil {
			rows.Close()
			return err
		}
		job.Processed++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	job.Phase = "verifying"
	if err = publish(true); err != nil {
		return err
	}
	rows, err = s.store.db.QueryContext(ctx, `SELECT path,root,bytes,modified,digest FROM duplicate_hashes WHERE run_id=? AND digest IN (SELECT digest FROM duplicate_hashes WHERE run_id=? GROUP BY digest,bytes HAVING COUNT(*)>1) ORDER BY digest,bytes,path`, job.ID, job.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var representative indexedEntry
	previous := ""
	group := 0
	members := 0
	for rows.Next() {
		var e indexedEntry
		var digest string
		if err = rows.Scan(&e.Path, &e.Root, &e.Bytes, &e.Modified, &digest); err != nil {
			return err
		}
		job.CurrentPath = e.Path
		if err = publish(false); err != nil {
			return err
		}
		key := digest + ":" + strconv.FormatInt(e.Bytes, 10)
		if key != previous {
			representative = e
			previous = key
			members = 0
			group++
			continue
		}
		equal, compareErr := equalFiles(representative, e, budget)
		if compareErr != nil || !equal {
			if err = publish(false); err != nil {
				return err
			}
			job.Skipped++
			continue
		}
		if members == 0 {
			if _, err = s.store.db.ExecContext(ctx, "INSERT INTO duplicate_members VALUES(?,?,?,?)", job.ID, group, representative.Path, representative.Bytes); err != nil {
				return err
			}
			members++
			job.Groups++
		}
		if _, err = s.store.db.ExecContext(ctx, "INSERT INTO duplicate_members VALUES(?,?,?,?)", job.ID, group, e.Path, e.Bytes); err != nil {
			return err
		}
		members++
		job.LogicalDuplicateBytes += e.Bytes
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return publish(true)
}
