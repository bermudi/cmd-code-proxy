package version

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Provider returns the current CommandCode CLI version string.
type Provider interface {
	Get() string
}

// NPMProvider fetches the latest version from the npm registry with caching.
type NPMProvider struct {
	mu       sync.RWMutex
	cached   string
	valid    bool
	cachedAt time.Time
	cacheTTL time.Duration
	client   *http.Client
}

// NewNPMProvider returns a Provider that polls the npm registry.
func NewNPMProvider() *NPMProvider {
	return &NPMProvider{
		cacheTTL: 30 * time.Minute,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// NewNPMProviderWithClient returns a Provider using the given HTTP client (for tests).
func NewNPMProviderWithClient(client *http.Client) *NPMProvider {
	p := NewNPMProvider()
	p.client = client
	return p
}

// Get returns the latest version, using the cache when valid.
func (p *NPMProvider) Get() string {
	p.mu.RLock()
	if p.valid && time.Since(p.cachedAt) < p.cacheTTL {
		v := p.cached
		p.mu.RUnlock()
		return v
	}
	p.mu.RUnlock()

	v, err := p.fetch()
	if err != nil {
		// Return stale cache on error, or unknown
		p.mu.RLock()
		if p.valid {
			v = p.cached
			p.mu.RUnlock()
			return v
		}
		p.mu.RUnlock()
		return "unknown"
	}

	p.mu.Lock()
	p.cached = v
	p.valid = true
	p.cachedAt = time.Now()
	p.mu.Unlock()

	return v
}

func (p *NPMProvider) fetch() (string, error) {
	resp, err := p.client.Get("https://registry.npmjs.org/command-code/latest")
	if err != nil {
		return "", fmt.Errorf("failed to fetch version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("npm registry returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse version: %w", err)
	}

	v, ok := result["version"].(string)
	if !ok {
		return "", fmt.Errorf("version field not found or invalid")
	}
	return v, nil
}

// StaticProvider returns a fixed version string. For tests.
type StaticProvider struct {
	Version string
}

// Get returns the fixed version.
func (s *StaticProvider) Get() string { return s.Version }
