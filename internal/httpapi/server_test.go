package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-at-least-32-bytes-long!!"

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func token(t *testing.T, method jwt.SigningMethod, key any, mutate func(*jwt.RegisteredClaims)) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		Subject:   "user-123",
		Audience:  jwt.ClaimStrings{"authenticated"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	if mutate != nil {
		mutate(&claims)
	}
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newHandler(v *Verifier, db Pinger) http.Handler {
	return New(Deps{DB: db, Auth: v, AllowedOrigins: []string{"http://localhost:3000"}}).Handler()
}

func TestHealth(t *testing.T) {
	v := &Verifier{}
	if got := do(newHandler(v, fakeDB{}), "GET", "/health", nil).Code; got != http.StatusOK {
		t.Errorf("healthy db: status %d, want 200", got)
	}
	if got := do(newHandler(v, fakeDB{errors.New("down")}), "GET", "/health", nil).Code; got != http.StatusServiceUnavailable {
		t.Errorf("db down: status %d, want 503", got)
	}
}

func TestAuthHS256(t *testing.T) {
	h := newHandler(&Verifier{secret: []byte(testSecret)}, fakeDB{})
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no header", nil, http.StatusUnauthorized},
		{"not a bearer header", map[string]string{"Authorization": "Basic abc"}, http.StatusUnauthorized},
		{"garbage token", bearer("nope"), http.StatusUnauthorized},
		{"valid", bearer(token(t, jwt.SigningMethodHS256, []byte(testSecret), nil)), http.StatusOK},
		{"wrong secret", bearer(token(t, jwt.SigningMethodHS256, []byte("another-secret-another-secret-123456"), nil)), http.StatusUnauthorized},
		{"expired", bearer(token(t, jwt.SigningMethodHS256, []byte(testSecret), func(c *jwt.RegisteredClaims) {
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		})), http.StatusUnauthorized},
		{"wrong audience", bearer(token(t, jwt.SigningMethodHS256, []byte(testSecret), func(c *jwt.RegisteredClaims) {
			c.Audience = jwt.ClaimStrings{"someone-else"}
		})), http.StatusUnauthorized},
		{"no expiry", bearer(token(t, jwt.SigningMethodHS256, []byte(testSecret), func(c *jwt.RegisteredClaims) {
			c.ExpiresAt = nil
		})), http.StatusUnauthorized},
		{"no subject", bearer(token(t, jwt.SigningMethodHS256, []byte(testSecret), func(c *jwt.RegisteredClaims) {
			c.Subject = ""
		})), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := do(h, "GET", "/api/me", tt.headers).Code; got != tt.want {
				t.Errorf("status %d, want %d", got, tt.want)
			}
		})
	}

	// The handler sees the user ID from the token.
	rec := do(h, "GET", "/api/me", bearer(token(t, jwt.SigningMethodHS256, []byte(testSecret), nil)))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["user_id"] != "user-123" {
		t.Errorf("body = %s, want user_id user-123", rec.Body.String())
	}
}

func TestAuthNotConfigured(t *testing.T) {
	h := newHandler(&Verifier{}, fakeDB{})
	if got := do(h, "GET", "/api/me", map[string]string{"Authorization": "Bearer x"}).Code; got != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", got)
	}
}

func TestAuthES256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "k1",
		"x": enc(key.PublicKey.X.FillBytes(make([]byte, 32))),
		"y": enc(key.PublicKey.Y.FillBytes(make([]byte, 32))),
	}}})
	kf, err := keyfunc.NewJWKSetJSON(jwks)
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(&Verifier{jwks: kf.Keyfunc}, fakeDB{})

	good := token(t, jwt.SigningMethodES256, key, nil)
	if got := do(h, "GET", "/api/me", map[string]string{"Authorization": "Bearer " + good}).Code; got != http.StatusOK {
		t.Errorf("valid ES256: status %d, want 200", got)
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	bad := token(t, jwt.SigningMethodES256, other, nil)
	if got := do(h, "GET", "/api/me", map[string]string{"Authorization": "Bearer " + bad}).Code; got != http.StatusUnauthorized {
		t.Errorf("ES256 signed with another key: status %d, want 401", got)
	}

	// An HS256 token must not be accepted when only a JWKS is configured.
	hs := token(t, jwt.SigningMethodHS256, []byte(testSecret), nil)
	if got := do(h, "GET", "/api/me", map[string]string{"Authorization": "Bearer " + hs}).Code; got != http.StatusUnauthorized {
		t.Errorf("HS256 with no secret configured: status %d, want 401", got)
	}
}

func TestCORS(t *testing.T) {
	h := newHandler(&Verifier{}, fakeDB{})

	rec := do(h, "OPTIONS", "/api/me", map[string]string{
		"Origin": "http://localhost:3000", "Access-Control-Request-Method": "GET",
	})
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("allowed origin header = %q", got)
	}

	rec = do(h, "GET", "/health", map[string]string{"Origin": "https://evil.example"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin got Allow-Origin %q, want none", got)
	}
}
