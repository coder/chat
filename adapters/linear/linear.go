package linear

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/chat"
)

var defaultClientCredentialScopes = []string{"read", "write", "app:mentionable", "app:assignable"}

// Linear agent activity content types. Linear validates these server-side; the
// adapter only enforces the ephemeral-allowed rule and funnels every type
// through the generic agent-activity codec.
const (
	activityTypeThought     = "thought"
	activityTypeResponse    = "response"
	activityTypeAction      = "action"
	activityTypeElicitation = "elicitation"
	activityTypeError       = "error"
)

const (
	adapterName               = "linear"
	defaultAPIBaseURL         = "https://api.linear.app"
	defaultSignatureTolerance = time.Minute
	maxWebhookBodyBytes       = 1 << 20
	tokenRefreshBuffer        = time.Hour
)

// ClientCredentials configures Linear app-actor authentication.
type ClientCredentials struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// LinearInstall is the adapter-specific Install.Credential payload for Linear, a
// Platform Escape Hatch for credentials. Linear signs webhooks per install and
// authorizes per org, so both the verification material and the reply credentials
// ride here: the per-install webhook secret plus the per-org App-Actor Client
// Credentials (or a pre-exchanged installation AccessToken). When only client
// credentials are supplied the adapter performs the lazy client-credentials token
// exchange per tenant (ADR-0001). BotUserID is an optional pre-discovered app
// actor id used for tenant-correct self-filtering, taking second place behind
// Install.BotActorID.
type LinearInstall struct {
	WebhookSecret     string
	ClientCredentials ClientCredentials
	AccessToken       string
	BotUserID         string
}

// Options configures the Linear adapter.
type Options struct {
	WebhookSecret     string
	ClientCredentials ClientCredentials
	// InstallStore selects Multi-Tenant Adapter mode: per-org webhook secrets and
	// credentials are resolved per webhook (and per Thread Handle reconstruction)
	// from the store instead of static WebhookSecret/ClientCredentials. Supplying
	// InstallStore together with either a WebhookSecret or ClientCredentials, or
	// supplying none of them, is a Runtime Construction error. Linear signs
	// per-install, so the webhook secret comes from the install record in this mode.
	InstallStore       chat.InstallStore
	APIBaseURL         string
	Client             *http.Client
	Now                func() time.Time
	SignatureTolerance time.Duration
	Logger             *slog.Logger
	// RetryPolicy bounds outbound Linear rate-limit retry/backoff. The zero value
	// applies a conservative default that stays well under the Agent Session
	// Timing Contract first-thought window (ADR 0005, ADR 0008).
	RetryPolicy RetryPolicy
}

var _ chat.Adapter = (*Adapter)(nil)

// Adapter implements chat.Adapter for Linear app-actor agent sessions.
type Adapter struct {
	webhookSecret      string
	clientCredentials  ClientCredentials
	installStore       chat.InstallStore
	apiBaseURL         string
	client             *http.Client
	now                func() time.Time
	signatureTolerance time.Duration
	logger             *slog.Logger
	retryPolicy        RetryPolicy

	mu             sync.Mutex
	accessToken    string
	tokenExpiry    time.Time
	organizationID string
	botUserID      string
	botName        string

	// tenantTokensMu guards the per-tenant derived-token cache used in multi-tenant
	// mode. Tokens are derived from the install record's client credentials and kept
	// keyed by Platform Tenant with ADR-0001 lazy refresh; the durable install
	// record is re-fetched per event from the app's store, so a revoked install
	// stops resolving once its record is gone.
	tenantTokensMu sync.Mutex
	tenantTokens   map[string]*tenantToken
}

type tenantToken struct {
	accessToken string
	expiry      time.Time
}

