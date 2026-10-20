package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type inspectionSettings struct {
	Enabled          bool     `json:"enabled"`
	At               string   `json:"at"`
	Timezone         string   `json:"timezone"`
	Roots            []string `json:"roots"`
	Excludes         []string `json:"excludes"`
	EntriesPerSecond int      `json:"entriesPerSecond"`
	TimeoutMinutes   int      `json:"timeoutMinutes"`
	GrowthEnabled    bool     `json:"growthEnabled"`
	GrowthBytes      int64    `json:"growthBytes"`
	GrowthPercent    int      `json:"growthPercent"`
	FreeEnabled      bool     `json:"freeEnabled"`
	FreePercent      int      `json:"freePercent"`
}

func defaultInspection() inspectionSettings {
	return inspectionSettings{At: "03:00", Timezone: "Asia/Shanghai", Roots: []string{}, Excludes: []string{}, EntriesPerSecond: 512,
		TimeoutMinutes: 360, GrowthEnabled: true, GrowthBytes: 10 << 30, GrowthPercent: 20, FreeEnabled: true, FreePercent: 10}
}

func (s *store) inspection(ctx context.Context) (inspectionSettings, error) {
	settings := defaultInspection()
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM preferences WHERE key='inspection'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	err = json.Unmarshal([]byte(raw), &settings)
	return settings, err
}

func validateInspection(in inspectionSettings, allowed []string) (inspectionSettings, error) {
	parsed, err := time.Parse("15:04", in.At)
	if err != nil || parsed.Format("15:04") != in.At {
		return in, fmt.Errorf("开始时间必须为 HH:MM")
	}
	if len(in.Timezone) > 100 || in.Timezone == "" || in.Timezone == "Local" {
		return in, fmt.Errorf("请选择明确的时区")
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return in, fmt.Errorf("时区无效")
	}
	if in.EntriesPerSecond < 0 || in.EntriesPerSecond > 10000 || in.EntriesPerSecond > 0 && in.EntriesPerSecond < 32 {
		return in, fmt.Errorf("扫描速率须为 32 至 10000 项/秒，或 0（不限速）")
	}
	if in.TimeoutMinutes < 1 || in.TimeoutMinutes > 1440 {
		return in, fmt.Errorf("最长扫描时间须为 1 至 1440 分钟")
	}
	if in.GrowthBytes < 1 || in.GrowthBytes > 1<<50 || in.GrowthPercent < 0 || in.GrowthPercent > 10000 || in.FreePercent < 1 || in.FreePercent > 99 {
		return in, fmt.Errorf("提醒阈值超出范围")
	}
	if len(in.Roots) == 0 && !in.Enabled {
		in.Roots, in.Excludes = []string{}, []string{}
		return in, nil
	}
	opts, err := validateOptions(scanOptions{Roots: in.Roots, Excludes: in.Excludes}, allowed)
	if err != nil {
		return in, err
	}
	in.Roots, in.Excludes = opts.Roots, opts.Excludes
	return in, nil
}

func (s *server) inspectionAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	if s.store == nil {
		writeError(w, 503, "索引不可用")
		return
	}
	roots, _, err := s.roots(r.Context())
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	if r.Method == http.MethodPost {
		var in inspectionSettings
		if err := decodeBody(w, r, &in); err != nil {
			writeError(w, 400, "设置格式错误")
			return
		}
		in, err = validateInspection(in, roots)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		s.autoMu.Lock()
		defer s.autoMu.Unlock()
		s.mu.RLock()
		busy := s.taskBusyLocked()
		s.mu.RUnlock()
		if busy {
			writeError(w, 409, "请在磁盘任务结束后保存设置")
			return
		}
		raw, _ := json.Marshal(in)
		if _, err := s.store.db.ExecContext(r.Context(), "INSERT INTO preferences(key,value) VALUES('inspection',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
			writeError(w, 500, "设置保存失败")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	settings, err := s.store.inspection(r.Context())
	if err != nil {
		writeError(w, 500, "设置读取失败")
		return
	}
	scopeValid := len(settings.Roots) == 0 || resultAllowed(&scanResult{Roots: settings.Roots}, roots)
	if !scopeValid {
		settings.Roots, settings.Excludes = []string{}, []string{}
	}
	var attemptedAt, runState string
	err = s.store.db.QueryRowContext(r.Context(), "SELECT attempted_at,state FROM inspection_runs ORDER BY attempted_at DESC LIMIT 1").Scan(&attemptedAt, &runState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 500, "任务状态读取失败")
		return
	}
	next := ""
	if settings.Enabled && scopeValid {
		at, key, err := scheduleSlot(settings, time.Now())
		if err == nil {
			var exists int
			if err := s.store.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM inspection_runs WHERE day=?", key).Scan(&exists); err == nil {
				if exists > 0 || time.Now().After(at.Add(time.Hour)) {
					at = at.AddDate(0, 0, 1)
				}
				next = at.Format(time.RFC3339)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"settings": settings, "scopeValid": scopeValid, "nextRun": next, "lastAttempt": attemptedAt, "lastState": runState})
}

