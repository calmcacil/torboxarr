package main

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mrjoiny/torboxarr/internal/compat"
)

const addServerAddress = "http://127.0.0.1:8085"

const (
	addRequestTimeout   = 30 * time.Second
	maxTorrentFileBytes = 256 << 20
	maxNZBFileBytes     = compat.MaxSABUploadBytes
)

type addInput struct {
	Category    string
	Magnet      string
	TorrentPath string
	NZBPath     string
}

func runAddCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	category := fs.String("category", "", "existing TorBoxarr category for the download (required)")
	magnet := fs.String("magnet", "", "magnet URI of the torrent to submit")
	torrentPath := fs.String("torrent", "", "path to a .torrent file visible inside the container")
	nzbPath := fs.String("nzb", "", "path to an .nzb file visible inside the container")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; provide exactly one --magnet, --torrent, or --nzb", fs.Arg(0))
	}
	return runAddInput(ctx, addInput{
		Category:    *category,
		Magnet:      *magnet,
		TorrentPath: *torrentPath,
		NZBPath:     *nzbPath,
	})
}

func runAdd(ctx context.Context, category, magnet, torrentPath string) error {
	return runAddInput(ctx, addInput{Category: category, Magnet: magnet, TorrentPath: torrentPath})
}

func runAddAt(ctx context.Context, baseURL, category, magnet, torrentPath string) error {
	return runAddInputAt(ctx, baseURL, addInput{Category: category, Magnet: magnet, TorrentPath: torrentPath})
}

func runAddInput(ctx context.Context, input addInput) error {
	return runAddInputAt(ctx, addServerAddress, input)
}

func runAddInputAt(ctx context.Context, baseURL string, input addInput) error {
	input.Category = strings.TrimSpace(input.Category)
	input.Magnet = strings.TrimSpace(input.Magnet)
	input.TorrentPath = strings.TrimSpace(input.TorrentPath)
	input.NZBPath = strings.TrimSpace(input.NZBPath)

	if input.Category == "" {
		return fmt.Errorf("--category is required; use an existing TorBoxarr category (e.g. --category sonarr)")
	}
	sources := 0
	if input.Magnet != "" {
		sources++
	}
	if input.TorrentPath != "" {
		sources++
	}
	if input.NZBPath != "" {
		sources++
	}
	switch sources {
	case 0:
		return fmt.Errorf("exactly one of --magnet, --torrent, or --nzb is required")
	case 1:
	default:
		return fmt.Errorf("--magnet, --torrent, and --nzb are mutually exclusive; submit exactly one download at a time")
	}

	switch {
	case input.Magnet != "":
		if err := validateMagnet(input.Magnet); err != nil {
			return err
		}
	case input.TorrentPath != "":
		if err := validateTorrentFile(input.TorrentPath); err != nil {
			return err
		}
	case input.NZBPath != "":
		if err := validateNZBFile(input.NZBPath); err != nil {
			return err
		}
	}

	if input.NZBPath != "" {
		apiKey := strings.TrimSpace(os.Getenv("TORBOXARR_SAB_API_KEY"))
		if apiKey == "" {
			return fmt.Errorf("TORBOXARR_SAB_API_KEY is not set; it must match the API key of the running TorBoxarr server")
		}
		return (&sabAddClient{
			http:    &http.Client{Timeout: addRequestTimeout},
			baseURL: baseURL,
			apiKey:  apiKey,
		}).run(ctx, input.Category, input.NZBPath)
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
	return client.run(ctx, input.Category, input.Magnet, input.TorrentPath)
}

func addEndpoint(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addEndpoint(c.baseURL, "/api/v2/auth/login"), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("cannot prepare login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		if interrupted(err) {
			return fmt.Errorf("authentication interrupted: %w", err)
		}
		return fmt.Errorf("cannot reach the TorBoxarr server: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("authentication failed: the server returned an unreadable response")
	}

	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("authentication failed (status %d); check TORBOXARR_QBIT_PASSWORD", resp.StatusCode)
	}
	return nil
}

