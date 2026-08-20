package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrjoiny/torboxarr/internal/api"
	"github.com/mrjoiny/torboxarr/internal/auth"
	"github.com/mrjoiny/torboxarr/internal/config"
	"github.com/mrjoiny/torboxarr/internal/files"
	"github.com/mrjoiny/torboxarr/internal/store"
)

type addServerOptions struct {
	loginStatus      int
	loginBody        string
	categoriesStatus int
	categoriesBody   string
	addStatus        int
	addBody          string
	addDelay         time.Duration
}

type addRequestRecord struct {
	contentType string
	form        map[string]string
	fileName    string
	fileContent string
}

type addServer struct {
	mu    sync.Mutex
	paths []string
	adds  []addRequestRecord
}

func (s *addServer) recordPath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, path)
}

func (s *addServer) recordAdd(add addRequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adds = append(s.adds, add)
}

func (s *addServer) snapshot() ([]string, []addRequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths), slices.Clone(s.adds)
}

func defaultAddServerOptions() addServerOptions {
	return addServerOptions{
		loginStatus:      http.StatusOK,
		loginBody:        "Ok.",
		categoriesStatus: http.StatusOK,
		categoriesBody:   `{"sonarr":{"name":"sonarr","savePath":"/data/completed/sonarr"},"radarr":{"name":"radarr","savePath":"/data/completed/radarr"}}`,
		addStatus:        http.StatusOK,
		addBody:          "Ok.",
	}
}

type sabAddServerOptions struct {
	configStatus int
	configBody   string
	addStatus    int
	addBody      string
	addDelay     time.Duration
	apiKey       string
}

type sabAddRequestRecord struct {
	method      string
	query       url.Values
	contentType string
	form        map[string]string
	fileName    string
	fileContent []byte
}

type sabAddServer struct {
	mu      sync.Mutex
	paths   []string
	queries []url.Values
	adds    []sabAddRequestRecord
}

func (s *sabAddServer) record(path string, query url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, path)
	s.queries = append(s.queries, cloneURLValues(query))
}

func (s *sabAddServer) recordAdd(add sabAddRequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adds = append(s.adds, add)
}

func (s *sabAddServer) snapshot() ([]string, []url.Values, []sabAddRequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queries := make([]url.Values, len(s.queries))
	for i, query := range s.queries {
		queries[i] = cloneURLValues(query)
	}
	return slices.Clone(s.paths), queries, slices.Clone(s.adds)
}

func cloneURLValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, items := range values {
		clone[key] = slices.Clone(items)
	}
	return clone
}

func defaultSABAddServerOptions() sabAddServerOptions {
	return sabAddServerOptions{
		configStatus: http.StatusOK,
		configBody:   `{"config":{"categories":[{"name":"tv"},{"name":"radarr"}]}}`,
		addStatus:    http.StatusOK,
		addBody:      `{"status":true,"nzo_ids":["TBOX-nzo-123"]}`,
		apiKey:       "sab-secret",
	}
}

