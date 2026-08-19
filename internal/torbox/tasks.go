package torbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (c *HTTPClient) CreateTorrentTask(ctx context.Context, req CreateTorrentTaskRequest) (*CreateTaskResponse, error) {
	c.debug("creating torrent task", "has_magnet", req.Magnet != "", "has_payload", req.PayloadPath != "", "name", req.Name)
	if err := c.wait(ctx, c.createLimiter); err != nil {
		return nil, err
	}
	values := map[string]string{}
	if req.Name != "" {
		values["name"] = req.Name
	}
	if req.Magnet != "" {
		values["magnet"] = req.Magnet
	}
	if req.Seed != 0 {
		values["seed"] = strconv.Itoa(req.Seed)
	}
	if req.AllowZip {
		values["allow_zip"] = "true"
	}
	if req.AsQueued {
		values["as_queued"] = "true"
	}
	if req.AddOnlyIfCached {
		values["add_only_if_cached"] = "true"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if req.PayloadPath != "" {
		if err := attachFile(writer, "file", req.PayloadPath); err != nil {
			return nil, err
		}
	}
	for key, value := range values {
		if value == "" {
			continue
		}
		if err := writer.WriteField(key, value); err != nil {
			return nil, fmt.Errorf("write field %s: %w", key, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close writer: %w", err)
	}

	env, err := c.do(ctx, http.MethodPost, "/api/torrents/createtorrent", &body, writer.FormDataContentType(), true)
	if err != nil {
		return nil, err
	}
	return parseCreateTask(env, req.AsQueued)
}

func (c *HTTPClient) CreateUsenetTask(ctx context.Context, req CreateUsenetTaskRequest) (*CreateTaskResponse, error) {
	postProcessingValue := any(nil)
	if req.PostProcessing != nil {
		postProcessingValue = *req.PostProcessing
	}
	c.debug("creating usenet task", "has_link", req.Link != "", "has_payload", req.PayloadPath != "", "name", req.Name, "has_post_processing", req.PostProcessing != nil, "post_processing", postProcessingValue)
	if err := c.wait(ctx, c.createLimiter); err != nil {
		return nil, err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if req.PayloadPath != "" {
		if err := attachFile(writer, "file", req.PayloadPath); err != nil {
			return nil, err
		}
	}
	fields := map[string]string{}
	if req.Link != "" {
		fields["link"] = req.Link
	}
	if req.Name != "" {
		fields["name"] = req.Name
	}
	if req.Password != "" {
		fields["password"] = req.Password
	}
	if req.PostProcessing != nil {
		fields["post_processing"] = strconv.Itoa(*req.PostProcessing)
	}
	if req.AsQueued {
		fields["as_queued"] = "true"
	}
	if req.AddOnlyIfCached {
		fields["add_only_if_cached"] = "true"
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return nil, fmt.Errorf("write field %s: %w", key, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close writer: %w", err)
	}

	env, err := c.do(ctx, http.MethodPost, "/api/usenet/createusenetdownload", &body, writer.FormDataContentType(), true)
	if err != nil {
		return nil, err
	}
	return parseCreateTask(env, req.AsQueued)
}

func (c *HTTPClient) GetTaskStatus(ctx context.Context, sourceType string, remoteID string) (*TaskStatus, error) {
	c.debug("fetching torbox task status", "source_type", sourceType, "remote_id", remoteID)
	return c.FindActiveTask(ctx, sourceType, remoteID, "", "")
}

func (c *HTTPClient) GetQueuedStatus(ctx context.Context, sourceType string, queuedID string) (*TaskStatus, error) {
	if strings.TrimSpace(queuedID) == "" {
		return nil, fmt.Errorf("queued id is required")
	}
	return c.FindQueuedTask(ctx, sourceType, queuedID, "", "")
}

func (c *HTTPClient) FindQueuedTask(ctx context.Context, sourceType, queuedID, queueAuthID, remoteHash string) (*TaskStatus, error) {
	c.debug("fetching torbox queued status", "source_type", sourceType, "queued_id", queuedID, "queue_auth_id", queueAuthID, "has_hash", strings.TrimSpace(remoteHash) != "")
	if strings.TrimSpace(queuedID) == "" && strings.TrimSpace(queueAuthID) == "" && strings.TrimSpace(remoteHash) == "" {
		return nil, fmt.Errorf("queued id, queue auth id, or remote hash is required")
	}
	if err := c.wait(ctx, c.pollLimiter); err != nil {
		return nil, err
	}
	items, err := c.getQueuedItems(ctx, sourceType, queuedID)
	queueListFallback := false
	lookupErr := err
	if err != nil {
		if strings.TrimSpace(queuedID) == "" || isUsenetSource(sourceType) {
			return nil, err
		}
		// TorBox can reject an active-looking queue ID even while the queue
		// list is available. Retry without the ID so the content hash can
		// reconcile the two views.
		items, err = c.getQueuedItems(ctx, sourceType, "")
		if err != nil {
			return nil, err
		}
		queueListFallback = true
	}
	if status, matched := matchQueuedTask(sourceType, items, queuedID, queueAuthID, remoteHash); matched {
		return status, nil
	}
	if !queueListFallback && !isUsenetSource(sourceType) && strings.TrimSpace(queuedID) != "" {
		// An ID-filtered queue request can return an empty successful result
		// while the item is still present in the unfiltered queue.
		items, err = c.getQueuedItems(ctx, sourceType, "")
		if err != nil {
			return nil, err
		}
		if status, matched := matchQueuedTask(sourceType, items, queuedID, queueAuthID, remoteHash); matched {
			return status, nil
		}
	}
	if queueListFallback {
		return nil, lookupErr
	}
	return nil, nil
}

func matchQueuedTask(sourceType string, items []map[string]any, queuedID, queueAuthID, remoteHash string) (*TaskStatus, bool) {
	for _, item := range items {
		itemID := extractQueuedID(item)
		itemAuthID := extractQueueAuthID(sourceType, item)
		itemHash := firstString(item, "hash")
		if identifiersConflict(itemHash, remoteHash) {
			continue
		}
		matchesID := strings.TrimSpace(queuedID) != "" && itemID == strings.TrimSpace(queuedID)
		matchesAuthID := isUsenetSource(sourceType) && strings.TrimSpace(queueAuthID) != "" && itemAuthID == strings.TrimSpace(queueAuthID)
		matchesHash := strings.TrimSpace(remoteHash) != "" && strings.EqualFold(itemHash, strings.TrimSpace(remoteHash))
		if !matchesID && !matchesAuthID && !matchesHash {
			continue
		}
		status := parseTaskStatus(sourceType, item, false)
		status.QueuedID = itemID
		status.QueueAuthID = itemAuthID
		return status, true
	}
	return nil, false
}

func (c *HTTPClient) FindActiveTask(ctx context.Context, sourceType string, remoteID, queueAuthID, remoteHash string) (*TaskStatus, error) {
	c.debug("finding active torbox task",
		"source_type", sourceType,
		"remote_id", remoteID,
		"queue_auth_id", queueAuthID,
		"has_hash", strings.TrimSpace(remoteHash) != "",
	)
	if err := c.wait(ctx, c.pollLimiter); err != nil {
		return nil, err
	}
	items, err := c.getRemoteItems(ctx, sourceType, remoteID)
	activeListFallback := false
	lookupErr := err
	if err != nil {
		if strings.TrimSpace(remoteID) == "" {
			return nil, err
		}
		// A stale queue ID can make the ID-filtered active endpoint fail. A
		// full active list can confirm the ID directly or use the other
		// available identities.
		items, err = c.getRemoteItems(ctx, sourceType, "")
		if err != nil {
			return nil, err
		}
		activeListFallback = true
	}
	status, matched := matchActiveTask(sourceType, items, remoteID, queueAuthID, remoteHash)
	if matched {
		return status, nil
	}
	if !activeListFallback && strings.TrimSpace(remoteID) != "" && (strings.TrimSpace(remoteHash) != "" || strings.TrimSpace(queueAuthID) != "") {
		// The ID-filtered endpoint may return an empty successful response for
		// a queue ID rather than an HTTP error. Reconcile by hash as well.
		items, err = c.getRemoteItems(ctx, sourceType, "")
		if err != nil {
			return nil, err
		}
		status, matched = matchActiveTask(sourceType, items, "", queueAuthID, remoteHash)
		if matched {
			return status, nil
		}
	}
	if activeListFallback {
		return nil, lookupErr
	}
	return nil, nil
}

func (c *HTTPClient) ForceStartQueuedTask(ctx context.Context, sourceType, queuedID string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(queuedID), 10, 64)
	if err != nil || id < 0 {
		return fmt.Errorf("force start: queued id %q is not a non-negative numeric TorBox queue id", queuedID)
	}
	if err := c.wait(ctx, c.pollLimiter); err != nil {
		return err
	}
	body := fmt.Sprintf(`{"queued_id":%d,"operation":"start"}`, id)
	if _, err := c.do(ctx, http.MethodPost, "/api/queued/controlqueued", strings.NewReader(body), "application/json", true); err != nil {
		return fmt.Errorf("force start queued %s: %w", sourceType, err)
	}
	return nil
}

func matchActiveTask(sourceType string, items []map[string]any, remoteID, queueAuthID, remoteHash string) (*TaskStatus, bool) {
	for _, item := range items {
		itemID := extractActiveID(sourceType, item, true)
		itemHash := firstString(item, "hash")
		itemAuthID := extractQueueAuthID(sourceType, item)
		if identifiersConflict(itemHash, remoteHash) {
			continue
		}

		switch {
		case strings.TrimSpace(remoteID) != "" && itemID == strings.TrimSpace(remoteID):
		case isUsenetSource(sourceType) && strings.TrimSpace(queueAuthID) != "" && itemAuthID == strings.TrimSpace(queueAuthID):
		case strings.TrimSpace(remoteHash) != "" && strings.EqualFold(itemHash, strings.TrimSpace(remoteHash)):
		default:
			continue
		}

		status := parseTaskStatus(sourceType, item, true)
		return status, true
	}
	return nil, false
}

func identifiersConflict(candidateHash, expectedHash string) bool {
	candidateHash = strings.TrimSpace(candidateHash)
	expectedHash = strings.TrimSpace(expectedHash)
	return candidateHash != "" && expectedHash != "" && !strings.EqualFold(candidateHash, expectedHash)
}

func isUsenetSource(sourceType string) bool {
	return strings.EqualFold(sourceType, "usenet") || strings.EqualFold(sourceType, "nzb")
}

func (c *HTTPClient) getRemoteItems(ctx context.Context, sourceType string, remoteID string) ([]map[string]any, error) {
	endpoint := ""
	query := url.Values{}
	query.Set("bypass_cache", "true")
	switch strings.ToLower(sourceType) {
	case "torrent":
		endpoint = "/api/torrents/mylist"
	case "nzb", "usenet":
		endpoint = "/api/usenet/mylist"
	default:
		return nil, fmt.Errorf("unknown source type %q", sourceType)
	}
	if strings.TrimSpace(remoteID) != "" {
		query.Set("id", strings.TrimSpace(remoteID))
	}

	env, err := c.do(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil, "", true)
	if err != nil {
		return nil, err
	}
	return parseItemsEnvelope(env)
}

func (c *HTTPClient) getQueuedItems(ctx context.Context, sourceType string, queuedID string) ([]map[string]any, error) {
	query := url.Values{}
	queueType := strings.ToLower(sourceType)
	if queueType == "nzb" {
		queueType = "usenet"
	}
	query.Set("type", queueType)
	query.Set("bypass_cache", "true")
	if !isUsenetSource(sourceType) && strings.TrimSpace(queuedID) != "" {
		query.Set("id", strings.TrimSpace(queuedID))
	}

	env, err := c.do(ctx, http.MethodGet, "/api/queued/getqueued?"+query.Encode(), nil, "", true)
	if err != nil {
		return nil, err
	}
	return parseItemsEnvelope(env)
}

func (c *HTTPClient) DeleteTask(ctx context.Context, sourceType string, remoteID string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(remoteID), 10, 64)
	if err != nil {
		return fmt.Errorf("upstream delete: remote id %q is not a numeric TorBox task id: %w", remoteID, err)
	}
	switch strings.ToLower(sourceType) {
	case "torrent":
		body := fmt.Sprintf(`{"operation":"delete","torrent_id":%d}`, id)
		_, err := c.do(ctx, http.MethodPost, "/api/torrents/controltorrent", strings.NewReader(body), "application/json", true)
		if err != nil {
			return fmt.Errorf("delete torrent: %w", err)
		}
		return nil
	case "nzb", "usenet":
		body := fmt.Sprintf(`{"operation":"delete","usenet_id":%d}`, id)
		_, err := c.do(ctx, http.MethodPost, "/api/usenet/controlusenetdownload", strings.NewReader(body), "application/json", true)
		if err != nil {
			return fmt.Errorf("delete usenet: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown source type for upstream delete: %q", sourceType)
	}
}

func attachFile(writer *multipart.Writer, field, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	part, err := writer.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("copy form file: %w", err)
	}
	return nil
}
