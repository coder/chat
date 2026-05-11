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
	Message       *Message
	Retry         RetryMetadata
	Raw           any
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
