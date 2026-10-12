package slackapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/coder/chat"
)

// ErrFileTooLarge is returned, wrapped, by DownloadFile when the file is larger
// than the size limit.
var ErrFileTooLarge = errors.New("file exceeds the size limit")

// maxErrorBodyBytes caps the response body that DownloadFile keeps in
// APIError.Raw for a non-2xx status.
const maxErrorBodyBytes = 64 << 10

// maxRedirects is the redirect limit of http.Client when CheckRedirect is nil.
const maxRedirects = 10

// GetUploadURLExternalRequest is the request of files.getUploadURLExternal.
type GetUploadURLExternalRequest struct {
	// Filename is the name of the file.
	Filename string
	// Length is the size of the file in bytes.
	Length int64
	// AltText is the description of an image for screen readers.
	AltText string
	// SnippetType is the syntax type of a snippet, for example "go" or "text".
	SnippetType string
}

// GetUploadURLExternalResponse is the response of files.getUploadURLExternal.
type GetUploadURLExternalResponse struct {
	// UploadURL is the URL that receives the file content.
	UploadURL string `json:"upload_url"`
	// FileID is the ID of the new file.
	FileID string `json:"file_id"`
}

// FileSummary identifies an uploaded file in files.completeUploadExternal.
type FileSummary struct {
	// ID is the file ID from files.getUploadURLExternal.
	ID string `json:"id"`
	// Title is the file title. Empty uses the file name.
	Title string `json:"title,omitempty"`
}

// CompleteUploadExternalRequest is the request of
// files.completeUploadExternal.
type CompleteUploadExternalRequest struct {
	// Files are the uploaded files to complete.
	Files []FileSummary `json:"files"`
	// ChannelID is the channel to share the files in. Empty keeps the files
	// private.
	ChannelID string `json:"channel_id,omitempty"`
	// ThreadTS is the timestamp of the thread root to share the files in. It
	// requires ChannelID.
	ThreadTS string `json:"thread_ts,omitempty"`
	// InitialComment is the text of the message that shares the files.
	InitialComment string `json:"initial_comment,omitempty"`
}

// CompleteUploadExternalResponse is the response of
// files.completeUploadExternal.
type CompleteUploadExternalResponse struct {
	// Files are the completed files.
	Files []File `json:"files"`
}

// UploadFileRequest is the request of UploadFile.
type UploadFileRequest struct {
	// Filename is the name of the file.
	Filename string
	// Title is the file title. Empty uses Filename.
	Title string
	// Content is the file content.
	Content []byte
	// AltText is the description of an image for screen readers.
	AltText string
	// SnippetType is the syntax type of a snippet, for example "go" or "text".
	SnippetType string
	// ChannelID is the channel to share the file in. Empty keeps the file
	// private.
	ChannelID string
	// ThreadTS is the timestamp of the thread root to share the file in. It
	// requires ChannelID.
	ThreadTS string
	// InitialComment is the text of the message that shares the file.
	InitialComment string
}