func newSABAddServer(t *testing.T, opts sabAddServerOptions) (*httptest.Server, *sabAddServer) {
	t.Helper()
	server := &sabAddServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.record(r.URL.Path, r.URL.Query())
		if r.URL.Path != "/sabnzbd/api" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("mode") {
		case "get_config":
			if r.URL.Query().Get("apikey") != opts.apiKey {
				http.Error(w, "API Key Incorrect", http.StatusForbidden)
				return
			}
			w.WriteHeader(opts.configStatus)
			_, _ = w.Write([]byte(opts.configBody))
		case "addfile":
			if r.URL.Query().Get("apikey") != opts.apiKey {
				http.Error(w, "API Key Incorrect", http.StatusForbidden)
				return
			}
			if opts.addDelay > 0 {
				select {
				case <-time.After(opts.addDelay):
				case <-r.Context().Done():
				}
			}
			record := sabAddRequestRecord{
				method:      r.Method,
				query:       r.URL.Query(),
				contentType: r.Header.Get("Content-Type"),
				form:        map[string]string{},
			}
			if err := r.ParseMultipartForm(2 << 20); err == nil && r.MultipartForm != nil {
				for key, values := range r.MultipartForm.Value {
					if len(values) > 0 {
						record.form[key] = values[0]
					}
				}
				for _, headers := range r.MultipartForm.File {
					for _, header := range headers {
						file, err := header.Open()
						if err != nil {
							continue
						}
						record.fileName = header.Filename
						record.fileContent, _ = io.ReadAll(file)
						file.Close()
					}
				}
			}
			server.recordAdd(record)
			w.WriteHeader(opts.addStatus)
			_, _ = w.Write([]byte(opts.addBody))
		default:
			http.Error(w, "unsupported mode", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, server
}

func newAddServer(t *testing.T, opts addServerOptions) (*httptest.Server, *addServer) {
	t.Helper()
	server := &addServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.recordPath(r.URL.Path)
		switch r.URL.Path {
		case "/api/v2/auth/login":
			w.WriteHeader(opts.loginStatus)
			_, _ = w.Write([]byte(opts.loginBody))
		case "/api/v2/torrents/categories":
			w.WriteHeader(opts.categoriesStatus)
			_, _ = w.Write([]byte(opts.categoriesBody))
		case "/api/v2/torrents/add":
			if opts.addDelay > 0 {
				select {
				case <-time.After(opts.addDelay):
				case <-r.Context().Done():
				}
			}
			record := addRequestRecord{
				contentType: r.Header.Get("Content-Type"),
				form:        map[string]string{},
			}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err == nil {
					mr := multipart.NewReader(r.Body, params["boundary"])
					for {
						part, err := mr.NextPart()
						if err != nil {
							break
						}
						if part.FormName() == "" {
							continue
						}
						content, _ := io.ReadAll(part)
						if part.FileName() != "" {
							record.fileName = part.FileName()
							record.fileContent = string(content)
						} else {
							record.form[part.FormName()] = string(content)
						}
					}
				}
			} else {
				body, _ := io.ReadAll(r.Body)
				if values, err := url.ParseQuery(string(body)); err == nil {
					for key, items := range values {
						if len(items) > 0 {
							record.form[key] = items[0]
						}
					}
				}
			}
			server.recordAdd(record)
			w.WriteHeader(opts.addStatus)
			_, _ = w.Write([]byte(opts.addBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, server
}

func newAddTestClient(t *testing.T, baseURL string, timeout time.Duration) *addClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &addClient{
		http:     &http.Client{Jar: jar, Timeout: timeout},
		baseURL:  baseURL,
		username: "admin",
		password: "secret",
	}
}

func withAddPassword(t *testing.T) {
	t.Helper()
	t.Setenv("TORBOXARR_QBIT_PASSWORD", "secret")
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.torrent")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validTorrentBytes() []byte {
	return []byte("d8:announce4:test4:infod6:lengthi123e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:" + strings.Repeat("x", 20) + "ee")
}

func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestRunAdd_RequiresCategory(t *testing.T) {
	err := runAdd(context.Background(), "", "", "")
	if err == nil || !strings.Contains(err.Error(), "--category") {
		t.Fatalf("expected --category required error, got %v", err)
	}
}

func TestRunAdd_RequiresMagnetOrTorrent(t *testing.T) {
	err := runAdd(context.Background(), "sonarr", "", "")
	if err == nil || !strings.Contains(err.Error(), "exactly one of --magnet, --torrent, or --nzb") {
		t.Fatalf("expected magnet/torrent required error, got %v", err)
	}
}

func TestRunAdd_MutuallyExclusive(t *testing.T) {
	err := runAdd(context.Background(), "sonarr", "magnet:?xt=urn:btih:abc", "/tmp/x.torrent")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

func TestRunAdd_RejectsInvalidMagnetWithoutEchoingIt(t *testing.T) {
	for _, magnet := range []string{
		"https://example.com/movie.torrent",
		"magnet:",
		"magnet:?dn=movie",
		"magnet:?xt=urn:sha1:01234567890123456789",
	} {
		err := runAdd(context.Background(), "sonarr", magnet, "")
		if err == nil || !strings.Contains(err.Error(), "--magnet") {
			t.Fatalf("magnet %q: expected --magnet validation error, got %v", magnet, err)
		}
		if strings.Contains(err.Error(), magnet) {
			t.Fatalf("magnet %q: error must not echo the magnet value, got %v", magnet, err)
		}
	}
}

func TestRunAdd_MissingTorrentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.torrent")
	err := runAdd(context.Background(), "sonarr", "", path)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing file error, got %v", err)
	}
}

func TestRunAdd_TorrentPathIsDirectory(t *testing.T) {
	err := runAdd(context.Background(), "sonarr", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

func TestRunAdd_InvalidTorrentFile(t *testing.T) {
	path := writeTempFile(t, "this is not a bencoded torrent")
	err := runAdd(context.Background(), "sonarr", "", path)
	if err == nil || !strings.Contains(err.Error(), "not a valid .torrent file") {
		t.Fatalf("expected invalid torrent error, got %v", err)
	}
}

func TestRunAdd_MissingPassword(t *testing.T) {
	t.Setenv("TORBOXARR_QBIT_PASSWORD", "")
	err := runAdd(context.Background(), "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "TORBOXARR_QBIT_PASSWORD") {
		t.Fatalf("expected password error, got %v", err)
	}
}

func TestRunAddMagnet_Success(t *testing.T) {
	srv, server := newAddServer(t, defaultAddServerOptions())
	withAddPassword(t)

	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Movie"
	output := captureOutput(t, func() {
		if err := runAddAt(context.Background(), srv.URL, "radarr", magnet, ""); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})

	if !strings.Contains(output, "submitted magnet") || !strings.Contains(output, "radarr") {
		t.Errorf("confirmation = %q, want submitted magnet for category radarr", output)
	}
	paths, adds := server.snapshot()
	wantPaths := []string{"/api/v2/auth/login", "/api/v2/torrents/categories", "/api/v2/torrents/add"}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("request order = %v, want %v", paths, wantPaths)
	}
	if len(adds) != 1 {
		t.Fatalf("expected one add request, got %d", len(adds))
	}
	add := adds[0]
	if add.form["urls"] != magnet {
		t.Errorf("urls = %q, want %q", add.form["urls"], magnet)
	}
	if add.form["category"] != "radarr" {
		t.Errorf("category = %q, want %q", add.form["category"], "radarr")
	}
	if _, ok := add.form["rename"]; ok {
		t.Error("manual add must not send a rename field")
	}
	if add.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q, want urlencoded", add.contentType)
	}
}

func TestRunAddTorrentFile_Success(t *testing.T) {
	srv, server := newAddServer(t, defaultAddServerOptions())
	withAddPassword(t)

	path := writeTempFile(t, string(validTorrentBytes()))
	filename := filepath.Base(path)
	output := captureOutput(t, func() {
		if err := runAddAt(context.Background(), srv.URL, "sonarr", "", path); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})

	if !strings.Contains(output, "submitted torrent") || !strings.Contains(output, filename) || !strings.Contains(output, "sonarr") {
		t.Errorf("confirmation = %q, want submitted torrent %s for category sonarr", output, filename)
	}
	_, adds := server.snapshot()
	if len(adds) != 1 {
		t.Fatalf("expected one add request, got %d", len(adds))
	}
	add := adds[0]
	if add.form["category"] != "sonarr" {
		t.Errorf("category = %q, want %q", add.form["category"], "sonarr")
	}
	if add.fileName != filename {
		t.Errorf("file name = %q, want %q", add.fileName, filename)
	}
	if add.fileContent != string(validTorrentBytes()) {
		t.Error("file content was not uploaded intact")
	}
	if !strings.HasPrefix(add.contentType, "multipart/form-data") {
		t.Errorf("content type = %q, want multipart", add.contentType)
	}
}

func TestRunAddFileConfirmationsRemainOneLine(t *testing.T) {
	torrentServer, _ := newAddServer(t, defaultAddServerOptions())
	withAddPassword(t)
	torrentPath := filepath.Join(t.TempDir(), "sample\nname.torrent")
	if err := os.WriteFile(torrentPath, validTorrentBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	torrentOutput := captureOutput(t, func() {
		if err := runAddAt(context.Background(), torrentServer.URL, "sonarr", "", torrentPath); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Count(torrentOutput, "\n") != 1 || !strings.Contains(torrentOutput, `sample\nname.torrent`) {
		t.Fatalf("torrent confirmation is not one escaped line: %q", torrentOutput)
	}

	sabServer, _ := newSABAddServer(t, defaultSABAddServerOptions())
	withSABAPIKey(t)
	nzbPath := filepath.Join(t.TempDir(), "sample\nname.nzb")
	if err := os.WriteFile(nzbPath, []byte(`<nzb></nzb>`), 0o644); err != nil {
		t.Fatal(err)
	}
	nzbOutput := captureOutput(t, func() {
		if err := runAddInputAt(context.Background(), sabServer.URL, addInput{Category: "tv", NZBPath: nzbPath}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Count(nzbOutput, "\n") != 1 || !strings.Contains(nzbOutput, `sample\nname.nzb`) {
		t.Fatalf("NZB confirmation is not one escaped line: %q", nzbOutput)
	}
}

func TestRunAdd_CategoryUnknown(t *testing.T) {
	srv, server := newAddServer(t, defaultAddServerOptions())
	withAddPassword(t)

	err := runAddAt(context.Background(), srv.URL, "missing-cat", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "missing-cat") {
		t.Fatalf("expected unknown category error, got %v", err)
	}
	paths, adds := server.snapshot()
	if len(adds) != 0 {
		t.Errorf("expected no add request for unknown category, got %d", len(adds))
	}
	wantPaths := []string{"/api/v2/auth/login", "/api/v2/torrents/categories"}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("request order = %v, want %v", paths, wantPaths)
	}
}

func TestRunAdd_LoginFails(t *testing.T) {
	opts := defaultAddServerOptions()
	opts.loginStatus = http.StatusUnauthorized
	opts.loginBody = "Fails."
	srv, _ := newAddServer(t, opts)
	withAddPassword(t)

	err := runAddAt(context.Background(), srv.URL, "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected login error, got %v", err)
	}
}

func TestRunAdd_CategoryCheckFails(t *testing.T) {
	opts := defaultAddServerOptions()
	opts.categoriesStatus = http.StatusInternalServerError
	srv, _ := newAddServer(t, opts)
	withAddPassword(t)

	err := runAddAt(context.Background(), srv.URL, "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "cannot check categories") {
		t.Fatalf("expected category check error, got %v", err)
	}
}

func TestRunAdd_CategoryListUnreadable(t *testing.T) {
	opts := defaultAddServerOptions()
	opts.categoriesBody = "not json"
	srv, _ := newAddServer(t, opts)
	withAddPassword(t)

	err := runAddAt(context.Background(), srv.URL, "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "cannot check categories") {
		t.Fatalf("expected category check error, got %v", err)
	}
}

func TestRunAdd_ServerUnreachable(t *testing.T) {
	withAddPassword(t)

	err := runAddAt(context.Background(), "http://127.0.0.1:1", "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "cannot reach the TorBoxarr server") {
		t.Fatalf("expected unreachable server error, got %v", err)
	}
}

func TestRunAdd_ServerRejected(t *testing.T) {
	opts := defaultAddServerOptions()
	opts.addBody = "Fails. Add rejected."
	srv, _ := newAddServer(t, opts)
	withAddPassword(t)

	err := runAddAt(context.Background(), srv.URL, "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got %v", err)
	}
}

func TestRunAdd_ServerErrorStatus(t *testing.T) {
	opts := defaultAddServerOptions()
	opts.addStatus = http.StatusInternalServerError
	srv, _ := newAddServer(t, opts)
	withAddPassword(t)

	err := runAddAt(context.Background(), srv.URL, "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got %v", err)
	}
}

func TestRunAdd_TimeoutStatusUnknown(t *testing.T) {
	opts := defaultAddServerOptions()
	opts.addDelay = 200 * time.Millisecond
	srv, _ := newAddServer(t, opts)
	client := newAddTestClient(t, srv.URL, 20*time.Millisecond)

	err := client.run(context.Background(), "sonarr", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "")
	if err == nil || !strings.Contains(err.Error(), "submission status is unknown") {
		t.Fatalf("expected unknown status error, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "inspect the torboxarr queue") {
		t.Fatalf("expected queue inspection advice, got %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRunAdd_CanceledStatusUnknown(t *testing.T) {
	client := &addClient{
		http: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled
		})},
		baseURL: "http://127.0.0.1:8085",
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, client.baseURL+"/api/v2/torrents/add", nil)
	if err != nil {
		t.Fatal(err)
	}

	err = client.submit(context.Background(), req, "submitted magnet")
	if err == nil || !strings.Contains(err.Error(), "submission was not sent") {
		t.Fatalf("expected definitive pre-send cancellation error, got %v", err)
	}
	if strings.Contains(err.Error(), "status is unknown") || strings.Contains(strings.ToLower(err.Error()), "inspect the torboxarr queue") {
		t.Fatalf("pre-send cancellation must not be uncertain, got %v", err)
	}
}

func TestValidateMagnet(t *testing.T) {
	valid := []string{
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"magnet:?dn=Movie&xt=urn:btih:abc&tr=https://tracker.example.com/announce",
		"MAGNET:?XT=URN:BTIH:ABC",
		"magnet://tracker.example.com?xt=urn:btih:abc",
	}
	for _, magnet := range valid {
		if err := validateMagnet(magnet); err != nil {
			t.Errorf("magnet %q: unexpected error %v", magnet, err)
		}
	}

	invalid := []string{
		"",
		" ",
		"https://example.com/movie.torrent",
		"magnet:",
		"magnet:?dn=movie",
		"magnet:?xt=urn:sha1:01234567890123456789",
		"magnet:?xt=urn:btih:",
	}
	for _, magnet := range invalid {
		if err := validateMagnet(magnet); err == nil {
			t.Errorf("magnet %q: expected error", magnet)
		}
	}
}

func TestIsValidTorrent(t *testing.T) {
	valid := []string{
		string(validTorrentBytes()),
		"d4:infodee",
	}
	for _, data := range valid {
		if !isValidTorrent([]byte(data)) {
			t.Errorf("data %q: expected valid torrent", data)
		}
	}

	invalid := []string{
		"",
		"garbage",
		"d8:announce4:teste",
		"d4:info4:teste",
		"d4:infoe",
		"d4:infoxx",
		"d4:infodee trailing",
		"d4:infod1:ai-0eee",
		"d4:infod1:ai03eee",
		"d4:infod1:ai+3eee",
		"d04:infodee",
		"d4:infofoo:bare",
	}
	for _, data := range invalid {
		if isValidTorrent([]byte(data)) {
			t.Errorf("data %q: expected invalid torrent", data)
		}
	}
}

func writeTempNZB(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.nzb")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func withSABAPIKey(t *testing.T) {
	t.Helper()
	t.Setenv("TORBOXARR_SAB_API_KEY", "sab-secret")
}

func TestRunAddCommand_RejectsMixedSources(t *testing.T) {
	args := []string{
		"--category", "tv",
		"--magnet", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"--nzb", "/tmp/sample.nzb",
	}
	err := runAddCommand(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mixed-source error, got %v", err)
	}
}

func TestRunAddCommand_RejectsPositionalArgumentsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"--category", "tv", "unexpected"},
		{"--category", "tv", "--magnet", "magnet:?xt=urn:btih:abc", "unexpected"},
	} {
		err := runAddCommand(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Fatalf("args %v: expected positional argument error, got %v", args, err)
		}
	}
}

func TestRunAddCommand_RejectsBlankSourceFlags(t *testing.T) {
	err := runAddCommand(context.Background(), []string{"--category", "tv", "--nzb", "   "})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected blank source error, got %v", err)
	}
}

