package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultAddBaseURL = "http://localhost:8085"

func runAddCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	category := fs.String("category", "", "Arr category for the manual add (e.g. sonarr, radarr)")
	displayName := fs.String("name", "", "Optional display name / rename for the download")
	magnet := fs.String("magnet", "", "Magnet URI to add")
	torrentPath := fs.String("torrent", "", "Path to a .torrent file to add")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runAdd(ctx, *category, *displayName, *magnet, *torrentPath)
}

type addConfig struct {
	baseURL  string
	username string
	password string
}

func loadAddConfig() addConfig {
	baseURL := os.Getenv("TORBOXARR_SERVER_BASE_URL")
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAddBaseURL
	}
	username := os.Getenv("TORBOXARR_QBIT_USERNAME")
	if strings.TrimSpace(username) == "" {
		username = "admin"
	}
	password := os.Getenv("TORBOXARR_QBIT_PASSWORD")
	return addConfig{
		baseURL:  strings.TrimSpace(baseURL),
		username: strings.TrimSpace(username),
		password: password,
	}
}

func runAdd(ctx context.Context, category, displayName, magnet, torrentPath string) error {
	if strings.TrimSpace(category) == "" {
		return fmt.Errorf("--category is required")
	}
	if strings.TrimSpace(magnet) == "" && strings.TrimSpace(torrentPath) == "" {
		return fmt.Errorf("one of --magnet or --torrent is required")
	}
	if strings.TrimSpace(magnet) != "" && strings.TrimSpace(torrentPath) != "" {
		return fmt.Errorf("--magnet and --torrent are mutually exclusive")
	}

	cfg := loadAddConfig()
	if strings.TrimSpace(cfg.password) == "" {
		return fmt.Errorf("TORBOXARR_QBIT_PASSWORD is not set; cannot authenticate to the server")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	if err := qbitLogin(ctx, client, cfg); err != nil {
		return err
	}

	if strings.TrimSpace(magnet) != "" {
		return addMagnet(ctx, client, cfg, category, displayName, magnet)
	}
	return addTorrentFile(ctx, client, cfg, category, displayName, torrentPath)
}

func qbitLogin(ctx context.Context, client *http.Client, cfg addConfig) error {
	form := url.Values{}
	form.Set("username", cfg.username)
	form.Set("password", cfg.password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !strings.Contains(string(body), "Ok") {
		return fmt.Errorf("login rejected: %s", strings.TrimSpace(string(body)))
	}
	return nil
}

func addMagnet(ctx context.Context, client *http.Client, cfg addConfig, category, displayName, magnet string) error {
	form := url.Values{}
	form.Set("urls", strings.TrimSpace(magnet))
	form.Set("category", category)
	if strings.TrimSpace(displayName) != "" {
		form.Set("rename", displayName)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL+"/api/v2/torrents/add", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return doAdd(ctx, client, req)
}

func addTorrentFile(ctx context.Context, client *http.Client, cfg addConfig, category, displayName, torrentPath string) error {
	f, err := os.Open(torrentPath)
	if err != nil {
		return fmt.Errorf("open torrent file: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("category", category); err != nil {
		return err
	}
	if strings.TrimSpace(displayName) != "" {
		if err := mw.WriteField("rename", displayName); err != nil {
			return err
		}
	}

	part, err := mw.CreateFormFile("torrents", filepath.Base(torrentPath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL+"/api/v2/torrents/add", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	return doAdd(ctx, client, req)
}

func doAdd(ctx context.Context, client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("add request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("add failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if strings.Contains(string(body), "Fails") {
		return fmt.Errorf("server rejected submission: %s", strings.TrimSpace(string(body)))
	}
	fmt.Println("Ok.")
	return nil
}
