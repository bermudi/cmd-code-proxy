package proxy

import (
	"context"
	"fmt"
	"io"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// Upstream is the seam between handler logic and the CommandCode API.
// The real adapter sends HTTP with retries, headers, and version-spoofing.
// Tests inject a fake that returns canned NDJSON.
type Upstream interface {
	// Generate calls CommandCode /alpha/generate and returns the raw response body.
	// The caller is responsible for closing the returned ReadCloser.
	// A non-nil *UpstreamError means the server responded with a non-200 status.
	Generate(ctx context.Context, ccBody api.CCRequestBody, apiKey string) (io.ReadCloser, error)

	// FetchModels returns the available model list from upstream.
	// Returns unfiltered results; the caller applies filtering.
	FetchModels(ctx context.Context, apiKey string) ([]api.OpenAIModel, error)
}

// UpstreamError carries a non-200 HTTP status and body from the CommandCode API.
type UpstreamError struct {
	StatusCode int
	Body       string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.StatusCode, e.Body)
}