func TestValidateNZBFile(t *testing.T) {
	valid := []string{
		`<?xml version="1.0"?><nzb></nzb>`,
		`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file/></nzb>`,
	}
	for _, content := range valid {
		path := writeTempNZB(t, content)
		if err := validateNZBFile(path); err != nil {
			t.Errorf("content %q: unexpected error %v", content, err)
		}
	}

	invalid := []string{
		"",
		"<rss></rss>",
		"<nzb>",
		"<nzb></nzb>trailing",
		"<nzb></nzb><nzb></nzb>",
		"prefix<nzb></nzb>",
	}
	for _, content := range invalid {
		path := writeTempNZB(t, content)
		if err := validateNZBFile(path); err == nil {
			t.Errorf("content %q: expected validation error", content)
		}
	}
}

func TestValidateNZBFile_RejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.nzb")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxNZBFileBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	err = validateNZBFile(path)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected oversized NZB error, got %v", err)
	}
}

func TestValidateTorrentFile_RejectsEmptyOversizedAndNonRegular(t *testing.T) {
	empty := writeTempFile(t, "")
	if err := validateTorrentFile(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty torrent error = %v", err)
	}

	oversized := filepath.Join(t.TempDir(), "large.torrent")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxTorrentFileBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateTorrentFile(oversized); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized torrent error = %v", err)
	}

	if err := validateTorrentFile(os.DevNull); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("non-regular torrent error = %v", err)
	}
}

