// Package fourshared implements provider.Provider for 4shared using its v1_2
// REST API. Unlike MEGA, 4shared has no password-based API login: every request
// is signed with OAuth 1.0a using an app-level consumer key/secret plus a
// per-account access token obtained once via the authorize helper
// (cmd/fourshared-auth). Those credentials are supplied at construction; Login
// only validates that a token is present.
//
// NOTE: 4shared's API is sparsely documented. The endpoints and response shapes
// below follow the published v1_2 reference, but field names are decoded
// defensively and this provider should be validated end-to-end against a real
// authorized account.
package fourshared

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/provider/oauth1"
)

const (
	apiBase    = "https://api.4shared.com/v1_2"
	uploadBase = "https://upload.4shared.com/v1_2"
)

// Client is a 4shared API client bound to one account's OAuth access token.
type Client struct {
	signer    *oauth1.Signer
	http      *http.Client
	chunkSize int64
	debug     bool
	limiter   provider.RateLimiter
}

// New returns a 4shared client. consumerKey/consumerSecret are the application
// credentials for this account; token/tokenSecret are the per-account access
// token from the authorize step. chunkSize controls the size of each
// Content-Range upload part. limiter (may be nil) paces every API request and
// upload chunk. Set FOURSHARED_DEBUG=1 to log signed requests and raw responses
// for troubleshooting auth.
func New(chunkSize int64, limiter provider.RateLimiter, consumerKey, consumerSecret, token, tokenSecret string) *Client {
	if chunkSize <= 0 {
		chunkSize = 100 << 20 // 100MB fallback
	}
	debug := os.Getenv("FOURSHARED_DEBUG") != ""
	return &Client{
		signer: &oauth1.Signer{
			ConsumerKey:    consumerKey,
			ConsumerSecret: consumerSecret,
			Token:          token,
			TokenSecret:    tokenSecret,
			Debug:          debug,
		},
		http: &http.Client{
			Timeout: 0, // no overall timeout: uploads are long
			// 4shared signals "resume incomplete" with 308, which Go's client
			// would otherwise treat as a redirect and re-send the chunk body to
			// the Location. Stop after the first response so we read the 308 as-is.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && strings.Contains(via[0].URL.Path, "/upload/") {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
		chunkSize: chunkSize,
		debug:     debug,
		limiter:   limiter,
	}
}

func (c *Client) Name() string { return "fourshared" }

// do paces the request through the rate limiter (one request token) and sends
// it. A nil limiter is a no-op. The request's own context drives the wait.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.limiter != nil {
		if err := c.limiter.WaitRequest(req.Context()); err != nil {
			return nil, err
		}
	}
	return c.http.Do(req)
}

func (c *Client) Login(ctx context.Context, email, password string) error {
	if c.signer.ConsumerKey == "" || c.signer.ConsumerSecret == "" {
		return fmt.Errorf("4shared app credentials missing (set FOURSHARED_CONSUMER_KEY/SECRET in .env)")
	}
	if c.signer.Token == "" || c.signer.TokenSecret == "" {
		return fmt.Errorf("4shared account %q not authorized: run cmd/fourshared-auth and add its OAuth token to .env", email)
	}
	// A cheap signed call confirms the credentials are accepted.
	if _, _, err := c.GetQuota(ctx); err != nil {
		return fmt.Errorf("4shared auth check failed for %q: %w", email, err)
	}
	return nil
}

// user mirrors the fields we consume from GET /user.
type user struct {
	RootFolderID string `json:"rootFolderId"`
	TotalSpace   int64  `json:"totalSpace"`
	FreeSpace    int64  `json:"freeSpace"`
}

