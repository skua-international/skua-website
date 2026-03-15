package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	htmltpl "html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"

	skuasite "github.com/skua-international/skua-site"
)

type DocEntry struct {
	Slug    string
	Title   string
	Content htmltpl.HTML
}

type DocsCache struct {
	mu      sync.RWMutex
	docs    map[string]*DocEntry
	ordered []*DocEntry
	etag    string
}

type YouTubeVideo struct {
	ID    string
	Title string
}

type YouTubeCache struct {
	mu    sync.RWMutex
	video *YouTubeVideo
}

type Preset struct {
	Name     string `json:"name"`
	Filename string `json:"filename"`
}

type CertAllowlist struct {
	mu           sync.RWMutex
	fingerprints map[string]bool
	path         string
}

func NewCertAllowlist(path string) *CertAllowlist {
	al := &CertAllowlist{
		fingerprints: make(map[string]bool),
		path:         path,
	}
	al.Load()
	return al
}

func (al *CertAllowlist) Load() {
	al.mu.Lock()
	defer al.mu.Unlock()

	al.fingerprints = make(map[string]bool)

	f, err := os.Open(al.path)
	if err != nil {
		log.Printf("cert allowlist: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Normalize: strip colons, lowercase.
		fp := strings.ToLower(strings.ReplaceAll(line, ":", ""))
		al.fingerprints[fp] = true
	}

	log.Printf("cert allowlist: loaded %d fingerprints", len(al.fingerprints))
}

func (al *CertAllowlist) IsAllowed(fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	fp := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
	al.mu.RLock()
	defer al.mu.RUnlock()
	return al.fingerprints[fp]
}

// ServerInfo holds live status for display and the API.
type ServerInfo struct {
	Label      string      `json:"label"`
	Type       string      `json:"type"`
	Address    string      `json:"address"`
	Link       htmltpl.URL `json:"link"`
	Map        string      `json:"map"`
	Players    int         `json:"players"`
	MaxPlayers int         `json:"maxPlayers"`
	Online     bool        `json:"online"`
}

// GameServer defines a server to query. Loaded from servers.json.
type GameServer struct {
	Label     string `json:"label"`
	Type      string `json:"type"` // "steam" or "ts3"
	Host      string `json:"host"`
	GamePort  int    `json:"gamePort"`
	QueryPort int    `json:"queryPort,omitempty"` // optional; defaults to gamePort+1 for steam, 10011 for ts3
}

func (gs GameServer) resolvedQueryPort() int {
	if gs.QueryPort > 0 {
		return gs.QueryPort
	}
	if gs.Type == "ts3" {
		return 10011
	}
	return gs.GamePort + 1
}

func (gs GameServer) displayAddr() string {
	if gs.Type == "ts3" {
		return gs.Host
	}
	return fmt.Sprintf("%s:%d", gs.Host, gs.GamePort)
}

func (gs GameServer) displayLink() string {
	if gs.Type == "ts3" {
		return fmt.Sprintf("ts3server://%s", gs.Host)
	}
	return ""
}

func (gs GameServer) displayTemplateLink() htmltpl.URL {
	return htmltpl.URL(gs.displayLink())
}

func loadGameServers(path string) ([]GameServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var servers []GameServer
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// ServerStatusCache holds live status for all servers.
type ServerStatusCache struct {
	mu      sync.RWMutex
	servers []GameServer
	status  []*ServerInfo // ordered, matches servers slice
}

func NewServerStatusCache(servers []GameServer) *ServerStatusCache {
	status := make([]*ServerInfo, len(servers))
	for i, s := range servers {
		status[i] = &ServerInfo{Label: s.Label, Type: s.Type, Address: s.displayAddr(), Link: s.displayTemplateLink(), Online: false}
	}
	return &ServerStatusCache{servers: servers, status: status}
}

func (sc *ServerStatusCache) Refresh() {
	for i, srv := range sc.servers {
		var info *ServerInfo
		switch srv.Type {
		case "ts3":
			info = queryTS3(srv.Host, srv.GamePort, srv.resolvedQueryPort())
		default:
			info = queryA2SInfo(srv.Host, srv.resolvedQueryPort())
		}
		if info == nil {
			sc.mu.Lock()
			sc.status[i] = &ServerInfo{Label: srv.Label, Type: srv.Type, Address: srv.displayAddr(), Link: srv.displayTemplateLink(), Online: false}
			sc.mu.Unlock()
			continue
		}
		info.Label = srv.Label
		info.Type = srv.Type
		info.Address = srv.displayAddr()
		info.Link = srv.displayTemplateLink()
		info.Online = true
		sc.mu.Lock()
		sc.status[i] = info
		sc.mu.Unlock()
	}
}

func (sc *ServerStatusCache) GetAll() []*ServerInfo {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	out := make([]*ServerInfo, len(sc.status))
	for i, v := range sc.status {
		cp := *v
		out[i] = &cp
	}
	return out
}

// A2S_INFO query per Valve protocol.
// Request: \xFF\xFF\xFF\xFF\x54Source Engine Query\x00
// May receive challenge response \xFF\xFF\xFF\xFF\x41 + 4 bytes, then re-query with challenge appended.
func queryA2SInfo(host string, port int) *ServerInfo {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("udp", addr, 3*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// A2S_INFO request packet.
	payload := []byte("\xFF\xFF\xFF\xFF\x54Source Engine Query\x00")

	if _, err := conn.Write(payload); err != nil {
		return nil
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil || n < 5 {
		return nil
	}

	data := buf[:n]

	// Check for challenge response.
	if data[4] == 0x41 && n >= 9 {
		// Resend with challenge appended.
		challenge := data[5:9]
		payload = append([]byte("\xFF\xFF\xFF\xFF\x54Source Engine Query\x00"), challenge...)
		if _, err := conn.Write(payload); err != nil {
			return nil
		}
		n, err = conn.Read(buf)
		if err != nil || n < 5 {
			return nil
		}
		data = buf[:n]
	}

	// Response header: \xFF\xFF\xFF\xFF\x49
	if data[4] != 0x49 {
		return nil
	}

	return parseA2SInfo(data[5:])
}

func parseA2SInfo(data []byte) *ServerInfo {
	r := bytes.NewReader(data)

	// Protocol version (1 byte).
	if _, err := r.ReadByte(); err != nil {
		return nil
	}

	// Server name (skip — we use our own label).
	if _, err := readCString(r); err != nil {
		return nil
	}
	mapName, err := readCString(r)
	if err != nil {
		return nil
	}
	// Folder.
	if _, err := readCString(r); err != nil {
		return nil
	}
	// Game name.
	if _, err := readCString(r); err != nil {
		return nil
	}

	// Steam AppID (uint16 LE).
	var appID uint16
	if err := binary.Read(r, binary.LittleEndian, &appID); err != nil {
		return nil
	}

	// Players (byte), MaxPlayers (byte).
	players, err := r.ReadByte()
	if err != nil {
		return nil
	}
	maxPlayers, err := r.ReadByte()
	if err != nil {
		return nil
	}

	return &ServerInfo{
		Map:        mapName,
		Players:    int(players),
		MaxPlayers: int(maxPlayers),
	}
}

func readCString(r *bytes.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == 0 {
			return string(buf), nil
		}
		buf = append(buf, b)
	}
}

// queryTS3 queries a TeamSpeak 3 server via the ServerQuery protocol (TCP).
// Connects to queryPort, selects the virtual server by voicePort, runs serverinfo.
func queryTS3(host string, voicePort, queryPort int) *ServerInfo {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", queryPort))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)

	// Select virtual server by voice port.
	if _, err := runTS3Command(reader, conn, fmt.Sprintf("use port=%d", voicePort)); err != nil {
		return nil
	}

	// Request server info.
	line, err := runTS3Command(reader, conn, "serverinfo")
	if err != nil {
		if ts3IsPermissionDenied(err) {
			// Query account can reach/select the virtual server but lacks serverinfo permission.
			// Treat it as online with limited details.
			fmt.Fprintf(conn, "quit\n")
			return &ServerInfo{}
		}
		return nil
	}

	// Send quit (best effort).
	fmt.Fprintf(conn, "quit\n")

	return parseTS3ServerInfo(line)
}

func runTS3Command(reader *bufio.Reader, conn net.Conn, cmd string) (string, error) {
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", err
	}

	var dataLine string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "error ") {
			if !ts3ErrorOK(line) {
				return "", fmt.Errorf("teamspeak command failed: %s", line)
			}
			return dataLine, nil
		}

		// Ignore async event notifications and banner lines.
		if strings.HasPrefix(line, "notify") || line == "TS3" || strings.HasPrefix(line, "Welcome to the TeamSpeak") {
			continue
		}

		dataLine = line
	}
}

