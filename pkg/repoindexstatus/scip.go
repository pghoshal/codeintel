// SCIP code-intel format helper. Direct port of
// packages/web/src/features/repos/codeIntelStatus.ts:
//   - ScipCodeIntelIndex input type      (lines 19-32)
//   - CodeIntelLanguageIndex input type  (lines 4-17)
//   - FormatScipCodeIntelIndex           (lines 83-141)
//
// Output JSON shape matches the legacy projection key-for-key
// so an API client written against the legacy can decode this
// port's response unmodified.
package repoindexstatus

import (
	"sort"
	"time"
)

// CodeIntelLanguageIndexInput mirrors the legacy input shape
// (codeIntelStatus.ts:4-17). All-string fields plus a few
// optional pointer-fields. Sourced from the
// CodeIntelLanguageIndex table.
type CodeIntelLanguageIndexInput struct {
	Language             string  `json:"language"`
	ProjectRoot          string  `json:"projectRoot"`
	Indexer              string  `json:"indexer"`
	WorkerClass          *string `json:"workerClass,omitempty"`
	Status               string  `json:"status"`
	ArtifactPath         *string `json:"artifactPath,omitempty"`
	ToolchainFingerprint *string `json:"toolchainFingerprint,omitempty"`
	ToolchainVersion     *string `json:"toolchainVersion,omitempty"`
	ToolchainPath        *string `json:"toolchainPath,omitempty"`
	ToolchainSha256      *string `json:"toolchainSha256,omitempty"`
	DurationMs           *int64  `json:"durationMs,omitempty"`
	ErrorMessage         *string `json:"errorMessage,omitempty"`
}

// ScipCodeIntelIndexInput mirrors codeIntelStatus.ts:19-32.
type ScipCodeIntelIndexInput struct {
	ID                string
	Kind              string
	Status            string
	Revision          string
	CommitHash        string
	LanguageCount     int32
	SymbolCount       int32
	OccurrenceCount   int32
	RelationshipCount int32
	IndexedAt         *time.Time
	ErrorMessage      *string
	LanguageIndexes   []CodeIntelLanguageIndexInput
}

// FormatScipCodeIntelOptions controls the language-row shape.
// includeArtifactPaths toggles emission of toolchainPath +
// artifactPath columns (legacy codeIntelStatus.ts:34-36).
type FormatScipCodeIntelOptions struct {
	IncludeArtifactPaths bool
}

// LanguageIndexRow is the per-language output shape. The
// optional toolchainPath / artifactPath fields are wire-
// conditional on FormatScipCodeIntelOptions.IncludeArtifactPaths.
// Both use `*string` with omitempty so the legacy
// `...(options.includeArtifactPaths ? { x: ... } : {})` spread
// is mirrored: when the option is FALSE the keys are absent
// entirely (not present-with-null).
type LanguageIndexRow struct {
	Language             string  `json:"language"`
	ProjectRoot          string  `json:"projectRoot"`
	Indexer              string  `json:"indexer"`
	WorkerClass          *string `json:"workerClass"`
	Status               string  `json:"status"`
	ToolchainFingerprint *string `json:"toolchainFingerprint"`
	ToolchainVersion     *string `json:"toolchainVersion"`
	ToolchainPath        *string `json:"toolchainPath,omitempty"`
	ToolchainSha256      *string `json:"toolchainSha256"`
	DurationMs           *int64  `json:"durationMs"`
	ErrorMessage         *string `json:"errorMessage"`
	ArtifactPath         *string `json:"artifactPath,omitempty"`
}

