package main

import (
	"container/heap"
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type progress struct {
	TotalBytes  int64  `json:"totalBytes"`
	FileCount   int    `json:"fileCount"`
	DirCount    int    `json:"dirCount"`
	Skipped     int    `json:"skipped"`
	Excluded    int    `json:"excluded"`
	Symlinks    int    `json:"symlinks"`
	ReadErrors  int    `json:"readErrors"`
	DurationMs  int64  `json:"durationMs"`
	CurrentPath string `json:"currentPath"`
}

type scanResult struct {
	ID              string    `json:"id"`
	Root            string    `json:"root"`
	Roots           []string  `json:"roots"`
	Excludes        []string  `json:"excludes"`
	ScannedAt       time.Time `json:"scannedAt"`
	CoverageVersion int       `json:"coverageVersion"`
	progress
	TopDirs    []entry    `json:"topDirs"`
	TopFiles   []entry    `json:"topFiles"`
	Categories []category `json:"categories"`
}
type entry struct {
	Path         string    `json:"path"`
	AbsolutePath string    `json:"absolutePath"`
	Bytes        int64     `json:"bytes"`
	Files        int       `json:"files"`
	Modified     time.Time `json:"modified"`
}
type scanOptions struct {
	Roots          []string `json:"roots"`
	Excludes       []string `json:"excludes"`
	entryRate      int
	timeoutMinutes int
	inspection     *inspectionSettings
	authorize      func(context.Context) error
}
type indexedEntry struct {
	Path      string `json:"path"`
	Root      string `json:"root"`
	Parent    string `json:"parent"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Bytes     int64  `json:"bytes"`
	Files     int    `json:"files"`
	Modified  int64  `json:"modified"`
	Extension string `json:"extension"`
	Category  string `json:"category"`
	Depth     int    `json:"-"`
}
type category struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}
type fileHeap []entry

func (h fileHeap) Len() int           { return len(h) }
func (h fileHeap) Less(i, j int) bool { return h[i].Bytes < h[j].Bytes }
func (h fileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(x any)        { *h = append(*h, x.(entry)) }
func (h *fileHeap) Pop() any          { old := *h; x := old[len(old)-1]; *h = old[:len(old)-1]; return x }

func analyze(root string) (*scanResult, error) {
	return analyzeContext(context.Background(), root, nil)
}

func analyzeContext(ctx context.Context, root string, report func(progress)) (*scanResult, error) {
	return analyzeOptions(ctx, scanOptions{Roots: []string{root}}, report, nil)
}

func excluded(root, rel string, rules []string) bool {
	full := filepath.Join(root, rel)
	for _, rule := range rules {
		if filepath.IsAbs(rule) {
			if full == rule || strings.HasPrefix(full, rule+string(filepath.Separator)) {
				return true
			}
			continue
		}
		if rel == rule || strings.HasPrefix(rel, rule+"/") {
			return true
		}
		if matched, _ := path.Match(rule, rel); matched {
			return true
		}
		if !strings.Contains(rule, "/") {
			for _, component := range strings.Split(rel, "/") {
				if matched, _ := path.Match(rule, component); matched {
					return true
				}
			}
		}
	}
	return false
}

func analyzeOptions(ctx context.Context, options scanOptions, report func(progress), sink func(indexedEntry) error) (*scanResult, error) {
	started := time.Now()
	result := &scanResult{Root: options.Roots[0], Roots: options.Roots, Excludes: options.Excludes, ScannedAt: started, CoverageVersion: 1, TopDirs: []entry{}, TopFiles: []entry{}, Categories: []category{}}
	visited := 0
	childDirs := map[string]*entry{}
	categories := map[string]*category{}
	files := &fileHeap{}
	lastReport := time.Time{}
	publish := func(force bool) {
		if report != nil && (force || time.Since(lastReport) >= 150*time.Millisecond) {
			result.DurationMs = time.Since(started).Milliseconds()
			report(result.progress)
			lastReport = time.Now()
		}
	}
	for _, root := range options.Roots {
		// Rooted filesystem access prevents traversal outside the chosen directory.
		directory, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		err = fs.WalkDir(directory.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			visited++
			if options.entryRate > 0 && visited%32 == 0 {
				wait := time.Duration(float64(visited)/float64(options.entryRate)*float64(time.Second)) - time.Since(started)
				if wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case <-timer.C:
					}
				}
			}
			result.CurrentPath = filepath.Join(root, path)
			publish(false)
			if walkErr != nil {
				if path == "." {
					return walkErr
				}
				result.Skipped++
				result.ReadErrors++
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				result.Skipped++
				result.Symlinks++
				return nil
			}
			if path != "." && excluded(root, path, options.Excludes) {
				result.Skipped++
				result.Excluded++
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				result.Skipped++
				result.ReadErrors++
				if path == "." {
					return err
				}
				return nil
			}
			absolute := filepath.Join(root, path)
			depth := strings.Count(path, "/") + 1
			if path == "." {
				depth = 0
			}
			indexed := indexedEntry{Path: absolute, Root: root, Parent: filepath.Dir(absolute), Name: filepath.Base(absolute), Kind: "directory", Modified: info.ModTime().UnixNano(), Depth: depth}
			if d.IsDir() {
				if path != "." {
					result.DirCount++
					if !strings.Contains(path, "/") {
						label := path
						if len(options.Roots) > 1 {
							label = absolute
						}
						childDirs[absolute] = &entry{Path: label, AbsolutePath: absolute}
					}
				}
				if sink != nil {
					return sink(indexed)
				}
				return nil
			}
			if !info.Mode().IsRegular() {
				result.Skipped++
				return nil
			}
			size := info.Size()
			result.TotalBytes += size
			result.FileCount++
			if first, _, found := strings.Cut(path, "/"); found {
				key := filepath.Join(root, first)
				item := childDirs[key]
				if item == nil {
					label := first
					if len(options.Roots) > 1 {
						label = key
					}
					item = &entry{Path: label, AbsolutePath: key}
					childDirs[key] = item
				}
				item.Bytes += size
				item.Files++
			}
			label := path
			if len(options.Roots) > 1 {
				label = absolute
			}
			item := entry{Path: label, AbsolutePath: absolute, Bytes: size, Files: 1, Modified: info.ModTime()}
			if files.Len() < 50 {
				heap.Push(files, item)
			} else if size > (*files)[0].Bytes {
				(*files)[0] = item
				heap.Fix(files, 0)
			}
			name := categoryName(filepath.Ext(path))
			group := categories[name]
			if group == nil {
				group = &category{Name: name}
				categories[name] = group
			}
			group.Bytes += size
			group.Files++
			indexed.Kind, indexed.Bytes, indexed.Files = "file", size, 1
			indexed.Extension, indexed.Category = strings.ToLower(filepath.Ext(path)), name
			if sink != nil {
				return sink(indexed)
			}
			return nil
		})
		directory.Close()
		publish(true)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, item := range childDirs {
		result.TopDirs = append(result.TopDirs, *item)
	}
	for _, item := range categories {
		result.Categories = append(result.Categories, *item)
	}
	result.TopFiles = append(result.TopFiles, (*files)...)
	sort.Slice(result.TopDirs, func(i, j int) bool { return result.TopDirs[i].Bytes > result.TopDirs[j].Bytes })
	sort.Slice(result.TopFiles, func(i, j int) bool { return result.TopFiles[i].Bytes > result.TopFiles[j].Bytes })
	sort.Slice(result.Categories, func(i, j int) bool { return result.Categories[i].Bytes > result.Categories[j].Bytes })
	if len(result.TopDirs) > 20 {
		result.TopDirs = result.TopDirs[:20]
	}
	result.CurrentPath = ""
	result.DurationMs = time.Since(started).Milliseconds()
	return result, nil
}

func categoryName(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".svg", ".raw":
		return "图片"
	case ".mp4", ".mkv", ".mov", ".avi", ".m4v", ".ts":
		return "视频"
	case ".mp3", ".flac", ".wav", ".m4a", ".aac":
		return "音频"
	case ".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz":
		return "压缩包"
	case ".iso", ".img", ".vmdk", ".qcow2":
		return "镜像"
	case ".pdf", ".txt", ".md", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx":
		return "文档"
	case ".log":
		return "日志"
	case ".db", ".sqlite", ".sqlite3":
		return "数据库"
	default:
		return "其他"
	}
}
