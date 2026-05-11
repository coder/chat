package chat

import (
	"context"
	"errors"
)

type Thread struct {
	runtime *Chat
	adapter Adapter
	ref     ThreadRef
}

func (c *Chat) newThread(adapter Adapter, ref ThreadRef) *Thread {
	assert(adapter != nil, "newThread requires adapter")
	assert(ref.ID != "", "newThread requires thread id")
	return &Thread{runtime: c, adapter: adapter, ref: ref}
}

func (t *Thread) ID() ThreadID {
	assert(t != nil, "ID called on nil thread")
	return t.ref.ID
}

func (t *Thread) Subscribe(ctx context.Context) error {
	assert(t != nil, "Subscribe called on nil thread")
	return t.runtime.state.SubscribeThread(ctx, t.ref.ID)
}

func (t *Thread) Unsubscribe(ctx context.Context) error {
	assert(t != nil, "Unsubscribe called on nil thread")
	return t.runtime.state.UnsubscribeThread(ctx, t.ref.ID)
}

func (t *Thread) Post(ctx context.Context, msg PostableMessage) (*SentMessage, error) {
	assert(t != nil, "Post called on nil thread")
	if msg.Text == "" {
		return nil, errors.New("chat: post message text is required")
	}
	return t.adapter.PostMessage(ctx, t.ref, msg)
}

func (t *Thread) PostEphemeral(ctx context.Context, actor Actor, msg PostableMessage, opts EphemeralOptions) (*SentMessage, error) {
	assert(t != nil, "PostEphemeral called on nil thread")
	if actor.ID == "" {
		return nil, errors.New("chat: ephemeral actor id is required")
	}
	if msg.Text == "" {
		return nil, errors.New("chat: ephemeral message text is required")
	}
	poster, ok := t.adapter.(EphemeralPoster)
	if !ok {
		return nil, ErrUnsupportedCapability
	}
	return poster.PostEphemeralMessage(ctx, t.ref, actor, msg, opts)
}
