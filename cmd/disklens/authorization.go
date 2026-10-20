package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func normalizeRoots(paths []string) []string {
	roots := []string{}
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		if path == "/" || seen[path] {
			continue
		}
		roots = append(roots, path)
		seen[path] = true
	}
	return roots
}

func configuredRoots() []string {
	if os.Getenv("TRIM_APPNAME") != "" {
		return normalizeRoots(filepath.SplitList(os.Getenv("TRIM_DATA_ACCESSIBLE_PATHS")))
	}
	if raw := os.Getenv("DISKLENS_ROOTS"); raw != "" {
		return normalizeRoots(strings.Split(raw, ","))
	}
	cwd, _ := os.Getwd()
	return normalizeRoots([]string{cwd})
}

func resolveRoots(ctx context.Context) ([]string, string, error) {
	if os.Getenv("TRIM_APPNAME") == "" {
		return configuredRoots(), "", nil
	}
	token := os.Getenv("TRIM_API_TOKEN")
	socket := "/var/run/trim_open_gateway_apiscope.socket"
	_, statErr := os.Stat(socket)
	if token == "" || os.IsNotExist(statErr) {
		return configuredRoots(), "当前系统使用启动时授权目录；修改授权后请停止并重新启动应用。", nil
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	roots, err := fetchSharedRoots(ctx, client, token, os.Getenv("TRIM_APPNAME"))
	if err != nil {
		return nil, "", err
	}
	// An empty successful response means all authorizations were revoked.
	return normalizeRoots(roots), "", nil
}

func fetchSharedRoots(ctx context.Context, client *http.Client, token, appName string) ([]string, error) {
	body, _ := json.Marshal(map[string]any{
		"reqId":   fmt.Sprint(time.Now().UnixNano()),
		"req":     "trim.file.getSharedAccessibleFolders",
		"appName": appName, "data": map[string]any{},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/api/v1/trimapp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("飞牛授权服务暂时无法连接，请刷新重试")
	}
	defer resp.Body.Close()
	var result struct {
		Code *int `json:"code"`
		Data *struct {
			Paths []string `json:"paths"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("飞牛授权查询失败（HTTP %d），请重启应用后重试", resp.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil || result.Code == nil || *result.Code != 0 || result.Data == nil {
		return nil, fmt.Errorf("飞牛授权响应异常，请重启应用后重试")
	}
	return result.Data.Paths, nil
}

func pathAllowed(root string, roots []string) bool {
	if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return false
	}
	actual, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	for _, allowed := range roots {
		canonical, err := filepath.EvalSymlinks(allowed)
		if err != nil || canonical == "/" {
			continue
		}
		rel, err := filepath.Rel(canonical, actual)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
