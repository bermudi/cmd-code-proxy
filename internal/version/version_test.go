package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNPMProvider_FetchesVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"version": "3.2.1"})
	}))
	defer srv.Close()

	p := NewNPMProviderWithClient(srv.Client())
	p.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = srv.Listener.Addr().String()
		return srv.Client().Transport.RoundTrip(req)
	})}

	got := p.Get()
	if got != "3.2.1" {
		t.Errorf("Get() = %q, want %q", got, "3.2.1")
	}
}

func TestNPMProvider_CachesResult(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"version": "1.0.0"})
	}))
	defer srv.Close()

	p := NewNPMProviderWithClient(srv.Client())
	p.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = srv.Listener.Addr().String()
		return srv.Client().Transport.RoundTrip(req)
	})}

	v1 := p.Get()
	v2 := p.Get()
	if v1 != "1.0.0" || v2 != "1.0.0" {
		t.Errorf("Get() = %q, %q; want both 1.0.0", v1, v2)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (should be cached)", calls)
	}
}

func TestNPMProvider_RefreshesAfterTTL(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"version": "2.0.0"})
	}))
	defer srv.Close()

	p := NewNPMProviderWithClient(srv.Client())
	p.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = srv.Listener.Addr().String()
		return srv.Client().Transport.RoundTrip(req)
	})}
	p.cacheTTL = 1 * time.Millisecond

	p.Get()
	time.Sleep(5 * time.Millisecond)
	p.Get()

	if calls != 2 {
		t.Errorf("calls = %d, want 2 (cache should have expired)", calls)
	}
}

func TestNPMProvider_ReturnsUnknownOnFirstFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewNPMProviderWithClient(srv.Client())
	p.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = srv.Listener.Addr().String()
		return srv.Client().Transport.RoundTrip(req)
	})}

	got := p.Get()
	if got != "unknown" {
		t.Errorf("Get() = %q, want %q on first failure", got, "unknown")
	}
}

func TestNPMProvider_ReturnsStaleOnSubsequentFailure(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"version": "4.0.0"})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	p := NewNPMProviderWithClient(srv.Client())
	p.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = srv.Listener.Addr().String()
		return srv.Client().Transport.RoundTrip(req)
	})}
	p.cacheTTL = 1 * time.Millisecond

	v1 := p.Get()
	if v1 != "4.0.0" {
		t.Fatalf("first Get() = %q, want %q", v1, "4.0.0")
	}

	time.Sleep(5 * time.Millisecond)
	v2 := p.Get()
	if v2 != "4.0.0" {
		t.Errorf("second Get() = %q, want stale %q on upstream failure", v2, "4.0.0")
	}
}

func TestStaticProvider(t *testing.T) {
	s := &StaticProvider{Version: "9.9.9"}
	if got := s.Get(); got != "9.9.9" {
		t.Errorf("Get() = %q, want %q", got, "9.9.9")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
