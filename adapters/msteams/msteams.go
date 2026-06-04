package msteams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/chat"
)

const defaultMaxWebhookBodyBytes = 1 << 20

// Options configures the Teams adapter. The single-install model mirrors Slack's
// MVP: one Azure Bot registration (MicrosoftAppID + MicrosoftAppPassword) serving
// the deployment. Multi-tenant credential resolution is deferred to ADR 0006.
type Options struct {
	// MicrosoftAppID is the bot's Microsoft App (client) ID. It is the required
	// inbound JWT audience and the outbound client_credentials client id.
	MicrosoftAppID string
	// MicrosoftAppPassword is the bot's client secret for outbound token minting.
	// (Managed identity is a spike-time alternative, ADR 0007 Open Question 4.)
	MicrosoftAppPassword string
	// BotID is the bot's Teams ChannelAccount id as it appears in Activity.recipient
	// and in a self-authored Activity.from -- conventionally "28:"+MicrosoftAppID,
	// which is the default when this is empty. It drives tenant-safe Self Message
	// filtering and is the outbound message sender id. The exact self id format is
	// spike-required (ADR 0007 Open Question 10).
	BotID string
	// BotName is an optional display name set on outbound messages.
	BotName string
	// TenantID is the bot's home Azure AD tenant, used only for BotActor().Tenant.
	// Inbound Actors always carry the per-Activity conversation tenant regardless.
	TenantID string

	// HTTPClient is injected for all inbound-key, token, and Connector calls; nil
	// uses http.DefaultClient.
	HTTPClient *http.Client
	// OpenIDMetadataURL overrides the Bot Connector OpenID metadata document (for
	// tests). Empty uses the production Bot Framework URL.
	OpenIDMetadataURL string
	// TokenURL overrides the outbound client_credentials endpoint (for tests). Empty
	// uses the single-tenant Bot Framework URL.
	TokenURL string
	// Now injects the clock for token expiry, JWKS cache freshness, and the JWT
	// validity window; nil uses time.Now.
	Now func() time.Time
	// Logger receives structured adapter logs; nil discards.
	Logger *slog.Logger
	// Observer receives adapter-facing observations (ObsAdapterCall, ObsRateLimit);
	// nil is a no-op. It is adapter-owned wiring, not on the core Adapter interface.
	Observer chat.Observer
	// RetryPolicy bounds outbound Connector rate-limit retry (ADR 0005). The zero
	// value applies a conservative default that stays under the Teams turn budget.
	RetryPolicy RetryPolicy
	// MaxWebhookBodyBytes caps the inbound Activity body; zero applies a 1 MiB
	// default.
	MaxWebhookBodyBytes int64
}

// Adapter is the Microsoft Teams Platform Adapter. It satisfies chat.Adapter and
// owns the Bot Framework boundary (inbound JWT/JWKS validation, outbound token
// minting, Activity normalization, the opaque conversationReference Thread ID, and
// Connector posting); the runtime is unchanged.
type Adapter struct {
	botID       string
	botName     string
	tenantID    string
	client      *http.Client
	now         func() time.Time
	logger      *slog.Logger
	observer    chat.Observer
	retryPolicy RetryPolicy
	maxBody     int64

	validator *authValidator
	tokens    *tokenSource
}

var _ chat.Adapter = (*Adapter)(nil)

// New validates configuration and constructs the adapter. Missing App ID or
// password is a fail-fast Runtime Construction error, consistent with fallible
// adapter construction elsewhere.
func New(ctx context.Context, opts Options) (*Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.MicrosoftAppID == "" {
		return nil, errors.New("msteams: microsoft app id is required")
	}
	if opts.MicrosoftAppPassword == "" {
		return nil, errors.New("msteams: microsoft app password is required")
	}

	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	observer := opts.Observer
	if observer == nil {
		observer = noopObserver{}
	}
	botID := opts.BotID
	if botID == "" {
		botID = "28:" + opts.MicrosoftAppID
	}
	metaURL := opts.OpenIDMetadataURL
	if metaURL == "" {
		metaURL = openIDMetadataURL
	}
	tokenURL := opts.TokenURL
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	maxBody := opts.MaxWebhookBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxWebhookBodyBytes
	}

	return &Adapter{
		botID:       botID,
		botName:     opts.BotName,
		tenantID:    opts.TenantID,
		client:      client,
		now:         now,
		logger:      logger,
		observer:    observer,
		retryPolicy: opts.RetryPolicy.withDefaults(),
		maxBody:     maxBody,
		validator: &authValidator{
			appID:         opts.MicrosoftAppID,
			openIDMetaURL: metaURL,
			issuer:        botConnectorIssuer,
			client:        client,
			now:           now,
			cacheTTL:      defaultJWKSCacheTTL,
		},
		tokens: &tokenSource{
			appID:     opts.MicrosoftAppID,
			appSecret: opts.MicrosoftAppPassword,
			tokenURL:  tokenURL,
			scope:     connectorScope,
			client:    client,
			now:       now,
		},
	}, nil
}

