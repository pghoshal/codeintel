// CodeGraph format helpers. Direct port of
// packages/web/src/features/repos/codeIntelStatus.ts:
//   - CodeGraphRevision input         (lines 38-42)
//   - CodeGraphIndex input             (lines 44-73)
//   - FormatCodeGraphIndex             (lines 143-183)
//   - SelectCurrentCodeGraphIndex      (lines 75-77)
//   - SortCodeGraphIndexesForStatus    (lines 79-81)
//   - compareCodeGraphIndexForCurrent  (lines 185-190)
//   - isCurrentGraphSnapshot           (lines 192-193)
//   - statusRank                       (lines 195-211)
package repoindexstatus

import (
	"sort"
	"time"
)

// CodeGraphRevisionInput mirrors codeIntelStatus.ts:38-42.
type CodeGraphRevisionInput struct {
	Revision    string
	CommitHash  string
	ActivatedAt *time.Time
}

// CodeGraphIndexInput mirrors codeIntelStatus.ts:44-73.
// Optional `*Count` fields take a pointer so the legacy
// `?? _count?.<x>` fallback chain can distinguish "explicitly
// 0" from "not present". The Counts.SemanticFacts /
// SemanticEdges / SemanticHyperedges nested struct mirrors the
// legacy `_count` Prisma aggregation.
type CodeGraphIndexInput struct {
	ID                        string
	Provider                  string
	Status                    string
	SourceRevision            *string
	CommitHash                string
	GraphSpace                *string
	WorkspaceID               string
	SchemaVersion             int32
	BuilderVersion            string
	VertexCount               int32
	EdgeCount                 int32
	AnchorCount               int32
	LinkedEdgeCount           int32
	SemanticFactCount         *int32
	SemanticEdgeCount         *int32
	SemanticHyperedgeCount    *int32
	SemanticRejectedFactCount *int32
	SemanticBatchCount        *int32
	Counts                    *CodeGraphCounts
	IndexedAt                 *time.Time
	SupersededAt              *time.Time
	DeleteAfter               *time.Time
	ErrorMessage              *string
	Revisions                 []CodeGraphRevisionInput
}

// CodeGraphCounts wraps the legacy `_count` Prisma aggregate.
type CodeGraphCounts struct {
	SemanticFacts      *int32
	SemanticEdges      *int32
	SemanticHyperedges *int32
}

// CodeGraphRevisionOutput is the per-revision projection. Three
// fields; matches codeIntelStatus.ts:149-153.
type CodeGraphRevisionOutput struct {
	Revision    string     `json:"revision"`
	CommitHash  string     `json:"commitHash"`
	ActivatedAt *time.Time `json:"activatedAt"`
}

// CodeGraphIndexOutput is the legacy `codeIntel.codeGraph[*]`
// projection (codeIntelStatus.ts:156-182). Field order matches
// the legacy property order in the return literal so a byte-
// equal comparison against legacy survives.
type CodeGraphIndexOutput struct {
	ID                        string                    `json:"id"`
	Provider                  string                    `json:"provider"`
	IsActive                  bool                      `json:"isActive"`
	Status                    string                    `json:"status"`
	SourceRevision            *string                   `json:"sourceRevision"`
	CommitHash                string                    `json:"commitHash"`
	GraphSpace                *string                   `json:"graphSpace"`
	WorkspaceID               string                    `json:"workspaceId"`
	SchemaVersion             int32                     `json:"schemaVersion"`
	BuilderVersion            string                    `json:"builderVersion"`
	VertexCount               int32                     `json:"vertexCount"`
	EdgeCount                 int32                     `json:"edgeCount"`
	AnchorCount               int32                     `json:"anchorCount"`
	LinkedEdgeCount           int32                     `json:"linkedEdgeCount"`
	SemanticFactCount         int32                     `json:"semanticFactCount"`
	SemanticEdgeCount         int32                     `json:"semanticEdgeCount"`
	SemanticHyperedgeCount    int32                     `json:"semanticHyperedgeCount"`
	SemanticRejectedFactCount int32                     `json:"semanticRejectedFactCount"`
	SemanticBatchCount        int32                     `json:"semanticBatchCount"`
	ActiveRevisionCount       int                       `json:"activeRevisionCount"`
	ActiveRevisions           []CodeGraphRevisionOutput `json:"activeRevisions"`
	IndexedAt                 *time.Time                `json:"indexedAt"`
	SupersededAt              *time.Time                `json:"supersededAt"`
	DeleteAfter               *time.Time                `json:"deleteAfter"`
	ErrorMessage              *string                   `json:"errorMessage"`
}

