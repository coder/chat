module github.com/coder/chat/examples/slack-redis-state

go 1.26.3

require (
	github.com/coder/chat v0.1.0
	github.com/coder/chat/state/redis v0.1.0
	github.com/redis/go-redis/v9 v9.19.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
)

// Examples are clone-and-run documentation: build against the local tree so
// they never depend on sibling tags that are published after the code lands.
replace github.com/coder/chat => ../..

replace github.com/coder/chat/state/redis => ../../state/redis
