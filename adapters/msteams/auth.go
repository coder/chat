package msteams

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// botConnectorIssuer is the only accepted iss for Bot Connector -> bot tokens.
	botConnectorIssuer = "https://api.botframework.com"
	// openIDMetadataURL is the static Bot Connector OpenID metadata document whose
	// jwks_uri yields the signing keys (with Bot Framework endorsement annotations).
	openIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	// defaultJWKSCacheTTL keeps fetched keys at least a day, refreshed on a kid miss
	// (key rotation) regardless.
	defaultJWKSCacheTTL = 24 * time.Hour
	// clockSkew is the tolerated exp/nbf skew, per Microsoft guidance.
	clockSkew = 5 * time.Minute
	// msteamsChannel is the Bot Framework channel id this adapter serves. The
	// endorsement check is bound to this constant, NOT to the channelId in the
	// (unauthenticated) Activity body, so the body cannot weaken or skip it.
	msteamsChannel = "msteams"
)

// errKeysUnavailable marks an inbound failure caused by the adapter being unable to
// fetch signing keys (metadata/JWKS transport failure with no usable cached key),
// as opposed to a token that is genuinely invalid. The caller maps it to a
// retryable 5xx so the Bot Connector redelivers, rather than a 403 that the
// Connector treats as a permanent rejection and drops.
var errKeysUnavailable = errors.New("msteams: signing keys temporarily unavailable")

// authValidator performs every mandatory inbound Bot Connector JWT check against a
// JWKS resolved from the Bot Framework OpenID metadata, plus the Bot Framework
// channel-endorsement check. It is a deep adapter-internal module behind a single
// validate method; there is deliberately no switch to disable any check. It is
// testable with a fake metadata+JWKS server and in-test RSA keys.
//
// JWT parsing and RS256 verification are stdlib-only (crypto/rsa over a public key
// rebuilt from the JWK n/e), so the otherwise zero-dependency core module gains no
// JWT library. This is a deliberate spike finding for ADR 0007 Open Question 9
// (msbotbuilder-go is not adopted, and even golang-jwt is unnecessary here).
type authValidator struct {
	appID         string
	openIDMetaURL string
	issuer        string
	client        *http.Client
	now           func() time.Time
	cacheTTL      time.Duration

	mu        sync.Mutex
	keys      map[string]jwk
	fetchedAt time.Time
}

