package gitea

import (
	"io"
	"strings"
	"testing"
)

func TestResponseLimitBody(t *testing.T) {
	t.Run("allows response at limit", func(t *testing.T) {
		body := &responseLimitBody{ReadCloser: io.NopCloser(strings.NewReader("123")), remaining: 3, maxBytes: 3}
		got, err := io.ReadAll(body)
		if err != nil || string(got) != "123" {
			t.Fatalf("ReadAll() = %q, %v; want %q, nil", got, err, "123")
		}
	})

	t.Run("rejects response over limit", func(t *testing.T) {
		body := &responseLimitBody{ReadCloser: io.NopCloser(strings.NewReader("1234")), remaining: 3, maxBytes: 3}
		if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "exceeds the 3-byte limit") {
			t.Fatalf("ReadAll() error = %v; want response-limit error", err)
		}
	})
}
