package api

import (
	"context"
	"strconv"
	"time"

	"codeintel/pkg/audit"
	"codeintel/internal/auth"
	"codeintel/internal/obs"
)

// auditEmitter returns the configured emitter or the no-op
// default. Centralised so handler call sites never see a nil and
// the wiring choice can change without touching every handler.
func (s *Server) auditEmitter() audit.Emitter {
	if s.cfg.AuditEmitter != nil {
		return s.cfg.AuditEmitter
	}
	return audit.NoopEmitter{}
}

// emitAudit fires a structured audit event. Failures are logged
// but never bubble to the caller — an audit-backend outage must
// never fail a successful business operation. The request context
// flows through so a slow emitter can be cancelled with the
// request.
func (s *Server) emitAudit(ctx context.Context, ev audit.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.RequestID == "" {
		ev.RequestID = obs.RequestIDFromContext(ctx)
	}
	if err := s.auditEmitter().Emit(ctx, ev); err != nil {
		s.cfg.Logger.With("logger", "audit").
			Error("audit emit failed", "err", err, "action", ev.Action, "orgId", ev.OrgID)
	}
}

// auditActor maps an AuthContext into the (actorID, actorType)
// pair an audit event records. API-key-authenticated callers
// register their user-id with actorType "api_key"; anonymous /
// session paths register an empty actorID with the matching kind.
func auditActor(authCtx auth.AuthContext) (string, audit.ActorType) {
	switch authCtx.AuthSource {
	case "api_key":
		return authCtx.UserID, audit.ActorApiKey
	case "anonymous":
		return "", audit.ActorSystem
	default:
		return authCtx.UserID, audit.ActorUser
	}
}

// targetIDFromInt is a small helper so handlers convert numeric
// row ids to the string-typed TargetID without repeating the
// strconv.Itoa idiom.
func targetIDFromInt(n int32) string {
	return strconv.FormatInt(int64(n), 10)
}