// GetUploadURLExternal calls files.getUploadURLExternal to get an upload URL
// and an ID for a new file. It sends a form-encoded body.
func (c *Client) GetUploadURLExternal(ctx context.Context, req GetUploadURLExternalRequest) (*GetUploadURLExternalResponse, error) {
	values := url.Values{
		"filename": {req.Filename},
		"length":   {strconv.FormatInt(req.Length, 10)},
	}
	setFormString(values, "alt_txt", req.AltText)
	setFormString(values, "snippet_type", req.SnippetType)
	var resp GetUploadURLExternalResponse
	if err := c.Call(ctx, "files.getUploadURLExternal", values, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UploadToURL POSTs content as raw bytes to uploadURL, the upload_url of a
// files.getUploadURLExternal response. The upload URL is pre-authorized, so the
// request carries no token. It retries throttling like Call. The origin of
// uploadURL must be in Options.FileOrigins, else UploadToURL returns an error
// and sends nothing. The origin of every redirect must also be in
// Options.FileOrigins, else UploadToURL returns an error and does not follow
// the redirect. A 301, 302, or 303 redirect also returns an error, because
// net/http follows it with a GET that has no content. A non-2xx status returns
// *APIError with Method "upload_url".
func (c *Client) UploadToURL(ctx context.Context, uploadURL string, content []byte) error {
	const method = "upload_url"
	if err := c.checkFileURL(method, uploadURL); err != nil {
		return err
	}
	upload := *c
	upload.httpClient = c.fileHTTPClient("")
	_, _, err := upload.send(ctx, method, uploadURL, "", "application/octet-stream", content)
	return err
}

// CompleteUploadExternal calls files.completeUploadExternal to finish uploads
// and optionally share the files in a channel or a thread.
func (c *Client) CompleteUploadExternal(ctx context.Context, req CompleteUploadExternalRequest) (*CompleteUploadExternalResponse, error) {
	var resp CompleteUploadExternalResponse
	if err := c.Call(ctx, "files.completeUploadExternal", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UploadFile uploads req.Content as a new file with GetUploadURLExternal,
// UploadToURL, and CompleteUploadExternal, and returns the completed file. The
// error names the step that failed, for example "upload file: get upload URL:
// slack: files.getUploadURLExternal failed: invalid_auth", and wraps the error
// of that step, so *APIError and *RateLimited stay reachable through errors.As.
func (c *Client) UploadFile(ctx context.Context, req UploadFileRequest) (*File, error) {
	upload, err := c.GetUploadURLExternal(ctx, GetUploadURLExternalRequest{
		Filename:    req.Filename,
		Length:      int64(len(req.Content)),
		AltText:     req.AltText,
		SnippetType: req.SnippetType,
	})
	if err != nil {
		return nil, fmt.Errorf("upload file: get upload URL: %w", err)
	}
	if err := c.UploadToURL(ctx, upload.UploadURL, req.Content); err != nil {
		return nil, fmt.Errorf("upload file: upload content: %w", err)
	}
	resp, err := c.CompleteUploadExternal(ctx, CompleteUploadExternalRequest{
		Files:          []FileSummary{{ID: upload.FileID, Title: req.Title}},
		ChannelID:      req.ChannelID,
		ThreadTS:       req.ThreadTS,
		InitialComment: req.InitialComment,
	})
	if err != nil {
		return nil, fmt.Errorf("upload file: complete upload: %w", err)
	}
	if len(resp.Files) == 0 {
		return nil, errors.New("slack: upload file: complete upload: response has no files")
	}
	return &resp.Files[0], nil
}

// DownloadFile GETs fileURL, normally the url_private_download of a File, with
// the bearer token and copies the body to w. It returns the number of bytes
// written. It does not retry.
//
// The origin of fileURL must be in Options.FileOrigins, and so must the origin
// of every redirect, else DownloadFile returns an error. DownloadFile sends the
// bearer token on every redirect, because each redirect goes to an origin in
// Options.FileOrigins. An empty fileURL, for example the missing
// url_private_download of an external file, returns an error. A non-2xx status
// returns *APIError with Method "files.download". A bad token, or a token
// without the files:read scope, makes Slack redirect to the sign-in page of the
// workspace, whose origin is not in the default Options.FileOrigins. A
// text/html response is an error, because Slack serves stored files, HTML
// files too, as application/force-download, so an HTML response is a sign-in
// or error page. A body larger than maxBytes returns an error that wraps
// ErrFileTooLarge after DownloadFile has written maxBytes bytes to w. maxBytes
// must be positive.
func (c *Client) DownloadFile(ctx context.Context, fileURL string, w io.Writer, maxBytes int64) (int64, error) {
	const method = "files.download"
	if maxBytes <= 0 {
		return 0, fmt.Errorf("slack: %s: maxBytes must be positive, got %d", method, maxBytes)
	}
	if err := c.checkFileURL(method, fileURL); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return 0, fmt.Errorf("slack: %s request: %w", method, err)
	}
	setBearer(req, c.token)
	c.observer.Event(ctx, chat.ObsAdapterCall, chat.AdapterAttr(adapterName))
	resp, err := c.fileHTTPClient(c.token).Do(req)
	if err != nil {
		return 0, fmt.Errorf("slack: %s request: %w", method, err)
	}
	defer resp.Body.Close()

	if !successStatus(resp.StatusCode) {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return 0, &APIError{Method: method, StatusCode: resp.StatusCode, Raw: raw}
	}
	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "text/html" {
		return 0, fmt.Errorf("slack: %s returned an HTML page instead of the file; check the token and the files:read scope", method)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return n, fmt.Errorf("slack: %s copy body: %w", method, err)
	}
	extra, err := io.CopyN(io.Discard, resp.Body, 1)
	if extra > 0 {
		return n, fmt.Errorf("slack: %s: %w of %d bytes", method, ErrFileTooLarge, maxBytes)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("slack: %s copy body: %w", method, err)
	}
	return n, nil
}

// checkFileURL returns an error when rawURL is empty or its origin is not in
// the file origins of c.
func (c *Client) checkFileURL(method, rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("slack: %s: file URL is empty", method)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("slack: %s: parse URL: %w", method, err)
	}
	if !c.fileOriginAllowed(u) {
		return fmt.Errorf("slack: %s: origin %q is not in FileOrigins", method, originOf(u))
	}
	return nil
}

func (c *Client) fileOriginAllowed(u *url.URL) bool {
	return u.Scheme != "" && u.Host != "" && slices.Contains(c.fileOrigins, originOf(u))
}

// fileHTTPClient returns a copy of the HTTP client of c with the redirect
// policy of fileRedirectPolicy for token.
func (c *Client) fileHTTPClient(token string) *http.Client {
	client := *c.httpClient
	client.CheckRedirect = c.fileRedirectPolicy(token, client.CheckRedirect)
	return &client
}

// fileRedirectPolicy returns a CheckRedirect function that rejects a redirect
// to an origin that is not in the file origins of c or a redirect that changes
// the method of the request. It sets token as the bearer token of the redirect
// when token is not empty, then applies next, or the default limit of 10
// redirects when next is nil. http.Client removes the Authorization header on
// a redirect to another host, so the token must be set again for an allowed
// origin such as https://slack.com after https://files.slack.com.
func (c *Client) fileRedirectPolicy(token string, next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !c.fileOriginAllowed(req.URL) {
			return fmt.Errorf("redirect origin %q is not in FileOrigins", originOf(req.URL))
		}
		if req.Method != via[0].Method {
			return fmt.Errorf("redirect changes the method from %s to %s", via[0].Method, req.Method)
		}
		setBearer(req, token)
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// setBearer sets token as the bearer token of req when token is not empty.
func setBearer(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
