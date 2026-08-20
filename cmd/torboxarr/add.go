package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const addServerAddress = "http://127.0.0.1:8085"

const (
	addRequestTimeout   = 30 * time.Second
	maxTorrentFileBytes = 256 << 20
)

func runAddCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	category := fs.String("category", "", "existing TorBoxarr category for the download (required)")
	magnet := fs.String("magnet", "", "magnet URI of the torrent to submit")
	torrentPath := fs.String("torrent", "", "path to a .torrent file visible inside the container")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; provide exactly one --magnet or --torrent", fs.Arg(0))
	}
	return runAdd(ctx, *category, *magnet, *torrentPath)
}

func runAdd(ctx context.Context, category, magnet, torrentPath string) error {
	return runAddAt(ctx, addServerAddress, category, magnet, torrentPath)
}

func runAddAt(ctx context.Context, baseURL, category, magnet, torrentPath string) error {
	category = strings.TrimSpace(category)
	magnet = strings.TrimSpace(magnet)
	torrentPath = strings.TrimSpace(torrentPath)

	if category == "" {
		return fmt.Errorf("--category is required; use an existing TorBoxarr category (e.g. --category sonarr)")
	}
	hasMagnet, hasTorrent := magnet != "", torrentPath != ""
	switch {
	case !hasMagnet && !hasTorrent:
		return fmt.Errorf("exactly one of --magnet or --torrent is required")
	case hasMagnet && hasTorrent:
		return fmt.Errorf("--magnet and --torrent are mutually exclusive; submit exactly one download at a time")
	}
	if hasMagnet {
		if err := validateMagnet(magnet); err != nil {
			return err
		}
	} else if err := validateTorrentFile(torrentPath); err != nil {
		return err
	}

	password := strings.TrimSpace(os.Getenv("TORBOXARR_QBIT_PASSWORD"))
	if password == "" {
		return fmt.Errorf("TORBOXARR_QBIT_PASSWORD is not set; it must match the password of the running TorBoxarr server")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return fmt.Errorf("cannot prepare HTTP session: %w", err)
	}
	client := &addClient{
		http:     &http.Client{Jar: jar, Timeout: addRequestTimeout},
		baseURL:  baseURL,
		username: "admin",
		password: password,
	}
	return client.run(ctx, category, magnet, torrentPath)
}

type addClient struct {
	http     *http.Client
	baseURL  string
	username string
	password string
}

func (c *addClient) run(ctx context.Context, category, magnet, torrentPath string) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	if err := c.requireCategory(ctx, category); err != nil {
		return err
	}
	if magnet != "" {
		return c.submitMagnet(ctx, category, magnet)
	}
	return c.submitTorrentFile(ctx, category, torrentPath)
}

func (c *addClient) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("cannot prepare login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		if interrupted(err) {
			return fmt.Errorf("authentication interrupted: %w", err)
		}
		return fmt.Errorf("cannot reach the TorBoxarr server at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("authentication failed (status %d); check TORBOXARR_QBIT_PASSWORD", resp.StatusCode)
	}
	return nil
}

func (c *addClient) requireCategory(ctx context.Context, category string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/torrents/categories", nil)
	if err != nil {
		return fmt.Errorf("cannot prepare category request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if interrupted(err) {
			return fmt.Errorf("category check interrupted: %w", err)
		}
		return fmt.Errorf("cannot reach the TorBoxarr server at %s to check categories: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cannot check categories: the server returned status %d", resp.StatusCode)
	}
	var categories map[string]json.RawMessage
	if err := json.Unmarshal(body, &categories); err != nil {
		return fmt.Errorf("cannot check categories: the server returned an unreadable category list")
	}
	if _, ok := categories[category]; !ok {
		return fmt.Errorf("category %q is unknown to the server; create it before submitting, the server only accepts existing categories", category)
	}
	return nil
}

func (c *addClient) submitMagnet(ctx context.Context, category, magnet string) error {
	form := url.Values{}
	form.Set("urls", magnet)
	form.Set("category", category)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/torrents/add", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("cannot prepare submission request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return c.submit(ctx, req, fmt.Sprintf("submitted magnet with info hash %s to category %q", magnetInfoHash(magnet), category))
}

