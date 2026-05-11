package chat

import "errors"

var (
	ErrUnsupportedCapability = errors.New("chat: unsupported adapter capability")
)

func assert(ok bool, message string) {
	if !ok {
		panic("chat: assertion failed: " + message)
	}
}