func ts3ErrorOK(line string) bool {
	for _, pair := range strings.Fields(line) {
		k, v, ok := strings.Cut(pair, "=")
		if ok && k == "id" {
			return v == "0"
		}
	}
	return false
}

func ts3IsPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "error id=2568")
}

func parseTS3ServerInfo(line string) *ServerInfo {
	fields := make(map[string]string)
	for _, pair := range strings.Fields(line) {
		k, v, _ := strings.Cut(pair, "=")
		// TS3 escapes: \s = space, \/ = /, \p = pipe, \\ = backslash.
		v = strings.ReplaceAll(v, "\\s", " ")
		v = strings.ReplaceAll(v, "\\/", "/")
		v = strings.ReplaceAll(v, "\\p", "|")
		v = strings.ReplaceAll(v, "\\\\", "\\")
		fields[k] = v
	}

	players := 0
	maxPlayers := 0
	fmt.Sscanf(fields["virtualserver_clientsonline"], "%d", &players)
	fmt.Sscanf(fields["virtualserver_maxclients"], "%d", &maxPlayers)

	// Subtract query clients from the count.
	var queryClients int
	fmt.Sscanf(fields["virtualserver_queryclientsonline"], "%d", &queryClients)
	players -= queryClients
	if players < 0 {
		players = 0
	}

	return &ServerInfo{
		Map:        fields["virtualserver_name"],
		Players:    players,
		MaxPlayers: maxPlayers,
	}
}