func TestIsValidTorrent_RejectsDeeplyNestedInput(t *testing.T) {
	data := "d4:info" + strings.Repeat("l", 18) + strings.Repeat("e", 18) + "e"
	if isValidTorrent([]byte(data)) {
		t.Fatal("deeply nested torrent was accepted")
	}
}

func TestValidateNZBFile_RejectsMissingDirectoryAndNonRegular(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.nzb")
	if err := validateNZBFile(missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing file error = %v", err)
	}

	directory := t.TempDir()
	if err := validateNZBFile(directory); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory error = %v", err)
	}

	if err := validateNZBFile(os.DevNull); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("non-regular error = %v", err)
	}
}

func TestRunAddNZB_MissingAPIKeyBeforeNetwork(t *testing.T) {
	t.Setenv("TORBOXARR_SAB_API_KEY", "")
	srv, server := newSABAddServer(t, defaultSABAddServerOptions())
	path := writeTempNZB(t, `<nzb></nzb>`)

	err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path})
	if err == nil || !strings.Contains(err.Error(), "TORBOXARR_SAB_API_KEY") {
		t.Fatalf("expected API key error, got %v", err)
	}
	paths, _, _ := server.snapshot()
	if len(paths) != 0 {
		t.Fatalf("expected no network requests, got %v", paths)
	}
}

