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
	tempDir    = "media/streams/tmp"
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
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Persist bool   `json:"persist"`
	Active  bool   `json:"active"`
}

type streamManager struct {
	mu      sync.Mutex
	streams []streamItem
	procs   map[string]*exec.Cmd
}

type roomState struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Position float64   `json:"position"`
	Paused   bool      `json:"paused"`
	Updated  time.Time `json:"updated"`
}

type roomHub struct {
	mu      sync.Mutex
	rooms   map[string]*roomState
	clients map[string]map[chan roomMessage]bool
}

type roomMessage struct {
	Type     string  `json:"type"`
	User     string  `json:"user"`
	Text     string  `json:"text"`
	URL      string  `json:"url"`
	Position float64 `json:"position"`
	Paused   bool    `json:"paused"`
}

func newStreamManager() *streamManager {
	return &streamManager{procs: make(map[string]*exec.Cmd)}
}

func newRoomHub() *roomHub {
	return &roomHub{
		rooms:   make(map[string]*roomState),
		clients: make(map[string]map[chan roomMessage]bool),
	}
}

func (rh *roomHub) create(name, url string) *roomState {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	id := fmt.Sprintf("room-%d", time.Now().UnixNano())
	room := &roomState{
		ID:      id,
		Name:    name,
		URL:     url,
		Paused:  true,
		Updated: time.Now(),
	}
	rh.rooms[id] = room
	return room
}

func (rh *roomHub) get(id string) (*roomState, bool) {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	room, ok := rh.rooms[id]
	return room, ok
}

func (rh *roomHub) attach(roomID string) chan roomMessage {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	if rh.clients[roomID] == nil {
		rh.clients[roomID] = make(map[chan roomMessage]bool)
	}
	ch := make(chan roomMessage, 8)
	rh.clients[roomID][ch] = true
	return ch
}

func (rh *roomHub) detach(roomID string, ch chan roomMessage) {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	if clients, ok := rh.clients[roomID]; ok {
		delete(clients, ch)
		if len(clients) == 0 {
			delete(rh.clients, roomID)
		}
	}
	close(ch)
}

func (rh *roomHub) broadcast(roomID string, payload roomMessage) {
	rh.mu.Lock()
	clients := rh.clients[roomID]
	rh.mu.Unlock()

	for ch := range clients {
		select {
		case ch <- payload:
		default:
		}
	}
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

func (sm *streamManager) get(id string) (streamItem, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, item := range sm.streams {
		if item.ID == id {
			return item, true
		}
	}
	return streamItem{}, false
}

func (sm *streamManager) add(name, url string) (streamItem, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	id := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	item := streamItem{ID: id, Name: name, URL: url, Persist: true}
	sm.streams = append(sm.streams, item)
	return item, nil
}

func (sm *streamManager) start(id string, persist bool) (string, error) {
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

	stream.Persist = persist

	if proc, ok := sm.procs[id]; ok {
		if proc.ProcessState == nil || !proc.ProcessState.Exited() {
			return fmt.Sprintf("/streams/%s/index.m3u8", id), nil
		}
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not found")
	}

	outputDir := filepath.Join(streamDir, id)
	if !persist {
		outputDir = filepath.Join(tempDir, id)
	}
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
		"-hls_time", "4",
		"-hls_list_size", "21600",
		"-hls_flags", "delete_segments+append_list+program_date_time+independent_segments",
		"-hls_segment_filename", filepath.Join(outputDir, "segment_%05d.ts"),
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
		if !persist {
			_ = os.RemoveAll(outputDir)
		}
	}()
	sm.procs[id] = cmd

	if persist {
		return fmt.Sprintf("/streams/%s/index.m3u8", id), nil
	}
	return fmt.Sprintf("/streams/tmp/%s/index.m3u8", id), nil
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
				Name    string `json:"name"`
				URL     string `json:"url"`
				Persist bool   `json:"persist"`
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
			item.Persist = payload.Persist
			for i := range sm.streams {
				if sm.streams[i].ID == item.ID {
					sm.streams[i].Persist = payload.Persist
					break
				}
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

		var payload struct {
			Persist *bool `json:"persist"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		persist := true
		if payload.Persist != nil {
			persist = *payload.Persist
		} else if existing, ok := sm.get(id); ok {
			persist = existing.Persist
		}

		url, err := sm.start(id, persist)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := sm.save(); err != nil {
			log.Printf("failed to save streams: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": url})
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	pathParam := r.URL.Query().Get("path")
	if pathParam == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	clean, err := sanitizePath(pathParam)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	target := filepath.Join(mediaDir, clean)
	info, err := os.Stat(target)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "cannot download directory", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	http.ServeFile(w, r, target)
}

func roomsHandler(rh *roomHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var payload struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid payload", http.StatusBadRequest)
				return
			}
			room := rh.create(strings.TrimSpace(payload.Name), strings.TrimSpace(payload.URL))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(room)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func roomDetailHandler(rh *roomHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/rooms/")
		if strings.HasSuffix(id, "/events") {
			id = strings.TrimSuffix(id, "/events")
			if id == "" {
				http.Error(w, "missing room id", http.StatusBadRequest)
				return
			}
			switch r.Method {
			case http.MethodGet:
				flusher, ok := w.(http.Flusher)
				if !ok {
					http.Error(w, "streaming unsupported", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")

				ch := rh.attach(id)
				defer rh.detach(id, ch)

				if room, ok := rh.get(id); ok {
					initial := roomMessage{
						Type:     "sync",
						URL:      room.URL,
						Position: room.Position,
						Paused:   room.Paused,
					}
					data, _ := json.Marshal(initial)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}

				ctx := r.Context()
				for {
					select {
					case <-ctx.Done():
						return
					case msg := <-ch:
						data, _ := json.Marshal(msg)
						fmt.Fprintf(w, "data: %s\n\n", data)
						flusher.Flush()
					}
				}
			case http.MethodPost:
				var msg roomMessage
				if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
					http.Error(w, "invalid payload", http.StatusBadRequest)
					return
				}
				if msg.Type == "" {
					http.Error(w, "missing type", http.StatusBadRequest)
					return
				}
				if msg.Type == "sync" {
					rh.mu.Lock()
					if room, ok := rh.rooms[id]; ok {
						if msg.URL != "" {
							room.URL = msg.URL
						}
						room.Position = msg.Position
						room.Paused = msg.Paused
						room.Updated = time.Now()
					}
					rh.mu.Unlock()
				}
				rh.broadcast(id, msg)
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}

		if id == "" {
			http.Error(w, "missing room id", http.StatusBadRequest)
			return
		}
		room, ok := rh.get(id)
		if !ok {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(room)
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
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		log.Fatalf("failed to create temp stream dir: %v", err)
	}

	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	streams := newStreamManager()
	rooms := newRoomHub()
	if err := streams.load(); err != nil {
		log.Printf("failed to load streams: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler(tmpl))
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/download", downloadHandler)
	mux.HandleFunc("/api/library", libraryHandler)
	mux.HandleFunc("/api/streams", streamsHandler(streams))
	mux.HandleFunc("/api/streams/", streamStartHandler(streams))
	mux.HandleFunc("/api/rooms", roomsHandler(rooms))
	mux.HandleFunc("/api/rooms/", roomDetailHandler(rooms))

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
