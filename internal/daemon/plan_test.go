package daemon

import (
	"strings"
	"testing"
)

const reply = `{"memories":[` +
	`{"title":"New fact","content":"c1","type":"fact"},` +
	`{"title":"Replaces old","content":"c2","type":"decision","op":"update","target_id":"sim1"},` +
	`{"title":"Already known","content":"c3","type":"fact","op":"noop","target_id":"sim2"},` +
	`{"title":"Bogus update","content":"c4","type":"rule","op":"update","target_id":"invented"}],` +
	`"feedback":[{"id":"shown1","verdict":"used"},{"id":"sim1","verdict":"wrong"},{"id":"invented","verdict":"used"}]}`

func TestPlanExtractionInteractive(t *testing.T) {
	mems, verdicts, err := planExtraction(reply, false, []string{"sim1", "sim2"}, []string{"shown1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range mems {
		got = append(got, m.Title+"="+m.Kind+":"+m.Op+":"+m.TargetID)
	}
	want := "New fact=fact:add:,Replaces old=decision:update:sim1,Already known=fact:noop:sim2,Bogus update=rule:add:"
	if strings.Join(got, ",") != want {
		t.Fatalf("want %s\n got %s", want, strings.Join(got, ","))
	}
	// Only memories the assistant was shown may be graded.
	if len(verdicts) != 1 || verdicts[0].ID != "shown1" || verdicts[0].Verdict != "used" {
		t.Fatalf("verdicts must be whitelisted to shown ids: %+v", verdicts)
	}
}

// Agent transcripts keep today's semantics: add-only, rules and preferences
// demoted to facts, no feedback — a bot's text never retires or grades the
// user's memories.
func TestPlanExtractionForeignIsAddOnly(t *testing.T) {
	mems, verdicts, err := planExtraction(reply, true, []string{"sim1", "sim2"}, []string{"shown1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 0 {
		t.Fatalf("no feedback from agent transcripts: %+v", verdicts)
	}
	for _, m := range mems {
		if m.Op != "add" || m.TargetID != "" || m.Kind == "rule" || m.Kind == "preference" {
			t.Fatalf("agent memory must be a plain demoted add: %+v", m)
		}
	}
	if len(mems) != 3 {
		t.Fatalf("the noop (a claim about the user's memory) is dropped, the rest added: got %d", len(mems))
	}
}