func TestRunAddNZB_Success(t *testing.T) {
	srv, server := newSABAddServer(t, defaultSABAddServerOptions())
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb xmlns="urn:test"><file/></nzb>`)

	output := captureOutput(t, func() {
		if err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path}); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})
	if strings.Contains(output, "sab-secret") || !strings.Contains(output, "submitted NZB file") || !strings.Contains(output, filepath.Base(path)) || !strings.Contains(output, "TBOX-nzo-123") {
		t.Fatalf("confirmation = %q, want non-secret NZB confirmation", output)
	}

	paths, queries, adds := server.snapshot()
	if !slices.Equal(paths, []string{"/sabnzbd/api", "/sabnzbd/api"}) {
		t.Errorf("request paths = %v, want category lookup then upload", paths)
	}
	if len(queries) != 2 || queries[0].Get("mode") != "get_config" || queries[0].Get("output") != "json" || queries[0].Get("apikey") != "sab-secret" {
		t.Errorf("category lookup query = %v, want authenticated get_config JSON request", queries)
	}
	if len(queries) != 2 || queries[1].Get("mode") != "addfile" || queries[1].Get("apikey") != "sab-secret" {
		t.Errorf("upload query = %v, want authenticated addfile request", queries)
	}
	if len(adds) != 1 {
		t.Fatalf("expected one add request, got %d", len(adds))
	}
	add := adds[0]
	if add.method != http.MethodPost || add.query.Get("mode") != "addfile" || add.query.Get("apikey") != "sab-secret" {
		t.Errorf("upload request = method %q query %v, want POST addfile with API key", add.method, add.query)
	}
	if add.form["mode"] != "addfile" || add.form["output"] != "json" || add.form["cat"] != "tv" {
		t.Errorf("upload fields = %v, want addfile/json/tv", add.form)
	}
	if add.fileName != filepath.Base(path) || string(add.fileContent) != `<nzb xmlns="urn:test"><file/></nzb>` {
		t.Errorf("upload file = %q %q, want base filename and intact content", add.fileName, add.fileContent)
	}
}

func TestSafeSABNZOID(t *testing.T) {
	for _, id := range []string{"TBOX-123", "nzo-abc"} {
		if !safeSABNZOID(id) {
			t.Errorf("safeSABNZOID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "has space", "has\nnewline", "unicode-\u00e9", strings.Repeat("x", 257)} {
		if safeSABNZOID(id) {
			t.Errorf("safeSABNZOID(%q) = true, want false", id)
		}
	}
}

func TestRunAddNZB_UnknownCategoryDoesNotUpload(t *testing.T) {
	srv, server := newSABAddServer(t, defaultSABAddServerOptions())
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb></nzb>`)

	err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "missing", NZBPath: path})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown category error, got %v", err)
	}
	_, _, adds := server.snapshot()
	if len(adds) != 0 {
		t.Fatalf("expected no upload for unknown category, got %d", len(adds))
	}
}

