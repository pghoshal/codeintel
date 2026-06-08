package repoindex

import (
	"strings"
	"testing"
)

func TestJobType_Valid(t *testing.T) {
	cases := []struct {
		t    JobType
		want bool
	}{
		{JobTypeIndex, true},
		{JobTypeCleanup, true},
		{JobTypeRemoveIndex, true},
		{"", false},
		{"FOO", false},
		{"index", false}, // case-sensitive: legacy enum is upper
	}
	for _, c := range cases {
		t.Run(string(c.t), func(t *testing.T) {
			if c.t.Valid() != c.want {
				t.Errorf("Valid(%q): got %v, want %v", c.t, c.t.Valid(), c.want)
			}
		})
	}
}

func TestMarshalUnmarshal_RoundTrip(t *testing.T) {
	in := TaskPayload{
		Type:     JobTypeIndex,
		JobID:    "11112222-3333-4444-5555-666677778888",
		RepoID:   42,
		OrgID:    7,
		RepoName: "github.com/owner/repo",
		Ref:      "refs/heads/release",
	}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Field names byte-equal legacy.
	for _, want := range []string{`"type":"INDEX"`, `"jobId":"`, `"repoId":42`, `"orgId":7`, `"repoName":"`, `"ref":"refs/heads/release"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in %s", want, b)
		}
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != in {
		t.Errorf("round-trip:\n got=%+v\nwant=%+v", got, in)
	}
}

func TestMarshal_RejectsBadType(t *testing.T) {
	_, err := Marshal(TaskPayload{Type: "WHATEVER", JobID: "j", RepoID: 1, OrgID: 1})
	if err == nil {
		t.Fatalf("expected error for bad type")
	}
}

func TestMarshal_RejectsMissingOrgID(t *testing.T) {
	_, err := Marshal(TaskPayload{Type: JobTypeIndex, JobID: "j", RepoID: 1})
	if err == nil {
		t.Fatalf("expected error for missing orgId")
	}
}

func TestUnmarshal_RejectsBadType(t *testing.T) {
	_, err := Unmarshal([]byte(`{"type":"WHATEVER","jobId":"j","repoId":1,"orgId":1}`))
	if err == nil {
		t.Fatalf("expected error for bad type")
	}
}

func TestUnmarshal_RejectsMissingOrgID(t *testing.T) {
	_, err := Unmarshal([]byte(`{"type":"INDEX","jobId":"j","repoId":1}`))
	if err == nil {
		t.Fatalf("expected error for missing orgId")
	}
}

func TestUnmarshalLegacyForBackfill_AllowsMissingOrgID(t *testing.T) {
	got, err := UnmarshalLegacyForBackfill([]byte(`{"type":"INDEX","jobId":"j","repoId":1}`))
	if err != nil {
		t.Fatalf("UnmarshalLegacyForBackfill: %v", err)
	}
	if got.OrgID != 0 || got.RepoID != 1 || got.Type != JobTypeIndex {
		t.Fatalf("legacy payload mismatch: %+v", got)
	}
}

func TestUnmarshal_RejectsMalformed(t *testing.T) {
	_, err := Unmarshal([]byte(`not json`))
	if err == nil {
		t.Fatalf("expected error for malformed input")
	}
}