// ScipCodeIntelIndexOutput is the legacy `codeIntel.scip[*]`
// projection (codeIntelStatus.ts:116-140). 14 fields in the
// legacy order — JSON field-ordering matters for byte-equal
// responses, so the field order in this struct mirrors the
// legacy property order.
type ScipCodeIntelIndexOutput struct {
	ID                string             `json:"id"`
	Kind              string             `json:"kind"`
	Status            string             `json:"status"`
	Revision          string             `json:"revision"`
	CommitHash        string             `json:"commitHash"`
	LanguageCount     int32              `json:"languageCount"`
	ProjectCount      int                `json:"projectCount"`
	DetectedLanguages []string           `json:"detectedLanguages"`
	ReadyLanguages    []string           `json:"readyLanguages"`
	SkippedLanguages  []string           `json:"skippedLanguages"`
	FailedLanguages   []string           `json:"failedLanguages"`
	SymbolCount       int32              `json:"symbolCount"`
	OccurrenceCount   int32              `json:"occurrenceCount"`
	RelationshipCount int32              `json:"relationshipCount"`
	IndexedAt         *time.Time         `json:"indexedAt"`
	ErrorMessage      *string            `json:"errorMessage"`
	LanguageIndexes   []LanguageIndexRow `json:"languageIndexes"`
}

// FormatScipCodeIntelIndex is the parity port of
// codeIntelStatus.ts:83-141. Returns nil when index is nil
// (mirrors the legacy `if (!index) return null;`).
func FormatScipCodeIntelIndex(index *ScipCodeIntelIndexInput, opts FormatScipCodeIntelOptions) *ScipCodeIntelIndexOutput {
	if index == nil {
		return nil
	}

	rows := make([]LanguageIndexRow, 0, len(index.LanguageIndexes))
	for _, li := range index.LanguageIndexes {
		row := LanguageIndexRow{
			Language:             li.Language,
			ProjectRoot:          li.ProjectRoot,
			Indexer:              li.Indexer,
			WorkerClass:          li.WorkerClass,
			Status:               li.Status,
			ToolchainFingerprint: li.ToolchainFingerprint,
			ToolchainVersion:     li.ToolchainVersion,
			ToolchainSha256:      li.ToolchainSha256,
			DurationMs:           li.DurationMs,
			ErrorMessage:         li.ErrorMessage,
		}
		if opts.IncludeArtifactPaths {
			row.ToolchainPath = li.ToolchainPath
			row.ArtifactPath = li.ArtifactPath
		}
		rows = append(rows, row)
	}

	// Sort by (projectRoot ASC, language ASC, indexer ASC) —
	// matches codeIntelStatus.ts:110-114 localeCompare chain.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ProjectRoot != rows[j].ProjectRoot {
			return rows[i].ProjectRoot < rows[j].ProjectRoot
		}
		if rows[i].Language != rows[j].Language {
			return rows[i].Language < rows[j].Language
		}
		return rows[i].Indexer < rows[j].Indexer
	})

	// Per-status language buckets. The legacy filters on
	// `status === "READY" / "SKIPPED" / "FAILED"` exactly.
	pickLanguages := func(status string) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			if r.Status == status {
				out = append(out, r.Language)
			}
		}
		return uniqueSorted(out)
	}

	detected := make([]string, 0, len(rows))
	for _, r := range rows {
		detected = append(detected, r.Language)
	}

	return &ScipCodeIntelIndexOutput{
		ID:                index.ID,
		Kind:              index.Kind,
		Status:            index.Status,
		Revision:          index.Revision,
		CommitHash:        index.CommitHash,
		LanguageCount:     index.LanguageCount,
		ProjectCount:      len(rows),
		DetectedLanguages: uniqueSorted(detected),
		ReadyLanguages:    pickLanguages("READY"),
		SkippedLanguages:  pickLanguages("SKIPPED"),
		FailedLanguages:   pickLanguages("FAILED"),
		SymbolCount:       index.SymbolCount,
		OccurrenceCount:   index.OccurrenceCount,
		RelationshipCount: index.RelationshipCount,
		IndexedAt:         index.IndexedAt,
		ErrorMessage:      index.ErrorMessage,
		LanguageIndexes:   rows,
	}
}

// uniqueSorted dedups + sorts a string slice, dropping empty
// entries. Mirrors codeIntelStatus.ts:215.
func uniqueSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
