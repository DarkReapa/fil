package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mediaDir   = "media"
	streamDir  = "media/streams"
	tempDir    = "media/streams/tmp"
	uploadDir  = "media/.uploads"
	libraryCfg = "media/library.json"
	streamsCfg = "media/streams.json"
)

type libraryItem struct {
	Name     string    `json:"name"`
	Title    string    `json:"title"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	IsDir    bool      `json:"isDir"`
	MimeType string    `json:"mimeType"`
	Tags     []string  `json:"tags"`
	Genres   []string  `json:"genres"`
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
	history map[string][]roomMessage
}

type roomMessage struct {
	Type     string  `json:"type"`
	User     string  `json:"user"`
	Text     string  `json:"text"`
	URL      string  `json:"url"`
	Position float64 `json:"position"`
	Paused   bool    `json:"paused"`
}

type libraryMeta struct {
	Title  string   `json:"title"`
	Tags   []string `json:"tags"`
	Genres []string `json:"genres"`
}

type libraryStore struct {
	mu    sync.Mutex
	items map[string]libraryMeta
}

type uploadSession struct {
	ID   string
	Path string
	Size int64
}

type uploadManager struct {
	mu       sync.Mutex
	sessions map[string]uploadSession
}

func newStreamManager() *streamManager {
	return &streamManager{procs: make(map[string]*exec.Cmd)}
}

func newRoomHub() *roomHub {
	return &roomHub{
		rooms:   make(map[string]*roomState),
		clients: make(map[string]map[chan roomMessage]bool),
		history: make(map[string][]roomMessage),
	}
}

func newLibraryStore() *libraryStore {
	return &libraryStore{items: make(map[string]libraryMeta)}
}

func newUploadManager() *uploadManager {
	return &uploadManager{sessions: make(map[string]uploadSession)}
}

func (ls *libraryStore) load() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	file, err := os.Open(libraryCfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	return json.NewDecoder(file).Decode(&ls.items)
}

func (ls *libraryStore) save() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(libraryCfg), 0o755); err != nil {
		return err
	}

	file, err := os.Create(libraryCfg)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(ls.items)
}

func (ls *libraryStore) get(path string) (libraryMeta, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	meta, ok := ls.items[path]
	return meta, ok
}

func (ls *libraryStore) set(path string, meta libraryMeta) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	ls.items[path] = meta
}

func (ls *libraryStore) delete(path string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	delete(ls.items, path)
}

func (um *uploadManager) create(path string, size int64) uploadSession {
	um.mu.Lock()
	defer um.mu.Unlock()

	id := fmt.Sprintf("upload-%d", time.Now().UnixNano())
	session := uploadSession{ID: id, Path: path, Size: size}
	um.sessions[id] = session
	return session
}

func (um *uploadManager) get(id string) (uploadSession, bool) {
	um.mu.Lock()
	defer um.mu.Unlock()

	session, ok := um.sessions[id]
	return session, ok
}

func (um *uploadManager) delete(id string) {
	um.mu.Lock()
	defer um.mu.Unlock()

	delete(um.sessions, id)
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
	if payload.Type == "chat" {
		rh.history[roomID] = append(rh.history[roomID], payload)
	}
	clients := rh.clients[roomID]
	rh.mu.Unlock()

	for ch := range clients {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (rh *roomHub) delete(roomID string) {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	delete(rh.rooms, roomID)
	delete(rh.history, roomID)
	if clients, ok := rh.clients[roomID]; ok {
		for ch := range clients {
			close(ch)
		}
		delete(rh.clients, roomID)
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

func (sm *streamManager) delete(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for i, item := range sm.streams {
		if item.ID == id {
			if proc, ok := sm.procs[id]; ok && proc.Process != nil {
				_ = proc.Process.Kill()
				delete(sm.procs, id)
			}
			sm.streams = append(sm.streams[:i], sm.streams[i+1:]...)
			_ = os.RemoveAll(filepath.Join(streamDir, id))
			_ = os.RemoveAll(filepath.Join(tempDir, id))
			return true
		}
	}
	return false
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

func listLibrary(root string, store *libraryStore) ([]libraryItem, error) {
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
		if rel == filepath.Base(uploadDir) || strings.HasPrefix(rel, filepath.Base(uploadDir)+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == filepath.Base(libraryCfg) {
			return nil
		}
		if rel == "streams" || strings.HasPrefix(rel, "streams"+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		mimeType := ""
		if !d.IsDir() {
			mimeType = mime.TypeByExtension(filepath.Ext(path))
		}
		meta, _ := store.get(filepath.ToSlash(rel))
		title := meta.Title
		if title == "" {
			title = info.Name()
		}

		items = append(items, libraryItem{
			Name:     info.Name(),
			Title:    title,
			Path:     filepath.ToSlash(rel),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			IsDir:    d.IsDir(),
			MimeType: mimeType,
			Tags:     meta.Tags,
			Genres:   meta.Genres,
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

func uploadStartHandler(um *uploadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		clean, err := sanitizePath(payload.Path)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if payload.Size <= 0 {
			http.Error(w, "invalid size", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(uploadDir, 0o755); err != nil {
			http.Error(w, "failed to create upload dir", http.StatusInternalServerError)
			return
		}
		session := um.create(clean, payload.Size)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": session.ID})
	}
}

func uploadStatusHandler(um *uploadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		session, ok := um.get(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		tempPath := filepath.Join(uploadDir, id+".part")
		info, err := os.Stat(tempPath)
		offset := int64(0)
		if err == nil {
			offset = info.Size()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"offset": offset,
			"size":   session.Size,
		})
	}
}

func uploadChunkHandler(um *uploadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		session, ok := um.get(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		offsetHeader := r.Header.Get("X-Upload-Offset")
		if offsetHeader == "" {
			http.Error(w, "missing offset", http.StatusBadRequest)
			return
		}
		offset, err := parseInt64(offsetHeader)
		if err != nil {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		tempPath := filepath.Join(uploadDir, id+".part")
		file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			http.Error(w, "failed to write", http.StatusInternalServerError)
			return
		}
		defer file.Close()

		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			http.Error(w, "failed to seek", http.StatusInternalServerError)
			return
		}
		written, err := io.Copy(file, r.Body)
		if err != nil {
			http.Error(w, "failed to write", http.StatusInternalServerError)
			return
		}
		currentOffset := offset + written

		if currentOffset >= session.Size {
			target := filepath.Join(mediaDir, session.Path)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				http.Error(w, "failed to finalize", http.StatusInternalServerError)
				return
			}
			if err := os.Rename(tempPath, target); err != nil {
				http.Error(w, "failed to finalize", http.StatusInternalServerError)
				return
			}
			um.delete(id)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"offset": currentOffset})
	}
}

func uploadCancelHandler(um *uploadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		um.delete(id)
		_ = os.Remove(filepath.Join(uploadDir, id+".part"))
		w.WriteHeader(http.StatusNoContent)
	}
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}

func isYouTubeHost(host string) bool {
	host = strings.ToLower(host)
	return strings.Contains(host, "youtube.com") || strings.Contains(host, "youtu.be")
}

func isYouTubeMediaHost(host string) bool {
	host = strings.ToLower(host)
	return strings.Contains(host, "googlevideo.com") || strings.Contains(host, "youtube.com") || strings.Contains(host, "youtu.be")
}

func extractYouTubeVideoID(target *url.URL) string {
	host := strings.ToLower(target.Host)
	if strings.Contains(host, "youtu.be") {
		parts := strings.Split(strings.Trim(target.Path, "/"), "/")
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}
	if strings.Contains(host, "youtube.com") {
		if target.Path == "/watch" {
			return target.Query().Get("v")
		}
		if strings.HasPrefix(target.Path, "/shorts/") {
			parts := strings.Split(strings.Trim(target.Path, "/"), "/")
			if len(parts) > 1 {
				return parts[1]
			}
		}
		if strings.HasPrefix(target.Path, "/embed/") {
			parts := strings.Split(strings.Trim(target.Path, "/"), "/")
			if len(parts) > 1 {
				return parts[1]
			}
		}
	}
	return ""
}

func extractJSONBlock(input, marker string) (string, bool) {
	idx := strings.Index(input, marker)
	if idx == -1 {
		return "", false
	}
	braceStart := strings.Index(input[idx:], "{")
	if braceStart == -1 {
		return "", false
	}
	start := idx + braceStart
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(input); i++ {
		ch := input[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				return input[start : i+1], true
			}
		}
	}
	return "", false
}

func fetchYouTubePlayerResponse(ctx context.Context, videoID, listID, userAgent string) (map[string]interface{}, error) {
	infoURL := fmt.Sprintf("https://www.youtube.com/get_video_info?video_id=%s&html5=1&c=TVHTML5&cver=7.20201028", url.QueryEscape(videoID))
	if listID != "" {
		infoURL = infoURL + "&list=" + url.QueryEscape(listID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "ru,en;q=0.9")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Referer", "https://www.youtube.com/")
	client := youtubeHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(bodyBytes))
	if err != nil {
		return nil, err
	}
	playerResponse := values.Get("player_response")
	if playerResponse == "" {
		return nil, fmt.Errorf("player response missing")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(playerResponse), &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func resolveYouTubeMediaURL(ctx context.Context, target *url.URL, userAgent string) (string, error) {
	videoID := extractYouTubeVideoID(target)
	if videoID == "" {
		return "", fmt.Errorf("missing video id")
	}
	listID := target.Query().Get("list")
	watchURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", url.QueryEscape(videoID))
	if listID != "" {
		watchURL = watchURL + "&list=" + url.QueryEscape(listID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru,en;q=0.9")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Referer", "https://www.youtube.com/")
	client := youtubeHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	body := string(bodyBytes)
	playerJSON, ok := extractJSONBlock(body, "ytInitialPlayerResponse")
	var parsed map[string]interface{}
	if ok {
		if err := json.Unmarshal([]byte(playerJSON), &parsed); err != nil {
			return "", err
		}
	} else {
		fallback, err := fetchYouTubePlayerResponse(ctx, videoID, listID, userAgent)
		if err != nil {
			return "", fmt.Errorf("player response not found")
		}
		parsed = fallback
	}
	streaming, ok := parsed["streamingData"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("streaming data missing")
	}
	var formats []interface{}
	if items, ok := streaming["formats"].([]interface{}); ok {
		formats = append(formats, items...)
	}
	if items, ok := streaming["adaptiveFormats"].([]interface{}); ok {
		formats = append(formats, items...)
	}
	for _, item := range formats {
		format, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		mimeType, _ := format["mimeType"].(string)
		if !strings.Contains(mimeType, "video/mp4") {
			continue
		}
		if urlValue, ok := format["url"].(string); ok && urlValue != "" {
			return urlValue, nil
		}
		if cipher, ok := format["signatureCipher"].(string); ok && cipher != "" {
			if resolved := resolveYouTubeCipher(cipher); resolved != "" {
				return resolved, nil
			}
		}
		if cipher, ok := format["cipher"].(string); ok && cipher != "" {
			if resolved := resolveYouTubeCipher(cipher); resolved != "" {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("stream url not found")
}

func resolveYouTubeCipher(cipher string) string {
	parsed, err := url.ParseQuery(cipher)
	if err != nil {
		return ""
	}
	rawURL := parsed.Get("url")
	if rawURL == "" {
		return ""
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	signature := parsed.Get("sig")
	if signature == "" {
		signature = parsed.Get("signature")
	}
	if signature == "" {
		return ""
	}
	param := parsed.Get("sp")
	if param == "" {
		param = "signature"
	}
	query := target.Query()
	query.Set(param, signature)
	target.RawQuery = query.Encode()
	return target.String()
}

func youtubeHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp6", addr)
	}
	return &http.Client{Transport: transport}
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	targetURL, err := url.Parse(raw)
	if err != nil || (targetURL.Scheme != "http" && targetURL.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	isYouTubeRequest := isYouTubeHost(targetURL.Host)

	userAgent := r.Header.Get("User-Agent")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}
	if isYouTubeRequest {
		if mediaURL, err := resolveYouTubeMediaURL(r.Context(), targetURL, userAgent); err == nil {
			if parsed, err := url.Parse(mediaURL); err == nil {
				targetURL = parsed
			}
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL.String(), nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	} else {
		req.Header.Set("Accept", "*/*")
	}
	if acceptLanguage := r.Header.Get("Accept-Language"); acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	} else {
		req.Header.Set("Accept-Language", "ru,en;q=0.9")
	}
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Accept-Encoding", "identity")
	if isYouTubeRequest || isYouTubeMediaHost(targetURL.Host) {
		req.Header.Set("Referer", "https://www.youtube.com/")
		req.Header.Set("Origin", "https://www.youtube.com")
	}
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	client := http.DefaultClient
	if isYouTubeRequest || isYouTubeMediaHost(targetURL.Host) {
		client = youtubeHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/vnd.apple.mpegurl") || strings.Contains(contentType, "application/x-mpegurl") || strings.HasSuffix(targetURL.Path, ".m3u8") {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "failed to read", http.StatusBadGateway)
			return
		}
		rewrite := rewritePlaylist(string(bodyBytes), targetURL)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write([]byte(rewrite))
		return
	}

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func rewritePlaylist(body string, base *url.URL) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ref, err := base.Parse(trimmed)
		if err != nil {
			continue
		}
		lines[i] = "/api/proxy?url=" + url.QueryEscape(ref.String())
	}
	return strings.Join(lines, "\n")
}

func libraryHandler(store *libraryStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := listLibrary(mediaDir, store)
		if err != nil {
			http.Error(w, "failed to read library", http.StatusInternalServerError)
			return
		}

		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		tags := parseCSV(r.URL.Query().Get("tags"))
		genres := parseCSV(r.URL.Query().Get("genres"))

		filtered := make([]libraryItem, 0, len(items))
		for _, item := range items {
			if query != "" {
				target := strings.ToLower(item.Title)
				if target == "" {
					target = strings.ToLower(item.Name)
				}
				if !strings.Contains(target, query) {
					continue
				}
			}
			if len(tags) > 0 && !containsAll(item.Tags, tags) {
				continue
			}
			if len(genres) > 0 && !containsAll(item.Genres, genres) {
				continue
			}
			filtered = append(filtered, item)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(filtered)
	}
}

func libraryItemHandler(store *libraryStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			var payload struct {
				Path   string   `json:"path"`
				Title  string   `json:"title"`
				Tags   []string `json:"tags"`
				Genres []string `json:"genres"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid payload", http.StatusBadRequest)
				return
			}
			clean, err := sanitizePath(payload.Path)
			if err != nil {
				http.Error(w, "invalid path", http.StatusBadRequest)
				return
			}
			meta := libraryMeta{
				Title:  strings.TrimSpace(payload.Title),
				Tags:   cleanList(payload.Tags),
				Genres: cleanList(payload.Genres),
			}
			store.set(filepath.ToSlash(clean), meta)
			if err := store.save(); err != nil {
				log.Printf("failed to save library metadata: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
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
			if err := os.RemoveAll(target); err != nil {
				http.Error(w, "failed to delete", http.StatusInternalServerError)
				return
			}
			store.delete(filepath.ToSlash(clean))
			if err := store.save(); err != nil {
				log.Printf("failed to save library metadata: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func libraryFolderHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Name    string `json:"name"`
			Seasons int    `json:"seasons"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(payload.Name)
		if name == "" {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		if payload.Seasons < 1 || payload.Seasons > 50 {
			http.Error(w, "invalid seasons", http.StatusBadRequest)
			return
		}
		clean, err := sanitizePath(name)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		seriesDir := filepath.Join(mediaDir, clean)
		if err := os.MkdirAll(seriesDir, 0o755); err != nil {
			http.Error(w, "failed to create folder", http.StatusInternalServerError)
			return
		}
		for i := 1; i <= payload.Seasons; i++ {
			seasonName := fmt.Sprintf("Сезон %d", i)
			seasonDir := filepath.Join(seriesDir, seasonName)
			if err := os.MkdirAll(seasonDir, 0o755); err != nil {
				http.Error(w, "failed to create folder", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
	}
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

func streamActionHandler(sm *streamManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/streams/")
		if id == "" {
			http.Error(w, "missing stream id", http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(id, "/start") {
			id = strings.TrimSuffix(id, "/start")
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
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
			return
		}
		switch r.Method {
		case http.MethodDelete:
			if ok := sm.delete(id); !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if err := sm.save(); err != nil {
				log.Printf("failed to save streams: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
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

				rh.mu.Lock()
				history := append([]roomMessage(nil), rh.history[id]...)
				rh.mu.Unlock()

				if len(history) > 0 {
					payload := struct {
						Type     string        `json:"type"`
						Messages []roomMessage `json:"messages"`
					}{
						Type:     "history",
						Messages: history,
					}
					data, _ := json.Marshal(payload)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}

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
		switch r.Method {
		case http.MethodGet:
			room, ok := rh.get(id)
			if !ok {
				http.Error(w, "room not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(room)
		case http.MethodDelete:
			if _, ok := rh.get(id); !ok {
				http.Error(w, "room not found", http.StatusNotFound)
				return
			}
			rh.delete(id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func parseCSV(input string) []string {
	parts := strings.Split(input, ",")
	var out []string
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			out = append(out, strings.ToLower(value))
		}
	}
	return out
}

func cleanList(values []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func containsAll(source []string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	lookup := make(map[string]bool)
	for _, item := range source {
		lookup[strings.ToLower(item)] = true
	}
	for _, filter := range filters {
		if !lookup[strings.ToLower(filter)] {
			return false
		}
	}
	return true
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
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Fatalf("failed to create upload dir: %v", err)
	}

	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	streams := newStreamManager()
	rooms := newRoomHub()
	library := newLibraryStore()
	uploads := newUploadManager()
	if err := streams.load(); err != nil {
		log.Printf("failed to load streams: %v", err)
	}
	if err := library.load(); err != nil {
		log.Printf("failed to load library metadata: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler(tmpl))
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/api/upload/start", uploadStartHandler(uploads))
	mux.HandleFunc("/api/upload/status", uploadStatusHandler(uploads))
	mux.HandleFunc("/api/upload/chunk", uploadChunkHandler(uploads))
	mux.HandleFunc("/api/upload/cancel", uploadCancelHandler(uploads))
	mux.HandleFunc("/download", downloadHandler)
	mux.HandleFunc("/api/proxy", proxyHandler)
	mux.HandleFunc("/api/library", libraryHandler(library))
	mux.HandleFunc("/api/library/item", libraryItemHandler(library))
	mux.HandleFunc("/api/library/folder", libraryFolderHandler())
	mux.HandleFunc("/api/streams", streamsHandler(streams))
	mux.HandleFunc("/api/streams/", streamActionHandler(streams))
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
