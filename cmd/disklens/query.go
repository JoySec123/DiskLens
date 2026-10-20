package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const entryColumns = "path,root,parent,name,kind,bytes,files,modified,extension,category,depth"

type entryPage struct {
	Items      []indexedEntry `json:"items"`
	Total      int            `json:"total"`
	TotalBytes int64          `json:"totalBytes"`
	Page       int            `json:"page"`
	PageSize   int            `json:"pageSize"`
}
type queryOptions struct {
	where, order string
	args         []any
	page, size   int
}

func parseQuery(values url.Values, id string, browse bool) (queryOptions, error) {
	q := queryOptions{where: "scan_id=?", args: []any{id}, order: "bytes DESC,path ASC", page: 1, size: 50}
	add := func(clause string, args ...any) { q.where += " AND " + clause; q.args = append(q.args, args...) }
	if browse {
		if path := values.Get("path"); path != "" {
			add("parent=?", path)
		} else {
			add("depth=0")
		}
	} else {
		add("kind='file'")
	}
	if text := strings.TrimSpace(values.Get("q")); text != "" {
		add("instr(lower(path),lower(?))>0", text)
	}
	if kind := values.Get("category"); kind != "" {
		add("category=?", kind)
	}
	if raw := strings.TrimSpace(values.Get("ext")); raw != "" {
		exts := strings.Split(raw, ",")
		if len(exts) > 20 {
			return q, fmt.Errorf("扩展名最多 20 项")
		}
		clauses := []string{}
		args := []any{}
		for _, ext := range exts {
			ext = strings.TrimSpace(strings.ToLower(ext))
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			clauses = append(clauses, "extension=?")
			args = append(args, ext)
		}
		if len(clauses) > 0 {
			add("("+strings.Join(clauses, " OR ")+")", args...)
		}
	}
	var min, max int64
	for _, field := range []struct {
		key, clause string
		target      *int64
	}{{"min", "bytes>=?", &min}, {"max", "bytes<=?", &max}} {
		if raw := values.Get(field.key); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return q, fmt.Errorf("文件大小必须为非负整数")
			}
			*field.target = value
			add(field.clause, value)
		}
	}
	if values.Has("max") && values.Get("max") != "" && max < min {
		return q, fmt.Errorf("最大大小不能小于最小大小")
	}
	var since, until time.Time
	for _, field := range []struct {
		key, clause string
		target      *time.Time
	}{{"since", "modified>=?", &since}, {"until", "modified<?", &until}} {
		if raw := values.Get(field.key); raw != "" {
			value, err := time.Parse(time.RFC3339, raw)
			if err != nil || value.Year() < 1970 || value.Year() > 2261 {
				return q, fmt.Errorf("修改时间格式错误")
			}
			*field.target = value
			add(field.clause, value.UnixNano())
		}
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return q, fmt.Errorf("结束时间须晚于开始时间")
	}
	column := map[string]string{"size": "bytes", "name": "name COLLATE NOCASE", "modified": "modified", "path": "path COLLATE NOCASE"}[values.Get("sort")]
	if values.Get("sort") != "" && column == "" {
		return q, fmt.Errorf("不支持的排序字段")
	}
	if column == "" {
		column = "bytes"
	}
	order := values.Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		return q, fmt.Errorf("排序方向错误")
	}
	q.order = column + " " + strings.ToUpper(order) + ",path ASC"
	if browse {
		q.order = "kind ASC," + q.order
	}
	for _, field := range []struct {
		key    string
		target *int
		max    int
	}{{"page", &q.page, 10000000}, {"pageSize", &q.size, 200}} {
		if raw := values.Get(field.key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > field.max {
				return q, fmt.Errorf("分页参数超出范围")
			}
			*field.target = n
		}
	}
	return q, nil
}

func readEntry(row interface{ Scan(...any) error }) (indexedEntry, error) {
	var e indexedEntry
	err := row.Scan(&e.Path, &e.Root, &e.Parent, &e.Name, &e.Kind, &e.Bytes, &e.Files, &e.Modified, &e.Extension, &e.Category, &e.Depth)
	return e, err
}

