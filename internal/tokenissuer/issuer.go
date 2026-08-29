package tokenissuer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenType is the JWT header "typ" haybale requires on a sandbox
// identity token, per RFC 9068 (OAuth 2.0 access tokens).
const tokenType = "at+jwt"

// repoScopeClaim carries the haybale.dev repo-access grant. Omitted
// entirely when a job's scope is empty — haybale's default-deny policy
// treats an absent or empty claim as "no repos", not "all repos".
const repoScopeClaim = "haybale.dev/repos"

// jtiBytes is the length of a minted token's random jti claim.
const jtiBytes = 16

// Issuer mints sandbox identity tokens under one ES256 signing key and
// serves the matching JWKS document.
type Issuer struct {
	priv     *ecdsa.PrivateKey
	kid      string
	jwks     []byte
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time
}

// Option adjusts an Issuer at construction.
type Option func(*Issuer)

// WithClock overrides the Issuer's time source. Tests only.
func WithClock(now func() time.Time) Option {
	return func(i *Issuer) { i.now = now }
}

// New returns an Issuer signing with priv, which must be a P-256 key.
// issuer and audience become the token's iss and aud claims verbatim;
// ttl bounds how long a minted token is valid for.
func New(priv *ecdsa.PrivateKey, issuer, audience string, ttl time.Duration, opts ...Option) (*Issuer, error) {
	if priv == nil || priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("signing key must be a P-256 ECDSA key")
	}
	if issuer == "" {
		return nil, fmt.Errorf("issuer is required")
	}
	if audience == "" {
		return nil, fmt.Errorf("audience is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be positive")
	}

	kid, err := KeyID(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	doc, err := JWKSDocument(&priv.PublicKey)
	if err != nil {
		return nil, err
	}

	i := &Issuer{
		priv:     priv,
		kid:      kid,
		jwks:     doc,
		issuer:   issuer,
		audience: audience,
		ttl:      ttl,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(i)
	}
	return i, nil
}

// KeyID returns the kid this Issuer signs with, matching the JWKS entry
// a verifier looks it up by.
func (i *Issuer) KeyID() string { return i.kid }

// JWKS returns the JWKS document for this Issuer's public key. The
// returned bytes are shared and must not be mutated.
func (i *Issuer) JWKS() []byte { return i.jwks }

// Mint signs a sandbox identity token for sub (the run identity —
// hairpin uses the job ID), scoped to repoScope. An empty repoScope
// omits the claim entirely rather than encoding an empty array, so
// haybale's policy is what decides "no repos" versus "some repos".
func (i *Issuer) Mint(sub string, repoScope []string) (token string, expiresAt time.Time, err error) {
	now := i.now().UTC()
	exp := now.Add(i.ttl)

	jti, err := randomJTI()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate jti: %w", err)
	}

	claims := jwt.MapClaims{
		"iss": i.issuer,
		"aud": i.audience,
		"sub": sub,
		"iat": now.Unix(),
		"exp": exp.Unix(),
		"jti": jti,
	}
	if len(repoScope) > 0 {
		claims[repoScopeClaim] = repoScope
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = i.kid
	tok.Header["typ"] = tokenType

	signed, err := tok.SignedString(i.priv)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, exp, nil
}

// randomJTI returns a fresh random jti claim value.
func randomJTI() (string, error) {
	var b [jtiBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// jwksMaxAge is how long a caller may cache the JWKS document before
// re-fetching. It bounds how quickly a rotated key propagates to
// haybale's HTTP-fetched jwksURL — the file-mounted deployment path is
// unaffected (see docs/deployment.md#sandbox-identity-tokens).
const jwksMaxAge = 5 * time.Minute

// JWKSHandler serves this Issuer's JWKS document at GET /.
func (i *Issuer) JWKSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(jwksMaxAge.Seconds())))
		_, _ = w.Write(i.jwks)
	})
}
