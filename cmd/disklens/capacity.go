package main

import (
	"fmt"
	"net/http"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type capacity struct {
	ID        string   `json:"id"`
	Roots     []string `json:"roots"`
	Total     uint64   `json:"total"`
	Free      uint64   `json:"free"`
	Available uint64   `json:"available"`
}

func (s *server) capacities(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "不支持的请求方法")
		return
	}
	roots, _, err := s.roots(r.Context())
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	items, unavailable := readCapacities(roots)
	writeJSON(w, 200, map[string]any{"items": items, "unavailable": unavailable})
}

func readCapacities(roots []string) ([]*capacity, []string) {
	items := []*capacity{}
	byDevice := map[string]*capacity{}
	unavailable := []string{}
	for _, root := range roots {
		if !pathAllowed(root, roots) {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			unavailable = append(unavailable, root)
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		id := fmt.Sprint(stat.Dev)
		if item := byDevice[id]; item != nil {
			item.Roots = append(item.Roots, root)
			continue
		}
		var fs unix.Statfs_t
		if err := unix.Statfs(root, &fs); err != nil {
			unavailable = append(unavailable, root)
			continue
		}
		item := &capacity{ID: id, Roots: []string{root}, Total: fs.Blocks * uint64(fs.Bsize), Free: fs.Bfree * uint64(fs.Bsize), Available: fs.Bavail * uint64(fs.Bsize)}
		items = append(items, item)
		byDevice[id] = item
	}
	return items, unavailable
}
