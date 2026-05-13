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

const (
	adapterName               = "linear"
	defaultAPIBaseURL         = "https://api.linear.app"
	defaultSignatureTolerance = time.Minute
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
	creds := opts.ClientCredentials
	if len(creds.Scopes) == 0 {
		creds.Scopes = []string{"read", "write", "comments:create", "issues:create", "app:mentionable", "app:assignable"}
	}
	return &Adapter{
		webhookSecret:      opts.WebhookSecret,
		clientCredentials:  creds,
		apiBaseURL:         apiBaseURL,
		client:             client,
		now:                now,
		signatureTolerance: tolerance,
		logger:             logger,
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
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
		event, ok := a.normalizeEvent(envelope, body)
		if ok {
			if err := dispatch(r.Context(), event); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (a *Adapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	payload, err := decodeThreadID(id)
	if err != nil {
		return chat.ThreadRef{}, err
	}
	return chat.ThreadRef{ID: id, Adapter: adapterName, Tenant: payload.Organization, Channel: payload.Issue, Root: payload.Session, Raw: payload}, nil
}

func (a *Adapter) PostMessage(ctx context.Context, thread chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	if err := validatePostableMessage(msg); err != nil {
		return nil, err
	}
	payload, err := payloadFromThread(thread)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, agentActivityContent{Type: "response", Body: msg.Text}, false)
}

// PostThought posts an ephemeral Linear thought activity in an agent session.
func (a *Adapter) PostThought(ctx context.Context, id chat.ThreadID, text string) (*chat.SentMessage, error) {
	assertAdapter(a)
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("linear: thought text is required")
	}
	payload, err := decodeThreadID(id)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, agentActivityContent{Type: "thought", Body: text}, true)
}

func (a *Adapter) BotActor() chat.Actor {
	assertAdapter(a)
	a.mu.Lock()
	defer a.mu.Unlock()
	return chat.Actor{Adapter: adapterName, Tenant: a.organizationID, ID: a.botUserID, Name: a.botName, BotKind: chat.BotBot}
}

func (a *Adapter) normalizeEvent(envelope webhookEnvelope, raw []byte) (*chat.Event, bool) {
	if envelope.Type != "AgentSessionEvent" {
		return nil, false
	}
	if envelope.OrganizationID == "" || envelope.AgentSession.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear agent session event", "reason", "missing organization or session")
		return nil, false
	}
	issueID := firstNonEmpty(envelope.AgentSession.IssueID, envelope.AgentSession.Issue.ID)
	if issueID == "" {
		a.logger.Warn("ignoring Linear agent session event without issue", "session_id", envelope.AgentSession.ID)
		return nil, false
	}
	if envelope.AgentSession.AppUserID != "" && envelope.AgentSession.AppUserID != a.BotActor().ID {
		a.logger.Warn("ignoring Linear agent session event for another app actor", "session_id", envelope.AgentSession.ID)
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
		if envelope.AgentSession.Comment == nil || envelope.AgentSession.Comment.ID == "" {
			a.logger.Warn("ignoring Linear created agent session without source comment", "session_id", envelope.AgentSession.ID)
			return nil, false
		}
		sourceID = envelope.AgentSession.Comment.ID
		text = envelope.AgentSession.Comment.Body
		if envelope.AgentSession.Creator != nil {
			author = actorFromWebhook(envelope.OrganizationID, *envelope.AgentSession.Creator)
		} else {
			author = a.BotActor()
		}
	case "prompted":
		activity := envelope.AgentActivity
		if activity == nil || activity.SourceCommentID == "" {
			a.logger.Warn("ignoring Linear prompted agent session without source comment", "session_id", envelope.AgentSession.ID)
			return nil, false
		}
		sourceID = activity.SourceCommentID
		text = activity.Content.Body
		author = actorFromWebhook(envelope.OrganizationID, activity.User)
	default:
		return nil, false
	}
	if sourceID == "" || author.ID == "" {
		a.logger.Warn("ignoring unbuildable Linear agent session event", "session_id", envelope.AgentSession.ID)
		return nil, false
	}
	threadID, err := encodeThreadID(threadPayload{Organization: envelope.OrganizationID, Issue: issueID, Comment: rootCommentID, Session: envelope.AgentSession.ID})
	if err != nil {
		a.logger.Warn("ignoring Linear agent session event with invalid thread", "error", err)
		return nil, false
	}
	return &chat.Event{
		ID:       "linear:" + envelope.OrganizationID + ":message:" + sourceID,
		Adapter:  adapterName,
		Tenant:   envelope.OrganizationID,
		ThreadID: threadID,
		Raw:      json.RawMessage(raw),
		Message:  &chat.Message{ID: sourceID, Text: text, Author: author, Mentioned: true, Raw: json.RawMessage(raw)},
	}, true
}

