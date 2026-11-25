// Package claudemanagedruntime bridges Multica's daemon task protocol to the Claude Managed Agents sessions API.
package claudemanagedruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// errTooLarge marks a file download that exceeded the caller's limit.
var errTooLarge = errors.New("file exceeds size limit")

// DefaultAnthropicURL is the Claude Managed Agents API base.
const DefaultAnthropicURL = "https://api.anthropic.com/v1"

type api struct {
	base, token        string
	headers            map[string]string
	client             *http.Client
	detectWorkspaceKey bool
}
type apiError struct {
	Status int
	// RetryAfter is the server's Retry-After hint; zero when absent or unparsable.
	RetryAfter time.Duration
	// WorkspaceKeyRequired means Anthropic asked for anthropic-workspace-id,
	// i.e. the key is not workspace-scoped.
	WorkspaceKeyRequired bool
	// Code is the Multica API's error discriminator, when present. The body is
	// never surfaced; only this stable code is retained for classification.
	Code string
}

func (e *apiError) Error() string {
	if e.WorkspaceKeyRequired {
		return fmt.Sprintf("remote API returned HTTP %d: a workspace-scoped Anthropic API key is required", e.Status)
	}
	return fmt.Sprintf("remote API returned HTTP %d", e.Status)
}

// apiErrorDetails inspects an error body without ever returning it.
func apiErrorDetails(body io.Reader) (workspaceRequired bool, code string) {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Code string `json:"code"`
	}
	b, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	if json.Unmarshal(b, &envelope) != nil {
		return false, ""
	}
	return strings.Contains(strings.ToLower(envelope.Error.Message), "anthropic-workspace-id"), envelope.Code
}

const defaultRetryDelay = 30 * time.Second

// retryDelay honours a remote Retry-After hint and otherwise backs off by a fixed delay.
func retryDelay(err error) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter
	}
	return defaultRetryDelay
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// newAPI builds the Multica client; a non-empty workspace is sent as X-Workspace-ID.
func newAPI(base, token, workspace string) (*api, error) {
	headers := map[string]string{}
	if workspace != "" {
		headers["X-Workspace-ID"] = workspace
	}
	return newClient(base, token, headers, false, false)
}

func newAnthropicAPI(base, token string, profile Profile) (*api, error) {
	return newClient(base, token, map[string]string{"anthropic-version": "2023-06-01", "anthropic-beta": "managed-agents-2026-04-01"}, profile.AllowPlainHTTP, !profile.SkipWorkspaceKeyDetection)
}

func newClient(base, token string, headers map[string]string, allowPlainHTTP, detectWorkspaceKey bool) (*api, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid API base URL")
	}
	if u.Scheme == "http" && !allowPlainHTTP && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return nil, fmt.Errorf("API base URL must use HTTPS except on loopback")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("API token is required")
	}
	return &api{base: strings.TrimRight(base, "/"), token: token, headers: headers, detectWorkspaceKey: detectWorkspaceKey, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Errors deliberately omit remote bodies and URLs, which may echo credentials or task content.
func (a *api) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return a.do(ctx, method, path, "application/json", body, out)
}

// withBeta returns a client whose anthropic-beta header also names beta. The
// provider may require a different beta grant for a non-session endpoint.
func (a *api) withBeta(beta string) *api {
	clone := *a
	clone.headers = make(map[string]string, len(a.headers)+1)
	for name, value := range a.headers {
		clone.headers[name] = value
	}
	if existing := strings.TrimSpace(clone.headers["anthropic-beta"]); existing != "" {
		clone.headers["anthropic-beta"] = existing + ", " + beta
	} else {
		clone.headers["anthropic-beta"] = beta
	}
	return &clone
}

// upload sends one multipart file. The part carries contentType when it is
// known: the provider stores that header as the file's MIME type, so an image
// must not arrive as application/octet-stream.
func (a *api) upload(ctx context.Context, path, name, contentType string, data []byte, fields map[string]string, out any) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartName(name)))
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return a.do(ctx, http.MethodPost, path, w.FormDataContentType(), &body, out)
}

func escapeMultipartName(name string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "_", "\n", "_").Replace(name)
}

// download fetches at most limit bytes. A relative path uses this client's
// credentials; an absolute URL is a pre-signed storage link and gets none. The
// storage hop may redirect, so this follows redirects and Go drops the
// credential on a cross-host hop.
func (a *api) download(ctx context.Context, rawPath string, limit int64) ([]byte, error) {
	relative := strings.HasPrefix(rawPath, "/") && !strings.HasPrefix(rawPath, "//")
	target := rawPath
	if relative {
		target = a.base + rawPath
	} else if parsed, err := url.Parse(rawPath); err != nil || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && strings.HasPrefix(a.base, "http://"))) {
		return nil, fmt.Errorf("invalid download URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid download URL")
	}
	if relative {
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Accept", "application/octet-stream")
		for name, value := range a.headers {
			req.Header.Set(name, value)
		}
	}
	client := &http.Client{Timeout: 2 * time.Minute, Transport: a.client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("download transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		workspaceRequired, code := apiErrorDetails(resp.Body)
		return nil, &apiError{
			Status:               resp.StatusCode,
			RetryAfter:           parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			WorkspaceKeyRequired: a.detectWorkspaceKey && resp.StatusCode >= 400 && resp.StatusCode < 500 && workspaceRequired,
			Code:                 code,
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read API response failed")
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

func (a *api) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return fmt.Errorf("build API request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range a.headers {
		req.Header.Set(name, value)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("remote API transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		workspaceRequired, code := apiErrorDetails(resp.Body)
		return &apiError{
			Status:               resp.StatusCode,
			RetryAfter:           parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			WorkspaceKeyRequired: a.detectWorkspaceKey && resp.StatusCode >= 400 && resp.StatusCode < 500 && workspaceRequired,
			Code:                 code,
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return fmt.Errorf("read API response failed")
	}
	if len(b) > 8<<20 {
		return fmt.Errorf("API response exceeds 8 MiB")
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("invalid API JSON response")
		}
	}
	return nil
}