const (
	ghOwner   = "skua-international"
	ghRepo    = "certifications"
	ghAPIBase = "https://api.github.com"

	ytChannelURL = "https://www.youtube.com/@SkuaIntl.PublicRelations"
)

var (
	templates *htmltpl.Template
	ghBranch  string
)

func main() {
	addr := getEnv("ADDR", ":3000")
	ghBranch = getEnv("GITHUB_BRANCH", "main")
	presetsDir := getEnv("PRESETS_DIR", "./presets")
	allowlist := NewCertAllowlist(getEnv("CERTS_ALLOWLIST", "./certs.allow"))

	var err error
	templates, err = htmltpl.ParseFS(skuasite.TemplateFS, "templates/*.html")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	cache := &DocsCache{docs: make(map[string]*DocEntry)}
	ytCache := &YouTubeCache{}
	gameServers, err := loadGameServers(getEnv("SERVERS_FILE", "./servers.json"))
	if err != nil {
		log.Printf("warning: failed to load servers.json: %v", err)
	}
	srvCache := NewServerStatusCache(gameServers)

	// Refresh caches in background so server starts instantly.
	go func() {
		if err := cache.Refresh(); err != nil {
			log.Printf("warning: initial docs fetch failed: %v", err)
		}
	}()
	go func() {
		if err := ytCache.Refresh(); err != nil {
			log.Printf("warning: initial youtube fetch failed: %v", err)
		}
	}()
	go srvCache.Refresh()

	// Periodic refresh: docs/youtube every 30m, servers every 60s.
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := cache.Refresh(); err != nil {
				log.Printf("periodic docs refresh failed: %v", err)
			}
			if err := ytCache.Refresh(); err != nil {
				log.Printf("periodic youtube refresh failed: %v", err)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			srvCache.Refresh()
		}
	}()

	mux := http.NewServeMux()

	// Static files
	staticSub, err := fs.Sub(skuasite.StaticFS, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Home
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			render404(w)
			return
		}

		ytCache.mu.RLock()
		video := ytCache.video
		ytCache.mu.RUnlock()

		render(w, r, "home.html", map[string]any{
			"Active":      "home",
			"Title":       "",
			"Path":        "/",
			"LatestVideo": video,
			"Presets":     listPresets(presetsDir),
			"Servers":     srvCache.GetAll(),
		})
	})

	// Presets download (public)
	mux.HandleFunc("/presets/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/presets/")
		filename = strings.TrimSuffix(filename, "/")

		if filename == "" || strings.Contains(filename, "..") || strings.Contains(filename, "/") {
			render404(w)
			return
		}

		if !strings.HasSuffix(strings.ToLower(filename), ".html") {
			render404(w)
			return
		}

		path := filepath.Join(presetsDir, filename)
		if _, err := os.Stat(path); err != nil {
			render404(w)
			return
		}

		w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
		http.ServeFile(w, r, path)
	})

	// Presets API — CRUD
	// GET /api/presets        — list all (public)
	// POST /api/presets/{name} — create (mTLS)
	// PUT /api/presets/{name}  — update (mTLS)
	// DELETE /api/presets/{name} — delete (mTLS)
	mux.HandleFunc("/api/presets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(listPresets(presetsDir))
	})

	mux.HandleFunc("/api/presets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/presets/")
		name = strings.TrimSuffix(name, "/")

		if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
			render404(w)
			return
		}

		filename := name
		if !strings.HasSuffix(strings.ToLower(filename), ".html") {
			filename += ".html"
		}
		path := filepath.Join(presetsDir, filename)

		// Read is public
		if r.Method == http.MethodGet {
			if _, err := os.Stat(path); err != nil {
				render404(w)
				return
			}
			http.ServeFile(w, r, path)
			return
		}

		// CUD requires mTLS
		fp := r.Header.Get("X-Client-Cert-Fingerprint")
		if !allowlist.IsAllowed(fp) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		switch r.Method {
		case http.MethodPost:
			if _, err := os.Stat(path); err == nil {
				http.Error(w, "preset already exists", http.StatusConflict)
				return
			}
			if err := writePreset(path, r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, "created %s\n", filename)

		case http.MethodPut:
			if _, err := os.Stat(path); err != nil {
				render404(w)
				return
			}
			if err := writePreset(path, r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, "updated %s\n", filename)

		case http.MethodDelete:
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					render404(w)
				} else {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
				return
			}
			fmt.Fprintf(w, "deleted %s\n", filename)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Docs index
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		cache.mu.RLock()
		ordered := cache.ordered
		cache.mu.RUnlock()

		if len(ordered) > 0 {
			http.Redirect(w, r, "/docs/"+ordered[0].Slug, http.StatusTemporaryRedirect)
			return
		}

		render(w, r, "docs.html", map[string]any{
			"Active":  "docs",
			"Title":   "Docs",
			"Path":    "/docs",
			"Sidebar": []*DocEntry{},
			"Doc":     nil,
		})
	})

	// Docs single
	mux.HandleFunc("/docs/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/docs/")
		slug = strings.TrimSuffix(slug, "/")

		if slug == "" {
			http.Redirect(w, r, "/docs", http.StatusTemporaryRedirect)
			return
		}

		if strings.Contains(slug, "..") || strings.Contains(slug, "/") {
			render404(w)
			return
		}

		cache.mu.RLock()
		doc, ok := cache.docs[slug]
		ordered := cache.ordered
		cache.mu.RUnlock()

		if !ok {
			render404(w)
			return
		}

		render(w, r, "docs.html", map[string]any{
			"Active":      "docs",
			"Title":       doc.Title,
			"Description": doc.Title + " — Skua International certification documentation.",
			"Path":        "/docs/" + slug,
			"Sidebar":     ordered,
			"Doc":         doc,
			"ActiveSlug":  slug,
		})
	})

	// Refresh (POST, GitHub Action)
	mux.HandleFunc("/api/docs/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if secret := os.Getenv("REFRESH_SECRET"); secret != "" {
			if r.Header.Get("Authorization") != "Bearer "+secret {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if err := cache.Refresh(); err != nil {
			http.Error(w, fmt.Sprintf("refresh failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})

	// Server status API (JSON)
	mux.HandleFunc("/api/servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=30")
		json.NewEncoder(w).Encode(srvCache.GetAll())
	})

	// Health
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write([]byte("ok\n"))
	})

	// favicon.ico
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/img/logo.png", http.StatusMovedPermanently)
	})

	// robots.txt
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nAllow: /\n\nSitemap: https://skua.international/sitemap.xml\n")
	})

	// sitemap.xml
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		cache.mu.RLock()
		ordered := cache.ordered
		cache.mu.RUnlock()

		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`)
		fmt.Fprint(w, `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		fmt.Fprint(w, `<url><loc>https://skua.international/</loc><priority>1.0</priority></url>`)
		for _, doc := range ordered {
			fmt.Fprintf(w, `<url><loc>https://skua.international/docs/%s</loc><priority>0.7</priority></url>`, doc.Slug)
		}
		fmt.Fprint(w, `</urlset>`)
	})

	log.Printf("skua-site listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf strings.Builder
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body := buf.String()
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("ETag", etag)

	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	io.WriteString(w, body)
}

