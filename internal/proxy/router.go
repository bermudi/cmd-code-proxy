package proxy

import (
	"log"
	"net/http"
	"time"
)

// NewRouter builds the HTTP mux with all routes bound to the proxy's handlers.
func NewRouter(p *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", logger(p.HandleChatCompletions))
	mux.HandleFunc("/chat/completions", logger(p.HandleChatCompletions))
	mux.HandleFunc("/v1/responses", logger(p.HandleResponses))
	mux.HandleFunc("/v1/models", logger(p.HandleModels))
	mux.HandleFunc("/health", logger(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	return mux
}

// logger is a middleware for logging requests.
func logger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[%s] %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next(w, r)
		log.Printf("[%s] %s done in %v", r.Method, r.URL.Path, time.Since(start))
	}
}
