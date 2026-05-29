package chat

import (
	"context"
	"net/http"
)

type BotKind int

const (
	BotUnknown BotKind = iota
	BotHuman
	BotBot
)

type Actor struct {
	Adapter string
	Tenant  string
	ID      string
	Name    string
	BotKind BotKind
}

type ThreadID string

type ThreadRef struct {
	ID      ThreadID
	Adapter string
	Tenant  string
	Channel string
	Root    string
	Direct  bool
	Raw     any
}

type RetryMetadata struct {
	Num    string
	Reason string
}

type Event struct {
	ID            string
	Adapter       string
	Tenant        string
	ThreadID      ThreadID
	DirectMessage bool
	// At most one of Message, Command, or Interaction is set: a Command Event and
	// an Interaction Event are Events, never Messages.
	Message     *Message
	Command     *Command
	Interaction *Interaction
	Retry       RetryMetadata
	Raw         any
}

type Message struct {
	ID        string
	Text      string
	Author    Actor
	Mentioned bool
	Raw       any
}

type MessageEvent struct {
	Event   *Event
	Thread  *Thread
	Message *Message
}

type MessageHandler func(context.Context, *MessageEvent) error

// Command is the normalized payload of a Command Event: a deliberate,
// parameterized platform invocation (a Slack slash command, a Teams
// command/invoke). A Command Event is an Event, never a Message.
type Command struct {
	Name  string
	Text  string
	Args  []string // advisory whitespace split of Text, not a parser
	Actor Actor
	Raw   any // Platform Escape Hatch: response_url, trigger_id, Teams invoke value
}

type CommandEvent struct {
	Event   *Event
	Thread  *Thread
	Command *Command
}

type CommandHandler func(context.Context, *CommandEvent) error

// InteractionKind classifies an Interaction Event. This slice supports
// block_actions (button clicks and menu selections) only.
type InteractionKind int

const (
	InteractionUnknown InteractionKind = iota
	// InteractionBlockAction is a Slack block_actions interaction: a button click
	// or a menu selection.
	InteractionBlockAction
)

// Interaction is the normalized payload of an Interaction Event: a Block Kit /
// card component action. Like a Command Event, an Interaction Event is an Event,
// never a Message.
type Interaction struct {
	Kind     InteractionKind
	ActionID string
	Actor    Actor
	Raw      any // Platform Escape Hatch: response_url, trigger_id, action values, view state
}

type InteractionEvent struct {
	Event       *Event
	Thread      *Thread
	Interaction *Interaction
}

type InteractionHandler func(context.Context, *InteractionEvent) error

type DispatchFunc func(context.Context, *Event) error

type Adapter interface {
	Name() string
	Init(context.Context) error
	Shutdown(context.Context) error
	Webhook(DispatchFunc) http.Handler
	ValidateThreadID(ThreadID) (ThreadRef, error)
	PostMessage(context.Context, ThreadRef, PostableMessage) (*SentMessage, error)
	BotActor() Actor
}

type MessageFormat int

const (
	MessageFormatText MessageFormat = iota
	MessageFormatMarkdown
)

type PostableMessage struct {
	Text   string
	Format MessageFormat
}

func Text(text string) PostableMessage {
	return PostableMessage{Text: text, Format: MessageFormatText}
}

func Markdown(text string) PostableMessage {
	return PostableMessage{Text: text, Format: MessageFormatMarkdown}
}

type SentMessage struct {
	ID       string
	ThreadID ThreadID
	Raw      any
}

type EphemeralOptions struct {
	FallbackToDM bool
}

type EphemeralPoster interface {
	PostEphemeralMessage(context.Context, ThreadRef, Actor, PostableMessage, EphemeralOptions) (*SentMessage, error)
}

// NativeContent carries an explicitly platform-native message body (Slack Block
// Kit blocks, a Teams Adaptive Card) as a Platform Escape Hatch. It is NOT a
// cross-platform card model and does NOT widen Postable Message; Plain Text and
// Portable Markdown remain the portable surface. Native content is reached only
// through the NativeContentPoster Optional Capability via typed Adapter Access.
type NativeContent struct {
	// Adapter must match the target adapter; a mismatch is an error, never a
	// silent portable downgrade.
	Adapter string
	Payload any
}

// NativeContentPoster is the Optional Capability for posting NativeContent.
// Adapters that support native rich content implement it; callers reach it via
// chat.AdapterAs. Absence of the capability is an explicit unsupported result,
// not a panic.
type NativeContentPoster interface {
	PostNative(context.Context, ThreadRef, NativeContent) (*SentMessage, error)
}
