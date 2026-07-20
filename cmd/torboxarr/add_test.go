package main

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// newAddTestServer returns an httptest server emulating the subset of the
// qBittorrent API the add subcommand uses, plus a recorder of what the client
// submitted. loginFails / addStatus / addBody control the server's responses.
func newAddTestServer(t *testing.T, loginFails bool, addStatus int, addBody string) (*httptest.Server, *addCapture) {
	t.Helper()
	cap := &addCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v2/auth/login"):
			if loginFails {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("Fails."))
				return
			}
			_, _ = w.Write([]byte("Ok."))
		case strings.HasSuffix(r.URL.Path, "/api/v2/torrents/add"):
			body, _ := io.ReadAll(r.Body)
			cap.method = r.Method
			cap.contentType = r.Header.Get("Content-Type")
			cap.form = map[string]string{}
			if strings.Contains(cap.contentType, "multipart/form-data") {
				_, params, err := mime.ParseMediaType(cap.contentType)
				if err == nil {
					mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
					for {
						p, err := mr.NextPart()
						if err != nil {
							break
						}
						name := p.FormName()
						if name == "" {
							continue
						}
						data, _ := io.ReadAll(p)
						cap.form[name] = string(data)
					}
				}
			} else {
				vals, err := url.ParseQuery(string(body))
				if err == nil {
					for k := range vals {
						if len(vals[k]) > 0 {
							cap.form[k] = vals[k][0]
						}
					}
				}
			}
			w.WriteHeader(addStatus)
			_, _ = w.Write([]byte(addBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type addCapture struct {
	method      string
	contentType string
	form        map[string]string
}

func withAddEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("TORBOXARR_SERVER_BASE_URL", baseURL)
	t.Setenv("TORBOXARR_QBIT_PASSWORD", "secret")
	t.Setenv("TORBOXARR_QBIT_USERNAME", "admin")
}

func TestRunAdd_RequiresCategory(t *testing.T) {
	srv, _ := newAddTestServer(t, false, http.StatusOK, "Ok.")
	withAddEnv(t, srv.URL)
	err := runAdd(context.Background(), "", "", "magnet:?xt=urn:btih:abc", "")
	if err == nil || !strings.Contains(err.Error(), "--category") {
		t.Fatalf("expected --category required error, got %v", err)
	}
}

func TestRunAdd_RequiresMagnetOrTorrent(t *testing.T) {
	srv, _ := newAddTestServer(t, false, http.StatusOK, "Ok.")
	withAddEnv(t, srv.URL)
	err := runAdd(context.Background(), "radarr", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "one of --magnet or --torrent") {
		t.Fatalf("expected magnet/torrent required error, got %v", err)
	}
}

func TestRunAdd_MutuallyExclusive(t *testing.T) {
	srv, _ := newAddTestServer(t, false, http.StatusOK, "Ok.")
	withAddEnv(t, srv.URL)
	err := runAdd(context.Background(), "radarr", "", "magnet:?xt=urn:btih:abc", "/tmp/x.torrent")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

func TestRunAdd_MissingPassword(t *testing.T) {
	srv, _ := newAddTestServer(t, false, http.StatusOK, "Ok.")
	t.Setenv("TORBOXARR_SERVER_BASE_URL", srv.URL)
	// password intentionally unset
	t.Setenv("TORBOXARR_QBIT_USERNAME", "admin")
	err := runAdd(context.Background(), "radarr", "", "magnet:?xt=urn:btih:abc", "")
	if err == nil || !strings.Contains(err.Error(), "TORBOXARR_QBIT_PASSWORD") {
		t.Fatalf("expected password error, got %v", err)
	}
}

func TestRunAdd_MagnetSuccess(t *testing.T) {
	srv, cap := newAddTestServer(t, false, http.StatusOK, "Ok.")
	withAddEnv(t, srv.URL)
	if err := runAdd(context.Background(), "radarr", "My Movie", "magnet:?xt=urn:btih:abc", ""); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if cap.form["urls"] != "magnet:?xt=urn:btih:abc" {
		t.Errorf("expected magnet in urls field, got %q", cap.form["urls"])
	}
	if cap.form["category"] != "radarr" {
		t.Errorf("expected category=radarr, got %q", cap.form["category"])
	}
	if cap.form["rename"] != "My Movie" {
		t.Errorf("expected rename=My Movie, got %q", cap.form["rename"])
	}
	if cap.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("expected urlencoded content type, got %q", cap.contentType)
	}
}

func TestRunAdd_TorrentFileSuccess(t *testing.T) {
	srv, cap := newAddTestServer(t, false, http.StatusOK, "Ok.")
	withAddEnv(t, srv.URL)

	f, err := os.CreateTemp(t.TempDir(), "*.torrent")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("d8:announce4:teste")
	f.Close()

	if err := runAdd(context.Background(), "sonarr", "", "", f.Name()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if cap.form["category"] != "sonarr" {
		t.Errorf("expected category=sonarr, got %q", cap.form["category"])
	}
	if _, ok := cap.form["torrents"]; !ok {
		t.Errorf("expected torrents form part to be uploaded")
	}
	if !strings.HasPrefix(cap.contentType, "multipart/form-data") {
		t.Errorf("expected multipart content type, got %q", cap.contentType)
	}
}

func TestRunAdd_LoginFails(t *testing.T) {
	srv, _ := newAddTestServer(t, true, http.StatusOK, "Ok.")
	withAddEnv(t, srv.URL)
	err := runAdd(context.Background(), "radarr", "", "magnet:?xt=urn:btih:abc", "")
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("expected login error, got %v", err)
	}
}

func TestRunAdd_AddFailsStatus(t *testing.T) {
	srv, _ := newAddTestServer(t, false, http.StatusInternalServerError, "boom")
	withAddEnv(t, srv.URL)
	err := runAdd(context.Background(), "radarr", "", "magnet:?xt=urn:btih:abc", "")
	if err == nil || !strings.Contains(err.Error(), "add failed") {
		t.Fatalf("expected add failed error, got %v", err)
	}
}

func TestRunAdd_AddRejectedBody(t *testing.T) {
	srv, _ := newAddTestServer(t, false, http.StatusOK, "Fails. Add rejected.")
	withAddEnv(t, srv.URL)
	err := runAdd(context.Background(), "radarr", "", "magnet:?xt=urn:btih:abc", "")
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejected submission error, got %v", err)
	}
}