func render404(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	templates.ExecuteTemplate(w, "404.html", map[string]any{
		"Active": "",
		"Title":  "Not Found",
		"Path":   "",
	})
}

func (c *DocsCache) Refresh() error {
	client := &http.Client{Timeout: 15 * time.Second}
	token := os.Getenv("GITHUB_TOKEN")

	treeURL := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s", ghAPIBase, ghOwner, ghRepo, ghBranch)
	req, err := http.NewRequest("GET", treeURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "skua-site/1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	c.mu.RLock()
	etag := c.etag
	c.mu.RUnlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("tree fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		log.Println("docs: not modified")
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tree fetch: status %d: %s", resp.StatusCode, string(body))
	}

	newEtag := resp.Header.Get("ETag")

	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return fmt.Errorf("tree decode: %w", err)
	}

	var mdFiles []string
	for _, entry := range tree.Tree {
		if entry.Type != "blob" || !strings.HasSuffix(entry.Path, ".md") {
			continue
		}
		if strings.Contains(entry.Path, "/") {
			continue
		}
		if strings.EqualFold(entry.Path, "readme.md") {
			continue
		}
		mdFiles = append(mdFiles, entry.Path)
	}

	sort.Strings(mdFiles)

	newDocs := make(map[string]*DocEntry, len(mdFiles))
	newOrdered := make([]*DocEntry, 0, len(mdFiles))

	for _, path := range mdFiles {
		contentsURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", ghAPIBase, ghOwner, ghRepo, path, ghBranch)
		rawReq, _ := http.NewRequest("GET", contentsURL, nil)
		rawReq.Header.Set("Accept", "application/vnd.github.raw+json")
		rawReq.Header.Set("User-Agent", "skua-site/1.0")
		if token != "" {
			rawReq.Header.Set("Authorization", "Bearer "+token)
		}

		rawResp, err := client.Do(rawReq)
		if err != nil {
			log.Printf("docs: failed to fetch %s: %v", path, err)
			continue
		}

		raw, err := io.ReadAll(rawResp.Body)
		rawResp.Body.Close()
		if err != nil || rawResp.StatusCode != http.StatusOK {
			log.Printf("docs: %s error (status %d)", path, rawResp.StatusCode)
			continue
		}

		slug := strings.TrimSuffix(path, ".md")
		rendered := renderMarkdown(raw)

		entry := &DocEntry{
			Slug:    slug,
			Title:   formatTitle(slug),
			Content: htmltpl.HTML(rendered),
		}
		newDocs[slug] = entry
		newOrdered = append(newOrdered, entry)
	}

	c.mu.Lock()
	c.docs = newDocs
	c.ordered = newOrdered
	c.etag = newEtag
	c.mu.Unlock()

	log.Printf("docs: refreshed %d certifications from github", len(newDocs))
	return nil
}

