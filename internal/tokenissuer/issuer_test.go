package tokenissuer

import (
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testIssuer(t *testing.T, opts ...Option) *Issuer {
	t.Helper()
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	iss, err := New(priv, "https://hairpin.internal", "https://haybale.internal", 15*time.Minute, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return iss
}

// parseVerified parses token against iss's public key, requiring ES256
// and rejecting everything jwt.Parse would otherwise accept.
func parseVerified(t *testing.T, iss *Issuer, token string) *jwt.Token {
	t.Helper()
	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		return &iss.priv.PublicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}))
	if err != nil {
		t.Fatalf("parse minted token: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("minted token did not verify")
	}
	return parsed
}

func TestMintRoundTrip(t *testing.T) {
	iss := testIssuer(t)

	token, expiresAt, err := iss.Mint("hp-01j000000000000000000000", []string{"github.com/rxbynerd/*"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	parsed := parseVerified(t, iss, token)

	if got := parsed.Header["typ"]; got != tokenType {
		t.Errorf("typ = %v, want %q", got, tokenType)
	}
	if got := parsed.Header["kid"]; got != iss.KeyID() {
		t.Errorf("kid = %v, want %q", got, iss.KeyID())
	}
	if got := parsed.Header["alg"]; got != "ES256" {
		t.Errorf("alg = %v, want ES256", got)
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims type = %T, want jwt.MapClaims", parsed.Claims)
	}
	if claims["iss"] != "https://hairpin.internal" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if claims["aud"] != "https://haybale.internal" {
		t.Errorf("aud = %v, want a plain string", claims["aud"])
	}
	if claims["sub"] != "hp-01j000000000000000000000" {
		t.Errorf("sub = %v", claims["sub"])
	}
	jti, _ := claims["jti"].(string)
	if len(jti) != jtiBytes*2 { // hex-encoded
		t.Errorf("jti = %q, want %d hex characters", jti, jtiBytes*2)
	}

	scope, ok := claims[repoScopeClaim].([]any)
	if !ok || len(scope) != 1 || scope[0] != "github.com/rxbynerd/*" {
		t.Errorf("%s = %v, want [github.com/rxbynerd/*]", repoScopeClaim, claims[repoScopeClaim])
	}

	exp, err := parsed.Claims.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("GetExpirationTime: %v", err)
	}
	// The wire claim is Unix seconds; compare at that resolution rather
	// than against Mint's sub-second expiresAt.
	if exp.Unix() != expiresAt.Unix() {
		t.Errorf("token exp = %v, Mint returned %v", exp.Time, expiresAt)
	}
	if d := time.Until(expiresAt); d <= 0 || d > 15*time.Minute {
		t.Errorf("expiresAt %v not within the configured 15m TTL", expiresAt)
	}
}

func TestMintOmitsEmptyRepoScope(t *testing.T) {
	iss := testIssuer(t)

	for _, scope := range [][]string{nil, {}} {
		token, _, err := iss.Mint("hp-empty", scope)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		claims := parseVerified(t, iss, token).Claims.(jwt.MapClaims)
		if _, present := claims[repoScopeClaim]; present {
			t.Errorf("scope %v: %s claim present, want omitted entirely", scope, repoScopeClaim)
		}
	}
}

func TestMintRejectsWrongKey(t *testing.T) {
	iss := testIssuer(t)
	other := testIssuer(t)

	token, _, err := iss.Mint("hp-x", nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		return &other.priv.PublicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}))
	if err == nil {
		t.Fatal("token verified against a different issuer's key")
	}
}

func TestNewRejectsNonP256Key(t *testing.T) {
	p384 := mustCurveKey(t, elliptic.P384())
	if _, err := New(p384, "iss", "aud", time.Minute); err == nil {
		t.Fatal("New succeeded with a non-P-256 key")
	}
}

func TestNewRequiresIssuerAudienceTTL(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cases := []struct {
		name             string
		issuer, audience string
		ttl              time.Duration
	}{
		{"no issuer", "", "aud", time.Minute},
		{"no audience", "iss", "", time.Minute},
		{"non-positive ttl", "iss", "aud", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(priv, tc.issuer, tc.audience, tc.ttl); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestKeyIDStableAndDistinct(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	a, err := New(priv, "iss", "aud", time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := New(priv, "different-iss", "different-aud", time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.KeyID() != b.KeyID() {
		t.Errorf("kid depends on issuer/audience/ttl: %q vs %q, want equal (same key)", a.KeyID(), b.KeyID())
	}
	if kid, err := KeyID(&priv.PublicKey); err != nil || kid != a.KeyID() {
		t.Errorf("KeyID(pub) = %q, %v, want %q, nil", kid, err, a.KeyID())
	}

	other := testIssuer(t)
	if other.KeyID() == a.KeyID() {
		t.Error("two distinct keys produced the same kid")
	}
}

func TestJWKSShape(t *testing.T) {
	iss := testIssuer(t)

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(iss.JWKS(), &doc); err != nil {
		t.Fatalf("unmarshal JWKS: %v", err)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("JWKS has %d keys, want 1", len(doc.Keys))
	}
	k := doc.Keys[0]
	if k.Kty != "EC" || k.Crv != "P-256" || k.Alg != "ES256" || k.Use != "sig" {
		t.Errorf("key fields = %+v", k)
	}
	if k.Kid != iss.KeyID() {
		t.Errorf("kid = %q, want %q", k.Kid, iss.KeyID())
	}
	for name, v := range map[string]string{"x": k.X, "y": k.Y} {
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Errorf("%s is not valid unpadded base64url: %v", name, err)
		}
		if len(b) != coordSize {
			t.Errorf("%s decodes to %d bytes, want %d", name, len(b), coordSize)
		}
	}
}

func TestJWKSHandler(t *testing.T) {
	iss := testIssuer(t)
	srv := httptest.NewServer(iss.JWKSHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a max-age directive", cc)
	}

	postResp, err := http.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = postResp.Body.Close() }()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", postResp.StatusCode)
	}
}
