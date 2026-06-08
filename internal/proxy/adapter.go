package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
	"github.com/bermudi/cmd-code-proxy/internal/version"
)

const defaultBaseURL = "https://api.commandcode.ai"
const defaultTimeout = 300 * time.Second

const (
	maxRetries     = 3
	baseRetryDelay = 1 * time.Second
	maxRetryDelay  = 30 * time.Second
)

const modelsCacheTTL = 5 * time.Minute

// ccAdapter implements Upstream by calling the real CommandCode API.
type ccAdapter struct {
	client          *http.Client
	baseURL         string
	debug           bool
	versionProvider version.Provider

	modelsCache     []api.OpenAIModel
	modelsCacheTime time.Time
	modelsCacheMu   sync.RWMutex
}

// NewCCAdapter creates a real upstream adapter.
func NewCCAdapter() *ccAdapter {
	return &ccAdapter{
		baseURL:         defaultBaseURL,
		client:          &http.Client{Timeout: defaultTimeout},
		versionProvider: version.NewNPMProvider(),
	}
}

// WithDebug enables debug logging on the adapter.
func (a *ccAdapter) WithDebug(debug bool) *ccAdapter {
	a.debug = debug
	return a
}

func (a *ccAdapter) debugf(format string, args ...any) {
	if a.debug {
		log.Printf(format, args...)
	}
}

// Generate implements Upstream.
func (a *ccAdapter) Generate(ctx context.Context, ccBody api.CCRequestBody, apiKey string) (io.ReadCloser, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := calculateBackoff(attempt)
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		ccReq, err := a.createUpstreamRequest(ctx, ccBody, apiKey)
		if err != nil {
			return nil, err
		}

		resp, err := a.client.Do(ccReq)
		if err != nil {
			lastErr = err
			a.debugf("[DEBUG] Request failed (attempt %d/%d): %v", attempt+1, maxRetries+1, err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode < 600) {
			lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if retryAfter > 0 {
				a.debugf("[DEBUG] Upstream %d, Retry-After=%s, retrying (%d/%d): %s", resp.StatusCode, retryAfter, attempt+1, maxRetries+1, string(body))
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(retryAfter):
				}
			} else {
				a.debugf("[DEBUG] Upstream %d, retrying (%d/%d): %s", resp.StatusCode, attempt+1, maxRetries+1, string(body))
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: string(body)}
		}

		return resp.Body, nil
	}
	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// FetchModels implements Upstream. Returns all models unfiltered.
func (a *ccAdapter) FetchModels(ctx context.Context, apiKey string) ([]api.OpenAIModel, error) {
	a.modelsCacheMu.RLock()
	if len(a.modelsCache) > 0 && time.Since(a.modelsCacheTime) < modelsCacheTTL {
		cached := a.modelsCache
		a.modelsCacheMu.RUnlock()
		return cached, nil
	}
	a.modelsCacheMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	var modelList api.OpenAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&modelList); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}

	models := make([]api.OpenAIModel, len(modelList.Data))
	for i, m := range modelList.Data {
		owner := m.OwnedBy
		if owner == "" {
			owner = extractOwner(m.ID)
		}
		models[i] = api.OpenAIModel{
			ID:      m.ID,
			Object:  "model",
			Created: 0,
			OwnedBy: owner,
		}
	}

	a.modelsCacheMu.Lock()
	a.modelsCache = models
	a.modelsCacheTime = time.Now()
	a.modelsCacheMu.Unlock()

	return models, nil
}

func (a *ccAdapter) createUpstreamRequest(ctx context.Context, ccBody api.CCRequestBody, apiKey string) (*http.Request, error) {
	reqJSON, err := json.Marshal(ccBody)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	a.debugf("[DEBUG] CommandCode request body: %s", truncateLog(string(reqJSON)))

	ccReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/alpha/generate", bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	ccReq.Header.Set("Content-Type", "application/json")
	ccReq.Header.Set("Authorization", "Bearer "+apiKey)
	ccReq.Header.Set("x-command-code-version", a.versionProvider.Get())
	ccReq.Header.Set("x-cli-environment", "production")
	ccReq.Header.Set("x-project-slug", projectSlugFromPath(ccBody.Config.WorkingDir))
	ccReq.Header.Set("x-taste-learning", "true")
	ccReq.Header.Set("x-co-flag", "false")
	ccReq.Header.Set("Accept", "text/event-stream")

	return ccReq, nil
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if sec, err := strconv.Atoi(value); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		until := time.Until(t)
		if until > 0 {
			return until
		}
	}
	return 0
}

func calculateBackoff(attempt int) time.Duration {
	base := baseRetryDelay * time.Duration(1<<attempt)    // 1s, 2s, 4s
	jitter := time.Duration(rand.Int63n(int64(base) / 5)) // ±20%
	return base + jitter
}

func projectSlugFromPath(pathName string) string {
	var b strings.Builder
	lastWasDash := false

	pathName = strings.ToLower(pathName)
	if len(pathName) >= 2 && pathName[1] == ':' && pathName[0] >= 'a' && pathName[0] <= 'z' {
		pathName = pathName[2:]
	}

	for _, r := range pathName {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			lastWasDash = false
			continue
		}
		if !lastWasDash && b.Len() > 0 {
			b.WriteByte('-')
			lastWasDash = true
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "project"
	}
	return slug
}
