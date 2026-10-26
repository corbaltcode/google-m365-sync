// Package httpx is a small JSON-over-HTTP helper with retry on throttling and
// transient server errors. Both Google and Graph return Retry-After on 429.
// It is used instead of the official Google and Graph SDKs; see "Dependencies"
// in README.md.
//
// POST is not idempotent here (it creates Entra users), so it is retried only
// on 429, which means the request was rejected unprocessed. After a network
// error or 5xx the create may already have happened, and a retry would fail
// with "already exists" and mark a successful create as failed; the next
// scheduled run sees the user and reconciles instead.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// StatusError is returned for non-2xx responses. Body holds the raw response,
// which for Graph contains error.code / error.message — the detail that
// "BadRequest" summaries throw away.
type StatusError struct {
	Method, URL string
	Status      int
	Body        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// Client wraps http.Client with bearer auth and retries.
type Client struct {
	HTTP       *http.Client
	Token      func(ctx context.Context) (string, error) // nil = no auth
	MaxRetries int                                       // default 5
	Headers    map[string]string

	// wait pauses between retries; tests replace it. It returns an error
	// instead of waiting when ctx ends or its deadline is too close.
	wait func(ctx context.Context, d time.Duration) error
}

// maxRetryAfter caps a server-supplied Retry-After.
const maxRetryAfter = 60 * time.Second

// DoJSON sends in (if non-nil) as JSON and decodes a JSON response into out
// (if non-nil). An empty body is an error when out is non-nil, and fine
// (e.g. 204 to a PATCH) when out is nil.
func (c *Client) DoJSON(ctx context.Context, method, url string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	retries := c.MaxRetries
	if retries == 0 {
		retries = 5
	}
	wait := c.wait
	if wait == nil {
		wait = waitCtx
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}

	idempotent := method != http.MethodPost

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}
		if c.Token != nil {
			tok, err := c.Token(ctx)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}

		resp, err := hc.Do(req)
		if err != nil {
			if idempotent && attempt < retries && wait(ctx, backoff(attempt, "", time.Now())) == nil {
				continue
			}
			return fmt.Errorf("%s %s: %w", method, url, err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			// A cut-off body must never pass as a short or empty page: a
			// partial Google listing would look like deleted users. Retry
			// like a network error (the request may already have run).
			if idempotent && attempt < retries && wait(ctx, backoff(attempt, "", time.Now())) == nil {
				continue
			}
			return fmt.Errorf("%s %s: HTTP %d: read response: %w", method, url, resp.StatusCode, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests || (idempotent && resp.StatusCode >= 500) {
			// If the wait can't happen (deadline), fall through and return
			// this response's error.
			if attempt < retries && wait(ctx, backoff(attempt, resp.Header.Get("Retry-After"), time.Now())) == nil {
				continue
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return &StatusError{Method: method, URL: url, Status: resp.StatusCode, Body: string(data)}
		}
		if out != nil {
			if len(data) == 0 {
				return fmt.Errorf("%s %s: HTTP %d: empty response body, expected JSON", method, url, resp.StatusCode)
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s %s: decode: %w", method, url, err)
			}
		}
		return nil
	}
}

// backoff honors Retry-After (seconds or an HTTP date, capped at
// maxRetryAfter), else waits exponentially up to 30s.
func backoff(attempt int, retryAfter string, now time.Time) time.Duration {
	if s, err := strconv.Atoi(retryAfter); err == nil && s > 0 {
		return min(time.Duration(s)*time.Second, maxRetryAfter)
	}
	if t, err := http.ParseTime(retryAfter); err == nil {
		return min(max(t.Sub(now), 0), maxRetryAfter)
	}
	return min(time.Duration(1<<attempt)*time.Second, 30*time.Second)
}

// waitCtx sleeps for d unless ctx ends first. It fails immediately if ctx's
// deadline is closer than d, so the caller can return the last error rather
// than being cut off mid-wait.
func waitCtx(ctx context.Context, d time.Duration) error {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		return context.DeadlineExceeded
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
