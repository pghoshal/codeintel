package audit

import (
	"context"
	"testing"
)

// TestNoopEmitter_Emit returns nil and does nothing observable.
// The handler call site treats a nil error as "audit logged".
func TestNoopEmitter_Emit(t *testing.T) {
	e := NoopEmitter{}
	if err := e.Emit(context.Background(), Event{Action: "test"}); err != nil {
		t.Fatalf("NoopEmitter.Emit returned %v, want nil", err)
	}
}

// TestActorTypeConstantsAreStable pins the wire-format values
// downstream emitters use to discriminate actors.
func TestActorTypeConstantsAreStable(t *testing.T) {
	cases := map[ActorType]string{
		ActorUser:   "user",
		ActorApiKey: "api_key",
		ActorSystem: "system",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("ActorType drift: %q != %q", string(got), want)
		}
	}
}

// TestTargetTypeConstantsAreStable pins the wire-format values
// downstream emitters use to discriminate targets.
func TestTargetTypeConstantsAreStable(t *testing.T) {
	cases := map[TargetType]string{
		TargetOrg:        "org",
		TargetConnection: "connection",
		TargetSecret:     "secret",
		TargetModel:      "model",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("TargetType drift: %q != %q", string(got), want)
		}
	}
}