func TestRunAddNZB_RejectsUnacceptedResponse(t *testing.T) {
	opts := defaultSABAddServerOptions()
	opts.addBody = `{"status":false,"nzo_ids":[]}`
	srv, _ := newSABAddServer(t, opts)
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb></nzb>`)

	err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected SAB rejection, got %v", err)
	}
}

func TestRunAddNZB_RejectsMalformedMissingAndMultipleIDs(t *testing.T) {
	for _, response := range []string{
		"not json",
		`{"status":true,"nzo_ids":[]}`,
		`{"status":true,"nzo_ids":["one","two"]}`,
		`{"status":true,"nzo_ids":["bad\nvalue"]}`,
	} {
		opts := defaultSABAddServerOptions()
		opts.addBody = response
		srv, _ := newSABAddServer(t, opts)
		withSABAPIKey(t)
		path := writeTempNZB(t, `<nzb></nzb>`)

		err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path})
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Errorf("response %q: expected rejection, got %v", response, err)
		}
	}
}

func TestRunAddNZB_RejectsAuthenticationFailure(t *testing.T) {
	opts := defaultSABAddServerOptions()
	opts.configStatus = http.StatusForbidden
	srv, _ := newSABAddServer(t, opts)
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb></nzb>`)

	err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path})
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "sab-secret") {
		t.Fatalf("expected redacted SAB authentication failure, got %v", err)
	}
}

