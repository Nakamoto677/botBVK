package telegram

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalFilePreservesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":45}}`))
	}))
	defer server.Close()
	c := NewClient("test")
	c.BaseURL = server.URL
	path := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	err := c.SendLocalFile(t.Context(), 1, 0, path, "attachment.txt")
	var api *APIError
	if !errors.As(err, &api) || api.Code != 429 || api.RetryAfter != 45 {
		t.Fatalf("upload lost Telegram retry delay: %#v", err)
	}
}