// The start window crosses midnight without creating an extra daily run.
func scheduleSlot(settings inspectionSettings, now time.Time) (time.Time, string, error) {
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		return time.Time{}, "", err
	}
	clock, err := time.Parse("15:04", settings.At)
	if err != nil {
		return time.Time{}, "", err
	}
	local := now.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	if now.Before(at) && now.Sub(at.AddDate(0, 0, -1)) < time.Hour {
		at = at.AddDate(0, 0, -1)
	}
	return at, settings.Timezone + ":" + at.Format("2006-01-02"), nil
}

func (s *server) scheduler(ctx context.Context) {
	defer s.workers.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var nextMaintenance time.Time
	for {
		now := time.Now()
		if !now.Before(nextMaintenance) {
			done, err := s.maintainLedger(ctx, now)
			if err != nil && ctx.Err() == nil {
				log.Printf("ledger retention: %v", err)
			}
			if done {
				nextMaintenance = now.Add(time.Hour)
			}
		}
		if err := s.scheduleTick(ctx, now); err != nil && ctx.Err() == nil {
			log.Printf("scheduled inspection: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) maintainLedger(ctx context.Context, now time.Time) (bool, error) {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	s.mu.RLock()
	busy := s.taskBusyLocked() || s.stopping
	s.mu.RUnlock()
	if busy {
		return false, nil
	}
	maintenance, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.store.pruneLedger(maintenance, now)
	return err == nil, err
}

func (s *server) scheduleTick(ctx context.Context, now time.Time) error {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	settings, err := s.store.inspection(ctx)
	if err != nil || !settings.Enabled {
		return err
	}
	at, day, err := scheduleSlot(settings, now)
	if err != nil || now.Before(at) || !now.Before(at.Add(time.Hour)) {
		return err
	}
	s.mu.RLock()
	busy := s.taskBusyLocked() || s.stopping
	s.mu.RUnlock()
	if busy {
		return nil
	}
	var exists int
	if err := s.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM inspection_runs WHERE day=?", day).Scan(&exists); err != nil || exists > 0 {
		return err
	}
	allowed, _, authErr := s.roots(ctx)
	opts, validErr := validateOptions(scanOptions{Roots: settings.Roots, Excludes: settings.Excludes}, allowed)
	state := "running"
	if authErr != nil || validErr != nil {
		state = "authorization_failed"
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	claim, err := s.store.db.ExecContext(ctx, "INSERT OR IGNORE INTO inspection_runs(day,attempted_at,state,scan_id) VALUES(?,?,?,?)", day, now.UTC().Format(ledgerTimeLayout), state, id)
	if err != nil {
		return err
	}
	n, err := claim.RowsAffected()
	if err != nil || n == 0 || state != "running" {
		return err
	}
	_, err = s.launchScan(opts, settings, id, "schedule")
	if err != nil {
		_, saveErr := s.store.db.ExecContext(ctx, "UPDATE inspection_runs SET state='interrupted' WHERE day=?", day)
		return errors.Join(err, saveErr)
	}
	return nil
}

func (s *server) launchScan(options scanOptions, settings inspectionSettings, id, source string) (*scanJob, error) {
	if s.store != nil {
		found := false
		for _, rule := range options.Excludes {
			if rule == s.store.directory {
				found = true
			}
		}
		if !found {
			options.Excludes = append(options.Excludes, s.store.directory)
		}
		sort.Strings(options.Excludes)
	}
	options.entryRate, options.timeoutMinutes, options.inspection = settings.EntriesPerSecond, settings.TimeoutMinutes, &settings
	options.authorize = func(ctx context.Context) error {
		roots, _, err := s.roots(ctx)
		if err != nil || !resultAllowed(&scanResult{Roots: options.Roots}, roots) {
			return fmt.Errorf("扫描授权已失效")
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taskBusyLocked() || s.stopping {
		return nil, fmt.Errorf("已有磁盘任务运行或服务正在停止")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(settings.TimeoutMinutes)*time.Minute)
	s.cancel = cancel
	s.job = &scanJob{ID: id, Root: options.Roots[0], Roots: options.Roots, State: "running", Source: source}
	s.duplicate = nil
	s.workers.Add(1)
	go s.runScan(ctx, cancel, options, id)
	return s.jobCopyLocked(), nil
}

func inspectionState(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case err != nil:
		return "error"
	default:
		return "completed"
	}
}

func pageIndex(r *http.Request) (int, error) {
	value := r.URL.Query().Get("page")
	if value == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 10000 {
		return 0, fmt.Errorf("页码无效")
	}
	return n, nil
}

func scopeDescription(roots []string) string { return strings.Join(roots, "、") }