func New(ctx context.Context, opts Options) (*Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	multiTenant := opts.InstallStore != nil
	if multiTenant {
		// Linear signs per install, so the webhook secret and credentials come from
		// the install record; supplying static ones alongside a store is ambiguous.
		if opts.WebhookSecret != "" || opts.ClientCredentials.ClientID != "" || opts.ClientCredentials.ClientSecret != "" {
			return nil, errors.New("linear: set either webhook secret/client credentials or install store, not both")
		}
	} else {
		if opts.WebhookSecret == "" {
			return nil, errors.New("linear: webhook secret or install store is required")
		}
		if opts.ClientCredentials.ClientID == "" {
			return nil, errors.New("linear: client credentials client id is required")
		}
		if opts.ClientCredentials.ClientSecret == "" {
			return nil, errors.New("linear: client credentials client secret is required")
		}
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	apiBaseURL := strings.TrimRight(opts.APIBaseURL, "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tolerance := opts.SignatureTolerance
	if tolerance == 0 {
		tolerance = defaultSignatureTolerance
	}
	if tolerance < 0 {
		return nil, errors.New("linear: signature tolerance must be non-negative")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	retryPolicy := opts.RetryPolicy.withDefaults()
	creds := opts.ClientCredentials
	if !multiTenant {
		creds.Scopes = normalizeScopeList(creds.Scopes)
		if len(creds.Scopes) == 0 {
			creds.Scopes = append([]string(nil), defaultClientCredentialScopes...)
		}
	}
	return &Adapter{
		webhookSecret:      opts.WebhookSecret,
		clientCredentials:  creds,
		installStore:       opts.InstallStore,
		apiBaseURL:         apiBaseURL,
		client:             client,
		now:                now,
		signatureTolerance: tolerance,
		logger:             logger,
		retryPolicy:        retryPolicy,
		tenantTokens:       map[string]*tenantToken{},
	}, nil
}

func (a *Adapter) Name() string { return adapterName }

// multiTenant reports whether this adapter resolves per-org credentials through an
// InstallStore instead of a single static client-credentials install.
func (a *Adapter) multiTenant() bool {
	return a.installStore != nil
}

func (a *Adapter) Init(ctx context.Context) error {
	assertAdapter(a)
	// Multi-tenant identity (org, app actor) is per install, resolved per webhook /
	// from each install record, so there is no single identity to discover here.
	if a.multiTenant() {
		return nil
	}
	if err := a.refreshToken(ctx); err != nil {
		return err
	}
	identity, err := a.fetchIdentity(ctx)
	if err != nil {
		return err
	}
	if identity.OrganizationID == "" {
		return errors.New("linear: identity did not return organization id")
	}
	if identity.BotUserID == "" {
		return errors.New("linear: identity did not return app user id")
	}
	a.mu.Lock()
	a.organizationID = identity.OrganizationID
	a.botUserID = identity.BotUserID
	a.botName = identity.Name
	a.mu.Unlock()
	return nil
}

func (a *Adapter) Shutdown(context.Context) error { return nil }

func (a *Adapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	assertAdapter(a)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "linear webhook too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		resolved, secret, ignored, err := a.resolveWebhookInstall(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ignored {
			// ErrInstallNotFound for the org parsed from the unverified body: Ignored
			// Event. Acknowledge without verifying or dispatching.
			w.WriteHeader(http.StatusOK)
			return
		}
		// Re-validate the unverified routing read by verifying the signature with the
		// resolved secret before any side effect.
		if err := a.verifySignatureWithSecret(r, body, secret); err != nil {
			http.Error(w, "invalid linear signature", http.StatusUnauthorized)
			return
		}
		var envelope webhookEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			http.Error(w, "invalid linear payload", http.StatusBadRequest)
			return
		}
		if err := a.verifyTimestamp(envelope.WebhookTimestamp); err != nil {
			http.Error(w, "invalid linear timestamp", http.StatusUnauthorized)
			return
		}
		event, ok, err := a.normalizeEvent(r.Context(), envelope, body, resolved)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			if err := dispatch(r.Context(), event); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			a.logger.Debug(
				"ignoring unsupported Linear webhook",
				"type", envelope.Type,
				"action", envelope.Action,
				"organization_id", envelope.OrganizationID,
				"webhook_id", envelope.WebhookID,
				"agent_session_id", envelope.AgentSession.ID,
			)
		}
		w.WriteHeader(http.StatusOK)
	})
}

// resolvedInstall carries the per-tenant identity threaded into normalization so a
// multi-tenant adapter validates against the install record rather than a single
// configured org/client/app-actor.
type resolvedInstall struct {
	tenant        string
	oauthClientID string
	botUserID     string
}

// routingEnvelope is the minimal shape parsed from an unverified body to learn the
// Platform Tenant (organizationId) for routing only. Linear signs per install, so
// the tenant must be read before the signing secret is known; it is re-validated
// by signature verification before any side effect.
type routingEnvelope struct {
	OrganizationID string `json:"organizationId"`
}

