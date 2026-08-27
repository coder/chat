module github.com/coder/chat/examples/slack-nats-state

go 1.26.3

require (
	github.com/coder/chat v0.1.0
	github.com/coder/chat/state/nats v0.1.0
	github.com/nats-io/nats.go v1.53.1
)

require (
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// Examples are clone-and-run documentation: build against the local tree so
// they never depend on sibling tags that are published after the code lands.
replace github.com/coder/chat => ../..

replace github.com/coder/chat/state/nats => ../../state/nats