func (c *Client) getUser(ctx context.Context) (*user, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/user", nil)
	if err != nil {
		return nil, err
	}
	c.signer.Sign(req, nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /user: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if c.debug {
		slog.Info("4shared GET /user response", "status", resp.StatusCode, "body", string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /user: 4shared returned %d: %s", resp.StatusCode, string(body))
	}
	var u user
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("decoding /user: %w", err)
	}
	return &u, nil
}

func (c *Client) GetQuota(ctx context.Context) (int64, int64, error) {
	u, err := c.getUser(ctx)
	if err != nil {
		return 0, 0, err
	}
	used := u.TotalSpace - u.FreeSpace
	if used < 0 {
		used = 0
	}
	return u.TotalSpace, used, nil
}

// uploadInit mirrors the FileResponse from POST /upload, whose id is the file's
// permanent id (reused for every chunk and for later download/delete).
type uploadInit struct {
	ID            string `json:"id"`
	ReceivedBytes int64  `json:"receivedBytes"`
}

func (c *Client) Upload(ctx context.Context, localPath, remoteName string, onProgress func(provider.Progress)) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", localPath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", localPath, err)
	}
	size := info.Size()

	u, err := c.getUser(ctx)
	if err != nil {
		return "", err
	}

	// Start (or, server-side, find) the upload session. allowReplace=true lets it
	// recover from a residual same-name file (see startUpload).
	init, err := c.startUpload(ctx, u.RootFolderID, remoteName, size, true)
	if err != nil {
		return "", err
	}

	// The file id is established by POST /upload and kept for the file's life;
	// chunk uploads only stream bytes (they return 308 while incomplete, 201 on
	// completion, and carry no id).
	if init.ID == "" {
		return "", fmt.Errorf("4shared upload init returned no file id")
	}

	chunksTotal := int((size + c.chunkSize - 1) / c.chunkSize)
	offset := init.ReceivedBytes // resume point reported by the server, if any
	buf := make([]byte, c.chunkSize)
	for offset < size {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := c.chunkSize
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		if _, err := f.ReadAt(buf[:n], offset); err != nil && err != io.EOF {
			return "", fmt.Errorf("reading at %d: %w", offset, err)
		}
		if err := c.uploadChunk(ctx, init.ID, buf[:n], offset, size); err != nil {
			return "", err
		}
		offset += n
		if onProgress != nil {
			onProgress(provider.Progress{
				UploadedBytes:  offset,
				TotalBytes:     size,
				ChunksUploaded: int((offset + c.chunkSize - 1) / c.chunkSize),
				ChunksTotal:    chunksTotal,
			})
		}
	}
	return init.ID, nil
}

// startUpload opens (or server-side resumes) an upload session for name in
// folderID. Unlike MEGA, 4shared rejects a second file with an existing name at
// upload-init time with 403.0201 ("already exists") instead of overwriting. The
// conflict pre-check (handlers.resolveConflicts) normally deletes a duplicate
// before the job is queued, but it can miss one — e.g. a prior upload that
// failed partway leaves the name reserved without showing in the folder listing.
// Because a queued job means the user already opted to upload to this account
// (choosing "skip" creates no job), allowReplace lets startUpload delete the
// existing same-name file and retry the init once rather than failing the job.
func (c *Client) startUpload(ctx context.Context, folderID, name string, size int64, allowReplace bool) (*uploadInit, error) {
	// 4shared requires POST/PUT parameters in the request body (form-encoded),
	// not the query string (error 400.0504 otherwise). Form params are also part
	// of the OAuth signature base string, so set req.Form for the signer.
	form := url.Values{}
	form.Set("name", name)
	form.Set("folderId", folderID)
	form.Set("size", strconv.FormatInt(size, 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadBase+"/upload", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Form = form // include the body params in the OAuth signature
	c.signer.Sign(req, nil)

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /upload: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if c.debug {
		slog.Info("4shared POST /upload response", "status", resp.StatusCode, "body", string(body))
	}
	if resp.StatusCode == http.StatusForbidden && isAlreadyExists(body) {
		if !allowReplace {
			return nil, fmt.Errorf("POST /upload: 4shared still reports %q exists after replace attempt: %s", name, string(body))
		}
		// A same-name file is occupying this name. Find and delete it, then retry
		// the init once (allowReplace=false so we don't loop).
		ref, found, ferr := c.FindByName(ctx, name)
		if ferr != nil {
			return nil, fmt.Errorf("POST /upload: %q already exists and the lookup to replace it failed: %w", name, ferr)
		}
		if !found {
			return nil, fmt.Errorf("POST /upload: 4shared reports %q already exists but it is not in the folder listing to replace "+
				"(likely an incomplete prior upload reserving the name); delete it from the 4shared web UI and retry: %s", name, string(body))
		}
		if derr := c.Delete(ctx, ref); derr != nil {
			return nil, fmt.Errorf("POST /upload: %q already exists and could not be replaced: %w", name, derr)
		}
		return c.startUpload(ctx, folderID, name, size, false)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("POST /upload: 4shared returned %d: %s", resp.StatusCode, string(body))
	}
	var init uploadInit
	if err := json.Unmarshal(body, &init); err != nil {
		return nil, fmt.Errorf("decoding upload init %q: %w", string(body), err)
	}
	if init.ID == "" {
		return nil, fmt.Errorf("4shared upload init returned no id: %s", string(body))
	}
	return &init, nil
}

// uploadChunk streams one chunk. 4shared returns 308 ("resume incomplete") for
// intermediate chunks and 200/201 when the upload is complete; neither carries a
// file id (the id comes from the init call). It returns an error only on an
// unexpected status.
func (c *Client) uploadChunk(ctx context.Context, uploadID string, chunk []byte, offset, total int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadBase+"/upload/"+uploadID, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	end := offset + int64(len(chunk)) - 1
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, total))
	req.Header.Set("Content-Length", strconv.Itoa(len(chunk)))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk))
	c.signer.Sign(req, nil)

	// Pace bandwidth before sending the chunk body (do() handles the request
	// token). A nil limiter is a no-op.
	if c.limiter != nil {
		if err := c.limiter.WaitBytes(ctx, len(chunk)); err != nil {
			return err
		}
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("POST /upload/%s: %w", uploadID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if c.debug {
		slog.Info("4shared chunk response", "status", resp.StatusCode, "body", string(body))
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusPermanentRedirect: // 200, 201, 308
		return nil
	default:
		return fmt.Errorf("POST /upload/%s: 4shared returned %d: %s", uploadID, resp.StatusCode, string(body))
	}
}

func (c *Client) Download(ctx context.Context, remoteRef string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/files/"+remoteRef+"/download", nil)
	if err != nil {
		return err
	}
	c.signer.Sign(req, nil)
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("GET /files/%s/download: %w", remoteRef, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError("download", resp)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("streaming download: %w", err)
	}
	return nil
}

// fileEntry mirrors the fields we consume from a folder's file listing.
type fileEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// folderFilesPaths are the candidate URL templates (relative to apiBase) for
// listing a folder's files. 4shared's docs are inconsistent about whether the
// folder resource is singular ("folder") or plural ("folders"), and the wrong
// one returns 404 ("Resource not found", 404.0400). listFolderFiles tries each
// in order and uses the first that doesn't 404, so we work regardless of which
// spelling the account's API actually honours.
var folderFilesPaths = []string{"/folders/%s/files", "/folder/%s/files"}

// FindByName lists the account's root folder and returns the id of the first
// file whose name matches.
func (c *Client) FindByName(ctx context.Context, name string) (string, bool, error) {
	u, err := c.getUser(ctx)
	if err != nil {
		return "", false, err
	}
	files, err := c.listFolderFiles(ctx, u.RootFolderID)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		if f.Name == name {
			return f.ID, true, nil
		}
	}
	return "", false, nil
}

