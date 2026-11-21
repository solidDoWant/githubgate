package proxy

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/model"
)

// auditEntry is what one proxied request's audit line says. It never holds
// a credential, a header value, the path or the query.
type auditEntry struct {
	started        time.Time
	caller         auth.Identity
	host           string
	method         string
	decided        Decided
	decisionResult *Decision
	reason         string
	githubStatus   string
	status         int
	bytes          int64
}

func (e *auditEntry) decision() string {
	if e.decisionResult != nil && e.decisionResult.Allowed() && e.reason == "allowed" {
		return "allowed"
	}

	return "refused"
}

// userAuthorization is the authorization an allowed user request is
// forwarded for, or nil.
func (e *auditEntry) userAuthorization() *model.UserAuthorization {
	if e.decisionResult == nil || e.decisionResult.Access != AccessUser {
		return nil
	}

	return e.decisionResult.UserAuthorization
}

func (e *auditEntry) write(ctx context.Context, logger *slog.Logger) {
	repositories := []string{}
	permissionSet := ""
	installation := ""
	rules := []string{}

	access := ""
	userAuthorization, login := "", ""

	if user := e.userAuthorization(); user != nil {
		userAuthorization, login = user.String(), user.Login
	}

	if result := e.decisionResult; result != nil && result.Allowed() {
		access = string(result.Access)
		repositories = result.Repositories
		installation = installationName(result.Installation)

		parts := make([]string, 0, len(result.Permissions))
		for name, level := range result.Permissions {
			parts = append(parts, name+":"+string(level))
		}

		sort.Strings(parts)
		permissionSet = strings.Join(parts, ",")

		for _, rule := range result.Rules {
			rules = append(rules, rule.Policy.String()+"/"+rule.Name)
		}
	}

	// A public read names the repository it reads, not the ones its token
	// is limited to; a refused request names the repository it asked for.
	switch {
	case (access == string(AccessPublicRead) || access == "") && e.decided.Repo != "":
		repositories = []string{unescaped(e.decided.Owner) + "/" + unescaped(e.decided.Repo)}
	case access == string(AccessPublicRead):
		repositories = []string{}
	}

	refs := e.decided.Refs
	if refs == nil {
		refs = []string{}
	}

	mutations := e.decided.Mutations
	if mutations == nil {
		mutations = []string{}
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "proxy request",
		slog.String("request_id", requestID()),
		slog.String("mode", "proxy"),
		slog.String("namespace", e.caller.Namespace),
		slog.String("service_account", e.caller.ServiceAccount),
		slog.String("pod", e.caller.Pod),
		slog.String("host", e.host),
		slog.String("method", e.method),
		slog.String("kind", string(e.decided.Kind)),
		slog.String("access", access),
		slog.String("operation", e.decided.Operation),
		slog.Any("mutations", mutations),
		slog.Any("refs", refs),
		slog.Any("repositories", repositories),
		slog.String("permissions", permissionSet),
		slog.String("installation", installation),
		slog.Any("rules", rules),
		slog.String("user_authorization", userAuthorization),
		slog.String("login", login),
		slog.String("decision", e.decision()),
		slog.String("reason", e.reason),
		slog.Int("status", e.status),
		slog.String("github_status", e.githubStatus),
		slog.Int64("bytes", e.bytes),
		slog.Duration("duration", time.Since(e.started)),
	)
}

func installationName(installation *model.Installation) string {
	if installation == nil {
		return ""
	}

	return installation.String()
}

func requestID() string {
	var raw [8]byte

	_, _ = rand.Read(raw[:])

	return hex.EncodeToString(raw[:])
}

// systemPoolWith returns the system roots plus the PEM certificates in pem.
func systemPoolWith(pem []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("the upstream CA file holds no PEM certificate")
	}

	return pool, nil
}

// auditRefusal writes the audit line of a request the handler refused
// before it reached the forwarder.
func auditRefusal(ctx context.Context, logger *slog.Logger, started time.Time, caller auth.Identity, host string, request *http.Request, failure *refusal) {
	entry := &auditEntry{
		started: started, caller: caller, host: host, method: request.Method,
		reason: failure.code, githubStatus: "not_called", status: failure.status,
	}
	entry.write(ctx, logger)
}