func TestRunAddNZB_RejectsServerError(t *testing.T) {
	opts := defaultSABAddServerOptions()
	opts.addStatus = http.StatusInternalServerError
	srv, _ := newSABAddServer(t, opts)
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb></nzb>`)

	err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path})
	if err == nil || !strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "status is unknown") {
		t.Fatalf("expected definitive SAB server rejection, got %v", err)
	}
}

func TestRunAddNZB_TimeoutIsUnknown(t *testing.T) {
	opts := defaultSABAddServerOptions()
	opts.addDelay = 200 * time.Millisecond
	srv, _ := newSABAddServer(t, opts)
	path := writeTempNZB(t, `<nzb></nzb>`)
	client := &sabAddClient{
		http:    &http.Client{Timeout: 20 * time.Millisecond},
		baseURL: srv.URL,
		apiKey:  "sab-secret",
	}

	err := client.run(context.Background(), "tv", path)
	if err == nil || !strings.Contains(err.Error(), "status is unknown") || !strings.Contains(strings.ToLower(err.Error()), "sab queue") {
		t.Fatalf("expected uncertain SAB submission, got %v", err)
	}
}

func newManualAddRouter(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	tmpDir := t.TempDir()
	layout := files.NewLayout(tmpDir, filepath.Join(tmpDir, "staging"), filepath.Join(tmpDir, "completed"), filepath.Join(tmpDir, "payloads"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Server.Address = ":0"
	cfg.Server.BaseURL = "http://localhost"
	cfg.Data.Root = tmpDir
	cfg.Data.Staging = filepath.Join(tmpDir, "staging")
	cfg.Data.Completed = filepath.Join(tmpDir, "completed")
	cfg.Data.Payloads = filepath.Join(tmpDir, "payloads")
	cfg.Auth.QBitUsername = "admin"
	cfg.Auth.QBitPassword = "password"
	cfg.Auth.SABAPIKey = "sab-secret"
	cfg.Auth.SABNZBKey = "sab-secret"
	cfg.Auth.SessionTTL = 24 * time.Hour
	cfg.Compatibility.QBitVersion = "5.0.0"
	cfg.Compatibility.QBitWebAPI = "2.11.3"
	cfg.Compatibility.SABVersion = "4.5.1"
	cfg.Compatibility.DefaultCategory = "torboxarr"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	qbitAuth := auth.NewQBitSessionManager(st, cfg.Auth.QBitUsername, cfg.Auth.QBitPassword, cfg.Auth.SessionTTL)
	sabAuth := auth.NewSABAuth(cfg.Auth.SABAPIKey, cfg.Auth.SABNZBKey)
	server := api.NewServer(&cfg, logger, st, layout, qbitAuth, sabAuth)
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	return httpServer, st
}

func assertQBitQueueCount(t *testing.T, baseURL string, want int) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	login := url.Values{"username": {"admin"}, "password": {"password"}}
	resp, err := client.Post(baseURL+"/api/v2/auth/login", "application/x-www-form-urlencoded", strings.NewReader(login.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("qBit login status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	resp, err = client.Get(baseURL + "/api/v2/torrents/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var torrents []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&torrents); err != nil {
		t.Fatal(err)
	}
	if len(torrents) != want {
		t.Fatalf("qBit queue contains %d jobs, want %d", len(torrents), want)
	}
}

func assertSABQueueCount(t *testing.T, baseURL string, want int) {
	t.Helper()
	query := url.Values{"mode": {"queue"}, "output": {"json"}, "apikey": {"sab-secret"}}
	resp, err := http.Get(baseURL + "/sabnzbd/api?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Queue struct {
			Slots []json.RawMessage `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Queue.Slots) != want {
		t.Fatalf("SAB queue contains %d jobs, want %d", len(result.Queue.Slots), want)
	}
}

