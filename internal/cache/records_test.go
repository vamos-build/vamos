package cache

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// TestResultTrace checks that a result's trace and the timing of the run that made it round-trip,
// that a result written before Vamos kept them still decodes without them, and that a negative duration is refused.
func TestResultTrace(t *testing.T) {
	trace := &Blob{Digest: digest.FromString("trace"), Size: 5}
	started := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r := &ActionResult{Epoch: 1, Trace: trace, Provenance: &Provenance{Started: started, Took: 2 * time.Second}}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got ActionResult
	if err := json.Unmarshal(data, &got); err != nil || got.Validate() != nil {
		t.Fatalf("decoding %s: %v, %v", data, err, got.Validate())
	}
	if *got.Trace != *trace || !got.Provenance.Started.Equal(started) || got.Provenance.Took != 2*time.Second {
		t.Errorf("decoded %+v with %+v; want trace %v, started %v, took 2s", got, got.Provenance, trace, started)
	}

	var old ActionResult
	if err := json.Unmarshal([]byte(`{"epoch":1,"provenance":{"platform":"linux/amd64"}}`), &old); err != nil ||
		old.Validate() != nil {
		t.Fatalf("a result without a trace or timing: %v, %v", err, old.Validate())
	}
	if old.Trace != nil || !old.Provenance.Started.IsZero() || old.Provenance.Took != 0 {
		t.Errorf("a result without a trace or timing decoded as %+v with %+v", old, old.Provenance)
	}
	if data, _ := json.Marshal(&old); string(data) != `{"epoch":1,"provenance":{"platform":"linux/amd64"}}` {
		t.Errorf("it encodes again as %s, with fields it lacked", data)
	}

	for name, bad := range map[string]*ActionResult{
		"negative took":   {Provenance: &Provenance{Took: -time.Second}},
		"malformed trace": {Trace: &Blob{Digest: "sha256:nope"}},
	} {
		if bad.Validate() == nil {
			t.Errorf("a result with a %s is valid", name)
		}
	}
}