// resolveWebhookInstall reads the tenant from the unverified body (routing only)
// and resolves the per-install signing secret and identity. In single-install mode
// it returns the static webhook secret and configured identity. In multi-tenant
// mode it parses organizationId, calls InstallStore.Lookup, and returns the
// install-record secret and identity; ErrInstallNotFound returns ignored=true (an
// Ignored Event), and any other store error returns a non-nil error (5xx).
func (a *Adapter) resolveWebhookInstall(ctx context.Context, body []byte) (resolved resolvedInstall, secret string, ignored bool, err error) {
	if !a.multiTenant() {
		bot := a.BotActor()
		return resolvedInstall{tenant: bot.Tenant, oauthClientID: a.clientCredentials.ClientID, botUserID: bot.ID}, a.webhookSecret, false, nil
	}
	var route routingEnvelope
	if jsonErr := json.Unmarshal(body, &route); jsonErr != nil || route.OrganizationID == "" {
		// An unparseable or org-less body cannot be routed to an install; treat it as
		// an Ignored Event rather than dispatching unsigned input.
		return resolvedInstall{}, "", true, nil
	}
	install, lookupErr := a.installStore.Lookup(ctx, adapterName, route.OrganizationID)
	if lookupErr != nil {
		if errors.Is(lookupErr, chat.ErrInstallNotFound) {
			return resolvedInstall{}, "", true, nil
		}
		return resolvedInstall{}, "", false, fmt.Errorf("linear: install lookup: %w", lookupErr)
	}
	cred, credErr := linearCredential(install.Credential)
	if credErr != nil {
		return resolvedInstall{}, "", false, credErr
	}
	if cred.WebhookSecret == "" {
		return resolvedInstall{}, "", false, errors.New("linear: install credential has no webhook secret")
	}
	return resolvedInstall{
		tenant:        route.OrganizationID,
		oauthClientID: cred.ClientCredentials.ClientID,
		botUserID:     firstNonEmpty(install.BotActorID, cred.BotUserID),
	}, cred.WebhookSecret, false, nil
}

func (a *Adapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	payload, err := a.validateThreadPayload(id)
	if err != nil {
		return chat.ThreadRef{}, err
	}
	return chat.ThreadRef{
		ID:      id,
		Adapter: adapterName,
		Tenant:  payload.Organization,
		Channel: payload.Issue,
		Root:    payload.Session,
		Raw:     payload,
	}, nil
}

func (a *Adapter) validateThreadPayload(id chat.ThreadID) (threadPayload, error) {
	payload, err := decodeThreadID(id)
	if err != nil {
		return threadPayload{}, err
	}
	// The single-install org guard is wrong in multi-tenant mode, where every org the
	// store serves is valid; the install lookup on the posting path decides instead.
	if !a.multiTenant() {
		bot := a.BotActor()
		if bot.Tenant != "" && payload.Organization != bot.Tenant {
			return threadPayload{}, fmt.Errorf(
				"linear: thread organization %q does not match initialized organization",
				payload.Organization,
			)
		}
	}
	return payload, nil
}

// agentSessionPayload validates the thread id, enforces tenant scoping, and
// rejects any thread kind other than agent-session. Agent-activity methods are
// agent-session-only (ADR 0008, ADR 0013).
func (a *Adapter) agentSessionPayload(id chat.ThreadID) (threadPayload, error) {
	payload, err := a.validateThreadPayload(id)
	if err != nil {
		return threadPayload{}, err
	}
	if payload.kind() != threadKindAgentSession {
		return threadPayload{}, fmt.Errorf("linear: agent activities are only valid on agent-session threads, not %q threads", payload.kind())
	}
	return payload, nil
}

func (a *Adapter) PostMessage(ctx context.Context, thread chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	if err := validatePostableMessage(msg); err != nil {
		return nil, err
	}
	payload, err := a.payloadFromThread(thread)
	if err != nil {
		return nil, err
	}
	switch payload.kind() {
	case threadKindComment:
		return a.createIssueComment(ctx, payload, msg.Text)
	default:
		return a.createAgentActivity(ctx, payload, activityRequest{content: map[string]any{"type": activityTypeResponse, "body": msg.Text}})
	}
}

// PostThought posts an ephemeral Linear thought activity in an agent session.
func (a *Adapter) PostThought(ctx context.Context, id chat.ThreadID, text string) (*chat.SentMessage, error) {
	assertAdapter(a)
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("linear: thought text is required")
	}
	payload, err := a.agentSessionPayload(id)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, activityRequest{content: map[string]any{"type": activityTypeThought, "body": text}, ephemeral: true})
}

func (a *Adapter) BotActor() chat.Actor {
	assertAdapter(a)
	a.mu.Lock()
	defer a.mu.Unlock()
	return chat.Actor{Adapter: adapterName, Tenant: a.organizationID, ID: a.botUserID, Name: a.botName, BotKind: chat.BotBot}
}

func (a *Adapter) normalizeEvent(_ context.Context, envelope webhookEnvelope, raw []byte, resolved resolvedInstall) (*chat.Event, bool, error) {
	switch envelope.Type {
	case "AgentSessionEvent":
		event, ok := a.normalizeAgentSessionEvent(envelope, raw, resolved)
		return event, ok, nil
	case "Comment":
		event, ok := a.normalizeCommentEvent(envelope, raw, resolved)
		return event, ok, nil
	default:
		return nil, false, nil
	}
}

