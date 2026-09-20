// Package httpx carries the BFF-injected identity contract: the admin-bff
// terminates the user session and forwards X-User/X-Permissions headers, and
// this middleware reconstructs the caller identity from them. Dev fallback
// mirrors incidents-admin's DevFallbackUser path for un-proxied local traffic.
package httpx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	UserHeader        = "X-User"
	PermissionsHeader = "X-Permissions"
)

// UserOf returns the X-User header value, falling back to "you" for requests
// that arrive without identity headers (dev and un-proxied traffic).
func UserOf(r *http.Request) string {
	if user := r.Header.Get(UserHeader); user != "" {
		return user
	}
	return "you"
}

// Permission is the e2ai permission ladder. The naive (v0-parity) access model:
// server-side requests are authenticated by the BFF's Okta wall, and tiering
// happens in the frontend, which hides elements per the viewer's shell roles.
type Permission string

const (
	RunViewPermission  Permission = "admin.e2ai.view"
	RunEditPermission  Permission = "admin.e2ai.edit"
	RunAdminPermission Permission = "admin.e2ai.admin"
)

type RequestIdentity struct {
	User        string
	Permissions map[Permission]struct{}
}

type IdentityConfig struct {
	DevFallbackUser string
}

// AllPermissions is the full e2ai permission ladder.
func AllPermissions() map[Permission]struct{} {
	return map[Permission]struct{}{
		RunViewPermission:  {},
		RunEditPermission:  {},
		RunAdminPermission: {},
	}
}

type identityContextKey struct{}

// IdentityWithConfig attaches the request identity to the context.
func IdentityWithConfig(config IdentityConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := RequestIdentity{
			User:        strings.TrimSpace(r.Header.Get(UserHeader)),
			Permissions: parsePermissions(r.Header.Get(PermissionsHeader)),
		}
		if identity.User == "" {
			// No BFF-injected identity: derive the caller from the Okta access
			// token the BFF authenticated and forwarded. The payload is decoded
			// without signature verification on purpose — the api's sole ingress
			// is the BFF's Okta wall, so anything that arrives here already
			// passed it (v0 behavior).
			if user, ok := bearerUser(r); ok {
				identity.User = user
				if len(identity.Permissions) == 0 {
					identity.Permissions = AllPermissions()
				}
			}
		}
		if config.DevFallbackUser != "" && (identity.User == "" || identity.User == config.DevFallbackUser) {
			identity.User = config.DevFallbackUser
			identity.Permissions = AllPermissions()
		}
		ctx := context.WithValue(r.Context(), identityContextKey{}, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerUser extracts the caller identity from an "Authorization: Bearer <jwt>"
// header: email claim, falling back to preferred_username and sub. Anything
// malformed or claim-less yields ("", false).
func bearerUser(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimSpace(auth[len(prefix):]), ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	for _, candidate := range []string{claims.Email, claims.PreferredUsername, claims.Sub} {
		if candidate != "" {
			return candidate, true
		}
	}
	return "", false
}

func IdentityFromContext(ctx context.Context) (RequestIdentity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(RequestIdentity)
	return identity, ok
}

func (identity RequestIdentity) HasPermission(permission Permission) bool {
	_, ok := identity.Permissions[permission]
	return ok
}

func parsePermissions(header string) map[Permission]struct{} {
	permissions := make(map[Permission]struct{})
	for _, raw := range strings.Split(header, ",") {
		if p := Permission(strings.TrimSpace(raw)); p != "" {
			permissions[p] = struct{}{}
		}
	}
	return permissions
}
