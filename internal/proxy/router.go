package proxy

import "net/http"

// NewRouter builds the HTTP mux with all routes bound to the proxy's handlers.
// RequestLoggingMiddleware wraps the entire mux, injecting request IDs and
// logging start/end with method, path, status, and duration.
func NewRouter(p *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", p.HandleChatCompletions)
	mux.HandleFunc("/chat/completions", p.HandleChatCompletions)
	mux.HandleFunc("/v1/responses", p.HandleResponses)
	mux.HandleFunc("/v1/models", p.HandleModels)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
	return RequestLoggingMiddleware(mux)
}
