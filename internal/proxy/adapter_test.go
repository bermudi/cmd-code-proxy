package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
	"github.com/bermudi/cmd-code-proxy/internal/version"
)

// stubProvider returns "test" — used in adapter tests that don't test version behavior.
var stubProvider = &version.StaticProvider{Version: "test"}

func TestCCAdapter_Generate_Success(t *testing.T) {
	ndjson := `{"type":"text-delta","text":"hello"}
{"type":"finish","finishReason":"stop"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		if v := r.Header.Get("x-command-code-version"); v == "" {
			t.Error("x-command-code-version header is empty")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ndjson))
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	body, err := a.Generate(context.Background(), api.CCRequestBody{}, "test-key", true, "sess_test0123456789")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	defer body.Close()

	data, _ := io.ReadAll(body)
	if string(data) != ndjson {
		t.Errorf("body = %q, want %q", string(data), ndjson)
	}
}

func TestCCAdapter_Generate_RetriesOn429(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("slow down"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"finish","finishReason":"stop"}`))
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	body, err := a.Generate(context.Background(), api.CCRequestBody{}, "key", true, "sess_test0123456789")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body.Close()

	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("attempts = %d, want 3", atomic.LoadInt32(&attempts))
	}
}

func TestCCAdapter_Generate_RetriesOn5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("broken"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"finish","finishReason":"stop"}`))
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	body, err := a.Generate(context.Background(), api.CCRequestBody{}, "key", true, "sess_test0123456789")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body.Close()

	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("attempts = %d, want 2", atomic.LoadInt32(&attempts))
	}
}

func TestCCAdapter_Generate_ExceedsMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	_, err := a.Generate(context.Background(), api.CCRequestBody{}, "key", true, "sess_test0123456789")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCCAdapter_Generate_UpstreamErrorOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	_, err := a.Generate(context.Background(), api.CCRequestBody{}, "key", true, "sess_test0123456789")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
	if ue.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", ue.StatusCode)
	}
	if ue.Body != `{"error":"bad"}` {
		t.Errorf("Body = %q", ue.Body)
	}
}

func TestCCAdapter_Generate_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := a.Generate(ctx, api.CCRequestBody{}, "key", true, "sess_test0123456789")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestCCAdapter_FetchModels_CachesResult(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(api.OpenAIModelList{
			Object: "list",
			Data: []api.OpenAIModel{
				{ID: "test/model", Object: "model", OwnedBy: "test"},
			},
		})
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	models1, err := a.FetchModels(context.Background(), "key")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models1) != 1 {
		t.Fatalf("models1 = %d, want 1", len(models1))
	}

	models2, err := a.FetchModels(context.Background(), "key")
	if err != nil {
		t.Fatalf("FetchModels cache: %v", err)
	}
	if len(models2) != 1 {
		t.Fatalf("models2 = %d, want 1", len(models2))
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (should be cached)", atomic.LoadInt32(&calls))
	}
}

func TestCCAdapter_FetchModels_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: stubProvider,
	}

	_, err := a.FetchModels(context.Background(), "key")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestNewCCAdapter(t *testing.T) {
	a := NewCCAdapter()
	if a.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", a.baseURL, defaultBaseURL)
	}
	if a.client == nil {
		t.Error("client is nil")
	}
	if a.versionProvider == nil {
		t.Error("versionProvider is nil")
	}
}

func TestUpstreamError_Error(t *testing.T) {
	ue := &UpstreamError{StatusCode: 429, Body: "rate limited"}
	want := "upstream status 429: rate limited"
	if ue.Error() != want {
		t.Errorf("Error() = %q, want %q", ue.Error(), want)
	}
}

func TestCCAdapter_VersionHeader_Injected(t *testing.T) {
	const wantVersion = "1.2.3-test"

	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("x-command-code-version")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"finish","finishReason":"stop"}`))
	}))
	defer srv.Close()

	a := &ccAdapter{
		client:          &http.Client{Timeout: 10 * time.Second},
		baseURL:         srv.URL,
		versionProvider: &version.StaticProvider{Version: wantVersion},
	}

	body, err := a.Generate(context.Background(), api.CCRequestBody{}, "key", true, "sess_test0123456789")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body.Close()

	if gotVersion != wantVersion {
		t.Errorf("x-command-code-version = %q, want %q", gotVersion, wantVersion)
	}
}
