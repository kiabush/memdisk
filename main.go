package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type Config struct {
	Root            string
	Port            string
	MaxUploadBytes  int64
	CleanupInterval time.Duration
}

type Server struct {
	cfg             Config
	startedAt       time.Time
	tmpDeletedTotal int64
}

type FileInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	ModTime   string `json:"modified_at"`
	Kind      string `json:"kind"`
}

type Stats struct {
	Root            string `json:"root"`
	TotalBytes      uint64 `json:"total_bytes"`
	UsedBytes       uint64 `json:"used_bytes"`
	FreeBytes       uint64 `json:"free_bytes"`
	FileCount       int64  `json:"file_count"`
	TmpDeletedTotal int64  `json:"tmp_deleted_total"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
}

func main() {
	cfg := Config{
		Root:            envOr("MEMDISK_ROOT", "/memdisk"),
		Port:            envOr("MEMDISK_PORT", "6380"),
		MaxUploadBytes:  parseSizeBytes(envOr("MEMDISK_MAX_UPLOAD", "512m"), 512*1024*1024),
		CleanupInterval: parseDuration(envOr("MEMDISK_CLEANUP_INTERVAL", "30s"), 30*time.Second),
	}
	cfg.Root = filepath.Clean(cfg.Root)

	s := &Server{cfg: cfg, startedAt: time.Now()}
	if err := s.ensureLayout(); err != nil {
		log.Fatalf("failed to initialize memDisk layout: %v", err)
	}

	go s.cleanupLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/list", s.handleList)
	mux.HandleFunc("/files/", s.handleFile)
	mux.HandleFunc("/tmp/", s.handleFile)
	mux.HandleFunc("/cache/", s.handleFile)
	mux.HandleFunc("/pinned/", s.handleFile)
	mux.HandleFunc("/", s.handleIndex)

	addr := ":" + cfg.Port
	log.Printf("memDisk listening on %s, root=%s, max_upload=%d bytes", addr, cfg.Root, cfg.MaxUploadBytes)
	if err := http.ListenAndServe(addr, logMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

func (s *Server) ensureLayout() error {
	dirs := []string{
		s.cfg.Root,
		filepath.Join(s.cfg.Root, "files"),
		filepath.Join(s.cfg.Root, "cache"),
		filepath.Join(s.cfg.Root, "pinned"),
		filepath.Join(s.cfg.Root, "tmp", "10m"),
		filepath.Join(s.cfg.Root, "tmp", "1h"),
		filepath.Join(s.cfg.Root, "tmp", "24h"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "memDisk",
		"description": "RAM-backed file store for Docker",
		"endpoints": []string{
			"PUT /files/{path}", "GET /files/{path}", "DELETE /files/{path}", "HEAD /files/{path}",
			"PUT /tmp/{ttl}/{path}", "GET /tmp/{ttl}/{path}", "DELETE /tmp/{ttl}/{path}",
			"GET /list", "GET /stats", "GET /health",
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	stats, err := s.computeStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	prefix := strings.Trim(r.URL.Query().Get("prefix"), "/")
	base := s.cfg.Root
	if prefix != "" {
		p, err := s.safePath("/" + prefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		base = p
	}

	files := []FileInfo{}
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == base {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.cfg.Root, path)
		kind := "file"
		if d.IsDir() {
			kind = "dir"
		}
		files = append(files, FileInfo{
			Path:      "/" + filepath.ToSlash(rel),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime().UTC().Format(time.RFC3339),
			Kind:      kind,
		})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path, err := s.safePath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(filepath.ToSlash(r.URL.Path), "/tmp/") {
		if err := validateTmpTTLPath(r.URL.Path); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	switch r.Method {
	case http.MethodPut:
		s.putFile(w, r, path)
	case http.MethodGet:
		s.getFile(w, r, path, false)
	case http.MethodHead:
		s.getFile(w, r, path, true)
	case http.MethodDelete:
		s.deleteFile(w, r, path)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request, path string) {
	if strings.HasSuffix(path, string(os.PathSeparator)) {
		http.Error(w, "path must be a file", http.StatusBadRequest)
		return
	}
	if r.ContentLength > s.cfg.MaxUploadBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tmp := path + ".uploading"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	limited := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	written, err := io.Copy(f, limited)
	if err != nil {
		os.Remove(tmp)
		http.Error(w, "upload failed or payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"path": publicPath(s.cfg.Root, path), "size_bytes": written})
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request, path string, headOnly bool) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory; use /list?prefix=...", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if headOnly {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request, path string) {
	if err := os.RemoveAll(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": publicPath(s.cfg.Root, path)})
}

func (s *Server) safePath(urlPath string) (string, error) {
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	allowed := []string{"/files", "/cache", "/pinned", "/tmp"}
	ok := false
	for _, p := range allowed {
		if clean == p || strings.HasPrefix(clean, p+"/") {
			ok = true
			break
		}
	}
	if !ok {
		return "", errors.New("path must start with /files, /cache, /pinned, or /tmp")
	}
	full := filepath.Join(s.cfg.Root, strings.TrimPrefix(clean, "/"))
	rootWithSep := s.cfg.Root + string(os.PathSeparator)
	if full != s.cfg.Root && !strings.HasPrefix(full, rootWithSep) {
		return "", errors.New("invalid path")
	}
	return full, nil
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(s.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		<-ticker.C
		deleted := s.cleanupExpiredTmp()
		if deleted > 0 {
			atomic.AddInt64(&s.tmpDeletedTotal, deleted)
			log.Printf("TTL cleanup deleted %d expired tmp files", deleted)
		}
	}
}

func (s *Server) cleanupExpiredTmp() int64 {
	now := time.Now()
	var deleted int64
	tmpRoot := filepath.Join(s.cfg.Root, "tmp")
	_ = filepath.WalkDir(tmpRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == tmpRoot {
			return nil
		}
		rel, err := filepath.Rel(tmpRoot, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		ttl, err := parseTmpTTL(parts[0])
		if err != nil {
			if d.IsDir() && len(parts) == 1 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if now.Sub(info.ModTime()) > ttl {
			if os.Remove(path) == nil {
				deleted++
			}
		}
		return nil
	})
	return deleted
}

func validateTmpTTLPath(urlPath string) error {
	clean := filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(urlPath, "/")))
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 3 || parts[0] != "tmp" {
		return errors.New("tmp path must be /tmp/{ttl}/{path}")
	}
	if _, err := parseTmpTTL(parts[1]); err != nil {
		return fmt.Errorf("invalid tmp ttl %q; use values like 30s, 1m, 10m, 2h, or 1d", parts[1])
	}
	return nil
}

func parseTmpTTL(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || days <= 0 {
			return 0, errors.New("invalid day duration")
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New("invalid duration")
	}
	return d, nil
}

func (s *Server) computeStats() (Stats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.Root, &st); err != nil {
		return Stats{}, err
	}
	var fileCount int64
	_ = filepath.WalkDir(s.cfg.Root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			fileCount++
		}
		return nil
	})
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	return Stats{
		Root:            s.cfg.Root,
		TotalBytes:      total,
		UsedBytes:       total - free,
		FreeBytes:       free,
		FileCount:       fileCount,
		TmpDeletedTotal: atomic.LoadInt64(&s.tmpDeletedTotal),
		UptimeSeconds:   int64(time.Since(s.startedAt).Seconds()),
	}, nil
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func parseSizeBytes(s string, fallback int64) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return fallback
	}
	units := map[string]int64{"k": 1024, "kb": 1024, "m": 1024 * 1024, "mb": 1024 * 1024, "g": 1024 * 1024 * 1024, "gb": 1024 * 1024 * 1024}
	for suffix, mult := range units {
		if strings.HasSuffix(s, suffix) {
			n := strings.TrimSpace(strings.TrimSuffix(s, suffix))
			v, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return fallback
			}
			return int64(v * float64(mult))
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

func publicPath(root, full string) string {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return full
	}
	return "/" + filepath.ToSlash(rel)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.String(), time.Since(start))
	})
}
