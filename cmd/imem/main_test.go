package main

import (
	"strings"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/client"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

// savedMessage is the only place the save side turns into user-facing text, so
// each branch gets a case — silence included, since a wrongly silent hook is
// the exact failure this feature exists to prevent.
func TestSavedMessageBranches(t *testing.T) {
	cases := []struct {
		name string
		in   client.SavedPayload
		want string
	}{
		{"nothing at all", client.SavedPayload{}, ""},
		{"one memory is singular", client.SavedPayload{Count: 1, MS: 8400}, "imem: saved 1 memory (8.4s)"},
		{"several are plural", client.SavedPayload{Count: 5, MS: 32700}, "imem: saved 5 memories (32.7s)"},
		{"still running", client.SavedPayload{Status: "running"}, "imem: extracting… (report at next prompt)"},
		{"ran but found nothing", client.SavedPayload{Status: "skipped"}, "imem: saved nothing — no new facts this turn"},
		{"queued", client.SavedPayload{DueInS: 45}, "imem: extracting in 45s"},
		{"failure", client.SavedPayload{Error: "claude: binary not found"}, "imem: save failed — claude: binary not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := savedMessage(c.in); got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

// An error outranks a count: a partially failed run must not read as a success.
func TestSavedMessageErrorWins(t *testing.T) {
	got := savedMessage(client.SavedPayload{Count: 3, MS: 100, Error: "save: memgraph down"})
	if !strings.HasPrefix(got, "imem: save failed") {
		t.Fatalf("error must win over the count, got %q", got)
	}
}

func TestSavedMessageAppendsSummary(t *testing.T) {
	got := savedMessage(client.SavedPayload{
		Count: 1, MS: 1000, Summary: "  [fact]      Something        new",
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + summary, got %q", got)
	}
	if !strings.HasPrefix(lines[1], "  [fact]") {
		t.Fatalf("summary should follow verbatim, got %q", lines[1])
	}
}

func TestPickPinTarget(t *testing.T) {
	ts := []graph.PinTarget{{ID: "a1", Title: "Code constitution"}, {ID: "b2", Title: "Code constitution limits"}}
	if got, ok := pickPinTarget(ts, "b2"); !ok || got.ID != "b2" {
		t.Fatalf("an exact id wins over ambiguity, got %+v %v", got, ok)
	}
	if _, ok := pickPinTarget(ts, "constitution"); ok {
		t.Fatal("an ambiguous title must not pick one")
	}
	if got, ok := pickPinTarget(ts[:1], "constitution"); !ok || got.ID != "a1" {
		t.Fatalf("a single match is picked, got %+v %v", got, ok)
	}
}

func TestStandingMessageNamesTheRulesFile(t *testing.T) {
	got := standingMessage(client.SessionStartResponse{RulesFile: 285, PreferencesFile: 116, Pinned: 3}, 39)
	if got != "imem: 285 rules + 116 preferences on demand (imem rules --here), 3 pinned inline (39ms)" {
		t.Fatalf("got %q", got)
	}
	if got := standingMessage(client.SessionStartResponse{Rules: 3, Preferences: 1}, 5); got != "imem: 3 standing rules, 1 preferences (5ms)" {
		t.Fatalf("got %q", got)
	}
}

func TestSavedMessageNamesTheDeferral(t *testing.T) {
	got := savedMessage(client.SavedPayload{Status: "deferred", Note: "deferred: 5-hour usage 90% is over extract_max_usage 85%"})
	if got != "imem: save deferred: 5-hour usage 90% is over extract_max_usage 85%" {
		t.Fatalf("got %q", got)
	}
}
