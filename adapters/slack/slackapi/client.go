package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/chat"
)

// DefaultBaseURL is the Slack Web API base URL that New uses when
// Options.BaseURL is empty.
const DefaultBaseURL = "https://slack.com/api"

const adapterName = "slack"

// defaultFileOrigins are the origins that serve Slack files (url_private,
// url_private_download, and upload URLs).
var defaultFileOrigins = []string{"https://files.slack.com", "https://slack.com"}

// Options configures a Client.
type Options struct {
	// Token is the bot or user token that Call sends as a bearer token.
	Token string
	// BaseURL is the Web API base URL. Empty uses DefaultBaseURL. New removes a
	// trailing slash.
	BaseURL string
	// HTTPClient sends every request. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// RetryPolicy bounds rate-limit retry. The zero value applies the default
	// policy (3 attempts, 2s of total backoff).
	RetryPolicy RetryPolicy
	// FileOrigins lists the origins (scheme and host) that serve Slack files.
	// Empty uses https://files.slack.com and https://slack.com. New normalizes
	// each entry to scheme://host and drops an entry that is not an absolute URL.
	FileOrigins []string
	// Observer receives an ObsAdapterCall for every attempt and an ObsRateLimit
	// for every throttled response. Nil is a no-op.
	Observer chat.Observer
	// Logger receives a warning for every throttled response. Nil discards it.
	Logger *slog.Logger
}

// Client calls the Slack Web API. It is safe for concurrent use.
type Client struct {
	token       string
	baseURL     string
	httpClient  *http.Client
	retryPolicy RetryPolicy
	fileOrigins []string
	observer    chat.Observer
	logger      *slog.Logger
}

// New returns a Client for opts, with defaults for every empty field.
func New(opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	fileOrigins := opts.FileOrigins
	if len(fileOrigins) == 0 {
		fileOrigins = defaultFileOrigins
	}
	origins := make([]string, 0, len(fileOrigins))
	for _, raw := range fileOrigins {
		if origin, ok := normalizeOrigin(raw); ok {
			origins = append(origins, origin)
		}
	}
	observer := opts.Observer
	if observer == nil {
		observer = noopObserver{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{
		token:       opts.Token,
		baseURL:     baseURL,
		httpClient:  httpClient,
		retryPolicy: opts.RetryPolicy.withDefaults(),
		fileOrigins: origins,
		observer:    observer,
		logger:      logger,
	}
}

// WithToken returns a copy of c that sends token instead of the token of c. The
// copy shares the HTTP client, retry policy, observer, and logger of c.
func (c *Client) WithToken(token string) *Client {
	clone := *c
	clone.token = token
	return &clone
}

// Call is the escape hatch for Web API methods with no typed wrapper. It POSTs
// payload as JSON to BaseURL/method with the bearer token, retries throttling
// within the RetryPolicy, and returns *RateLimited when retry is exhausted. A nil
// payload sends an empty JSON object. An ok:false response or a non-2xx status
// returns *APIError. Otherwise, when dest is not nil, Call decodes the response
// body into dest.
func (c *Client) Call(ctx context.Context, method string, payload, dest any) error {
	body := []byte("{}")
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("slack: encode %s request: %w", method, err)
		}
	}
	return c.call(ctx, method, "application/json", body, dest)
}

// PostResponseURL posts payload as JSON to the response_url of a slash command
// or an interaction. The response_url is pre-authorized, so the request carries
// no token. It retries throttling like Call. It checks only the HTTP status: a
// non-2xx status returns *APIError with Method "response_url".
func (c *Client) PostResponseURL(ctx context.Context, responseURL string, payload any) error {
	const method = "response_url"
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: encode %s request: %w", method, err)
	}
	_, _, err = c.send(ctx, method, responseURL, "", "application/json", body)
	return err
}

// call sends body to the Web API method with the given content type, checks the
// ok field, and decodes the response into dest when dest is not nil.
func (c *Client) call(ctx context.Context, method, contentType string, body []byte, dest any) error {
	status, payload, err := c.send(ctx, method, c.baseURL+"/"+method, c.token, contentType, body)
	if err != nil {
		return err
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("slack: decode %s response: %w", method, err)
	}
	if !envelope.OK {
		return &APIError{
			Method:     method,
			StatusCode: status,
			Code:       envelope.Error,
			Detail:     envelope.detail(),
			Raw:        payload,
		}
	}
	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(payload, dest); err != nil {
		return fmt.Errorf("slack: decode %s response: %w", method, err)
	}
	return nil
}

// send POSTs body to target with retry. It sends token as a bearer token when
// token is not empty. A non-2xx status after retry returns *APIError.
func (c *Client) send(ctx context.Context, method, target, token, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", contentType)
	status, payload, err := c.doWithRetry(ctx, method, req)
	if err != nil {
		return 0, nil, err
	}
	if !successStatus(status) {
		return 0, nil, &APIError{Method: method, StatusCode: status, Raw: payload}
	}
	return status, payload, nil
}

// normalizeOrigin returns the scheme://host origin of raw, lowercased. It
// reports false when raw is not an absolute URL with a host.
func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return originOf(u), true
}

// originOf returns the lowercased scheme://host origin of u.
func originOf(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

type noopObserver struct{}

func (noopObserver) Event(context.Context, chat.ObservationName, ...chat.Attr) {}

func (noopObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) End(chat.DispatchOutcome, ...chat.Attr) {}
