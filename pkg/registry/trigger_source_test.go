package registry

import (
	"encoding/json"
	"testing"
)

func TestTriggerSource_StringValues(t *testing.T) {
	cases := []struct {
		ts   TriggerSource
		want string
	}{
		{TriggerWebhook, "webhook"},
		{TriggerCron, "cron"},
		{TriggerManual, "manual"},
		{TriggerChain, "chain"},
		{TriggerDaemon, "daemon"},
		{TriggerReplay, "replay"},
	}
	for _, c := range cases {
		if string(c.ts) != c.want {
			t.Errorf("TriggerSource %q != %q", c.ts, c.want)
		}
	}
}

func TestTriggerSource_JSONRoundtrip(t *testing.T) {
	type wrapper struct {
		TS TriggerSource `json:"trigger_source"`
	}
	w := wrapper{TS: TriggerReplay}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"trigger_source":"replay"}` {
		t.Errorf("got %s", b)
	}
	var back wrapper
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.TS != TriggerReplay {
		t.Errorf("roundtrip: %q", back.TS)
	}
}
