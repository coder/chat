package msteams_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/chat/adapters/msteams"
)

const (
	testAppID      = "11111111-1111-1111-1111-111111111111"
	testBotID      = "28:11111111-1111-1111-1111-111111111111"
	testServiceURL = "https://smba.trafficmanager.net/teams/"
	testIssuer     = "https://api.botframework.com"
	testKid        = "test-key-1"
	testTenant     = "tenant-aaa"
	testConvID     = "19:conversation-xyz"
)

// testClock is a fixed instant so JWT validity windows and JWKS cache freshness are
// deterministic.
var testClock = time.Unix(1_700_000_000, 0).UTC()

func fixedNow() func() time.Time { return func() time.Time { return testClock } }

// fakeBotConnector stands in for the Bot Framework inbound-auth surface: it serves
// the OpenID metadata + JWKS (with endorsements) and signs JWTs with a test RSA key.
type fakeBotConnector struct {
	key          *rsa.PrivateKey
	kid          string
	endorsements []string
	server       *httptest.Server
	keyRequests  int
}

func newFakeBotConnector(t *testing.T) *fakeBotConnector {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	bf := &fakeBotConnector{key: key, kid: testKid, endorsements: []string{"msteams"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jwks_uri": bf.server.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		bf.keyRequests++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{bf.jwk()}})
	})
	bf.server = httptest.NewServer(mux)
	t.Cleanup(bf.server.Close)
	return bf
}

func (bf *fakeBotConnector) metadataURL() string { return bf.server.URL + "/openid" }

func (bf *fakeBotConnector) jwk() map[string]any {
	return map[string]any{
		"kty":          "RSA",
		"kid":          bf.kid,
		"n":            base64.RawURLEncoding.EncodeToString(bf.key.PublicKey.N.Bytes()),
		"e":            base64.RawURLEncoding.EncodeToString(big.NewInt(int64(bf.key.PublicKey.E)).Bytes()),
		"endorsements": bf.endorsements,
	}
}

// validClaims returns a claim set that passes every check at testClock; tests mutate
// it before signing.
func (bf *fakeBotConnector) validClaims() map[string]any {
	return map[string]any{
		"iss":        testIssuer,
		"aud":        testAppID,
		"serviceurl": testServiceURL,
		"iat":        testClock.Add(-time.Minute).Unix(),
		"nbf":        testClock.Add(-time.Minute).Unix(),
		"exp":        testClock.Add(time.Hour).Unix(),
	}
}

func (bf *fakeBotConnector) sign(t *testing.T, claims map[string]any) string {
	return signRS256(t, bf.key, bf.kid, claims)
}

// signForeign signs with a different key but advertises the real kid, producing a
// signature that must fail verification.
func (bf *fakeBotConnector) signForeign(t *testing.T, claims map[string]any) string {
	t.Helper()
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}
	return signRS256(t, foreign, bf.kid, claims)
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := seg(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + seg(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// messageActivity is a well-formed channel message Activity that @mentions the bot.
func messageActivity() map[string]any {
	return map[string]any{
		"type":       "message",
		"id":         "activity-1",
		"channelId":  "msteams",
		"serviceUrl": testServiceURL,
		"text":       "<at>Bot</at> hello there",
		"from":       map[string]any{"id": "29:user-aaa", "name": "Alice", "aadObjectId": "aad-alice"},
		"recipient":  map[string]any{"id": testBotID, "name": "Bot"},
		"conversation": map[string]any{
			"id":               testConvID,
			"tenantId":         testTenant,
			"conversationType": "channel",
			"isGroup":          true,
		},
		"entities": []map[string]any{
			{"type": "mention", "text": "<at>Bot</at>", "mentioned": map[string]any{"id": testBotID, "name": "Bot"}},
		},
	}
}

func newTestAdapter(t *testing.T, bf *fakeBotConnector, mutate func(*msteams.Options)) *msteams.Adapter {
	t.Helper()
	opts := msteams.Options{
		MicrosoftAppID:       testAppID,
		MicrosoftAppPassword: "test-secret",
		OpenIDMetadataURL:    bf.metadataURL(),
		Now:                  fixedNow(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	a, err := msteams.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("msteams.New: %v", err)
	}
	return a
}

func postActivity(t *testing.T, handler http.Handler, authHeader string, activity map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(activity)
	if err != nil {
		t.Fatalf("marshal activity: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// fakeTokenServer returns a server that mints the given outbound token, recording
// how many times it was called.
func fakeTokenServer(t *testing.T, token string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "client_credentials" {
			t.Errorf("token grant_type = %q", r.FormValue("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	return srv
}
