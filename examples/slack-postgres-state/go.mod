module github.com/coder/chat/examples/slack-postgres-state

go 1.26.3

require (
	github.com/coder/chat v0.1.0
	github.com/coder/chat/state/postgres v0.1.0
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.34.0 // indirect
)

// Examples are clone-and-run documentation: build against the local tree so
// they never depend on sibling tags that are published after the code lands.
replace github.com/coder/chat => ../..

replace github.com/coder/chat/state/postgres => ../../state/postgres
