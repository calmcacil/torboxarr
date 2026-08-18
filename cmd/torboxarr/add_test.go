package main

import (
	"context"
	"io"
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
	if err == nil || !strings.Contains(err.Error(), "exactly one of --magnet or --torrent") {
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
	if err == nil || !strings.Contains(err.Error(), "submission status is unknown") {
		t.Fatalf("expected unknown status error, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "inspect the torboxarr queue") {
		t.Fatalf("expected queue inspection advice, got %v", err)
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
	}
	for _, data := range invalid {
		if isValidTorrent([]byte(data)) {
			t.Errorf("data %q: expected invalid torrent", data)
		}
	}
}