func (c *addClient) submitTorrentFile(ctx context.Context, category, torrentPath string) error {
	file, err := os.Open(torrentPath)
	if err != nil {
		return fmt.Errorf("cannot open torrent file %s: %w", torrentPath, err)
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("category", category); err != nil {
		return fmt.Errorf("cannot build submission: %w", err)
	}
	part, err := writer.CreateFormFile("torrents", filepath.Base(torrentPath))
	if err != nil {
		return fmt.Errorf("cannot build submission: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("cannot build submission: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("cannot build submission: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/torrents/add", &body)
	if err != nil {
		return fmt.Errorf("cannot prepare submission request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return c.submit(ctx, req, fmt.Sprintf("submitted torrent %s to category %q", filepath.Base(torrentPath), category))
}

func (c *addClient) submit(ctx context.Context, req *http.Request, confirmation string) error {
	resp, err := c.http.Do(req)
	if err != nil {
		if interrupted(err) {
			return fmt.Errorf("submission status is unknown: %w; the download may already be queued. Inspect the TorBoxarr queue before retrying; retrying may create a duplicate submission", err)
		}
		return fmt.Errorf("cannot reach the TorBoxarr server at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("the server rejected the submission (status %d)", resp.StatusCode)
	}
	fmt.Println(confirmation)
	return nil
}

func interrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func validateMagnet(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("--magnet requires a non-empty magnet URI")
	}
	parsed, err := url.Parse(v)
	if err != nil || !strings.EqualFold(parsed.Scheme, "magnet") {
		return fmt.Errorf("--magnet is not a valid magnet URI; expected magnet:?xt=urn:btih:<infohash>")
	}

	rawQuery := parsed.RawQuery
	if rawQuery == "" && strings.HasPrefix(parsed.Opaque, "?") {
		rawQuery = strings.TrimPrefix(parsed.Opaque, "?")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return fmt.Errorf("--magnet is not a valid magnet URI; expected magnet:?xt=urn:btih:<infohash>")
	}
	for key, values := range query {
		if !strings.EqualFold(key, "xt") {
			continue
		}
		for _, xt := range values {
			xt = strings.TrimSpace(xt)
			const prefix = "urn:btih:"
			if strings.HasPrefix(strings.ToLower(xt), prefix) && strings.TrimSpace(xt[len(prefix):]) != "" {
				return nil
			}
		}
	}
	return fmt.Errorf("--magnet must contain an xt=urn:btih:<infohash> parameter")
}

func magnetInfoHash(v string) string {
	parsed, err := url.Parse(v)
	if err != nil {
		return "unknown"
	}
	rawQuery := parsed.RawQuery
	if rawQuery == "" && strings.HasPrefix(parsed.Opaque, "?") {
		rawQuery = strings.TrimPrefix(parsed.Opaque, "?")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "unknown"
	}
	for key, values := range query {
		if !strings.EqualFold(key, "xt") {
			continue
		}
		for _, xt := range values {
			xt = strings.TrimSpace(xt)
			const prefix = "urn:btih:"
			if strings.HasPrefix(strings.ToLower(xt), prefix) {
				return xt[len(prefix):]
			}
		}
	}
	return "unknown"
}

func validateTorrentFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("torrent file %s does not exist", path)
		}
		return fmt.Errorf("cannot access torrent file %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("torrent path %s is a directory, not a .torrent file", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("torrent path %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("torrent file %s is empty", path)
	}
	if info.Size() > maxTorrentFileBytes {
		return fmt.Errorf("torrent file %s is too large to be a .torrent file", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot read torrent file %s: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("cannot read torrent file %s: %w", path, err)
	}
	if !isValidTorrent(data) {
		return fmt.Errorf("%s is not a valid .torrent file; it must contain a bencoded info section", path)
	}
	return nil
}

func isValidTorrent(data []byte) bool {
	hasInfo := false
	end, ok := parseBencodeDict(data, 0, 0, &hasInfo)
	return ok && end == len(data) && hasInfo
}

func parseBencodeDict(data []byte, pos, depth int, hasInfo *bool) (int, bool) {
	if depth > 16 {
		return 0, false
	}
	if pos >= len(data) || data[pos] != 'd' {
		return 0, false
	}
	pos++
	for {
		if pos >= len(data) {
			return 0, false
		}
		if data[pos] == 'e' {
			return pos + 1, true
		}
		var key string
		var ok bool
		pos, key, ok = parseBencodeString(data, pos)
		if !ok {
			return 0, false
		}
		if depth == 0 && key == "info" {
			if pos >= len(data) || data[pos] != 'd' {
				return 0, false
			}
			*hasInfo = true
		}
		pos, ok = parseBencodeValue(data, pos, depth+1)
		if !ok {
			return 0, false
		}
	}
}

func parseBencodeValue(data []byte, pos, depth int) (int, bool) {
	if depth > 16 {
		return 0, false
	}
	if pos >= len(data) {
		return 0, false
	}
	switch data[pos] {
	case 'i':
		pos++
		for pos < len(data) && data[pos] != 'e' {
			pos++
		}
		if pos >= len(data) {
			return 0, false
		}
		return pos + 1, true
	case 'l':
		pos++
		for {
			if pos >= len(data) {
				return 0, false
			}
			if data[pos] == 'e' {
				return pos + 1, true
			}
			var ok bool
			pos, ok = parseBencodeValue(data, pos, depth+1)
			if !ok {
				return 0, false
			}
		}
	case 'd':
		ignored := false
		return parseBencodeDict(data, pos, depth, &ignored)
	default:
		var ok bool
		pos, _, ok = parseBencodeString(data, pos)
		if !ok {
			return 0, false
		}
		return pos, true
	}
}

func parseBencodeString(data []byte, pos int) (int, string, bool) {
	colon := bytes.IndexByte(data[pos:], ':')
	if colon < 0 {
		return 0, "", false
	}
	length, err := strconv.Atoi(string(data[pos : pos+colon]))
	if err != nil || length < 0 {
		return 0, "", false
	}
	start := pos + colon + 1
	if start > len(data) || length > len(data)-start {
		return 0, "", false
	}
	end := start + length
	return end, string(data[start:end]), true
}
