// T11: log redaction for the api surface. Secret-bearing request logs are
// attacker-controlled data (the report link's ?e2ai= token rides every
// artifact request): this middleware redacts the values of the configured
// secrets before the logger ever sees the record, replacing each with a
// "[REDACTED <ENV_NAME>]" marker that still tells operators which env var
// the value came from.
package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SecretReplacer rewrites every occurrence of a secret's value into
// "[REDACTED <env name>]" — exact literal matching, so arbitrary secret
// characters need no escaping.
type SecretReplacer struct {
	entries []redactEntry
}

type redactEntry struct {
	value string
	name  string
}

// NewSecretReplacer builds a replacer over env name → value pairs. Empty
// values are skipped (nothing to redact); longer values match first so a
// value that is a prefix of another cannot shadow it.
func NewSecretReplacer(secrets map[string]string) *SecretReplacer {
	entries := make([]redactEntry, 0, len(secrets))
	for name, value := range secrets {
		if value == "" {
			continue
		}
		entries = append(entries, redactEntry{value: value, name: name})
	}
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].value) > len(entries[j].value)
	})
	return &SecretReplacer{entries: entries}
}

// Replace rewrites every secret occurrence in text.
func (s *SecretReplacer) Replace(text string) string {
	for _, e := range s.entries {
		text = strings.ReplaceAll(text, e.value, "[REDACTED "+e.name+"]")
	}
	return text
}

// RedactSecrets wraps the logger so every record (message and attribute
// values) passes through the replacer.
func RedactSecrets(logger *slog.Logger, secrets map[string]string) *slog.Logger {
	return slog.New(redactHandler{next: logger.Handler(), replace: NewSecretReplacer(secrets)})
}

// redactHandler applies the secret replacement to every message and
// attribute value before delegating.
type redactHandler struct {
	next    slog.Handler
	replace *SecretReplacer
}

func (h redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	// Attrs iterate by value, so the record is rebuilt: message and every
	// attribute pass through the replacer before delegating.
	n := slog.NewRecord(r.Time, r.Level, h.replace.Replace(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		n.AddAttrs(slog.Attr{Key: a.Key, Value: slog.AnyValue(h.replace.Replace(a.Value.String()))})
		return true
	})
	return h.next.Handle(ctx, n)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for _, a := range attrs {
		a.Value = slog.AnyValue(h.replace.Replace(a.Value.String()))
	}
	return redactHandler{next: h.next.WithAttrs(attrs), replace: h.replace}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{next: h.next.WithGroup(name), replace: h.replace}
}

// statusWriter captures the status code for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// RequestLog middleware: one info line per request (method, path, query,
// status, duration) through the given logger. The query string carries the
// signed ?e2ai= token on artifact/report requests, so the caller must pass
// a RedactSecrets-wrapped logger for the token to stay out of the logs.
func RequestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
