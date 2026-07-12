package bgjob

import (
	"strings"
	"testing"
)

func TestScriptEmbedded(t *testing.T) {
	if !strings.Contains(Script, "spawner-job") {
		t.Fatal("embedded script missing")
	}
	// The detachment guarantees the whole design rests on: nested setsid (new
	// session/pgid) and stdin/stdout/stderr fully redirected off the turn channel.
	for _, want := range []string{"setsid nohup sh -c", "</dev/null", `>"$log" 2>&1 &`} {
		if !strings.Contains(Script, want) {
			t.Errorf("script missing detachment fragment %q", want)
		}
	}
}

func TestParseList(t *testing.T) {
	out := []byte(`[{"id":"a_1_ff","pid":42,"cmd":"sleep 1","started":100,"done":true,"exit":0},` +
		`{"id":"b_2_ee","pid":43,"cmd":"echo hi","started":101,"done":false,"exit":0}]`)
	recs, err := ParseList(out)
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].ID != "a_1_ff" || recs[0].PID != 42 || !recs[0].Done {
		t.Errorf("record 0 wrong: %+v", recs[0])
	}
	if recs[1].Done {
		t.Errorf("record 1 should be running: %+v", recs[1])
	}
}

func TestParseListEmpty(t *testing.T) {
	recs, err := ParseList([]byte(`[]`))
	if err != nil {
		t.Fatalf("ParseList empty: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("want 0, got %d", len(recs))
	}
}