var ytVideoIDRe = regexp.MustCompile(`"videoId":"([a-zA-Z0-9_-]{11})"`)

func (yc *YouTubeCache) Refresh() error {
	client := &http.Client{Timeout: 15 * time.Second}

	// First try the Atom RSS feed (requires channel ID).
	// Resolve channel ID from the channel page.
	req, err := http.NewRequest("GET", ytChannelURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "skua-site/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("youtube fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("youtube fetch: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("youtube read: %w", err)
	}

	page := string(body)

	// Extract channel ID for RSS feed.
	var channelID string
	if idx := strings.Index(page, `"externalId":"`); idx != -1 {
		start := idx + len(`"externalId":"`)
		if end := strings.Index(page[start:], `"`); end != -1 {
			channelID = page[start : start+end]
		}
	}

	// Try RSS feed first (more reliable ordering).
	if channelID != "" {
		video, err := yc.fetchFromRSS(client, channelID)
		if err == nil && video != nil {
			yc.mu.Lock()
			yc.video = video
			yc.mu.Unlock()
			log.Printf("youtube: refreshed latest video from RSS: %s", video.ID)
			return nil
		}
		log.Printf("youtube: RSS feed failed, falling back to page scrape: %v", err)
	}

	// Fallback: scrape video ID from channel page.
	match := ytVideoIDRe.FindStringSubmatch(page)
	if match == nil {
		return fmt.Errorf("youtube: no video found on channel page")
	}

	videoID := match[1]

	// Extract title from page for this video (best effort).
	title := "Skua International"
	titleKey := `"title":{"runs":[{"text":"`
	if idx := strings.Index(page, titleKey); idx != -1 {
		start := idx + len(titleKey)
		if end := strings.Index(page[start:], `"`); end != -1 {
			title = page[start : start+end]
		}
	}

	yc.mu.Lock()
	yc.video = &YouTubeVideo{ID: videoID, Title: title}
	yc.mu.Unlock()

	log.Printf("youtube: refreshed latest video from page scrape: %s", videoID)
	return nil
}

// atomFeed represents a minimal YouTube Atom feed.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	VideoID string `xml:"videoId"`
	Title   string `xml:"title"`
}

