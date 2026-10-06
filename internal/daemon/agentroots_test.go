package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentTranscriptAcceptedOnlyInsideARoot(t *testing.T) {
	root := t.TempDir()
	in := filepath.Join(root, "imem", "a.jsonl")
	out := filepath.Join(t.TempDir(), "b.jsonl")
	write(t, in)
	write(t, out)
	if !validTranscriptPath(in, []string{root}) {
		t.Fatal("transcript inside an agent root rejected")
	}
	if validTranscriptPath(out, []string{root}) {
		t.Fatal("transcript outside every root accepted")
	}
	if validTranscriptPath(in, nil) {
		t.Fatal("agent transcript accepted with no agent_roots configured")
	}
}

func TestSymlinkOutOfAnAgentRootRejected(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	write(t, secret)
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if agentRoot(link, []string{root}) != "" {
		t.Fatal("symlink pointing out of the root accepted")
	}
	if validTranscriptPath(link, []string{root}) {
		t.Fatal("symlink pointing out of the root passed validation")
	}
}

// A root is a directory, not a string prefix: ~/.on-call/imem must not admit
// ~/.on-call/imem-evil.
func TestSiblingPrefixOfAnAgentRootRejected(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "agent")
	write(t, filepath.Join(root, "a.jsonl")) // the root must exist to resolve
	evil := filepath.Join(tmp, "agent-evil", "x.jsonl")
	write(t, evil)
	if validTranscriptPath(evil, []string{root}) {
		t.Fatal("sibling directory sharing the root's prefix accepted")
	}
}

// A root configured through a symlink still matches the files it holds,
// since both sides resolve before comparing.
func TestAgentRootGivenThroughASymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(real, "a.jsonl")
	write(t, in)
	if !validTranscriptPath(in, []string{link}) {
		t.Fatal("transcript inside a symlinked root rejected")
	}
}

// Demotion fails closed: a transcript that matches no agent root (say, one
// swapped for a link out of the root after validation) is still demoted,
// because only ~/.claude/projects earns the right to save rules.
func TestDemotionFailsClosed(t *testing.T) {
	stray := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, stray)
	if agentRoot(stray, []string{t.TempDir()}) != "" {
		t.Fatal("setup: stray transcript should match no root")
	}
	if fromClaudeProjects(stray) {
		t.Fatal("transcript outside ~/.claude/projects escaped demotion")
	}
	if !fromClaudeProjects(filepath.Join(config.ClaudeProjectsDir(), "p", "s.jsonl")) {
		t.Fatal("Claude Code transcript demoted")
	}
}

func TestDemoteAgentKinds(t *testing.T) {
	mems := []graph.MemoryIn{{Kind: "rule"}, {Kind: "preference"}, {Kind: "fact"}, {Kind: "decision"}, {Kind: "reference"}}
	demoteAgentKinds(mems)
	want := []string{"fact", "fact", "fact", "decision", "reference"}
	for i, m := range mems {
		if m.Kind != want[i] {
			t.Fatalf("mems[%d].Kind = %q, want %q", i, m.Kind, want[i])
		}
	}
}

func TestHeadlessSessionsAreForeignEvenUnderClaudeProjects(t *testing.T) {
	home, _ := os.UserHomeDir()
	own := filepath.Join(home, ".claude", "projects", "-Users-x-repo", "s.jsonl")
	cases := []struct {
		job  Job
		want bool
	}{
		{Job{TranscriptPath: own}, false},
		{Job{TranscriptPath: own, Agent: true}, true},
		{Job{TranscriptPath: filepath.Join(home, ".on-call", "imem", "x.jsonl")}, true},
	}
	for _, c := range cases {
		if got := isForeign(c.job); got != c.want {
			t.Fatalf("%+v: foreign=%v, want %v", c.job, got, c.want)
		}
	}
}
