package proxy

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

// contextKey is an unexported type for context keys defined in this package.
type contextKey int

const (
	requestIDKey contextKey = iota
	requestAttrsKey
)

// requestAttrs carries the slog attributes that every log line for a request
// should carry. Stored in context so every function in the call chain can
// pull them without parameter threading.
type requestAttrs struct {
	Model      string
	Stream     bool
	WorkingDir string
}

// WithRequestID returns a context with the given request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext extracts the request ID, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithRequestAttrs returns a context with the given request-level slog attrs.
func WithRequestAttrs(ctx context.Context, attrs requestAttrs) context.Context {
	return context.WithValue(ctx, requestAttrsKey, attrs)
}

// requestAttrsFromContext extracts request attrs, or a zero-value.
func requestAttrsFromContext(ctx context.Context) requestAttrs {
	a, _ := ctx.Value(requestAttrsKey).(requestAttrs)
	return a
}

// ctxAttrs builds the slog attrs for the current request context.
// Always includes request_id. Adds model/stream/working_dir if available.
func ctxAttrs(ctx context.Context) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("request_id", RequestIDFromContext(ctx)),
	}
	a := requestAttrsFromContext(ctx)
	if a.Model != "" {
		attrs = append(attrs, slog.String("model", a.Model))
		attrs = append(attrs, slog.Bool("stream", a.Stream))
	}
	if a.WorkingDir != "" {
		attrs = append(attrs, slog.String("working_dir", a.WorkingDir))
	}
	return attrs
}

// ctxLogger returns a logger pre-loaded with the request context attrs.
func ctxLogger(ctx context.Context) *slog.Logger {
	return slog.New(slog.Default().Handler().WithAttrs(ctxAttrs(ctx)))
}

// --- Public convenience wrappers ---

// reqInfo logs at Info level with request context attrs.
func reqInfo(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx).InfoContext(ctx, msg, args...)
}

// reqWarn logs at Warn level with request context attrs.
func reqWarn(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx).WarnContext(ctx, msg, args...)
}

// reqError logs at Error level with request context attrs.
func reqError(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx).ErrorContext(ctx, msg, args...)
}

// reqDebug logs at Debug level with request context attrs.
func reqDebug(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx).DebugContext(ctx, msg, args...)
}

// --- HTTP response writer wrapper for status capture ---

// statusWriter wraps http.ResponseWriter to capture the status code.
// It delegates Flush and Hijack to the underlying writer so that
// streaming (http.Flusher) and WebSocket (http.Hijacker) type
// assertions in downstream handlers still work.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// --- Middleware ---

// RequestLoggingMiddleware injects a request ID into context and logs
// request start/end with method, path, status, and duration.
// The generated request ID is also set as the X-Request-Id response header.
func RequestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Honor client-sent request ID, or generate one.
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = "chatcmpl-" + uuid.New().String()[:29]
		}

		ctx := WithRequestID(r.Context(), reqID)
		r = r.WithContext(ctx)

		// Set on every response so callers can correlate without parsing the body.
		w.Header().Set("X-Request-Id", reqID)

		sw := newStatusWriter(w)
		next.ServeHTTP(sw, r)

		slog.Info("request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// --- Global logger setup (called from main) ---

// InitLogger configures the default slog logger.
// If PROXY_LOG_JSON is set (any non-empty value), output is JSON.
// Otherwise, text handler (human-readable, grep-friendly).
func InitLogger() {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if os.Getenv("PROXY_LOG_JSON") != "" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// EnableDebugLogging sets the slog level to Debug.
func EnableDebugLogging() {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if os.Getenv("PROXY_LOG_JSON") != "" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}