func (yc *YouTubeCache) fetchFromRSS(client *http.Client, channelID string) (*YouTubeVideo, error) {
	feedURL := fmt.Sprintf("https://www.youtube.com/feeds/videos.xml?channel_id=%s", channelID)
	resp, err := client.Get(feedURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss status %d", resp.StatusCode)
	}

	var feed atomFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("rss decode: %w", err)
	}

	if len(feed.Entries) == 0 {
		return nil, fmt.Errorf("rss: no entries")
	}

	return &YouTubeVideo{
		ID:    feed.Entries[0].VideoID,
		Title: feed.Entries[0].Title,
	}, nil
}

func renderMarkdown(src []byte) []byte {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs | parser.NoEmptyLineBeforeBlock
	p := parser.NewWithExtensions(extensions)
	doc := p.Parse(src)

	htmlFlags := html.CommonFlags | html.HrefTargetBlank
	opts := html.RendererOptions{Flags: htmlFlags}
	renderer := html.NewRenderer(opts)

	return markdown.Render(doc, renderer)
}

func formatTitle(slug string) string {
	s := strings.ReplaceAll(slug, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func writePreset(path string, body io.ReadCloser) error {
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 2<<20)) // 2 MiB max
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func listPresets(dir string) []Preset {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var presets []Preset
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".html") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		presets = append(presets, Preset{
			Name:     name,
			Filename: e.Name(),
		})
	}
	sort.Slice(presets, func(i, j int) bool {
		return presets[i].Name < presets[j].Name
	})
	return presets
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