// FormatCodeGraphIndex mirrors codeIntelStatus.ts:143-183.
// Returns nil for nil input.
func FormatCodeGraphIndex(index *CodeGraphIndexInput) *CodeGraphIndexOutput {
	if index == nil {
		return nil
	}

	revisions := make([]CodeGraphRevisionOutput, 0, len(index.Revisions))
	for _, r := range index.Revisions {
		revisions = append(revisions, CodeGraphRevisionOutput{
			Revision:    r.Revision,
			CommitHash:  r.CommitHash,
			ActivatedAt: r.ActivatedAt,
		})
	}
	sort.SliceStable(revisions, func(i, j int) bool {
		return revisions[i].Revision < revisions[j].Revision
	})

	// Each semantic-count field falls through:
	//   explicit Count -> _count fallback -> 0
	// Mirrors `index.semanticFactCount ?? index._count?.semanticFacts ?? 0`.
	pickCount := func(direct *int32, agg *int32) int32 {
		if direct != nil {
			return *direct
		}
		if agg != nil {
			return *agg
		}
		return 0
	}
	var aggFacts, aggEdges, aggHyper *int32
	if index.Counts != nil {
		aggFacts = index.Counts.SemanticFacts
		aggEdges = index.Counts.SemanticEdges
		aggHyper = index.Counts.SemanticHyperedges
	}

	return &CodeGraphIndexOutput{
		ID:                        index.ID,
		Provider:                  index.Provider,
		IsActive:                  len(revisions) > 0,
		Status:                    index.Status,
		SourceRevision:            index.SourceRevision,
		CommitHash:                index.CommitHash,
		GraphSpace:                index.GraphSpace,
		WorkspaceID:               index.WorkspaceID,
		SchemaVersion:             index.SchemaVersion,
		BuilderVersion:            index.BuilderVersion,
		VertexCount:               index.VertexCount,
		EdgeCount:                 index.EdgeCount,
		AnchorCount:               index.AnchorCount,
		LinkedEdgeCount:           index.LinkedEdgeCount,
		SemanticFactCount:         pickCount(index.SemanticFactCount, aggFacts),
		SemanticEdgeCount:         pickCount(index.SemanticEdgeCount, aggEdges),
		SemanticHyperedgeCount:    pickCount(index.SemanticHyperedgeCount, aggHyper),
		SemanticRejectedFactCount: pickCount(index.SemanticRejectedFactCount, nil),
		SemanticBatchCount:        pickCount(index.SemanticBatchCount, nil),
		ActiveRevisionCount:       len(revisions),
		ActiveRevisions:           revisions,
		IndexedAt:                 index.IndexedAt,
		SupersededAt:              index.SupersededAt,
		DeleteAfter:               index.DeleteAfter,
		ErrorMessage:              index.ErrorMessage,
	}
}

// SortCodeGraphIndexesForStatus returns a NEW slice sorted by
// the legacy comparator chain. Same order as the legacy spread
// + sort idiom (`[...indexes].sort(cmp)`).
func SortCodeGraphIndexesForStatus(indexes []CodeGraphIndexInput) []CodeGraphIndexInput {
	out := make([]CodeGraphIndexInput, len(indexes))
	copy(out, indexes)
	sort.SliceStable(out, func(i, j int) bool {
		return compareCodeGraphIndexForCurrent(out[i], out[j]) < 0
	})
	return out
}

// SelectCurrentCodeGraphIndex returns the first index after
// sorting, or nil for an empty input. Mirrors
// codeIntelStatus.ts:75-77.
func SelectCurrentCodeGraphIndex(indexes []CodeGraphIndexInput) *CodeGraphIndexInput {
	if len(indexes) == 0 {
		return nil
	}
	sorted := SortCodeGraphIndexesForStatus(indexes)
	return &sorted[0]
}

// compareCodeGraphIndexForCurrent mirrors
// codeIntelStatus.ts:185-190. Returns negative if a should sort
// before b, positive if after, zero if equal. The legacy
// comparator returns the chain via `||`:
//
//	1. isCurrentGraphSnapshot DESC (READY & has revisions first)
//	2. statusRank ASC
//	3. !supersededAt DESC (non-superseded before superseded)
//	4. indexedAt DESC (newer first)
func compareCodeGraphIndexForCurrent(a, b CodeGraphIndexInput) int {
	// Each step preserves the legacy `Number(b) - Number(a)`
	// arithmetic verbatim so the sort order matches byte-for-
	// byte.
	//
	// 1. Current-snapshot first.
	if v := boolToInt(isCurrentGraphSnapshot(b)) - boolToInt(isCurrentGraphSnapshot(a)); v != 0 {
		return v
	}
	// 2. statusRank ASC.
	if v := statusRank(a.Status) - statusRank(b.Status); v != 0 {
		return v
	}
	// 3. Non-superseded (b.supersededAt == nil) before superseded.
	if v := boolToInt(b.SupersededAt == nil) - boolToInt(a.SupersededAt == nil); v != 0 {
		return v
	}
	// 4. indexedAt DESC (newer first): Number(b.indexedAt) - Number(a.indexedAt).
	bms := dateMs(b.IndexedAt)
	ams := dateMs(a.IndexedAt)
	if bms != ams {
		if bms > ams {
			return 1
		}
		return -1
	}
	return 0
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func dateMs(d *time.Time) int64 {
	if d == nil {
		return 0
	}
	return d.UnixMilli()
}

func isCurrentGraphSnapshot(idx CodeGraphIndexInput) bool {
	return idx.Status == "READY" && len(idx.Revisions) > 0
}

// statusRank mirrors codeIntelStatus.ts:195-211. The legacy
// switch has implicit fall-through for the no-match case; the
// Go port mirrors that by using a default arm that maps unknown
// statuses to the worst rank (so they sort to the end).
func statusRank(status string) int {
	switch status {
	case "READY":
		return 0
	case "PARTIAL":
		return 1
	case "BUILDING", "PENDING":
		return 2
	case "FAILED":
		return 3
	case "SKIPPED":
		return 4
	case "DELETING":
		return 5
	}
	return 99
}