func (c *addClient) requireCategory(ctx context.Context, category string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addEndpoint(c.baseURL, "/api/v2/torrents/categories"), nil)
	if err != nil {
		return fmt.Errorf("cannot prepare category request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if interrupted(err) {
			return fmt.Errorf("category check interrupted: %w", err)
		}
		return fmt.Errorf("cannot reach the TorBoxarr server to check categories: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot check categories: the server returned an unreadable category list")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cannot check categories: the server returned status %d", resp.StatusCode)
	}
	var categories map[string]json.RawMessage
	if err := json.Unmarshal(body, &categories); err != nil {
		return fmt.Errorf("cannot check categories: the server returned an unreadable category list")
	}
	if categories == nil {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addEndpoint(c.baseURL, "/api/v2/torrents/add"), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("cannot prepare submission request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return c.submit(ctx, req, fmt.Sprintf("submitted magnet with info hash %q to category %q", magnetInfoHash(magnet), category))
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addEndpoint(c.baseURL, "/api/v2/torrents/add"), &body)
	if err != nil {
		return fmt.Errorf("cannot prepare submission request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return c.submit(ctx, req, fmt.Sprintf("submitted torrent %q to category %q", filepath.Base(torrentPath), category))
}

func (c *addClient) submit(ctx context.Context, req *http.Request, confirmation string) error {
	resp, err := doAddSubmission(c.http, req,
		"cannot reach the TorBoxarr server",
		"Inspect the TorBoxarr queue before retrying; retrying may create a duplicate submission",
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server rejected the submission (status %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("submission status is unknown: the request may have reached the server; Inspect the TorBoxarr queue before retrying; retrying may create a duplicate submission")
	}

	if strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("the server rejected the submission (status %d)", resp.StatusCode)
	}
	fmt.Println(confirmation)
	return nil
}

func doAddSubmission(client *http.Client, req *http.Request, reachability, queueAdvice string) (*http.Response, error) {
	wroteRequest := false
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			wroteRequest = true
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(req)
	if err != nil {
		if wroteRequest {
			return nil, fmt.Errorf("submission status is unknown: the request may have reached the server; %s", queueAdvice)
		}
		if interrupted(err) {
			return nil, fmt.Errorf("submission was not sent: request canceled before transmission")
		}
		return nil, errors.New(reachability)
	}
	return resp, nil
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
				return normalizeMagnetInfoHash(xt[len(prefix):])
			}
		}
	}
	return "unknown"
}

func normalizeMagnetInfoHash(v string) string {
	v = strings.TrimSpace(v)
	lower := strings.ToLower(v)
	if len(lower) == 40 {
		if _, err := hex.DecodeString(lower); err == nil {
			return lower
		}
	}
	if len(v) == 32 {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(v))
		if err == nil {
			return hex.EncodeToString(decoded)
		}
	}
	return lower
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

func validateNZBFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("NZB file %s does not exist", path)
		}
		return fmt.Errorf("cannot access NZB file %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("NZB path %s is a directory, not an .nzb file", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("NZB path %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("NZB file %s is empty", path)
	}
	if info.Size() > maxNZBFileBytes {
		return fmt.Errorf("NZB file %s is too large; the maximum upload size is %d MiB", path, maxNZBFileBytes/(1<<20))
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot read NZB file %s: %w", path, err)
	}
	defer file.Close()

	reader := &countingReader{Reader: io.LimitReader(file, maxNZBFileBytes+1)}
	decoder := xml.NewDecoder(reader)
	seenRoot := false
	rootClosed := false
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s is not a valid NZB XML document", path)
		}

		switch token := token.(type) {
		case xml.StartElement:
			if rootClosed {
				return fmt.Errorf("%s has trailing XML after the NZB document", path)
			}
			if depth == 0 {
				if seenRoot || token.Name.Local != "nzb" {
					return fmt.Errorf("%s must have an nzb document root", path)
				}
				seenRoot = true
			}
			depth++
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				rootClosed = true
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return fmt.Errorf("%s contains non-whitespace data outside the NZB document", path)
			}
		}
	}
	if reader.Count > maxNZBFileBytes {
		return fmt.Errorf("NZB file %s is too large; the maximum upload size is %d MiB", path, maxNZBFileBytes/(1<<20))
	}
	if !seenRoot || !rootClosed || depth != 0 {
		return fmt.Errorf("%s is not a complete NZB XML document", path)
	}
	return nil
}

type countingReader struct {
	io.Reader
	Count int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.Count += int64(n)
	return n, err
}

type sabAddClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

func (c *sabAddClient) run(ctx context.Context, category, nzbPath string) error {
	if err := c.requireCategory(ctx, category); err != nil {
		return err
	}
	return c.submitFile(ctx, category, nzbPath)
}

