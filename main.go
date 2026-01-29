package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	mediaDir   = "media"
	streamDir  = "media/streams"
	streamsCfg = "media/streams.json"
)

type libraryItem struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	IsDir    bool      `json:"isDir"`
	MimeType string    `json:"mimeType"`
}

type streamItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

type streamManager struct {
	mu      sync.Mutex
	streams []streamItem
	procs   map[string]*exec.Cmd
}

func newStreamManager() *streamManager {
	return &streamManager{procs: make(map[string]*exec.Cmd)}
}

func (sm *streamManager) load() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	file, err := os.Open(streamsCfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			sm.streams = []streamItem{}
			return nil
		}
		return err
	}
	defer file.Close()

	return json.NewDecoder(file).Decode(&sm.streams)
}

func (sm *streamManager) save() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(streamsCfg), 0o755); err != nil {
		return err
	}

	file, err := os.Create(streamsCfg)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(sm.streams)
}

func (sm *streamManager) list() []streamItem {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	items := make([]streamItem, len(sm.streams))
	copy(items, sm.streams)
	for i, item := range items {
		if _, ok := sm.procs[item.ID]; ok {
			items[i].Active = true
		}
	}
	return items
}

func (sm *streamManager) add(name, url string) (streamItem, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	id := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	item := streamItem{ID: id, Name: name, URL: url}
	sm.streams = append(sm.streams, item)
	return item, nil
}

func (sm *streamManager) start(id string) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	var stream *streamItem
	for i := range sm.streams {
		if sm.streams[i].ID == id {
			stream = &sm.streams[i]
			break
		}
	}
	if stream == nil {
		return "", fmt.Errorf("stream not found")
	}

	if !strings.HasPrefix(strings.ToLower(stream.URL), "rtmp://") {
		return stream.URL, nil
	}

	if proc, ok := sm.procs[id]; ok {
		if proc.ProcessState == nil || !proc.ProcessState.Exited() {
			return fmt.Sprintf("/streams/%s/index.m3u8", id), nil
		}
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not found")
	}

	outputDir := filepath.Join(streamDir, id)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", err
	}

	output := filepath.Join(outputDir, "index.m3u8")
	cmd := exec.Command(
		"ffmpeg",
		"-y",
		"-i", stream.URL,
		"-c", "copy",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "6",
		"-hls_flags", "delete_segments+append_list",
		output,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		_ = cmd.Wait()
		sm.mu.Lock()
		delete(sm.procs, id)
		sm.mu.Unlock()
	}()
	sm.procs[id] = cmd

	return fmt.Sprintf("/streams/%s/index.m3u8", id), nil
}

func sanitizePath(path string) (string, error) {
	clean := filepath.Clean(path)
	clean = strings.TrimPrefix(clean, string(filepath.Separator))
	clean = strings.TrimPrefix(clean, "..")
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid path")
	}
	return clean, nil
}

func listLibrary(root string) ([]libraryItem, error) {
	var items []libraryItem

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		mimeType := ""
		if !d.IsDir() {
			mimeType = mime.TypeByExtension(filepath.Ext(path))
		}

		items = append(items, libraryItem{
			Name:     info.Name(),
			Path:     filepath.ToSlash(rel),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			IsDir:    d.IsDir(),
			MimeType: mimeType,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	return items, nil
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(2 << 30); err != nil {
		http.Error(w, "failed to parse upload", http.StatusBadRequest)
		return
	}

	form := r.MultipartForm
	files := form.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files received", http.StatusBadRequest)
		return
	}

	for _, header := range files {
		file, err := header.Open()
		if err != nil {
			continue
		}

		relPath, err := sanitizePath(header.Filename)
		if err != nil {
			file.Close()
			continue
		}

		targetPath := filepath.Join(mediaDir, relPath)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			file.Close()
			continue
		}

		out, err := os.Create(targetPath)
		if err != nil {
			file.Close()
			continue
		}

		_, _ = io.Copy(out, file)
		out.Close()
		file.Close()
	}

	w.WriteHeader(http.StatusCreated)
}

func libraryHandler(w http.ResponseWriter, r *http.Request) {
	items, err := listLibrary(mediaDir)
	if err != nil {
		http.Error(w, "failed to read library", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func streamsHandler(sm *streamManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sm.list())
		case http.MethodPost:
			var payload struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid payload", http.StatusBadRequest)
				return
			}
			item, err := sm.add(strings.TrimSpace(payload.Name), strings.TrimSpace(payload.URL))
			if err != nil {
				http.Error(w, "failed to add stream", http.StatusInternalServerError)
				return
			}
			if err := sm.save(); err != nil {
				log.Printf("failed to save streams: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(item)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func streamStartHandler(sm *streamManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/streams/")
		id = strings.TrimSuffix(id, "/start")
		if id == "" {
			http.Error(w, "missing stream id", http.StatusBadRequest)
			return
		}

		url, err := sm.start(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": url})
	}
}

func indexHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_ = tmpl.Execute(w, nil)
	}
}

func main() {
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		log.Fatalf("failed to create media dir: %v", err)
	}
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		log.Fatalf("failed to create stream dir: %v", err)
	}

	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	streams := newStreamManager()
	if err := streams.load(); err != nil {
		log.Printf("failed to load streams: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler(tmpl))
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/api/library", libraryHandler)
	mux.HandleFunc("/api/streams", streamsHandler(streams))
	mux.HandleFunc("/api/streams/", streamStartHandler(streams))

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.Dir(mediaDir))))
	mux.Handle("/streams/", http.StripPrefix("/streams/", http.FileServer(http.Dir(streamDir))))

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("Neon Media Library running on http://localhost:8080")
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
