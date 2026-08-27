package chat_test

import (
	"os"
	"regexp"
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
		"Message history is application-owned",
		"Thread Application State",
		"HistoryReader",
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
		"OnCommand installs or atomically replaces the single command handler",
		"OnInteraction installs or atomically replaces the single interaction handler",
	} {
		if !strings.Contains(sourceText, phrase) {
			t.Fatalf("runtime GoDoc does not mention %q", phrase)
		}
	}
}

func TestAdapterDocumentationCoversMultiTenantInstall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path    string
		phrases []string
	}{
		{
			path: "adapters/slack/doc.go",
			phrases: []string{
				"Multi-tenant is opt-in",
				"account linking",
				"stay app-owned",
				"InstallStore is application-implemented",
				"slack.SlackInstall",
			},
		},
		{
			path: "adapters/linear/doc.go",
			phrases: []string{
				"Multi-tenant is opt-in",
				"account linking",
				"stay app-owned",
				"InstallStore is application-implemented",
				"linear.LinearInstall",
			},
		},
	}
	for _, tc := range cases {
		source, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		text := string(source)
		for _, phrase := range tc.phrases {
			if !strings.Contains(text, phrase) {
				t.Fatalf("%s does not mention %q", tc.path, phrase)
			}
		}
	}
}

func TestDocumentationCoversMessageHistoryCapability(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path    string
		phrases []string
	}{
		{
			path: "README.md",
			phrases: []string{
				"HistoryReader",
				"Optional Capability",
				"Thread Application State",
				"no runtime storage",
			},
		},
		{
			path: "adapters/slack/doc.go",
			phrases: []string{
				"HistoryReader Optional Capability",
				"storage-free",
				"Thread Application State",
			},
		},
		{
			path: "adapters/linear/doc.go",
			phrases: []string{
				"HistoryReader Optional Capability",
				"storage-free",
				"Thread Application State",
			},
		},
		{
			path: "types.go",
			phrases: []string{
				"HistoryReader is an Optional Capability",
				"It performs",
				"NO runtime storage",
				"Thread Application State",
			},
		},
	}
	for _, tc := range cases {
		source, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		text := string(source)
		for _, phrase := range tc.phrases {
			if !strings.Contains(text, phrase) {
				t.Fatalf("%s does not mention %q", tc.path, phrase)
			}
		}
	}
}

// TestLinearHowToSnippetsAreExtractedFromBuildableSource keeps the worked
// examples in the Linear how-to honest: every fenced Go block annotated with a
// `<!-- source: path -->` marker must appear verbatim (modulo whitespace) in
// the referenced buildable, tested source file.
func TestLinearHowToSnippetsAreExtractedFromBuildableSource(t *testing.T) {
	t.Parallel()

	doc, err := os.ReadFile("docs/how-to/linear-agent-sessions.md")
	if err != nil {
		t.Fatalf("read how-to: %v", err)
	}
	pattern := regexp.MustCompile("(?s)<!-- source: ([^>]+?) -->\\s*```go\\n(.*?)```")
	matches := pattern.FindAllStringSubmatch(string(doc), -1)
	if len(matches) < 8 {
		t.Fatalf("marked snippets = %d, want at least 8", len(matches))
	}
	normalizedSources := map[string]string{}
	for _, match := range matches {
		path := strings.TrimSpace(match[1])
		snippet := match[2]
		normalizedSource, ok := normalizedSources[path]
		if !ok {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read snippet source %s: %v", path, err)
			}
			normalizedSource = strings.Join(strings.Fields(string(source)), " ")
			normalizedSources[path] = normalizedSource
		}
		normalizedSnippet := strings.Join(strings.Fields(snippet), " ")
		if normalizedSnippet == "" {
			t.Fatalf("empty marked snippet for %s", path)
		}
		if !strings.Contains(normalizedSource, normalizedSnippet) {
			t.Fatalf("doc snippet drifted from %s:\n%s", path, snippet)
		}
	}
}