// jwk is the subset of a JSON Web Key the validator needs, plus the Bot Framework
// per-key endorsements list (a Bot Framework extension to the standard JWKS).
type jwk struct {
	Kid          string   `json:"kid"`
	Kty          string   `json:"kty"`
	N            string   `json:"n"`
	E            string   `json:"e"`
	Endorsements []string `json:"endorsements"`
	pub          *rsa.PublicKey
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type jwtPayload struct {
	Iss        string          `json:"iss"`
	Aud        json.RawMessage `json:"aud"`
	Exp        int64           `json:"exp"`
	Nbf        int64           `json:"nbf"`
	Iat        int64           `json:"iat"`
	ServiceURL string          `json:"serviceurl"`
}

// validate runs the full inbound check on the Authorization header for an Activity
// with the given serviceUrl and channelId. It returns nil only when every check
// passes; any failure is an error the caller maps to HTTP 403. The serviceUrl claim
// is bound to the Activity's serviceUrl so a token minted for one service URL
// cannot be replayed against a spoofed one.
func (v *authValidator) validate(ctx context.Context, authzHeader, serviceURL string) error {
	raw, err := bearerToken(authzHeader)
	if err != nil {
		return err
	}
	header, payload, signingInput, sig, err := splitJWT(raw)
	if err != nil {
		return err
	}
	if !strings.EqualFold(header.Alg, "RS256") {
		return fmt.Errorf("msteams: unexpected jwt alg %q", header.Alg)
	}
	if header.Kid == "" {
		return errors.New("msteams: jwt missing kid")
	}

	key, err := v.keyForKid(ctx, header.Kid)
	if err != nil {
		return err
	}
	if err := verifyRS256(key.pub, signingInput, sig); err != nil {
		return err
	}

	if payload.Iss != v.issuer {
		return fmt.Errorf("msteams: unexpected jwt issuer %q", payload.Iss)
	}
	if !audienceMatches(payload.Aud, v.appID) {
		return errors.New("msteams: jwt audience does not match bot app id")
	}
	// exp is mandatory: a token without it must fail closed, never be treated as
	// never-expiring.
	if payload.Exp == 0 {
		return errors.New("msteams: jwt missing exp claim")
	}
	if !validAt(payload, v.now(), clockSkew) {
		return errors.New("msteams: jwt outside validity window")
	}
	if payload.ServiceURL == "" {
		return errors.New("msteams: jwt missing serviceurl claim")
	}
	if !serviceURLMatches(payload.ServiceURL, serviceURL) {
		return errors.New("msteams: jwt serviceurl claim does not match activity")
	}
	// Endorsement is checked against this adapter's own channel constant, never the
	// channelId in the request body, so a caller cannot skip it by omitting/spoofing
	// channelId.
	if err := checkEndorsement(key, msteamsChannel); err != nil {
		return err
	}
	return nil
}

// keyForKid resolves the signing key for a kid, refreshing the JWKS on a miss
// (handling key rotation) or when the cache is stale. A refresh failure is tolerated
// only when an existing key for the kid is still cached, so a transient metadata
// outage does not reject otherwise-valid traffic.
func (v *authValidator) keyForKid(ctx context.Context, kid string) (jwk, error) {
	v.mu.Lock()
	key, ok := v.keys[kid]
	fresh := !v.fetchedAt.IsZero() && v.now().Sub(v.fetchedAt) < v.cacheTTL
	v.mu.Unlock()
	if ok && fresh {
		return key, nil
	}

	if err := v.refresh(ctx); err != nil {
		if ok {
			return key, nil
		}
		// No cached key and the metadata/JWKS fetch failed: this is an adapter-side
		// transient failure, not an invalid token. Mark it so the caller returns a
		// retryable 5xx instead of permanently dropping a possibly-valid Activity.
		return jwk{}, fmt.Errorf("%w: %v", errKeysUnavailable, err)
	}

	v.mu.Lock()
	key, ok = v.keys[kid]
	v.mu.Unlock()
	if !ok {
		return jwk{}, fmt.Errorf("msteams: no signing key for kid %q", kid)
	}
	return key, nil
}

// refresh fetches the OpenID metadata, then the JWKS, building an rsa.PublicKey for
// each RSA key. The HTTP fetches run without the cache lock held; only the swap is
// locked.
func (v *authValidator) refresh(ctx context.Context) error {
	jwksURI, err := v.discoverJWKSURI(ctx)
	if err != nil {
		return err
	}
	keys, err := v.fetchKeys(ctx, jwksURI)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = v.now()
	v.mu.Unlock()
	return nil
}

func (v *authValidator) discoverJWKSURI(ctx context.Context) (string, error) {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, v.openIDMetaURL, &meta); err != nil {
		return "", fmt.Errorf("msteams: fetch openid metadata: %w", err)
	}
	if meta.JWKSURI == "" {
		return "", errors.New("msteams: openid metadata has no jwks_uri")
	}
	return meta.JWKSURI, nil
}

func (v *authValidator) fetchKeys(ctx context.Context, jwksURI string) (map[string]jwk, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, jwksURI, &doc); err != nil {
		return nil, fmt.Errorf("msteams: fetch jwks: %w", err)
	}
	keys := make(map[string]jwk, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kid == "" || !strings.EqualFold(k.Kty, "RSA") {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(k)
		if err != nil {
			// Skip a malformed key rather than failing the whole rotation.
			continue
		}
		k.pub = pub
		keys[k.Kid] = k
	}
	if len(keys) == 0 {
		return nil, errors.New("msteams: jwks has no usable RSA keys")
	}
	return keys, nil
}