func actorFromWebhook(tenant string, actor webhookActor) chat.Actor {
	kind := chat.BotHuman
	if actor.Type == "bot" || actor.Type == "app" || actor.Type == "oauthClient" || actor.Type == "integration" {
		kind = chat.BotBot
	}
	return chat.Actor{Adapter: adapterName, Tenant: tenant, ID: actor.ID, Name: firstNonEmpty(actor.DisplayName, actor.Name), BotKind: kind}
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

func (a *Adapter) createAgentActivity(ctx context.Context, thread threadPayload, content agentActivityContent, ephemeral bool) (*chat.SentMessage, error) {
	variables := map[string]any{"input": map[string]any{"agentSessionId": thread.Session, "content": content, "ephemeral": ephemeral}}
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
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return err
	}
	return nil
}

type threadPayload struct {
	Organization string `json:"org"`
	Issue        string `json:"issue"`
	Comment      string `json:"comment,omitempty"`
	Session      string `json:"session"`
}

func encodeThreadID(payload threadPayload) (chat.ThreadID, error) {
	if payload.Organization == "" {
		return "", errors.New("linear: thread organization is required")
	}
	if payload.Issue == "" {
		return "", errors.New("linear: thread issue is required")
	}
	if payload.Session == "" {
		return "", errors.New("linear: thread agent session is required")
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
	if payload.Organization == "" || payload.Issue == "" || payload.Session == "" {
		return threadPayload{}, fmt.Errorf("linear: invalid thread id %q", id)
	}
	return payload, nil
}

func payloadFromThread(thread chat.ThreadRef) (threadPayload, error) {
	if thread.Adapter != adapterName {
		return threadPayload{}, fmt.Errorf("linear: thread adapter %q is not linear", thread.Adapter)
	}
	if payload, ok := thread.Raw.(threadPayload); ok {
		return payload, nil
	}
	return decodeThreadID(thread.ID)
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
	Type             string         `json:"type"`
	Action           string         `json:"action"`
	OrganizationID   string         `json:"organizationId"`
	CreatedAt        string         `json:"createdAt"`
	WebhookTimestamp int64          `json:"webhookTimestamp"`
	AgentSession     agentSession   `json:"agentSession"`
	AgentActivity    *agentActivity `json:"agentActivity"`
}

type agentSession struct {
	ID        string          `json:"id"`
	IssueID   string          `json:"issueId"`
	Issue     issueRef        `json:"issue"`
	AppUserID string          `json:"appUserId"`
	Comment   *sessionComment `json:"comment"`
	Creator   *webhookActor   `json:"creator"`
	URL       string          `json:"url"`
}

type issueRef struct {
	ID string `json:"id"`
}

type sessionComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type agentActivity struct {
	ID              string               `json:"id"`
	SourceCommentID string               `json:"sourceCommentId"`
	Content         agentActivityContent `json:"content"`
	User            webhookActor         `json:"user"`
	CreatedAt       string               `json:"createdAt"`
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