func (a *Adapter) normalizeAgentSessionEvent(envelope webhookEnvelope, raw []byte, resolved resolvedInstall) (*chat.Event, bool) {
	if envelope.OrganizationID == "" || envelope.AgentSession.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear agent session event", "reason", "missing organization or session")
		return nil, false
	}
	if resolved.tenant != "" && envelope.OrganizationID != resolved.tenant {
		a.logger.Warn(
			"ignoring Linear agent session event for another organization",
			"session_id", envelope.AgentSession.ID,
			"organization_id", envelope.OrganizationID,
		)
		return nil, false
	}
	if envelope.OAuthClientID != "" && resolved.oauthClientID != "" && envelope.OAuthClientID != resolved.oauthClientID {
		a.logger.Warn(
			"ignoring Linear agent session event for another OAuth client",
			"session_id", envelope.AgentSession.ID,
			"oauth_client_id", envelope.OAuthClientID,
		)
		return nil, false
	}
	if envelope.AgentSession.AppUserID != "" && envelope.AppUserID != "" && envelope.AgentSession.AppUserID != envelope.AppUserID {
		a.logger.Warn("ignoring Linear agent session event with conflicting app actors", "session_id", envelope.AgentSession.ID)
		return nil, false
	}
	appUserID := firstNonEmpty(envelope.AgentSession.AppUserID, envelope.AppUserID)
	if appUserID != "" && resolved.botUserID != "" && appUserID != resolved.botUserID {
		a.logger.Warn("ignoring Linear agent session event for another app actor", "session_id", envelope.AgentSession.ID)
		return nil, false
	}
	issueID := firstNonEmpty(envelope.AgentSession.IssueID, envelope.AgentSession.Issue.ID)
	if issueID == "" {
		a.logger.Warn("ignoring Linear agent session event without issue", "session_id", envelope.AgentSession.ID)
		return nil, false
	}

	var sourceID, text string
	var author chat.Actor
	rootCommentID := ""
	if envelope.AgentSession.Comment != nil {
		rootCommentID = envelope.AgentSession.Comment.ID
	}

	switch envelope.Action {
	case "created":
		if envelope.AgentSession.Comment == nil {
			envelope.AgentSession.Comment = &sessionComment{}
		}
		sourceID = firstNonEmpty(envelope.AgentSession.Comment.ID, envelope.AgentSession.ID)
		text = firstNonEmpty(envelope.AgentSession.Comment.Body, envelope.PromptContext, "Agent session created")
		if envelope.AgentSession.Creator != nil {
			author = actorFromWebhook(envelope.OrganizationID, *envelope.AgentSession.Creator)
		} else {
			author = systemActor(envelope.OrganizationID, envelope.AgentSession.ID)
		}
	case "prompted":
		activity := envelope.AgentActivity
		if activity == nil {
			a.logger.Warn("ignoring Linear prompted agent session without activity", "session_id", envelope.AgentSession.ID)
			return nil, false
		}
		sourceID = firstNonEmpty(activity.SourceCommentID, activity.ID)
		text = firstNonEmpty(activity.Body, activity.Content.Body)
		author = actorFromWebhook(envelope.OrganizationID, activity.User)
	default:
		return nil, false
	}
	if sourceID == "" || author.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear agent session event", "session_id", envelope.AgentSession.ID)
		return nil, false
	}
	// Tenant-correct self-filtering from the per-install app actor: drop the bot's
	// own authored events as an Ignored Event, since the runtime's single-valued
	// BotActor() filter cannot match in multi-tenant mode.
	if a.multiTenant() && resolved.botUserID != "" && author.BotKind == chat.BotBot && author.ID == resolved.botUserID {
		return nil, false
	}
	threadID, err := encodeThreadID(threadPayload{
		Organization: envelope.OrganizationID,
		Issue:        issueID,
		Comment:      rootCommentID,
		Session:      envelope.AgentSession.ID,
		Kind:         threadKindAgentSession,
	})
	if err != nil {
		a.logger.Warn("ignoring Linear agent session event with invalid thread", "error", err)
		return nil, false
	}
	rawMessage := a.agentSessionRawMessage(envelope, raw)
	return &chat.Event{
		ID:       "linear:" + envelope.OrganizationID + ":message:" + sourceID,
		Adapter:  adapterName,
		Tenant:   envelope.OrganizationID,
		ThreadID: threadID,
		Raw:      rawMessage,
		Message:  &chat.Message{ID: sourceID, Text: text, Author: author, Mentioned: true, Raw: rawMessage},
	}, true
}