func (c *sabAddClient) requireCategory(ctx context.Context, category string) error {
	query := url.Values{}
	query.Set("mode", "get_config")
	query.Set("output", "json")
	query.Set("apikey", c.apiKey)
	endpoint := addEndpoint(c.baseURL, "/sabnzbd/api") + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("cannot prepare SAB category request")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if interrupted(err) {
			return fmt.Errorf("SAB category check interrupted")
		}
		return fmt.Errorf("cannot reach the TorBoxarr server to check SAB categories")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("SAB authentication failed (status %d); check TORBOXARR_SAB_API_KEY", resp.StatusCode)
		}
		return fmt.Errorf("cannot check SAB categories: the server returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cannot check SAB categories: the server returned an unreadable category list")
	}

	var config struct {
		Config *struct {
			Categories []struct {
				Name string `json:"name"`
			} `json:"categories"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &config); err != nil || config.Config == nil || config.Config.Categories == nil {
		return fmt.Errorf("cannot check SAB categories: the server returned an unreadable category list")
	}
	for _, item := range config.Config.Categories {
		if item.Name == category {
			return nil
		}
	}
	return fmt.Errorf("SAB category %q is unknown to the server; create it before submitting, the server only accepts existing categories", category)
}

func (c *sabAddClient) submitFile(ctx context.Context, category, nzbPath string) error {
	file, err := os.Open(nzbPath)
	if err != nil {
		return fmt.Errorf("cannot open NZB file %s: %w", nzbPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("cannot stat NZB file %s: %w", nzbPath, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("NZB file %s changed after validation", nzbPath)
	}

	header, trailer, contentType, err := nzbMultipartParts(category, filepath.Base(nzbPath))
	if err != nil {
		return err
	}
	bodySize := int64(len(header)) + info.Size() + int64(len(trailer))
	if bodySize > maxNZBFileBytes {
		return fmt.Errorf("NZB submission exceeds the maximum upload size of %d MiB", maxNZBFileBytes/(1<<20))
	}

	body := io.MultiReader(
		bytes.NewReader(header),
		io.LimitReader(file, info.Size()),
		bytes.NewReader(trailer),
	)
	query := url.Values{}
	query.Set("mode", "addfile")
	query.Set("apikey", c.apiKey)
	endpoint := addEndpoint(c.baseURL, "/sabnzbd/api") + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("cannot prepare NZB submission request")
	}
	req.ContentLength = bodySize
	req.Header.Set("Content-Type", contentType)

	resp, err := doAddSubmission(c.http, req,
		"cannot reach the TorBoxarr server",
		"Inspect the SAB queue or history before retrying; retrying may create a duplicate submission",
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("SAB authentication failed (status %d); check TORBOXARR_SAB_API_KEY", resp.StatusCode)
		}
		return fmt.Errorf("the SAB server rejected the submission (status %d)", resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("submission status is unknown: the request may have reached the server; Inspect the SAB queue or history before retrying; retrying may create a duplicate submission")
	}

	var result compat.SABAddResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return fmt.Errorf("the SAB server rejected the submission: unreadable response")
	}
	if !result.Status || len(result.NzoIDs) != 1 || strings.TrimSpace(result.NzoIDs[0]) == "" {
		return fmt.Errorf("the SAB server rejected the submission: response did not confirm one accepted NZB")
	}
	nzoID := strings.TrimSpace(result.NzoIDs[0])
	if !safeSABNZOID(nzoID) {
		return fmt.Errorf("the SAB server rejected the submission: response contained an unsafe NZO ID")
	}
	fmt.Printf("submitted NZB file %q to category %q with NZO ID %s\n", filepath.Base(nzbPath), category, nzoID)
	return nil
}

func nzbMultipartParts(category, filename string) (header, trailer []byte, contentType string, err error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, field := range [][2]string{
		{"mode", "addfile"},
		{"output", "json"},
		{"cat", category},
	} {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return nil, nil, "", fmt.Errorf("cannot build NZB submission: %w", err)
		}
	}
	if _, err := writer.CreateFormFile("nzbfile", filename); err != nil {
		return nil, nil, "", fmt.Errorf("cannot build NZB submission: %w", err)
	}
	header = append([]byte(nil), body.Bytes()...)
	contentType = writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return nil, nil, "", fmt.Errorf("cannot build NZB submission: %w", err)
	}
	trailer = append([]byte(nil), body.Bytes()[len(header):]...)
	return header, trailer, contentType, nil
}

func safeSABNZOID(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if r < '!' || r > '~' || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
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
		return parseBencodeInteger(data, pos)
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

func parseBencodeInteger(data []byte, pos int) (int, bool) {
	if pos >= len(data) || data[pos] != 'i' {
		return 0, false
	}
	pos++
	start := pos
	if pos < len(data) && data[pos] == '-' {
		pos++
		if pos >= len(data) || data[pos] == '0' {
			return 0, false
		}
	}
	digits := pos
	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		pos++
	}
	if pos == digits || pos >= len(data) || data[pos] != 'e' {
		return 0, false
	}
	if data[digits] == '0' && pos-digits > 1 {
		return 0, false
	}
	if start == pos {
		return 0, false
	}
	return pos + 1, true
}

func parseBencodeString(data []byte, pos int) (int, string, bool) {
	if pos >= len(data) || data[pos] < '0' || data[pos] > '9' {
		return 0, "", false
	}
	colon := bytes.IndexByte(data[pos:], ':')
	if colon < 0 {
		return 0, "", false
	}
	lengthText := data[pos : pos+colon]
	if len(lengthText) > 1 && lengthText[0] == '0' {
		return 0, "", false
	}
	for _, digit := range lengthText {
		if digit < '0' || digit > '9' {
			return 0, "", false
		}
	}
	length, err := strconv.Atoi(string(lengthText))
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
