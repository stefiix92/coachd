// Package mcpserver serves coachd's tools over MCP (streamable HTTP,
// stateless, protocol 2026-07-28 and 2025-11-25): Garmin reads, request_sync,
// the goal and notes, and workout proposals. Tools return measurements, never
// advice, and none can approve, deliver, withdraw or decline a proposal.
package mcpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/ingest"
	"github.com/stefiix92/coachd/internal/proposals"
	"github.com/stefiix92/coachd/internal/store"
	"github.com/stefiix92/coachd/internal/telemetry"
	"github.com/stefiix92/coachd/internal/workout"
)

// maxBodyBytes caps an MCP request body.
const maxBodyBytes = 1 << 20

// SyncRequester starts an agent-requested Garmin sync (ingest.Service).
type SyncRequester interface {
	RequestSync(ctx context.Context) ingest.RequestResult
}

// Deps are the server's collaborators. Nil Log discards, nil Tracer is a
// no-op, nil Now is time.Now, nil TZ is UTC and nil Proposals is a
// proposals.Service over DB with workout.DefaultLimits.
type Deps struct {
	DB        *store.DB
	Sync      SyncRequester
	Proposals *proposals.Service
	TZ        *time.Location
	Now       func() time.Time
	// KeyExpires is the last day the Garmin key in use is valid; nil or a
	// zero date means unknown.
	KeyExpires func() civil.Date
	DataDir    string
	Metrics    *telemetry.Metrics
	Audit      auth.Auditor
	Log        *slog.Logger
	Tracer     trace.Tracer
	// Messages is told when the agent stores a message; nil means no push.
	Messages MessageNotifier
}

// MessageNotifier is told that the agent left a message. It never blocks and
// limits its own pushes.
type MessageNotifier interface{ AgentMessage() }

// svc holds the defaulted Deps the tool handlers run against.
type svc struct{ Deps }

// New builds the MCP server with all tools registered.
func New(d Deps, version string) *mcp.Server {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Tracer == nil {
		d.Tracer = noop.NewTracerProvider().Tracer("")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.TZ == nil {
		d.TZ = time.UTC
	}
	if d.Proposals == nil {
		d.Proposals = proposals.New(proposals.Deps{DB: d.DB, TZ: d.TZ, Now: d.Now, Limits: workout.DefaultLimits, Log: d.Log, Metrics: d.Metrics})
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "coachd", Version: version},
		&mcp.ServerOptions{SupportedProtocolVersions: []string{"2026-07-28", "2025-11-25"}})
	(&svc{d}).register(srv)
	return srv
}

// Handler serves s over stateless streamable HTTP with the body capped at 1 MiB.
// Callers must always put auth.Middleware in front: tools read the identity from
// the request context. go-sdk's localhost (DNS-rebinding) guard stays on; it
// only fires when the listener's local address is loopback while Host is not
// (for example a same-netns proxy), so it is no substitute for auth.
func Handler(s *mcp.Server) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		h.ServeHTTP(w, r)
	})
}