func (s *store) page(ctx context.Context, q queryOptions) (entryPage, error) {
	result := entryPage{Items: []indexedEntry{}, Page: q.page, PageSize: q.size}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM entries WHERE "+q.where, q.args...).Scan(&result.Total, &result.TotalBytes); err != nil {
		return result, err
	}
	args := append(append([]any{}, q.args...), q.size, (q.page-1)*q.size)
	rows, err := s.db.QueryContext(ctx, "SELECT "+entryColumns+" FROM entries WHERE "+q.where+" ORDER BY "+q.order+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := readEntry(rows)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, e)
	}
	return result, rows.Err()
}

func resultRoots(result *scanResult) []string {
	if len(result.Roots) > 0 {
		return result.Roots
	}
	return []string{result.Root}
}
func resultAllowed(result *scanResult, roots []string) bool {
	if result == nil {
		return false
	}
	for _, root := range resultRoots(result) {
		if !pathAllowed(root, roots) {
			return false
		}
	}
	return true
}

func (s *server) authorizedSnapshot(w http.ResponseWriter, r *http.Request, id string) (*scanResult, bool) {
	if s.store == nil {
		writeError(w, 503, "扫描索引尚未初始化")
		return nil, false
	}
	roots, _, err := s.roots(r.Context())
	if err != nil {
		writeError(w, 503, err.Error())
		return nil, false
	}
	result, err := s.store.get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "扫描记录不存在或已过期")
		return nil, false
	}
	if err != nil {
		writeError(w, 500, "无法读取扫描记录")
		return nil, false
	}
	if !resultAllowed(result, roots) {
		writeError(w, 403, "扫描目录的授权已失效，请刷新授权目录")
		return nil, false
	}
	return result, true
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	roots, _, err := s.roots(r.Context())
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	list := []*scanResult{}
	if s.store != nil {
		all, err := s.store.history(r.Context())
		if err != nil {
			writeError(w, 500, "无法读取扫描历史")
			return
		}
		for _, item := range all {
			if resultAllowed(item, roots) {
				list = append(list, item)
			}
		}
	}
	writeJSON(w, 200, list)
}

func (s *server) entries(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	_, ok := s.authorizedSnapshot(w, r, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	q, err := parseQuery(r.URL.Query(), r.URL.Query().Get("id"), r.URL.Path == "/api/browse")
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	page, err := s.store.page(r.Context(), q)
	if err != nil {
		writeError(w, 500, "无法查询文件索引")
		return
	}
	writeJSON(w, 200, page)
}

func csvSafe(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") || strings.ContainsAny(firstRune(trimmed), "=+-@") {
		return "'" + value
	}
	return value
}
func firstRune(value string) string {
	for _, r := range value {
		return string(r)
	}
	return ""
}

