package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryableStatus(t *testing.T) {
	ok := []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, c := range ok {
		if !retryableStatus(c) {
			t.Errorf("retryableStatus(%d) = false, want true", c)
		}
	}
	bad := []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusUnauthorized, 501}
	for _, c := range bad {
		if retryableStatus(c) {
			t.Errorf("retryableStatus(%d) = true, want false", c)
		}
	}
}

func TestPostJSONRetriesThenSucceeds(t *testing.T) {
	old := DefaultRetry
	DefaultRetry = RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxRetryAfter: time.Second}
	defer func() { DefaultRetry = old }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	resp, err := postJSON(context.Background(), srv.URL, nil, map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("postJSON err = %v, want nil after retries", err)
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestPostJSONAllAttemptsFail(t *testing.T) {
	old := DefaultRetry
	DefaultRetry = RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxRetryAfter: time.Second}
	defer func() { DefaultRetry = old }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{"x": 1})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("postJSON err = %v, want 503 after exhausting retries", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(2)
	defer l.Stop()

	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 桶已空，第三次应在 100ms 内因 ctx 超时返回错误。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("third Wait returned nil, want ctx deadline error")
	}
}

func TestLimiterDisabled(t *testing.T) {
	l := NewLimiter(0)
	defer l.Stop()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("disabled limiter Wait err = %v, want nil", err)
	}
}