// listFolderFiles returns the files in folderID, probing the candidate path
// spellings until one responds non-404. The response is decoded defensively: it
// may be a bare array or an object with a "files" array.
func (c *Client) listFolderFiles(ctx context.Context, folderID string) ([]fileEntry, error) {
	var lastErr error
	for _, tmpl := range folderFilesPaths {
		path := fmt.Sprintf(tmpl, folderID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
		if err != nil {
			return nil, err
		}
		c.signer.Sign(req, nil)
		resp, err := c.do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if c.debug {
			slog.Info("4shared folder files response", "path", path, "status", resp.StatusCode, "body", string(body))
		}
		if resp.StatusCode == http.StatusNotFound {
			// Wrong spelling for this API; remember the error and try the next.
			lastErr = fmt.Errorf("GET %s: 4shared returned 404: %s", path, string(body))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: 4shared returned %d: %s", path, resp.StatusCode, string(body))
		}
		files, err := decodeFileList(body)
		if err != nil {
			return nil, fmt.Errorf("decoding %s %q: %w", path, string(body), err)
		}
		slog.Info("4shared folder listing path resolved", "path", path)
		return files, nil
	}
	return nil, lastErr
}

// decodeFileList parses a folder's file listing, which 4shared may return as a
// bare array or as an object wrapping a "files" array.
func decodeFileList(body []byte) ([]fileEntry, error) {
	var files []fileEntry
	if err := json.Unmarshal(body, &files); err == nil {
		return files, nil
	}
	var wrapped struct {
		Files []fileEntry `json:"files"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Files, nil
}

func (c *Client) Delete(ctx context.Context, remoteRef string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, apiBase+"/files/"+remoteRef, nil)
	if err != nil {
		return err
	}
	c.signer.Sign(req, nil)
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("DELETE /files/%s: %w", remoteRef, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return apiError("delete", resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func apiError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("%s: 4shared returned %d: %s", op, resp.StatusCode, string(body))
}

// isAlreadyExists reports whether a 4shared error body is the "name already
// exists" rejection (code 403.0201) returned by upload-init for a duplicate name.
func isAlreadyExists(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "403.0201") || strings.Contains(s, "already exists")
}
