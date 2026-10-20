package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type scanSummary struct {
	scanResult
	Scope            string      `json:"scope"`
	Volumes          []*capacity `json:"volumes"`
	Unavailable      []string    `json:"unavailable"`
	CapacityAt       time.Time   `json:"capacityAt"`
	FreeAlertEnabled bool        `json:"freeAlertEnabled"`
	FreeAlertPercent int         `json:"freeAlertPercent"`
}

const ledgerTimeLayout = "2006-01-02T15:04:05.000000000Z"

func scopeKey(result *scanResult) string {
	roots := append([]string{}, resultRoots(result)...)
	excludes := append([]string{}, result.Excludes...)
	sort.Strings(roots)
	sort.Strings(excludes)
	raw, _ := json.Marshal([][]string{roots, excludes})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func recordSummary(ctx context.Context, tx *sql.Tx, result *scanResult, volumes []*capacity, unavailable []string, settings *inspectionSettings) error {
	summary := scanSummary{scanResult: *result, Scope: scopeKey(result), Volumes: volumes, Unavailable: unavailable, CapacityAt: time.Now()}
	summary.TopDirs, summary.TopFiles = nil, nil
	if settings != nil {
		summary.FreeAlertEnabled, summary.FreeAlertPercent = settings.FreeEnabled, settings.FreePercent
	}
	var previous scanSummary
	var rawPrevious string
	err := tx.QueryRowContext(ctx, "SELECT result FROM summaries WHERE scope=? AND scanned_at>=? ORDER BY scanned_at DESC,id DESC LIMIT 1", summary.Scope, time.Now().Add(-365*24*time.Hour).UTC().Format(ledgerTimeLayout)).Scan(&rawPrevious)
	if err == nil {
		if err = json.Unmarshal([]byte(rawPrevious), &previous); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO summaries(id,scope,scanned_at,result) VALUES(?,?,?,?)", result.ID, summary.Scope, result.ScannedAt.UTC().Format(ledgerTimeLayout), string(raw)); err != nil {
		return err
	}
	// Include every first-level directory, and a separate bucket for loose files.
	if _, err := tx.ExecContext(ctx, `INSERT INTO rollups(summary_id,path,kind,bytes,files)
 SELECT scan_id,path,'directory',bytes,files FROM entries WHERE scan_id=? AND depth=1 AND kind='directory'`, result.ID); err != nil {
		return err
	}
	for _, root := range resultRoots(result) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rollups(summary_id,path,kind,bytes,files)
 SELECT ?,?,'root_files',COALESCE(SUM(bytes),0),COUNT(*) FROM entries WHERE scan_id=? AND parent=? AND kind='file'`, result.ID, root, result.ID, root); err != nil {
			return err
		}
	}
	if settings != nil {
		insertAlert := func(kind, message string) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO alerts(summary_id,kind,message) VALUES(?,?,?)", result.ID, kind, message)
			return err
		}
		delta := result.TotalBytes - previous.TotalBytes
		if settings.GrowthEnabled && previous.ID != "" && previous.CoverageVersion > 0 && result.CoverageVersion > 0 && previous.ReadErrors == 0 && result.ReadErrors == 0 &&
			delta >= settings.GrowthBytes && (previous.TotalBytes == 0 || float64(delta)/float64(previous.TotalBytes)*100 >= float64(settings.GrowthPercent)) {
			if err := insertAlert("growth", fmt.Sprintf("同范围文件逻辑大小增加 %d 字节；基准扫描 %s", delta, previous.ScannedAt.Format(time.RFC3339))); err != nil {
				return err
			}
		}
		if settings.FreeEnabled {
			for _, volume := range volumes {
				if volume.Total == 0 || float64(volume.Available)/float64(volume.Total)*100 > float64(settings.FreePercent) {
					continue
				}
				alreadyLow := false
				for _, old := range previous.Volumes {
					if previous.FreeAlertEnabled && previous.FreeAlertPercent == settings.FreePercent && old.ID == volume.ID && old.Total == volume.Total && old.Total > 0 && float64(old.Available)/float64(old.Total)*100 <= float64(settings.FreePercent) {
						alreadyLow = true
					}
				}
				if !alreadyLow {
					if err := insertAlert("capacity:"+volume.ID, fmt.Sprintf("%s 所在文件系统可用空间 %.1f%%（阈值 %d%%）", scopeDescription(volume.Roots), float64(volume.Available)/float64(volume.Total)*100, settings.FreePercent)); err != nil {
						return err
					}
				}
			}
		}
		if result.ReadErrors > 0 {
			if err := insertAlert("coverage", fmt.Sprintf("本次扫描有 %d 项读取失败，空间账单覆盖不完整", result.ReadErrors)); err != nil {
				return err
			}
		}
	}
	return pruneLedgerTx(ctx, tx, time.Now())
}

func pruneLedgerTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	cutoff := now.Add(-365 * 24 * time.Hour).UTC().Format(ledgerTimeLayout)
	_, err := tx.ExecContext(ctx, `DELETE FROM summaries WHERE scanned_at < ? OR id NOT IN (SELECT id FROM summaries ORDER BY scanned_at DESC,id DESC LIMIT 2000)`, cutoff)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM inspection_runs WHERE attempted_at < ?", cutoff)
	return err
}

func (s *store) pruneLedger(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pruneLedgerTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) summaries(ctx context.Context) ([]scanSummary, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT result FROM summaries WHERE scanned_at>=? ORDER BY scanned_at DESC,id DESC LIMIT 2000", time.Now().Add(-365*24*time.Hour).UTC().Format(ledgerTimeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []scanSummary{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var summary scanSummary
		if err := json.Unmarshal([]byte(raw), &summary); err != nil {
			return nil, err
		}
		list = append(list, summary)
	}
	return list, rows.Err()
}

func (s *server) visibleSummaries(ctx context.Context) ([]scanSummary, error) {
	if s.store == nil {
		return nil, fmt.Errorf("索引不可用")
	}
	roots, _, err := s.roots(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.store.summaries(ctx)
	if err != nil {
		return nil, err
	}
	visible := []scanSummary{}
	checked := map[string]bool{}
	for _, item := range list {
		allowed, exists := checked[item.Scope]
		if !exists {
			allowed = resultAllowed(&item.scanResult, roots)
			checked[item.Scope] = allowed
		}
		if allowed {
			visible = append(visible, item)
		}
	}
	return visible, nil
}

type rollupChange struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Before int64  `json:"before"`
	After  int64  `json:"after"`
	Delta  int64  `json:"delta"`
}

func (s *store) rollupChanges(ctx context.Context, before, after string) ([]rollupChange, error) {
	rows, err := s.db.QueryContext(ctx, `WITH paths AS (
 SELECT path,kind FROM rollups WHERE summary_id=? UNION SELECT path,kind FROM rollups WHERE summary_id=?
 ) SELECT p.path,p.kind,COALESCE(a.bytes,0),COALESCE(b.bytes,0),COALESCE(b.bytes,0)-COALESCE(a.bytes,0)
 FROM paths p LEFT JOIN rollups a ON a.summary_id=? AND a.path=p.path AND a.kind=p.kind
 LEFT JOIN rollups b ON b.summary_id=? AND b.path=p.path AND b.kind=p.kind
 WHERE COALESCE(a.bytes,0)<>COALESCE(b.bytes,0) ORDER BY ABS(COALESCE(b.bytes,0)-COALESCE(a.bytes,0)) DESC,p.path,p.kind LIMIT 101`, before, after, before, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []rollupChange{}
	for rows.Next() {
		var item rollupChange
		if err := rows.Scan(&item.Path, &item.Kind, &item.Before, &item.After, &item.Delta); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *server) ledgerAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	list, err := s.visibleSummaries(r.Context())
	if err != nil {
		writeError(w, 503, "账单或授权暂时不可用")
		return
	}
	afterID, beforeID := r.URL.Query().Get("after"), r.URL.Query().Get("before")
	if afterID == "" && beforeID == "" {
		writeJSON(w, 200, list)
		return
	}
	var before, after *scanSummary
	for i := range list {
		if list[i].ID == beforeID {
			before = &list[i]
		}
		if list[i].ID == afterID {
			after = &list[i]
		}
	}
	if before == nil || after == nil {
		writeError(w, 404, "账单不存在或已无权访问")
		return
	}
	if before.Scope != after.Scope || before.ID == after.ID || before.ScannedAt.After(after.ScannedAt) {
		writeError(w, 400, "请选择同范围且时间更早的基准账单")
		return
	}
	changes, err := s.store.rollupChanges(r.Context(), before.ID, after.ID)
	if err != nil {
		writeError(w, 500, "账单对比失败")
		return
	}
	truncated := len(changes) > 100
	if truncated {
		changes = changes[:100]
	}
	writeJSON(w, 200, map[string]any{"before": before, "after": after, "deltaBytes": after.TotalBytes - before.TotalBytes,
		"directories": changes, "truncated": truncated, "partial": before.ReadErrors > 0 || after.ReadErrors > 0 || before.CoverageVersion == 0 || after.CoverageVersion == 0})
}

type ledgerAlert struct {
	ID           int64     `json:"id"`
	SummaryID    string    `json:"summaryId"`
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	Message      string    `json:"message"`
	Acknowledged bool      `json:"acknowledged"`
}

func (s *server) alertsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	list, err := s.visibleSummaries(r.Context())
	if err != nil {
		writeError(w, 503, "提醒或授权暂时不可用")
		return
	}
	allowed := map[string]time.Time{}
	for _, item := range list {
		allowed[item.ID] = item.ScannedAt
	}
	if r.Method == http.MethodPost {
		var input struct {
			ID int64 `json:"id"`
		}
		if decodeBody(w, r, &input) != nil || input.ID <= 0 {
			writeError(w, 400, "提醒编号无效")
			return
		}
		var id string
		if s.store.db.QueryRowContext(r.Context(), "SELECT summary_id FROM alerts WHERE id=?", input.ID).Scan(&id) != nil || allowed[id].IsZero() {
			writeError(w, 404, "提醒不存在或已无权访问")
			return
		}
		if _, err := s.store.db.ExecContext(r.Context(), "UPDATE alerts SET acknowledged=1 WHERE id=?", input.ID); err != nil {
			writeError(w, 500, "提醒确认失败，请在扫描结束后重试")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	page, err := pageIndex(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rows, err := s.store.db.QueryContext(r.Context(), "SELECT id,summary_id,kind,message,acknowledged FROM alerts ORDER BY id DESC")
	if err != nil {
		writeError(w, 500, "提醒读取失败")
		return
	}
	defer rows.Close()
	items := []ledgerAlert{}
	total, unread := 0, 0
	for rows.Next() {
		var item ledgerAlert
		if err := rows.Scan(&item.ID, &item.SummaryID, &item.Kind, &item.Message, &item.Acknowledged); err != nil {
			writeError(w, 500, "提醒读取失败")
			return
		}
		at, ok := allowed[item.SummaryID]
		if !ok {
			continue
		}
		item.At = at
		if !item.Acknowledged {
			unread++
		}
		if total >= (page-1)*25 && total < page*25 {
			items = append(items, item)
		}
		total++
	}
	if rows.Err() != nil {
		writeError(w, 500, "提醒读取失败")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "unread": unread, "total": total, "page": page, "pageSize": 25})
}
