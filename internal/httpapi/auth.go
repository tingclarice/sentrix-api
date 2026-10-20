package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

type ctxKey int

const userIDKey ctxKey = 0

// UserID returns the authenticated Supabase user's ID (the JWT "sub" claim).
func UserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userIDKey).(string)
	return id, ok && id != ""
}

// Verifier checks Supabase access tokens. HS256 tokens are verified with the shared
// secret; asymmetric tokens are verified against the project's JWKS. Either side may
// be absent; tokens that need a missing side are rejected.
type Verifier struct {
	secret []byte
	jwks   jwt.Keyfunc
}

// NewVerifier builds a Verifier. supabaseURL is the project URL
// (https://<ref>.supabase.co); its JWKS is fetched in the background until ctx ends.
func NewVerifier(ctx context.Context, secret, supabaseURL string) (*Verifier, error) {
	v := &Verifier{secret: []byte(secret)}
	if supabaseURL != "" {
		kf, err := keyfunc.NewDefaultCtx(ctx, []string{supabaseURL + "/auth/v1/.well-known/jwks.json"})
		if err != nil {
			return nil, fmt.Errorf("load JWKS: %w", err)
		}
		v.jwks = kf.Keyfunc
	}
	return v, nil
}

// Configured reports whether the verifier can accept any token at all.
func (v *Verifier) Configured() bool { return len(v.secret) > 0 || v.jwks != nil }

func (v *Verifier) keyfunc(t *jwt.Token) (any, error) {
	switch t.Method.Alg() {
	case "HS256":
		if len(v.secret) == 0 {
			return nil, errors.New("HS256 token but no SUPABASE_JWT_SECRET configured")
		}
		return v.secret, nil
	case "ES256", "RS256":
		if v.jwks == nil {
			return nil, errors.New("asymmetric token but no SUPABASE_URL configured")
		}
		return v.jwks(t)
	default:
		return nil, fmt.Errorf("unexpected signing method %s", t.Method.Alg())
	}
}

// Verify returns the user ID from a valid, unexpired access token.
func (v *Verifier) Verify(token string) (string, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(token, &claims, v.keyfunc,
		jwt.WithValidMethods([]string{"HS256", "ES256", "RS256"}),
		jwt.WithAudience("authenticated"),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", err
	}
	if claims.Subject == "" {
		return "", errors.New("token has no subject")
	}
	return claims.Subject, nil
}

// RequireAuth rejects requests without a valid "Authorization: Bearer <jwt>" header
// and stores the user ID in the request context for handlers.
func (v *Verifier) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !v.Configured() {
			writeError(w, http.StatusServiceUnavailable, "auth is not configured on the server")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		id, err := v.Verify(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, id)))
	})
}