func (v *authValidator) getJSON(ctx context.Context, url string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.Unmarshal(body, dest)
}

// checkEndorsement enforces that the signing key endorses the required channel: the
// key's endorsements must contain it, or the request is rejected (HTTP 403 at the
// caller). Callers pass the adapter's own channel constant, never an Activity-body
// value, so an empty/spoofed body channelId cannot bypass the check. This is the
// strict reading of ADR 0007; whether msteams strictly requires the endorsement, and
// the exact rule, is spike-required (Open Question 3). Failing closed is the safer
// default for a security check a human will validate before production.
func checkEndorsement(key jwk, channelID string) error {
	for _, e := range key.Endorsements {
		if strings.EqualFold(e, channelID) {
			return nil
		}
	}
	return fmt.Errorf("msteams: signing key does not endorse channel %q", channelID)
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", errors.New("msteams: missing authorization header")
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", errors.New("msteams: authorization is not a bearer token")
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", errors.New("msteams: empty bearer token")
	}
	return token, nil
}

func splitJWT(raw string) (jwtHeader, jwtPayload, []byte, []byte, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return jwtHeader{}, jwtPayload{}, nil, nil, errors.New("msteams: malformed jwt")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, jwtPayload{}, nil, nil, fmt.Errorf("msteams: decode jwt header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return jwtHeader{}, jwtPayload{}, nil, nil, fmt.Errorf("msteams: parse jwt header: %w", err)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, jwtPayload{}, nil, nil, fmt.Errorf("msteams: decode jwt payload: %w", err)
	}
	var payload jwtPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return jwtHeader{}, jwtPayload{}, nil, nil, fmt.Errorf("msteams: parse jwt payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtHeader{}, jwtPayload{}, nil, nil, fmt.Errorf("msteams: decode jwt signature: %w", err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	return header, payload, signingInput, sig, nil
}

func verifyRS256(pub *rsa.PublicKey, signingInput, sig []byte) error {
	if pub == nil {
		return errors.New("msteams: nil signing key")
	}
	hashed := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig); err != nil {
		return fmt.Errorf("msteams: jwt signature invalid: %w", err)
	}
	return nil
}

func rsaPublicKeyFromJWK(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
	if err != nil {
		return nil, fmt.Errorf("msteams: decode jwk modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
	if err != nil {
		return nil, fmt.Errorf("msteams: decode jwk exponent: %w", err)
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errors.New("msteams: jwk modulus or exponent empty")
	}
	// Guard the exponent against silent truncation by int(...) (it is consumed as a
	// platform int, 32-bit on some builds): reject anything that does not fit a
	// positive 32-bit int or is implausibly small.
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, errors.New("msteams: jwk exponent too large")
	}
	if ev := e.Int64(); ev < 2 || ev > 1<<31-1 {
		return nil, fmt.Errorf("msteams: jwk exponent %d out of range", ev)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}

// audienceMatches accepts the aud claim as either a JSON string or array of
// strings, matching the bot's Microsoft App ID.
func audienceMatches(aud json.RawMessage, appID string) bool {
	if len(aud) == 0 || appID == "" {
		return false
	}
	var single string
	if err := json.Unmarshal(aud, &single); err == nil {
		return single == appID
	}
	var many []string
	if err := json.Unmarshal(aud, &many); err == nil {
		for _, a := range many {
			if a == appID {
				return true
			}
		}
	}
	return false
}

func validAt(p jwtPayload, now time.Time, skew time.Duration) bool {
	if p.Exp != 0 && now.After(time.Unix(p.Exp, 0).Add(skew)) {
		return false
	}
	if p.Nbf != 0 && now.Before(time.Unix(p.Nbf, 0).Add(-skew)) {
		return false
	}
	return true
}

func serviceURLMatches(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}