func (s *server) export(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	result, ok := s.authorizedSnapshot(w, r, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	q, err := parseQuery(r.URL.Query(), result.ID, false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rows, err := s.store.db.QueryContext(r.Context(), "SELECT "+entryColumns+" FROM entries WHERE "+q.where+" ORDER BY "+q.order, q.args...)
	if err != nil {
		writeError(w, 500, "无法导出文件索引")
		return
	}
	defer rows.Close()
	file, err := os.CreateTemp(s.store.directory, "export-*.csv")
	if err != nil {
		writeError(w, 500, "导出临时空间不可用")
		return
	}
	defer func() { file.Close(); os.Remove(file.Name()) }()
	if _, err = file.WriteString("\xef\xbb\xbf"); err != nil {
		writeError(w, 500, "导出临时空间不足")
		return
	}
	writer := csv.NewWriter(file)
	_ = writer.Write([]string{"文件名称", "绝对路径", "类型", "扩展名", "逻辑大小(字节)", "修改时间(UTC)", "扫描时间(UTC)"})
	checked := time.Time{}
	check := func() error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if time.Since(checked) < time.Second {
			return nil
		}
		roots, _, err := s.roots(r.Context())
		if err != nil {
			return err
		}
		if !resultAllowed(result, roots) {
			return fmt.Errorf("导出目录的授权已失效")
		}
		checked = time.Now()
		return nil
	}
	for rows.Next() {
		if err := check(); err != nil {
			writeError(w, 403, err.Error())
			return
		}
		e, err := readEntry(rows)
		if err != nil {
			writeError(w, 500, "无法读取导出数据")
			return
		}
		if err = writer.Write([]string{csvSafe(e.Name), csvSafe(e.Path), e.Category, csvSafe(e.Extension), strconv.FormatInt(e.Bytes, 10), time.Unix(0, e.Modified).UTC().Format(time.RFC3339), result.ScannedAt.UTC().Format(time.RFC3339)}); err != nil {
			writeError(w, 500, "导出临时空间不足")
			return
		}
	}
	writer.Flush()
	if rows.Err() != nil || writer.Error() != nil {
		writeError(w, 500, "导出失败，请检查应用存储空间")
		return
	}
	rows.Close()
	checked = time.Time{}
	if err := check(); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="DiskLens-`+result.ID+`.csv"`)
	// The known Content-Length makes interrupted downloads detectable by browsers.
	http.ServeContent(w, r, "DiskLens.csv", result.ScannedAt, &checkedReader{ReadSeeker: file, check: check})
}

type checkedReader struct {
	io.ReadSeeker
	check func() error
}

func (r *checkedReader) Read(p []byte) (int, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(p)
}

type changedEntry struct {
	Path   string `json:"path"`
	Change string `json:"change"`
	Before int64  `json:"before"`
	After  int64  `json:"after"`
	Delta  int64  `json:"delta"`
}
type comparison struct {
	DeltaBytes  int64          `json:"deltaBytes"`
	DeltaFiles  int            `json:"deltaFiles"`
	Added       int            `json:"added"`
	Removed     int            `json:"removed"`
	Changed     int            `json:"changed"`
	Items       []changedEntry `json:"items"`
	Directories []changedEntry `json:"directories"`
	Page        int            `json:"page"`
	PageSize    int            `json:"pageSize"`
	Total       int            `json:"total"`
	Partial     bool           `json:"partial"`
}

func sameScope(a, b *scanResult) bool {
	normal := func(values []string) []string { out := append([]string{}, values...); sort.Strings(out); return out }
	return reflect.DeepEqual(normal(resultRoots(a)), normal(resultRoots(b))) && reflect.DeepEqual(normal(a.Excludes), normal(b.Excludes))
}

const changeCTE = `WITH changes AS (
 SELECT n.path,'added' AS change,0 AS before,n.bytes AS after,n.bytes AS delta,n.kind,n.depth FROM entries n LEFT JOIN entries o ON o.scan_id=? AND o.path=n.path AND o.kind=n.kind WHERE n.scan_id=? AND o.path IS NULL
 UNION ALL
 SELECT o.path,'removed',o.bytes,0,-o.bytes,o.kind,o.depth FROM entries o LEFT JOIN entries n ON n.scan_id=? AND n.path=o.path AND n.kind=o.kind WHERE o.scan_id=? AND n.path IS NULL
 UNION ALL
 SELECT n.path,'changed',o.bytes,n.bytes,n.bytes-o.bytes,n.kind,n.depth FROM entries n JOIN entries o ON o.scan_id=? AND o.path=n.path AND o.kind=n.kind WHERE n.scan_id=? AND (n.bytes<>o.bytes OR (n.kind='file' AND n.modified<>o.modified))
) `

func (s *server) compare(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	a, ok := s.authorizedSnapshot(w, r, r.URL.Query().Get("before"))
	if !ok {
		return
	}
	b, ok := s.authorizedSnapshot(w, r, r.URL.Query().Get("after"))
	if !ok {
		return
	}
	if a.ID == b.ID || !sameScope(a, b) {
		writeError(w, 400, "请选择两个扫描范围和排除规则一致的记录")
		return
	}
	q, err := parseQuery(r.URL.Query(), b.ID, false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	result := comparison{DeltaBytes: b.TotalBytes - a.TotalBytes, DeltaFiles: b.FileCount - a.FileCount, Items: []changedEntry{}, Directories: []changedEntry{}, Page: q.page, PageSize: q.size, Partial: a.ReadErrors > 0 || b.ReadErrors > 0 || a.CoverageVersion == 0 && a.Skipped > 0 || b.CoverageVersion == 0 && b.Skipped > 0}
	args := []any{a.ID, b.ID, b.ID, a.ID, a.ID, b.ID}
	err = s.store.db.QueryRowContext(r.Context(), changeCTE+`SELECT COALESCE(SUM(change='added'),0),COALESCE(SUM(change='removed'),0),COALESCE(SUM(change='changed'),0) FROM changes WHERE kind='file'`, args...).Scan(&result.Added, &result.Removed, &result.Changed)
	if err != nil {
		writeError(w, 500, "无法对比扫描记录")
		return
	}
	result.Total = result.Added + result.Removed + result.Changed
	for _, dirs := range []bool{false, true} {
		clause := "kind='file'"
		limit, offset := q.size, (q.page-1)*q.size
		if dirs {
			clause = "kind='directory' AND depth=1"
			limit, offset = 20, 0
		}
		queryArgs := append(append([]any{}, args...), limit, offset)
		rows, err := s.store.db.QueryContext(r.Context(), changeCTE+"SELECT path,change,before,after,delta FROM changes WHERE "+clause+" ORDER BY abs(delta) DESC,path LIMIT ? OFFSET ?", queryArgs...)
		if err != nil {
			writeError(w, 500, "无法对比扫描记录")
			return
		}
		for rows.Next() {
			var item changedEntry
			if err = rows.Scan(&item.Path, &item.Change, &item.Before, &item.After, &item.Delta); err != nil {
				break
			}
			if dirs {
				result.Directories = append(result.Directories, item)
			} else {
				result.Items = append(result.Items, item)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			writeError(w, 500, "无法读取对比结果")
			return
		}
	}
	writeJSON(w, 200, result)
}

func validateOptions(input scanOptions, allowed []string) (scanOptions, error) {
	if len(input.Roots) == 0 || len(input.Roots) > 32 {
		return input, fmt.Errorf("请选择 1 至 32 个授权目录")
	}
	roots := []string{}
	for _, root := range input.Roots {
		if !filepath.IsAbs(root) {
			return input, fmt.Errorf("请选择已授权的绝对路径")
		}
		canonical, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return input, fmt.Errorf("目录不存在或无法访问")
		}
		if !pathAllowed(canonical, allowed) {
			return input, fmt.Errorf("目录未授权，或不支持扫描系统根目录")
		}
		roots = append(roots, canonical)
	}
	sort.Strings(roots)
	dedup := []string{}
	for _, root := range roots {
		covered := false
		for _, parent := range dedup {
			if root == parent || strings.HasPrefix(root, parent+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			dedup = append(dedup, root)
		}
	}
	if len(input.Excludes) > 64 {
		return input, fmt.Errorf("排除规则最多 64 项")
	}
	excludes := []string{}
	seen := map[string]bool{}
	for _, rule := range input.Excludes {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if len(rule) > 4096 || rule == "." || rule == "/" || strings.ContainsRune(rule, 0) {
			return input, fmt.Errorf("排除规则无效")
		}
		if filepath.IsAbs(rule) {
			rule = filepath.Clean(rule)
		} else {
			rule = strings.TrimSuffix(rule, "/")
			if _, err := filepath.Match(rule, ""); err != nil {
				return input, fmt.Errorf("排除通配规则无效")
			}
			for _, part := range strings.Split(rule, "/") {
				if part == ".." {
					return input, fmt.Errorf("排除规则不能包含上级路径")
				}
			}
		}
		if !seen[rule] {
			excludes = append(excludes, rule)
			seen[rule] = true
		}
	}
	sort.Strings(excludes)
	return scanOptions{Roots: dedup, Excludes: excludes}, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return fmt.Errorf("application/json required")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return fmt.Errorf("unexpected JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
