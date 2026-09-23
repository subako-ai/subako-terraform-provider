// Package client speaks the Subako public API: the subset of `/v1` the
// provider manages. Shapes mirror crates/core-server/openapi/public.json.
package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// CODESYNC(workspace-header)
const workspaceHeader = "Kikuvi-Workspace"

// CODESYNC(idempotency-key-header)
const idempotencyHeader = "Idempotency-Key"

// Token markers, as the server dispatches on them.
const (
	// CODESYNC(credential-marker)
	APIKeyMarker = "sbk_ak_"
	// CODESYNC(credential-marker)
	AccessTokenMarker = "sbk_at_"
)

// How long one attempt may take, and how a repeatable call is repeated.
const (
	requestTimeout = 60 * time.Second
	// Two retries, so a repeatable call is attempted three times.
	maxRetries   = 2
	retryWaitMin = 500 * time.Millisecond
	retryWaitMax = 5 * time.Second
)

// Client is one configured connection to a Subako server.
type Client struct {
	base        *url.URL
	auth        Auth
	workspaceID string
	userAgent   string
	// The two retry policies a request is sent under, picked by its method.
	retrying *retryablehttp.Client
	once     *retryablehttp.Client
}

// Config is what a Client needs to reach a server.
type Config struct {
	Server string
	Auth   Auth
	// WorkspaceID names the workspace collection routes act in. A user token
	// needs one; an API key implies its own.
	WorkspaceID string
	UserAgent   string
}

// New validates cfg and builds a Client.
func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.Server, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("server must be an http(s) URL, got %q", cfg.Server)
	}
	if cfg.Auth == nil {
		return nil, errors.New("a token is required")
	}
	if !cfg.Auth.boundToAWorkspace() && cfg.WorkspaceID == "" {
		return nil, errors.New("a user access token needs a workspace id")
	}
	transport := &http.Client{Timeout: requestTimeout}
	return &Client{
		base:        base,
		auth:        cfg.Auth,
		workspaceID: cfg.WorkspaceID,
		userAgent:   cfg.UserAgent,
		retrying:    retrier(transport, maxRetries, checkRetry),
		once:        retrier(transport, 0, checkOnce),
	}, nil
}

// retrier is the retry library under one policy. Both clients share the
// transport, so they share its connection pool.
func retrier(transport *http.Client, retries int, check retryablehttp.CheckRetry) *retryablehttp.Client {
	return &retryablehttp.Client{
		HTTPClient:   transport,
		RetryWaitMin: retryWaitMin,
		RetryWaitMax: retryWaitMax,
		RetryMax:     retries,
		CheckRetry:   check,
		Backoff:      retryablehttp.DefaultBackoff,
		// The last response answers the call, so its body still decodes into
		// the error envelope rather than "giving up after N attempts".
		ErrorHandler: retryablehttp.PassthroughErrorHandler,
	}
}

// checkRetry is the policy a repeatable call is sent under: a transport
// error, a 429, or a 5xx is worth another attempt.
func checkRetry(ctx context.Context, res *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return true, nil
	}
	return res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500, nil
}

// checkOnce is the policy a call that must not be repeated is sent under:
// whatever the one attempt answers is the answer.
func checkOnce(ctx context.Context, _ *http.Response, _ error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, nil
}

// httpFor answers the client a method is sent through. PATCH is the one
// method this API offers no repeat-safe form of, so it is sent once; every
// other method is either naturally idempotent or carries an idempotency key.
func (c *Client) httpFor(method string) *retryablehttp.Client {
	if method == http.MethodPatch {
		return c.once
	}
	return c.retrying
}

// WorkspaceID is the workspace the client was configured with, or empty.
func (c *Client) WorkspaceID() string {
	return c.workspaceID
}

// Error is the server's error envelope, with the status it came with.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("subako: HTTP %d", e.Status)
	}
	return fmt.Sprintf("subako: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// IsNotFound reports whether err is the server saying the resource is gone.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

type envelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// request is one call to the API.
type request struct {
	method      string
	path        string
	query       url.Values
	body        []byte
	contentType string
}

func jsonRequest(method, path string, body any) (request, error) {
	req := request{method: method, path: path}
	if body == nil {
		return req, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return req, fmt.Errorf("encode request body: %w", err)
	}
	req.body = encoded
	req.contentType = "application/json"
	return req, nil
}

// newIdempotencyKey names one logical call. crypto/rand.Text draws from the
// same source as the rest of crypto/rand and cannot fail.
func newIdempotencyKey() string {
	return "tf-" + rand.Text()
}

// do sends req and decodes a 2xx body into out, when out is non-nil.
func (c *Client) do(ctx context.Context, req request, out any) error {
	status, body, err := c.send(ctx, req)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return decodeError(status, body)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", req.method, req.path, err)
	}
	return nil
}

// send makes one call, retried under its method's policy. The request is
// built once, so every attempt carries the same headers and the same body.
func (c *Client) send(ctx context.Context, req request) (int, []byte, error) {
	target := c.base.JoinPath(req.path)
	if len(req.query) > 0 {
		target.RawQuery = req.query.Encode()
	}
	var body any
	if req.body != nil {
		body = req.body
	}
	httpReq, err := retryablehttp.NewRequestWithContext(ctx, req.method, target.String(), body)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s %s: %w", req.method, req.path, err)
	}
	bearer, err := c.auth.bearer(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", req.method, req.path, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+bearer)
	httpReq.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		httpReq.Header.Set("User-Agent", c.userAgent)
	}
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	// Collection routes resolve the workspace from this header. By-id routes
	// never read it, and the only header an API key refuses is one naming
	// another workspace, so every request may carry it.
	if c.workspaceID != "" {
		httpReq.Header.Set(workspaceHeader, c.workspaceID)
	}
	// One key per logical call, reused by its retries, so a repeated POST is
	// one operation to the server.
	if req.method == http.MethodPost {
		httpReq.Header.Set(idempotencyHeader, newIdempotencyKey())
	}

	res, err := c.httpFor(req.method).Do(httpReq)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", req.method, req.path, err)
	}
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s %s response: %w", req.method, req.path, err)
	}
	return res.StatusCode, payload, nil
}

func decodeError(status int, body []byte) error {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		return &Error{Status: status, Message: strings.TrimSpace(string(body))}
	}
	return &Error{Status: status, Code: env.Error.Code, Message: env.Error.Message}
}

// Page is one page of a keyset listing.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// CODESYNC(page-limit)
const pageLimit = 200

// listAll walks every page of a listing.
func listAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	query := url.Values{"limit": {fmt.Sprint(pageLimit)}, "order": {"asc"}}
	for {
		var page Page[T]
		err := c.do(ctx, request{method: http.MethodGet, path: path, query: query}, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			return all, nil
		}
		query.Set("after", *page.NextCursor)
	}
}

// latest is the newest item of a listing, or nil when it is empty.
func latest[T any](ctx context.Context, c *Client, path string) (*T, error) {
	query := url.Values{"limit": {"1"}, "order": {"desc"}}
	var page Page[T]
	if err := c.do(ctx, request{method: http.MethodGet, path: path, query: query}, &page); err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, nil
	}
	return &page.Items[0], nil
}
