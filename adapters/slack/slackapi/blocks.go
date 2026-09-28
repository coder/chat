package slackapi

import "encoding/json"

// Block is a Slack Block Kit layout block. A value of any type that
// implements Block and encodes itself as a Block Kit JSON object can go in
// a []Block, so callers can add block types that this package does not
// define.
type Block interface {
	// BlockType returns the Block Kit "type" value of the block, such as
	// "markdown".
	BlockType() string
}

// MarkdownBlock is a Block Kit "markdown" block. Slack renders Text as
// standard Markdown. Text can hold at most MarkdownBlockLimit characters;
// use SplitMarkdown to split longer text.
type MarkdownBlock struct {
	// Text is the Markdown text of the block.
	Text string
	// BlockID is the optional block_id. It is omitted when empty.
	BlockID string
}

// BlockType returns "markdown".
func (MarkdownBlock) BlockType() string { return "markdown" }

// MarshalJSON encodes b as a Block Kit markdown block.
func (b MarkdownBlock) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		BlockID string `json:"block_id,omitempty"`
	}{
		Type:    b.BlockType(),
		Text:    b.Text,
		BlockID: b.BlockID,
	})
}

// ActionsBlock is a Block Kit "actions" block that holds buttons.
type ActionsBlock struct {
	// Elements are the buttons of the block. Nil encodes as an empty array.
	Elements []ButtonElement
	// BlockID is the optional block_id. It is omitted when empty.
	BlockID string
}

// BlockType returns "actions".
func (ActionsBlock) BlockType() string { return "actions" }

// MarshalJSON encodes b as a Block Kit actions block.
func (b ActionsBlock) MarshalJSON() ([]byte, error) {
	elements := b.Elements
	if elements == nil {
		elements = []ButtonElement{}
	}
	return json.Marshal(struct {
		Type     string          `json:"type"`
		Elements []ButtonElement `json:"elements"`
		BlockID  string          `json:"block_id,omitempty"`
	}{
		Type:     b.BlockType(),
		Elements: elements,
		BlockID:  b.BlockID,
	})
}

// ButtonElement is a Block Kit "button" element for an ActionsBlock.
type ButtonElement struct {
	// Text is the button label. It is sent as a plain_text object.
	Text string
	// URL is the optional link that the button opens. It is omitted when
	// empty.
	URL string
	// ActionID is the optional action_id. It is omitted when empty.
	ActionID string
	// Style is the optional button style, "primary" or "danger". It is
	// omitted when empty.
	Style string
}

// MarshalJSON encodes e as a Block Kit button element.
func (e ButtonElement) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string        `json:"type"`
		Text     plainTextJSON `json:"text"`
		URL      string        `json:"url,omitempty"`
		ActionID string        `json:"action_id,omitempty"`
		Style    string        `json:"style,omitempty"`
	}{
		Type:     "button",
		Text:     newPlainText(e.Text),
		URL:      e.URL,
		ActionID: e.ActionID,
		Style:    e.Style,
	})
}

// ImageBlock is a Block Kit "image" block.
type ImageBlock struct {
	// ImageURL is the public URL of the image.
	ImageURL string
	// AltText is the plain text description of the image.
	AltText string
	// Title is the optional title. It is sent as a plain_text object and
	// omitted when empty.
	Title string
	// BlockID is the optional block_id. It is omitted when empty.
	BlockID string
}

// BlockType returns "image".
func (ImageBlock) BlockType() string { return "image" }

// MarshalJSON encodes b as a Block Kit image block.
func (b ImageBlock) MarshalJSON() ([]byte, error) {
	var title plainTextJSON
	if b.Title != "" {
		title = newPlainText(b.Title)
	}
	return json.Marshal(struct {
		Type     string        `json:"type"`
		ImageURL string        `json:"image_url"`
		AltText  string        `json:"alt_text"`
		Title    plainTextJSON `json:"title,omitzero"`
		BlockID  string        `json:"block_id,omitempty"`
	}{
		Type:     b.BlockType(),
		ImageURL: b.ImageURL,
		AltText:  b.AltText,
		Title:    title,
		BlockID:  b.BlockID,
	})
}

type plainTextJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func newPlainText(text string) plainTextJSON {
	return plainTextJSON{Type: "plain_text", Text: text}
}
