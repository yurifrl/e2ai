// Tests for T11's log redaction: secret values become "[REDACTED <env name>]"
// markers in every logged message and attribute, and the request-log
// middleware's query string (which carries the ?e2ai= token) is redacted.
package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bufferLogger returns a logger writing JSON into the returned buffer.
func bufferLogger() (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	return buf, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestSecretReplacer replaces every occurrence of the value, keeps unrelated
// text, and marks which env var the redacted value came from.
func TestSecretReplacer(t *testing.T) {
	rep := NewSecretReplacer(map[string]string{
		"GITHUB_TOKEN":            "ghs_supersecret",
		"E2AI_REPORT_LINK_SECRET": "abc123",
		"EMPTY_SECRET":            "",
	})
	out := rep.Replace("token=ghs_supersecret and token2=ghs_supersecret then e2ai=abc123 end")
	if strings.Contains(out, "ghs_supersecret") || strings.Contains(out, "abc123") {
		t.Errorf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "[REDACTED GITHUB_TOKEN]") || !strings.Contains(out, "[REDACTED E2AI_REPORT_LINK_SECRET]") {
		t.Errorf("missing redaction markers: %s", out)
	}
	if out := rep.Replace("nothing to see here"); out != "nothing to see here" {
		t.Errorf("clean text mutated: %s", out)
	}
	// Longest match first: a value that is a prefix of another.
	rep2 := NewSecretReplacer(map[string]string{"A": "x", "B": "xyz"})
	if got := rep2.Replace("xyz x xyz"); got != "[REDACTED B] [REDACTED A] [REDACTED B]" {
		t.Errorf("prefix shadowing: %s", got)
	}
}

// TestRedactSecretsHandler proves the slog handler redacts messages and
// attribute values end to end.
func TestRedactSecretsHandler(t *testing.T) {
	buf, logger := bufferLogger()
	redacted := RedactSecrets(logger, map[string]string{"GITHUB_TOKEN": "ghs_leaky"})
	redacted.Info("fetched with token ghs_leaky ok", "header", "Bearer ghs_leaky", "clean", "untouched")
	line := buf.String()
	if strings.Contains(line, "ghs_leaky") {
		t.Fatalf("secret leaked: %s", line)
	}
	if !strings.Contains(line, "[REDACTED GITHUB_TOKEN]") || !strings.Contains(line, "untouched") {
		t.Errorf("redaction incomplete: %s", line)
	}
	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log line not valid json: %v (%s)", err, line)
	}
}

// TestRequestLogRedactsToken runs the request-log middleware behind a
// redacted logger: the request line lands and the ?e2ai= token does not.
func TestRequestLogRedactsToken(t *testing.T) {
	buf, logger := bufferLogger()
	redacted := RedactSecrets(logger, map[string]string{"E2AI_REPORT_LINK_SECRET": "tok123"})
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	RequestLog(redacted, next).ServeHTTP(rec, httptest.NewRequest("GET", "/artifacts/abc/shot.png?e2ai=tok123", nil))
	if !called {
		t.Fatal("next handler not called")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status passthrough: %d", rec.Code)
	}
	line := buf.String()
	if strings.Contains(line, "tok123") {
		t.Fatalf("token leaked into request log: %s", line)
	}
	for _, want := range []string{"artifacts/abc/shot.png", "GET", "418"} {
		if !strings.Contains(line, want) {
			t.Errorf("request log missing %q: %s", want, line)
		}
	}
}
