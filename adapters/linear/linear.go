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

// ClientCredentials configures single-install Linear app-actor authentication.
type ClientCredentials struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// Options configures the Linear adapter.
type Options struct {
	WebhookSecret      string
	ClientCredentials  ClientCredentials
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
}

func New(ctx context.Context, opts Options) (*Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.WebhookSecret == "" {
		return nil, errors.New("linear: webhook secret is required")
	}
	if opts.ClientCredentials.ClientID == "" {
		return nil, errors.New("linear: client credentials client id is required")
	}
	if opts.ClientCredentials.ClientSecret == "" {
		return nil, errors.New("linear: client credentials client secret is required")
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
	creds.Scopes = normalizeScopeList(creds.Scopes)
	if len(creds.Scopes) == 0 {
		creds.Scopes = append([]string(nil), defaultClientCredentialScopes...)
	}
	return &Adapter{
		webhookSecret:      opts.WebhookSecret,
		clientCredentials:  creds,
		apiBaseURL:         apiBaseURL,
		client:             client,
		now:                now,
		signatureTolerance: tolerance,
		logger:             logger,
		retryPolicy:        retryPolicy,
	}, nil
}

func (a *Adapter) Name() string { return adapterName }

func (a *Adapter) Init(ctx context.Context) error {
	assertAdapter(a)
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
		if err := a.verifySignature(r, body); err != nil {
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
		event, ok, err := a.normalizeEvent(r.Context(), envelope, body)
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
	bot := a.BotActor()
	if bot.Tenant != "" && payload.Organization != bot.Tenant {
		return threadPayload{}, fmt.Errorf(
			"linear: thread organization %q does not match initialized organization",
			payload.Organization,
		)
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

func (a *Adapter) normalizeEvent(_ context.Context, envelope webhookEnvelope, raw []byte) (*chat.Event, bool, error) {
	switch envelope.Type {
	case "AgentSessionEvent":
		event, ok := a.normalizeAgentSessionEvent(envelope, raw)
		return event, ok, nil
	case "Comment":
		event, ok := a.normalizeCommentEvent(envelope, raw)
		return event, ok, nil
	default:
		return nil, false, nil
	}
}

func (a *Adapter) normalizeAgentSessionEvent(envelope webhookEnvelope, raw []byte) (*chat.Event, bool) {
	if envelope.OrganizationID == "" || envelope.AgentSession.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear agent session event", "reason", "missing organization or session")
		return nil, false
	}
	bot := a.BotActor()
	if bot.Tenant != "" && envelope.OrganizationID != bot.Tenant {
		a.logger.Warn(
			"ignoring Linear agent session event for another organization",
			"session_id", envelope.AgentSession.ID,
			"organization_id", envelope.OrganizationID,
		)
		return nil, false
	}
	if envelope.OAuthClientID != "" && envelope.OAuthClientID != a.clientCredentials.ClientID {
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
	if appUserID != "" && appUserID != bot.ID {
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

func (a *Adapter) verifySignature(r *http.Request, body []byte) error {
	got := r.Header.Get("Linear-Signature")
	if got == "" {
		return errors.New("linear: missing signature")
	}
	decoded, err := hex.DecodeString(got)
	if err != nil {
		return errors.New("linear: invalid signature")
	}
	mac := hmac.New(sha256.New, []byte(a.webhookSecret))
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
	values := url.Values{}
	values.Set("grant_type", "client_credentials")
	values.Set("client_id", a.clientCredentials.ClientID)
	values.Set("client_secret", a.clientCredentials.ClientSecret)
	values.Set("scope", strings.Join(a.clientCredentials.Scopes, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/oauth/token", strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp oauthTokenResponse
	if err := a.doJSON(req, &resp); err != nil {
		return fmt.Errorf("linear: fetch client credentials token: %w", err)
	}
	if resp.AccessToken == "" {
		return errors.New("linear: token response did not return access token")
	}
	if err := verifyGrantedScopes(a.clientCredentials.Scopes, resp.Scope); err != nil {
		return err
	}

	var expiry time.Time
	if resp.ExpiresIn > 0 {
		expiry = a.now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	}
	a.mu.Lock()
	a.accessToken = resp.AccessToken
	a.tokenExpiry = expiry
	a.mu.Unlock()
	return nil
}

func (a *Adapter) fetchIdentity(ctx context.Context) (linearIdentity, error) {
	var resp graphQLResponse[identityData]
	if err := a.callGraphQL(ctx, `query ViewerIdentity { viewer { id name displayName organization { id } } }`, nil, &resp); err != nil {
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
	if err := a.callGraphQL(ctx, `mutation AgentActivityCreate($input: AgentActivityCreateInput!) { agentActivityCreate(input: $input) { success agentActivity { id } } }`, variables, &resp); err != nil {
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

func (a *Adapter) callGraphQL(ctx context.Context, query string, variables any, dest any) error {
	if err := a.ensureToken(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	a.mu.Lock()
	token := a.accessToken
	a.mu.Unlock()
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
