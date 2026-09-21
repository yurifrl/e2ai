// Package mcp is the T5 MCP read-only surface (MCP-01): a Streamable-HTTP
// JSON-RPC endpoint letting coding agents in PRs query "what failed and why"
// over run history and evidence. Tools wrap the existing runs/flakes/trends
// queries; every response cites run ids and carries token-gated evidence
// links. There are no mutation tools — the surface never mutates the app
// under test nor e2ai state (report-only).
package mcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
)

// maxRequestBytes bounds the JSON-RPC body (tool calls are tiny; anything
// larger is abuse or a broken client).
const maxRequestBytes = 1 << 20

// Surface is the MCP endpoint for main.go's mux.
type Surface struct {
	// Pool is the shared read pool; nil leaves the surface inert (mounted
	// but every tool call errors, matching the degraded-posture pattern of
	// the other surfaces when Postgres is down).
	Pool *pgxpool.Pool

	// RunnerToken is E2AI_RUNNER_SERVICE_TOKEN; empty disables the
	// service-token path (only BFF identity admits callers).
	RunnerToken string

	// ReportBase is the public admin origin evidence links point at; empty
	// means no link base is known (links are omitted, not fabricated).
	ReportBase string

	// LinkSecret is E2AI_REPORT_LINK_SECRET; token-gated links verify the
	// same way the report surface's gate does.
	LinkSecret string
}

// New builds the surface for main.go's mux.
func New(pool *pgxpool.Pool, runnerToken, reportBase, linkSecret string) *Surface {
	return &Surface{Pool: pool, RunnerToken: runnerToken, ReportBase: reportBase, LinkSecret: linkSecret}
}

// Mount registers the MCP endpoint. Method prefixes keep writes off the
// surface: Streamable-HTTP transports send the JSON-RPC session over POST;
// GET (SSE stream in the spec) is unsupported here and answered 405.
func (s *Surface) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /mcp", s.Handler)
	mux.HandleFunc("GET /mcp", s.unsupportedGet)
}

// Handler serves POST /mcp: gate, parse the JSON-RPC envelope, dispatch.
func (s *Surface) Handler(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r) {
		return
	}
	s.serveRPC(w, r)
}

// gate enforces the surface's auth posture: a BFF identity holding the
// run-view permission, or (for MCP clients outside the admin BFF) the runner
// service token. Identity-wall pattern from agents/http.go; 401/403 are
// answered pre-JSON-RPC.
func (s *Surface) gate(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case s.serviceToken(r):
		return true
	default:
		identity, ok := httpx.IdentityFromContext(r.Context())
		switch {
		case !ok:
			writeRPCError(w, http.StatusUnauthorized, nil, codeInvalidRequest, "unauthorized")
			return false
		case !identity.HasPermission(httpx.RunViewPermission):
			writeRPCError(w, http.StatusForbidden, nil, codeInvalidRequest, "forbidden")
			return false
		default:
			return true
		}
	}
}

func (s *Surface) serviceToken(r *http.Request) bool {
	if s.RunnerToken == "" {
		return false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.HasPrefix(auth, prefix) && auth[len(prefix):] == s.RunnerToken
}

// unsupportedGet answers the spec's GET/SSE stream with 405: this transport
// is strictly request/response.
func (s *Surface) unsupportedGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "mcp: use POST /mcp for JSON-RPC requests"})
}
