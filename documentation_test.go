package chat_test

import (
	"os"
	"strings"
	"testing"
)

func TestDocumentationCoversIntentionalVercelDifferences(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readmeText := string(readme)
	for _, phrase := range []string{
		"not a TypeScript API port",
		"Handlers are single-slot per hook",
		"no dedicated `OnDirectMessage` hook",
		"no public proactive `OpenDM`",
		"no thread application state APIs",
		"no full Vercel Chat SDK feature parity",
	} {
		if !strings.Contains(readmeText, phrase) {
			t.Fatalf("README.md does not mention %q", phrase)
		}
	}

	runtimeSource, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	sourceText := string(runtimeSource)
	for _, phrase := range []string{
		"OnNewMention installs or atomically replaces the single new-mention handler",
		"intentionally differs from Vercel Chat SDK",
		"OnSubscribedMessage installs or atomically replaces the single subscribed-message handler",
	} {
		if !strings.Contains(sourceText, phrase) {
			t.Fatalf("runtime GoDoc does not mention %q", phrase)
		}
	}
}