func TestRunAddNZB_RealRouterPersistsPayload(t *testing.T) {
	srv, st := newManualAddRouter(t)
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb xmlns="urn:test"><file/></nzb>`)

	if err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path}); err != nil {
		t.Fatalf("expected real-router success, got %v", err)
	}

	jobs, err := st.ListVisibleClientJobs(context.Background(), store.ClientKindSAB, "tv", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.SourceType != store.SourceTypeNZB || job.ClientKind != store.ClientKindSAB || job.Category != "tv" || job.PayloadRef == nil {
		t.Fatalf("job = %+v, want SAB NZB with tv category and payload", job)
	}
	if job.Metadata.UploadedFilename != filepath.Base(path) || job.State != store.StateSubmitPending {
		t.Fatalf("job metadata/state = %+v/%s, want uploaded filename and submit_pending", job.Metadata, job.State)
	}
	payload, err := os.ReadFile(*job.PayloadRef)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, []byte(`<nzb xmlns="urn:test"><file/></nzb>`)) {
		t.Fatalf("stored payload = %q, want original NZB", payload)
	}

	if err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path}); err != nil {
		t.Fatalf("expected duplicate submission response success, got %v", err)
	}
	jobs, err = st.ListVisibleClientJobs(context.Background(), store.ClientKindSAB, "tv", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("duplicate submission created %d jobs, want 1", len(jobs))
	}
	assertSABQueueCount(t, srv.URL, 1)
}

func TestRunAddMagnet_RealRouterPersistsJob(t *testing.T) {
	srv, st := newManualAddRouter(t)
	t.Setenv("TORBOXARR_QBIT_PASSWORD", "password")
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=movie"

	if err := runAddAt(context.Background(), srv.URL, "torboxarr", magnet, ""); err != nil {
		t.Fatalf("expected real-router success, got %v", err)
	}

	jobs, err := st.ListVisibleClientJobs(context.Background(), store.ClientKindQBit, "torboxarr", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.SourceType != store.SourceTypeTorrent || job.ClientKind != store.ClientKindQBit || job.SourceURI == nil || *job.SourceURI != magnet {
		t.Fatalf("job = %+v, want qBit torrent with original magnet", job)
	}
	if job.InfoHash == nil || *job.InfoHash != "0123456789abcdef0123456789abcdef01234567" || job.PayloadRef != nil {
		t.Fatalf("job hash/payload = %v/%v, want normalized hash and no payload", job.InfoHash, job.PayloadRef)
	}
	if err := runAddAt(context.Background(), srv.URL, "torboxarr", magnet, ""); err != nil {
		t.Fatalf("expected duplicate submission response success, got %v", err)
	}
	jobs, err = st.ListVisibleClientJobs(context.Background(), store.ClientKindQBit, "torboxarr", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("duplicate magnet created %d jobs, want 1", len(jobs))
	}
	assertQBitQueueCount(t, srv.URL, 1)
}

func TestRunAddTorrent_RealRouterPersistsPayload(t *testing.T) {
	srv, st := newManualAddRouter(t)
	t.Setenv("TORBOXARR_QBIT_PASSWORD", "password")
	path := writeTempFile(t, string(validTorrentBytes()))

	if err := runAddAt(context.Background(), srv.URL, "torboxarr", "", path); err != nil {
		t.Fatalf("expected real-router success, got %v", err)
	}

	jobs, err := st.ListVisibleClientJobs(context.Background(), store.ClientKindQBit, "torboxarr", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.SourceType != store.SourceTypeTorrent || job.ClientKind != store.ClientKindQBit || job.PayloadRef == nil {
		t.Fatalf("job = %+v, want qBit torrent with payload", job)
	}
	if job.Metadata.UploadedFilename != filepath.Base(path) || job.Metadata.OriginalFilename != filepath.Base(path) {
		t.Fatalf("job metadata = %+v, want uploaded and original filename", job.Metadata)
	}
	payload, err := os.ReadFile(*job.PayloadRef)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, validTorrentBytes()) {
		t.Fatalf("stored payload does not match torrent input")
	}
	if err := runAddAt(context.Background(), srv.URL, "torboxarr", "", path); err != nil {
		t.Fatalf("expected duplicate submission response success, got %v", err)
	}
	jobs, err = st.ListVisibleClientJobs(context.Background(), store.ClientKindQBit, "torboxarr", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("duplicate torrent created %d jobs, want 1", len(jobs))
	}
	assertQBitQueueCount(t, srv.URL, 1)
}

func TestRunAddNZB_ResponseDoesNotLeakAPIKey(t *testing.T) {
	opts := defaultSABAddServerOptions()
	opts.configBody = "not json"
	srv, _ := newSABAddServer(t, opts)
	withSABAPIKey(t)
	path := writeTempNZB(t, `<nzb></nzb>`)

	err := runAddInputAt(context.Background(), srv.URL, addInput{Category: "tv", NZBPath: path})
	if err == nil || strings.Contains(err.Error(), "sab-secret") || strings.Contains(err.Error(), "apikey=") {
		t.Fatalf("expected redacted category error, got %v", err)
	}
}