// agentSessionRawMessage builds the stable Linear Platform Escape Hatch for an
// agent-session event, preserving inbound signal / signalMetadata and structured
// session context, and surfacing the first-thought deadline on "created".
func (a *Adapter) agentSessionRawMessage(envelope webhookEnvelope, raw []byte) *RawMessage {
	session := &RawAgentSession{
		ID:               envelope.AgentSession.ID,
		PromptContext:    envelope.PromptContext,
		Guidance:         envelope.Guidance,
		PreviousComments: envelope.PreviousComments,
		Issue:            envelope.AgentSession.IssueRaw,
		Comment:          envelope.AgentSession.CommentRaw,
	}
	if envelope.Action == "created" {
		createdAt := firstNonEmpty(envelope.AgentSession.CreatedAt, envelope.CreatedAt)
		session.FirstThoughtDeadline = newFirstThoughtDeadline(createdAt, a.now)
	}
	rawMessage := &RawMessage{
		Kind:           threadKindAgentSession,
		Action:         envelope.Action,
		OrganizationID: envelope.OrganizationID,
		Session:        session,
		Envelope:       append(json.RawMessage(nil), raw...),
	}
	if envelope.AgentActivity != nil {
		rawMessage.Signal = envelope.AgentActivity.Signal
		rawMessage.SignalMetadata = envelope.AgentActivity.SignalMetadata
	}
	return rawMessage
}

func actorFromWebhook(tenant string, actor webhookActor) chat.Actor {
	kind := chat.BotHuman
	if actor.Type == "bot" || actor.Type == "app" || actor.Type == "oauthClient" || actor.Type == "integration" {
		kind = chat.BotBot
	}
	return chat.Actor{Adapter: adapterName, Tenant: tenant, ID: actor.ID, Name: firstNonEmpty(actor.DisplayName, actor.Name), BotKind: kind}
}

func systemActor(tenant string, sessionID string) chat.Actor {
	return chat.Actor{Adapter: adapterName, Tenant: tenant, ID: "agent-session:" + sessionID, BotKind: chat.BotUnknown}
}

