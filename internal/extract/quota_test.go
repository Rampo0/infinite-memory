package extract

import (
	"strings"
	"testing"
)

// The subscription windows come from the rate_limit_event that
// `claude -p --output-format stream-json --verbose` emits — the same signal
// the ai-review and on-call quota guards pause on.
func TestParseQuota(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1791306000,"unifiedWindows":{"five_hour":{"utilization":0.82,"resetsAt":1791306000},"seven_day":{"utilization":0.65,"resetsAt":1791446400}}}}`,
		`not json`,
		`{"type":"result","result":"ok"}`,
	}, "\n")
	q, err := ParseQuota([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if q.FiveHour != 0.82 || q.FiveHourResets != 1791306000 || q.SevenDay != 0.65 {
		t.Fatalf("got %+v", q)
	}
}

func TestParseQuotaMissing(t *testing.T) {
	if _, err := ParseQuota([]byte(`{"type":"result","result":"ok"}`)); err == nil {
		t.Fatal("no rate_limit_event must be an error, not a silent zero")
	}
}
