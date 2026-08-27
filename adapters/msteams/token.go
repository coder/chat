package msteams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// defaultTokenURL is the Bot Framework client_credentials token endpoint for the
	// single-tenant deployment model (ADR 0007 Open Question 4). Multi-tenant and
	// managed-identity use different URLs and are out of this slice.
	defaultTokenURL = "https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token"
	// connectorScope is the OAuth2 scope for outbound Connector calls.
	connectorScope = "https://api.botframework.com/.default"
	// tokenRefreshMargin re-mints slightly before expiry so an in-flight call never
	// uses a just-expired token.
	tokenRefreshMargin = 60 * time.Second
)

// tokenSource mints and caches the outbound client_credentials bearer token used to
// authorize Connector REST calls. The token lives only in adapter process memory
// and is refreshed lazily before expiry; it is never written to Runtime State,
// matching the Linear app-actor token-cache decision (ADR 0001).
type tokenSource struct {
	appID     string
	appSecret string
	tokenURL  string
	scope     string
	client    *http.Client
	now       func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// get returns a cached token when one is still comfortably valid, otherwise mints a
// fresh one. The HTTP mint runs without the lock held.
func (t *tokenSource) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.token != "" && t.now().Before(t.expiresAt.Add(-tokenRefreshMargin)) {
		token := t.token
		t.mu.Unlock()
		return token, nil
	}
	t.mu.Unlock()

	token, expiresIn, err := t.mint(ctx)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.token = token
	t.expiresAt = t.now().Add(time.Duration(expiresIn) * time.Second)
	t.mu.Unlock()
	return token, nil
}

func (t *tokenSource) mint(ctx context.Context) (string, int, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", t.appID)
	form.Set("client_secret", t.appSecret)
	form.Set("scope", t.scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("msteams: mint token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, fmt.Errorf("msteams: mint token status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, fmt.Errorf("msteams: decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("msteams: token response missing access_token (error %q: %s)", out.Error, out.ErrorDesc)
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return out.AccessToken, expiresIn, nil
}