func (a *Adapter) verifySignatureWithSecret(r *http.Request, body []byte, secret string) error {
	got := r.Header.Get("Linear-Signature")
	if got == "" {
		return errors.New("linear: missing signature")
	}
	decoded, err := hex.DecodeString(got)
	if err != nil {
		return errors.New("linear: invalid signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, decoded) {
		return errors.New("linear: signature mismatch")
	}
	return nil
}

func (a *Adapter) verifyTimestamp(timestamp int64) error {
	if timestamp == 0 {
		return errors.New("linear: webhook timestamp is required")
	}
	if a.signatureTolerance == 0 {
		return nil
	}
	sent := time.UnixMilli(timestamp)
	if absDuration(a.now().Sub(sent)) > a.signatureTolerance {
		return errors.New("linear: webhook timestamp outside tolerance")
	}
	return nil
}

func (a *Adapter) ensureToken(ctx context.Context) error {
	a.mu.Lock()
	needsRefresh := a.accessToken == "" || (!a.tokenExpiry.IsZero() && a.now().After(a.tokenExpiry.Add(-tokenRefreshBuffer)))
	a.mu.Unlock()
	if !needsRefresh {
		return nil
	}
	return a.refreshToken(ctx)
}

func (a *Adapter) refreshToken(ctx context.Context) error {
	token, expiry, err := a.exchangeClientCredentials(ctx, a.clientCredentials)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.accessToken = token
	a.tokenExpiry = expiry
	a.mu.Unlock()
	return nil
}

// exchangeClientCredentials performs the App-Actor Client Credentials grant and
// verifies the granted scopes, returning the access token and its expiry. It is
// shared by the single-install refresh and the per-tenant multi-tenant exchange.
func (a *Adapter) exchangeClientCredentials(ctx context.Context, creds ClientCredentials) (string, time.Time, error) {
	scopes := normalizeScopeList(creds.Scopes)
	if len(scopes) == 0 {
		scopes = append([]string(nil), defaultClientCredentialScopes...)
	}
	values := url.Values{}
	values.Set("grant_type", "client_credentials")
	values.Set("client_id", creds.ClientID)
	values.Set("client_secret", creds.ClientSecret)
	values.Set("scope", strings.Join(scopes, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/oauth/token", strings.NewReader(values.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp oauthTokenResponse
	if err := a.doJSON(req, &resp); err != nil {
		return "", time.Time{}, fmt.Errorf("linear: fetch client credentials token: %w", err)
	}
	if resp.AccessToken == "" {
		return "", time.Time{}, errors.New("linear: token response did not return access token")
	}
	if err := verifyGrantedScopes(scopes, resp.Scope); err != nil {
		return "", time.Time{}, err
	}
	var expiry time.Time
	if resp.ExpiresIn > 0 {
		expiry = a.now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	}
	return resp.AccessToken, expiry, nil
}

// tenantToken resolves the access token for one Platform Tenant in multi-tenant
// mode. A pre-exchanged installation AccessToken is used directly; otherwise the
// adapter performs the lazy client-credentials exchange and caches the derived
// token keyed by tenant (ADR-0001), reusing it until near expiry.
func (a *Adapter) tenantToken(ctx context.Context, tenant string, cred LinearInstall) (string, error) {
	if cred.AccessToken != "" {
		return cred.AccessToken, nil
	}
	if cred.ClientCredentials.ClientID == "" || cred.ClientCredentials.ClientSecret == "" {
		return "", errors.New("linear: install credential has neither access token nor client credentials")
	}
	a.tenantTokensMu.Lock()
	cached := a.tenantTokens[tenant]
	if cached != nil && cached.accessToken != "" && (cached.expiry.IsZero() || a.now().Before(cached.expiry.Add(-tokenRefreshBuffer))) {
		token := cached.accessToken
		a.tenantTokensMu.Unlock()
		return token, nil
	}
	a.tenantTokensMu.Unlock()

	token, expiry, err := a.exchangeClientCredentials(ctx, cred.ClientCredentials)
	if err != nil {
		return "", err
	}
	a.tenantTokensMu.Lock()
	a.tenantTokens[tenant] = &tenantToken{accessToken: token, expiry: expiry}
	a.tenantTokensMu.Unlock()
	return token, nil
}

// resolveToken returns the bearer token for an outbound call to the given Platform
// Tenant. Single-install uses the shared lazily-refreshed token; multi-tenant
// re-fetches the durable install record from the store and resolves a per-tenant
// derived token, so the post fails cleanly when the install record is gone.
func (a *Adapter) resolveToken(ctx context.Context, tenant string) (string, error) {
	if !a.multiTenant() {
		if err := a.ensureToken(ctx); err != nil {
			return "", err
		}
		a.mu.Lock()
		token := a.accessToken
		a.mu.Unlock()
		return token, nil
	}
	if tenant == "" {
		return "", errors.New("linear: thread organization is required")
	}
	install, err := a.installStore.Lookup(ctx, adapterName, tenant)
	if err != nil {
		return "", fmt.Errorf("linear: install lookup: %w", err)
	}
	cred, err := linearCredential(install.Credential)
	if err != nil {
		return "", err
	}
	return a.tenantToken(ctx, tenant, cred)
}

func linearCredential(credential any) (LinearInstall, error) {
	switch cred := credential.(type) {
	case LinearInstall:
		return cred, nil
	case *LinearInstall:
		if cred == nil {
			return LinearInstall{}, errors.New("linear: install credential is nil")
		}
		return *cred, nil
	default:
		return LinearInstall{}, fmt.Errorf("linear: install credential is not linear.LinearInstall, got %T", credential)
	}
}

func (a *Adapter) fetchIdentity(ctx context.Context) (linearIdentity, error) {
	a.mu.Lock()
	token := a.accessToken
	a.mu.Unlock()
	var resp graphQLResponse[identityData]
	if err := a.callGraphQLWithToken(ctx, token, `query ViewerIdentity { viewer { id name displayName organization { id } } }`, nil, &resp); err != nil {
		return linearIdentity{}, err
	}
	if err := resp.firstError(); err != nil {
		return linearIdentity{}, err
	}
	viewer := resp.Data.Viewer
	return linearIdentity{BotUserID: viewer.ID, Name: firstNonEmpty(viewer.DisplayName, viewer.Name), OrganizationID: viewer.Organization.ID}, nil
}

// activityRequest is the single low-level shape every agent-activity caller
// funnels through: the public CreateAgentActivity escape hatch, the typed
// helpers (PostAction / PostElicitation / PostError), PostThought, and the
// Thread.Post response path. It is the generic agent-activity codec the deep
// module is built around.
type activityRequest struct {
	content        map[string]any
	signal         string
	signalMetadata any
	ephemeral      bool
}

// createAgentActivity sends an AgentActivityCreate mutation for the given
// agent-session thread. Callers reach it only after agentSessionPayload (or, for
// the response path, PostMessage's non-comment branch) has confirmed the kind. It
// enforces Linear's rule that only thought and action activities may be
// ephemeral, omits empty signal / signalMetadata, and returns the created
// activity identity on the agent-session Thread ID.
func (a *Adapter) createAgentActivity(ctx context.Context, thread threadPayload, req activityRequest) (*chat.SentMessage, error) {
	contentType, _ := req.content["type"].(string)
	if req.ephemeral && contentType != activityTypeThought && contentType != activityTypeAction {
		return nil, fmt.Errorf("linear: ephemeral is only valid for thought and action activities, not %q", contentType)
	}
	input := map[string]any{
		"agentSessionId": thread.Session,
		"content":        req.content,
		"ephemeral":      req.ephemeral,
	}
	if req.signal != "" {
		input["signal"] = req.signal
	}
	if req.signalMetadata != nil {
		input["signalMetadata"] = req.signalMetadata
	}
	variables := map[string]any{"input": input}
	var resp graphQLResponse[agentActivityData]
	if err := a.callGraphQL(ctx, thread.Organization, `mutation AgentActivityCreate($input: AgentActivityCreateInput!) { agentActivityCreate(input: $input) { success agentActivity { id } } }`, variables, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	if !resp.Data.AgentActivityCreate.Success || resp.Data.AgentActivityCreate.AgentActivity.ID == "" {
		return nil, errors.New("linear: failed to create agent activity")
	}
	id, err := encodeThreadID(thread)
	if err != nil {
		return nil, err
	}
	return &chat.SentMessage{ID: resp.Data.AgentActivityCreate.AgentActivity.ID, ThreadID: id, Raw: resp.Data.AgentActivityCreate}, nil
}

// callGraphQL resolves the per-tenant bearer token for the given Platform Tenant
// and issues the GraphQL request. Single-install resolves the shared lazily
// refreshed token; multi-tenant resolves a per-tenant derived token.
func (a *Adapter) callGraphQL(ctx context.Context, tenant string, query string, variables any, dest any) error {
	token, err := a.resolveToken(ctx, tenant)
	if err != nil {
		return err
	}
	return a.callGraphQLWithToken(ctx, token, query, variables, dest)
}

func (a *Adapter) callGraphQLWithToken(ctx context.Context, token string, query string, variables any, dest any) error {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return a.doJSON(req, dest)
}

func (a *Adapter) doJSON(req *http.Request, dest any) error {
	return a.doJSONWithRetry(req, dest)
}

// Linear thread kinds carried inside the opaque, versioned Thread ID. The empty
// value decodes as threadKindAgentSession so agent-session Thread IDs minted
// before the thread-kind discriminator (ADR 0013) keep decoding unchanged.
const (
	threadKindAgentSession = "agent_session"
	threadKindComment      = "comment"
)

type threadPayload struct {
	Organization string `json:"org"`
	Issue        string `json:"issue"`
	Comment      string `json:"comment,omitempty"`
	Session      string `json:"session,omitempty"`
	// Kind discriminates agent-session threads (ADR 0008) from generic
	// issue-comment threads (ADR 0013). Empty means agent-session for backward
	// compatibility with Thread IDs minted before the discriminator existed.
	Kind string `json:"kind,omitempty"`
}

func (p threadPayload) kind() string {
	if p.Kind == "" {
		return threadKindAgentSession
	}
	return p.Kind
}

// validate checks the per-kind required identity fields: Organization and Issue
// for every kind, plus a Session for agent-session threads or a Comment for
// comment threads.
func (p threadPayload) validate() error {
	if p.Organization == "" {
		return errors.New("linear: thread organization is required")
	}
	if p.Issue == "" {
		return errors.New("linear: thread issue is required")
	}
	switch p.kind() {
	case threadKindAgentSession:
		if p.Session == "" {
			return errors.New("linear: thread agent session is required")
		}
	case threadKindComment:
		if p.Comment == "" {
			return errors.New("linear: thread comment is required")
		}
	default:
		return fmt.Errorf("linear: unsupported thread kind %q", p.Kind)
	}
	return nil
}

func encodeThreadID(payload threadPayload) (chat.ThreadID, error) {
	if err := payload.validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return chat.ThreadID("linear:v1:" + base64.RawURLEncoding.EncodeToString(body)), nil
}

func decodeThreadID(id chat.ThreadID) (threadPayload, error) {
	const prefix = "linear:v1:"
	if !strings.HasPrefix(string(id), prefix) {
		return threadPayload{}, fmt.Errorf("linear: malformed thread id %q", id)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(id), prefix))
	if err != nil {
		return threadPayload{}, fmt.Errorf("linear: decode thread id: %w", err)
	}
	var payload threadPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return threadPayload{}, fmt.Errorf("linear: parse thread id: %w", err)
	}
	if err := payload.validate(); err != nil {
		return threadPayload{}, fmt.Errorf("linear: invalid thread id %q: %w", id, err)
	}
	return payload, nil
}

func (a *Adapter) payloadFromThread(thread chat.ThreadRef) (threadPayload, error) {
	if thread.Adapter != adapterName {
		return threadPayload{}, fmt.Errorf("linear: thread adapter %q is not linear", thread.Adapter)
	}
	payload, err := a.validateThreadPayload(thread.ID)
	if err != nil {
		return threadPayload{}, err
	}
	return payload, nil
}

func validatePostableMessage(msg chat.PostableMessage) error {
	if msg.Text == "" {
		return errors.New("linear: post message text is required")
	}
	switch msg.Format {
	case chat.MessageFormatText, chat.MessageFormatMarkdown:
		return nil
	default:
		return fmt.Errorf("linear: unsupported message format %d", msg.Format)
	}
}

type webhookEnvelope struct {
	Type             string          `json:"type"`
	Action           string          `json:"action"`
	OrganizationID   string          `json:"organizationId"`
	OAuthClientID    string          `json:"oauthClientId"`
	AppUserID        string          `json:"appUserId"`
	CreatedAt        string          `json:"createdAt"`
	PromptContext    string          `json:"promptContext"`
	Guidance         string          `json:"guidance"`
	PreviousComments json.RawMessage `json:"previousComments"`
	WebhookID        string          `json:"webhookId"`
	WebhookTimestamp int64           `json:"webhookTimestamp"`
	AgentSession     agentSession    `json:"agentSession"`
	AgentActivity    *agentActivity  `json:"agentActivity"`
	// Data carries the Comment payload for type=="Comment" webhooks (ADR 0013).
	Data *commentData `json:"data"`
}

type agentSession struct {
	ID         string          `json:"id"`
	IssueID    string          `json:"issueId"`
	Issue      issueRef        `json:"issue"`
	IssueRaw   json.RawMessage `json:"-"`
	AppUserID  string          `json:"appUserId"`
	Comment    *sessionComment `json:"comment"`
	CommentRaw json.RawMessage `json:"-"`
	Creator    *webhookActor   `json:"creator"`
	URL        string          `json:"url"`
	CreatedAt  string          `json:"createdAt"`
}

// UnmarshalJSON preserves the verbatim issue and comment sub-objects on the
// Platform Escape Hatch (ADR 0008) while still decoding the typed fields.
func (s *agentSession) UnmarshalJSON(data []byte) error {
	type alias agentSession
	var raw struct {
		alias
		Issue   json.RawMessage `json:"issue"`
		Comment json.RawMessage `json:"comment"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = agentSession(raw.alias)
	if len(raw.Issue) > 0 && string(raw.Issue) != "null" {
		s.IssueRaw = append(json.RawMessage(nil), raw.Issue...)
		if err := json.Unmarshal(raw.Issue, &s.Issue); err != nil {
			return err
		}
	}
	if len(raw.Comment) > 0 && string(raw.Comment) != "null" {
		s.CommentRaw = append(json.RawMessage(nil), raw.Comment...)
		var comment sessionComment
		if err := json.Unmarshal(raw.Comment, &comment); err != nil {
			return err
		}
		s.Comment = &comment
	}
	return nil
}

type issueRef struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Identifier string  `json:"identifier"`
	URL        string  `json:"url"`
	Team       teamRef `json:"team"`
}

type teamRef struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

type sessionComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type agentActivity struct {
	ID              string               `json:"id"`
	SourceCommentID string               `json:"sourceCommentId"`
	Body            string               `json:"body"`
	Content         agentActivityContent `json:"content"`
	User            webhookActor         `json:"user"`
	CreatedAt       string               `json:"createdAt"`
	Signal          string               `json:"signal"`
	SignalMetadata  json.RawMessage      `json:"signalMetadata"`
}

type agentActivityContent struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

type webhookActor struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

type graphQLRequest struct {
	Query     string `json:"query"`
	Variables any    `json:"variables,omitempty"`
}

type graphQLResponse[T any] struct {
	Data   T              `json:"data"`
	Errors []graphQLError `json:"errors"`
}

func (r graphQLResponse[T]) firstError() error {
	if len(r.Errors) == 0 {
		return nil
	}
	return errors.New("linear: graphql error: " + r.Errors[0].Message)
}

type graphQLError struct {
	Message string `json:"message"`
}

type identityData struct {
	Viewer identityViewer `json:"viewer"`
}

type identityViewer struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	DisplayName  string       `json:"displayName"`
	Organization organization `json:"organization"`
}

type organization struct {
	ID string `json:"id"`
}

type linearIdentity struct {
	BotUserID      string
	Name           string
	OrganizationID string
}

type agentActivityData struct {
	AgentActivityCreate agentActivityCreate `json:"agentActivityCreate"`
}

type agentActivityCreate struct {
	Success       bool        `json:"success"`
	AgentActivity activityRef `json:"agentActivity"`
}

type activityRef struct {
	ID string `json:"id"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func normalizeScopeList(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	out := make([]string, 0, len(scopes))
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	return out
}

func verifyGrantedScopes(requested []string, granted string) error {
	grantedScopes := parseGrantedScopes(granted)
	if len(grantedScopes) == 0 {
		return errors.New("linear: token response did not return granted scopes")
	}
	var missing []string
	for _, scope := range normalizeScopeList(requested) {
		if _, ok := grantedScopes[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("linear: token response missing granted scopes: %s", strings.Join(missing, ","))
	}
	return nil
}

func parseGrantedScopes(value string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, scope := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		scope = strings.TrimSpace(scope)
		if scope != "" {
			out[scope] = struct{}{}
		}
	}
	return out
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func assertAdapter(a *Adapter) {
	if a == nil {
		panic("linear: nil adapter")
	}
}
