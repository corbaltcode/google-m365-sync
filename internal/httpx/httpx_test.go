package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// countingServer replies with each status in turn, then 200 {}.
func countingServer(t *testing.T, statuses ...int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= len(statuses) {
			w.WriteHeader(statuses[calls-1])
			w.Write([]byte(`{"error":{"code":"x"}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func noWait(context.Context, time.Duration) error { return nil }

func TestRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		method    string
		statuses  []int
		wantCalls int
		wantErr   bool
	}{
		{"GET retried on 5xx", http.MethodGet, []int{503, 500}, 3, false},
		{"PATCH retried on 5xx", http.MethodPatch, []int{502}, 2, false},
		{"POST retried on 429", http.MethodPost, []int{429, 429}, 3, false},
		{"POST not retried on 5xx", http.MethodPost, []int{500}, 1, true},
		{"4xx never retried", http.MethodGet, []int{400}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := countingServer(t, tc.statuses...)
			c := &Client{wait: noWait}
			err := c.DoJSON(context.Background(), tc.method, srv.URL, map[string]string{"a": "b"}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if *calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", *calls, tc.wantCalls)
			}
		})
	}
}

func TestPOSTNotRetriedOnNetworkError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Drop the connection after the request arrives, as if the response
		// were lost after the server acted on it.
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := &Client{wait: noWait}
	if err := c.DoJSON(context.Background(), http.MethodPost, srv.URL, map[string]string{"a": "b"}, nil); err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Fatalf("POST sent %d times, want 1", calls)
	}

	calls = 0
	c.DoJSON(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if calls != 6 {
		t.Fatalf("GET sent %d times, want 6 (1 + 5 retries)", calls)
	}
}

func TestBackoff(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		attempt    int
		retryAfter string
		want       time.Duration
	}{
		{"seconds", 0, "7", 7 * time.Second},
		{"seconds capped", 0, "3600", maxRetryAfter},
		{"HTTP date", 0, now.Add(12 * time.Second).Format(http.TimeFormat), 12 * time.Second},
		{"HTTP date capped", 0, now.Add(time.Hour).Format(http.TimeFormat), maxRetryAfter},
		{"HTTP date in the past", 0, now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"exponential", 3, "", 8 * time.Second},
		{"exponential capped", 10, "", 30 * time.Second},
		{"garbage falls back", 1, "soon", 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backoff(tc.attempt, tc.retryAfter, now); got != tc.want {
				t.Fatalf("backoff = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryStopsAtDeadline(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// The real wait: a 30s Retry-After can't fit in a 1s deadline, so the
	// 429 is returned at once instead of sleeping past the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := (&Client{}).DoJSON(ctx, http.MethodGet, srv.URL, nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the 429", err)
	}
	if calls != 1 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("calls = %d after %v; should give up without waiting", calls, time.Since(start))
	}
}

func TestWaitCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitCtx(ctx, time.Hour); err == nil {
		t.Fatal("waitCtx ignored a cancelled context")
	}
}

// truncatingServer promises a 100-byte 200 response, sends one byte, and
// drops the connection, so reading the body fails.
func truncatingServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		conn, buf, _ := w.(http.Hijacker).Hijack()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
		buf.Flush()
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestBodyReadErrorIsNotSuccess(t *testing.T) {
	srv, calls := truncatingServer(t)
	var out struct{ Users []string }
	c := &Client{wait: noWait}

	if err := c.DoJSON(context.Background(), http.MethodGet, srv.URL, nil, &out); err == nil {
		t.Fatal("GET with a cut-off body succeeded")
	}
	if *calls != 6 {
		t.Errorf("GET sent %d times, want 6 (retried)", *calls)
	}

	*calls = 0
	if err := c.DoJSON(context.Background(), http.MethodPost, srv.URL, map[string]string{"a": "b"}, &out); err == nil {
		t.Fatal("POST with a cut-off body succeeded")
	}
	if *calls != 1 {
		t.Errorf("POST sent %d times, want 1", *calls)
	}
}

func TestEmptyBodyWhenJSONExpected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // no body
	}))
	defer srv.Close()
	c := &Client{wait: noWait}

	var out struct{ NextPageToken string }
	if err := c.DoJSON(context.Background(), http.MethodGet, srv.URL, nil, &out); err == nil {
		t.Fatal("empty 200 accepted as an empty page")
	}
	// A PATCH that expects no body (204-style) is still fine.
	if err := c.DoJSON(context.Background(), http.MethodPatch, srv.URL, map[string]string{"a": "b"}, nil); err != nil {
		t.Fatalf("empty reply to PATCH rejected: %v", err)
	}
}