func (a *Adapter) Name() string { return adapterName }

// Init mints the outbound client_credentials token so invalid Bot Framework
// credentials fail fast before any webhook is served, and warms the token cache.
func (a *Adapter) Init(ctx context.Context) error {
	if _, err := a.tokens.get(ctx); err != nil {
		return fmt.Errorf("msteams: validate credentials: %w", err)
	}
	return nil
}

func (a *Adapter) Shutdown(context.Context) error { return nil }

// Webhook handles inbound Activities at the bot messaging endpoint. It decodes the
// Activity (a Supported Platform Shape), validates the inbound JWT before any
// normalization or dispatch (there is no path that skips validation), normalizes a
// message Activity into a runtime Event, dispatches synchronously, then acks with
// HTTP 200. The reply, if any, was sent by the handler via Thread.Post as a separate
// authenticated Connector call during dispatch -- there is no webhook-body reply
// shortcut for message activities. Work exceeding the turn budget is the runtime's
// Ack-Then-Work concern (ADR 0002), delivered later via a proactive Connector post.
func (a *Adapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.maxBody))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "msteams payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		var act activity
		if err := json.Unmarshal(body, &act); err != nil {
			http.Error(w, "invalid msteams activity", http.StatusBadRequest)
			return
		}

		if err := a.validator.validate(r.Context(), r.Header.Get("Authorization"), act.ServiceURL); err != nil {
			a.logger.Warn("msteams inbound auth rejected", "error", err)
			// A transient inability to fetch signing keys is the adapter's failure,
			// not a bad token: return a retryable 5xx so the Connector redelivers,
			// rather than a 403 it treats as permanent (silently dropping a valid
			// Activity).
			if errors.Is(err, errKeysUnavailable) {
				http.Error(w, "msteams signing keys unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "invalid msteams authorization", http.StatusForbidden)
			return
		}

		event, ok, err := a.normalizeActivity(act)
		if err != nil {
			a.logger.Warn("msteams normalize failed", "error", err)
			http.Error(w, "invalid msteams activity", http.StatusBadRequest)
			return
		}
		if ok {
			if err := dispatch(r.Context(), event); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

// ValidateThreadID decodes the opaque conversationReference Thread ID into a
// ThreadRef so Thread Handle reconstruction (out-of-webhook and proactive posting)
// works as it does for the other adapters.
func (a *Adapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	ref, err := decodeThreadID(id)
	if err != nil {
		return chat.ThreadRef{}, err
	}
	return chat.ThreadRef{
		ID:      id,
		Adapter: adapterName,
		Tenant:  ref.TenantID,
		Channel: ref.ConversationID,
		Direct:  ref.direct(),
		Raw:     ref,
	}, nil
}

// BotActor is the adapter's own identity, exposed for the chat.Adapter contract and
// Thread Handle use. Note: for Teams, Self Message filtering is done authoritatively
// in normalizeActivity (it drops the bot's own echo before dispatch), so the
// runtime's BotActor()/isSelfActor match does not independently fire for Teams
// messages — its Tenant is the configured home tenant while inbound actors carry the
// per-Activity conversation tenant. That self-drop is load-bearing and depends on
// BotID being correct (spike-required, ADR 0007 Open Question 10); set Options.BotID
// if the real Teams bot id is not "28:"+MicrosoftAppID.
func (a *Adapter) BotActor() chat.Actor {
	return chat.Actor{
		Adapter: adapterName,
		Tenant:  a.tenantID,
		ID:      a.botID,
		BotKind: chat.BotBot,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// noopObserver is the default Observer: it records nothing.
type noopObserver struct{}

func (noopObserver) Event(context.Context, chat.ObservationName, ...chat.Attr) {}

func (noopObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) End(chat.DispatchOutcome, ...chat.Attr) {}
