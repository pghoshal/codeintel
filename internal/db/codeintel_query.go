package db

import (
	"context"
	"fmt"
	"strings"
)

const defaultCodeIntelQueryLimit int32 = 25
const maxCodeIntelQueryLimit int32 = 100

// SymbolOccurrenceMode selects the SCIP occurrence roles exposed by
// FindOrgSymbolOccurrences.
type SymbolOccurrenceMode string

const (
	SymbolOccurrenceDefinitions SymbolOccurrenceMode = "definitions"
	SymbolOccurrenceReferences  SymbolOccurrenceMode = "references"
)

// FindOrgSymbolOccurrencesParams is the tenant-scoped query used by
// development tools that need precise SCIP definitions or references.
//
// RevisionCandidates must be pre-validated by the caller when it comes
// from user input. Public reads use the latest READY or PARTIAL SCIP
// indexes in active repos so polyglot repos can expose precise symbols
// for successful language/project roots while status APIs report the
// missing roots explicitly.
type FindOrgSymbolOccurrencesParams struct {
	OrgID              int32
	Symbol             string
	Repo               string
	Repos              []string
	RevisionCandidates []string
	RepoRevisionScopes []RepoRevisionScope
	DefinitionFile     string
	Mode               SymbolOccurrenceMode
	Limit              int32
}

// RepoRevisionScope pins a repo filter to the revisions proven indexed
// for that repo. Multi-repo MCP calls use this to avoid applying repo A's
// default branch to repo B when their defaults differ.
type RepoRevisionScope struct {
	Repo               string
	RevisionCandidates []string
}

// CodeIntelOccurrenceEvidence is one SCIP-backed symbol occurrence with
// enough symbol metadata to be useful to an agentic coding client.
type CodeIntelOccurrenceEvidence struct {
	RepoID           int32
	RepoName         string
	DisplayName      string
	Symbol           string
	Kind             *string
	Language         *string
	FilePath         string
	StartLine        int32
	StartCharacter   int32
	EndLine          int32
	EndCharacter     int32
	Role             string
	LineContent      *string
	Signature        *string
	EnclosingSymbol  *string
	Revision         string
	CommitHash       string
	CodeIntelIndexID string
}

// InspectOrgCodeGraphParams is the org-scoped graph inspection query.
// RevisionCandidates follows the same contract as
// FindOrgSymbolOccurrencesParams.
type InspectOrgCodeGraphParams struct {
	OrgID              int32
	Query              string
	Repo               string
	Repos              []string
	RevisionCandidates []string
	RepoRevisionScopes []RepoRevisionScope
	Limit              int32
	Compact            bool
}

type ListActiveCodeGraphScopesParams struct {
	OrgID              int32
	Repo               string
	Repos              []string
	RevisionCandidates []string
	RepoRevisionScopes []RepoRevisionScope
}

type CodeGraphActiveScope struct {
	GraphIndexID   string `json:"graphIndexId"`
	RepoID         int32  `json:"repoId"`
	Revision       string `json:"revision"`
	CommitHash     string `json:"commitHash"`
	WorkspaceID    string `json:"workspaceId"`
	SchemaVersion  int32  `json:"schemaVersion"`
	BuilderVersion string `json:"builderVersion"`
}

type CodeGraphSymbolEvidence struct {
	RepoID           int32
	RepoName         string
	DisplayName      string
	Symbol           string
	Kind             *string
	Language         *string
	FilePath         *string
	Revision         string
	CommitHash       string
	CodeIntelIndexID string
}

type CodeGraphRelationshipEvidence struct {
	RepoID           int32
	RepoName         string
	SourceSymbol     string
	TargetSymbol     string
	IsReference      bool
	IsImplementation bool
	IsTypeDefinition bool
	IsDefinition     bool
	Revision         string
	CommitHash       string
	CodeIntelIndexID string
}

type CodeGraphOccurrenceEvidence struct {
	RepoID           int32
	RepoName         string
	Symbol           string
	FilePath         string
	StartLine        int32
	Role             string
	LineContent      *string
	Language         *string
	Revision         string
	CommitHash       string
	CodeIntelIndexID string
}

type CodeGraphAnchorEvidence struct {
	RepoID           int32
	RepoName         string
	Kind             string
	Direction        string
	Key              string
	NormalizedKey    string
	NodeVID          string
	WorkspaceID      string
	EvidenceFilePath *string
	StartLine        *int32
	EndLine          *int32
	Confidence       float64
	Source           string
	Revision         string
	CommitHash       string
	GraphIndexID     string
}

type CodeGraphSemanticFactEvidence struct {
	RepoID         int32
	RepoName       string
	WorkspaceID    string
	ExternalID     string
	Kind           string
	Label          string
	SourceFile     string
	StartLine      *int32
	EndLine        *int32
	Evidence       *string
	Confidence     float64
	ConfidenceTier string
	Source         string
	Revision       string
	CommitHash     string
	GraphIndexID   string
}

type CodeGraphSemanticEdgeEvidence struct {
	RepoID           int32
	RepoName         string
	WorkspaceID      string
	SourceExternalID string
	TargetExternalID string
	Relation         string
	SourceFile       string
	StartLine        *int32
	EndLine          *int32
	Evidence         *string
	Rationale        *string
	Confidence       float64
	ConfidenceTier   string
	Source           string
	Revision         string
	CommitHash       string
	GraphIndexID     string
}

type CodeGraphSemanticHyperedgeEvidence struct {
	RepoID          int32
	RepoName        string
	ExternalID      string
	Label           string
	Relation        string
	NodeExternalIDs []string
	SourceFile      string
	StartLine       *int32
	EndLine         *int32
	Evidence        *string
	Confidence      float64
	ConfidenceTier  string
	Revision        string
	CommitHash      string
	GraphIndexID    string
}

type CodeGraphInspectionEvidence struct {
	OrgID               int32
	Query               string
	SearchedRepoCount   int32
	ActiveSnapshotCount int32
	WorkspaceIDs        []string
	ActiveScopes        []CodeGraphActiveScope
	Symbols             []CodeGraphSymbolEvidence
	Relationships       []CodeGraphRelationshipEvidence
	Occurrences         []CodeGraphOccurrenceEvidence
	Anchors             []CodeGraphAnchorEvidence
	SemanticFacts       []CodeGraphSemanticFactEvidence
	SemanticEdges       []CodeGraphSemanticEdgeEvidence
	SemanticHyperedges  []CodeGraphSemanticHyperedgeEvidence
	Warnings            []string
}

func (q *Queries) ListActiveCodeGraphScopes(ctx context.Context, p ListActiveCodeGraphScopesParams) ([]CodeGraphActiveScope, error) {
	if p.OrgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	repos := cleanStringList(append(p.Repos, p.Repo))
	args := []any{p.OrgID}
	activeRepoFilter := ""
	if len(repos) > 0 {
		args = append(args, repos)
		activeRepoFilter = fmt.Sprintf(` AND r.name = ANY($%d::text[])`, len(args))
	}
	activeRevisionFilter := ""
	if len(p.RepoRevisionScopes) > 0 {
		clauses := make([]string, 0, len(p.RepoRevisionScopes))
		for _, scope := range p.RepoRevisionScopes {
			repo := strings.TrimSpace(scope.Repo)
			if repo == "" {
				continue
			}
			revisions := cleanStringList(scope.RevisionCandidates)
			if len(revisions) == 0 {
				args = append(args, repo)
				clauses = append(clauses, fmt.Sprintf(`r.name = $%d`, len(args)))
				continue
			}
			args = append(args, repo)
			repoArg := len(args)
			args = append(args, revisions)
			revisionArg := len(args)
			clauses = append(clauses, fmt.Sprintf(`(r.name = $%d AND cgr.revision = ANY($%d::text[]))`, repoArg, revisionArg))
		}
		if len(clauses) > 0 {
			activeRevisionFilter = ` AND (` + strings.Join(clauses, ` OR `) + `)`
		}
	} else if len(p.RevisionCandidates) > 0 {
		args = append(args, cleanStringList(p.RevisionCandidates))
		activeRevisionFilter = fmt.Sprintf(` AND cgr.revision = ANY($%d::text[])`, len(args))
	}

	query := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", g."workspaceId", cgr.revision, g."schemaVersion", g."builderVersion"
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + activeRepoFilter + activeRevisionFilter + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
)
SELECT id, "repoId", "commitHash", "workspaceId", revision, "schemaVersion", "builderVersion" FROM active_graphs
ORDER BY "repoId", revision, id`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: ListActiveCodeGraphScopes: active graphs: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphActiveScope, 0)
	for rows.Next() {
		var scope CodeGraphActiveScope
		if err := rows.Scan(&scope.GraphIndexID, &scope.RepoID, &scope.CommitHash, &scope.WorkspaceID, &scope.Revision, &scope.SchemaVersion, &scope.BuilderVersion); err != nil {
			return nil, fmt.Errorf("db: ListActiveCodeGraphScopes: scan: %w", err)
		}
		out = append(out, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListActiveCodeGraphScopes: rows: %w", err)
	}
	return out, nil
}

func (q *Queries) FindOrgSymbolOccurrences(ctx context.Context, p FindOrgSymbolOccurrencesParams) ([]CodeIntelOccurrenceEvidence, error) {
	if p.OrgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	p.Symbol = strings.TrimSpace(p.Symbol)
	if p.Symbol == "" {
		return nil, fmt.Errorf("db: FindOrgSymbolOccurrences: symbol is required")
	}
	if p.Mode != SymbolOccurrenceDefinitions && p.Mode != SymbolOccurrenceReferences {
		return nil, fmt.Errorf("db: FindOrgSymbolOccurrences: unsupported mode %q", p.Mode)
	}
	limit := normalizeCodeIntelLimit(p.Limit)
	args := []any{p.OrgID, p.Symbol, scipSymbolSuffixPatterns(p.Symbol), limit}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	repos := cleanStringList(append(p.Repos, p.Repo))
	var where strings.Builder
	where.WriteString(` WHERE s."orgId" = $1`)
	where.WriteString(` AND ci."orgId" = s."orgId" AND ci."repoId" = s."repoId"`)
	where.WriteString(` AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")`)
	where.WriteString(` AND (LOWER(s."displayName") = LOWER($2) OR LOWER(s.symbol) = LOWER($2) OR s.symbol ILIKE ANY($3::text[]))`)
	if len(p.RepoRevisionScopes) > 0 {
		clauses := make([]string, 0, len(p.RepoRevisionScopes))
		for _, scope := range p.RepoRevisionScopes {
			repo := strings.TrimSpace(scope.Repo)
			if repo == "" {
				continue
			}
			revisions := cleanStringList(scope.RevisionCandidates)
			if len(revisions) == 0 {
				clauses = append(clauses, `r.name = `+arg(repo))
				continue
			}
			clauses = append(clauses, `(r.name = `+arg(repo)+` AND ci.revision = ANY(`+arg(revisions)+`::text[]))`)
		}
		if len(clauses) > 0 {
			where.WriteString(` AND (` + strings.Join(clauses, ` OR `) + `)`)
		}
	} else if len(repos) > 0 {
		where.WriteString(` AND r.name = ANY(` + arg(repos) + `::text[])`)
	}
	if len(p.RepoRevisionScopes) == 0 && len(p.RevisionCandidates) > 0 {
		where.WriteString(` AND ci.revision = ANY(` + arg(cleanStringList(p.RevisionCandidates)) + `::text[])`)
	}
	if p.DefinitionFile != "" {
		where.WriteString(` AND COALESCE(s."filePath", '') = ` + arg(p.DefinitionFile))
	}
	occurrenceRoleFilter := `o.role = 'REFERENCE'::"CodeIntelOccurrenceRole"`
	if p.Mode == SymbolOccurrenceDefinitions {
		occurrenceRoleFilter = `o.role IN ('DEFINITION'::"CodeIntelOccurrenceRole", 'FORWARD_DEFINITION'::"CodeIntelOccurrenceRole")`
	}

	query := `WITH latest_indexes AS (
  SELECT *
  FROM (
    SELECT ci.*,
      ROW_NUMBER() OVER (
        PARTITION BY ci."orgId", ci."repoId", ci."workspaceId", ci.branch, ci.revision, ci.kind
        ORDER BY COALESCE(ci."indexedAt", ci."updatedAt") DESC, ci."updatedAt" DESC, ci.id DESC
      ) AS rn
    FROM "CodeIntelIndex" ci
    WHERE ci.kind = 'SCIP'::"CodeIntelIndexKind"
      AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
  ) ranked_indexes
  WHERE rn = 1
),
matched_symbols AS (
  SELECT r.id AS "repoId", r.name AS "repoName", s."displayName", s.symbol, s.kind, s.language, s.signature, s."enclosingSymbol", ci.revision, ci."commitHash", ci.id AS "codeIntelIndexId",
    CASE
      WHEN LOWER(s."displayName") = LOWER($2) THEN 0
      WHEN LOWER(s.symbol) = LOWER($2) THEN 1
      ELSE 2
    END AS match_rank,
    COALESCE(ci."indexedAt", ci."updatedAt") AS indexed_at
  FROM "CodeIntelSymbol" s
  JOIN latest_indexes ci ON ci.id = s."codeIntelIndexId"
  JOIN "CodeIntelLanguageIndex" li
    ON li.id = s."codeIntelLanguageIndexId"
   AND li."codeIntelIndexId" = ci.id
   AND li.status = 'READY'::"CodeIntelIndexStatus"
  JOIN "Repo" r ON r.id = s."repoId" AND r."orgId" = s."orgId"` + where.String() + `
  ORDER BY match_rank ASC, indexed_at DESC, r.name ASC, s."displayName" ASC
  LIMIT GREATEST(32, $4::int * 8)
)
SELECT ms."repoId", ms."repoName", ms."displayName", ms.symbol, ms.kind, COALESCE(o.language, ms.language), o."filePath",
       o."startLine", o."startCharacter", o."endLine", o."endCharacter", o.role::text, o."lineContent",
       ms.signature, ms."enclosingSymbol", ms.revision, ms."commitHash", ms."codeIntelIndexId"
FROM matched_symbols ms
JOIN "CodeIntelOccurrence" o ON o."codeIntelIndexId" = ms."codeIntelIndexId" AND o.symbol = ms.symbol AND o."orgId" = $1 AND o."repoId" = ms."repoId"
JOIN "CodeIntelLanguageIndex" oli
  ON oli.id = o."codeIntelLanguageIndexId"
 AND oli."codeIntelIndexId" = o."codeIntelIndexId"
 AND oli.status = 'READY'::"CodeIntelIndexStatus"
WHERE ` + occurrenceRoleFilter + `
ORDER BY
  ms.match_rank ASC,
  ms.indexed_at DESC,
  ms."repoName" ASC,
  o."filePath" ASC,
  o."startLine" ASC,
  o."startCharacter" ASC
LIMIT $4`

	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: FindOrgSymbolOccurrences: %w", err)
	}
	defer rows.Close()

	out := make([]CodeIntelOccurrenceEvidence, 0, limit)
	for rows.Next() {
		var row CodeIntelOccurrenceEvidence
		if err := rows.Scan(
			&row.RepoID,
			&row.RepoName,
			&row.DisplayName,
			&row.Symbol,
			&row.Kind,
			&row.Language,
			&row.FilePath,
			&row.StartLine,
			&row.StartCharacter,
			&row.EndLine,
			&row.EndCharacter,
			&row.Role,
			&row.LineContent,
			&row.Signature,
			&row.EnclosingSymbol,
			&row.Revision,
			&row.CommitHash,
			&row.CodeIntelIndexID,
		); err != nil {
			return nil, fmt.Errorf("db: FindOrgSymbolOccurrences: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: FindOrgSymbolOccurrences: rows: %w", err)
	}
	return out, nil
}

func (q *Queries) InspectOrgCodeGraph(ctx context.Context, p InspectOrgCodeGraphParams) (CodeGraphInspectionEvidence, error) {
	if p.OrgID <= 0 {
		return CodeGraphInspectionEvidence{}, ErrInvalidOrgID
	}
	limit := normalizeCodeIntelLimit(p.Limit)
	repos := cleanStringList(append(p.Repos, p.Repo))
	terms := tokenizeCodeIntelQuery(p.Query)
	if p.Compact {
		terms = compactCodeGraphTerms(terms)
	}
	out := CodeGraphInspectionEvidence{
		OrgID:    p.OrgID,
		Query:    p.Query,
		Warnings: make([]string, 0),
	}
	patterns := likePatterns(terms)

	activeArgs := []any{p.OrgID}
	activeRepoFilter := ""
	if len(repos) > 0 {
		activeArgs = append(activeArgs, repos)
		activeRepoFilter = fmt.Sprintf(` AND r.name = ANY($%d::text[])`, len(activeArgs))
	}
	activeRevisionFilter := ""
	if len(p.RepoRevisionScopes) > 0 {
		clauses := make([]string, 0, len(p.RepoRevisionScopes))
		for _, scope := range p.RepoRevisionScopes {
			repo := strings.TrimSpace(scope.Repo)
			if repo == "" {
				continue
			}
			revisions := cleanStringList(scope.RevisionCandidates)
			if len(revisions) == 0 {
				activeArgs = append(activeArgs, repo)
				clauses = append(clauses, fmt.Sprintf(`r.name = $%d`, len(activeArgs)))
				continue
			}
			activeArgs = append(activeArgs, repo)
			repoArg := len(activeArgs)
			activeArgs = append(activeArgs, revisions)
			revisionArg := len(activeArgs)
			clauses = append(clauses, fmt.Sprintf(`(r.name = $%d AND cgr.revision = ANY($%d::text[]))`, repoArg, revisionArg))
		}
		if len(clauses) > 0 {
			activeRevisionFilter = ` AND (` + strings.Join(clauses, ` OR `) + `)`
		}
	} else if len(p.RevisionCandidates) > 0 {
		activeArgs = append(activeArgs, cleanStringList(p.RevisionCandidates))
		activeRevisionFilter = fmt.Sprintf(` AND cgr.revision = ANY($%d::text[])`, len(activeArgs))
	}
	activeGraphQuery := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", g."workspaceId", cgr.revision, g."schemaVersion", g."builderVersion"
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + activeRepoFilter + activeRevisionFilter + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
)
SELECT id, "repoId", "commitHash", "workspaceId", revision, "schemaVersion", "builderVersion" FROM active_graphs`
	activeRows, err := q.db.Query(ctx, activeGraphQuery, activeArgs...)
	if err != nil {
		return CodeGraphInspectionEvidence{}, fmt.Errorf("db: InspectOrgCodeGraph: active graphs: %w", err)
	}
	defer activeRows.Close()
	workspaceSeen := map[string]bool{}
	repoSeen := map[int32]bool{}
	for activeRows.Next() {
		var scope CodeGraphActiveScope
		if err := activeRows.Scan(&scope.GraphIndexID, &scope.RepoID, &scope.CommitHash, &scope.WorkspaceID, &scope.Revision, &scope.SchemaVersion, &scope.BuilderVersion); err != nil {
			return CodeGraphInspectionEvidence{}, fmt.Errorf("db: InspectOrgCodeGraph: active graph scan: %w", err)
		}
		out.ActiveScopes = append(out.ActiveScopes, scope)
		repoSeen[scope.RepoID] = true
		if scope.WorkspaceID != "" && !workspaceSeen[scope.WorkspaceID] {
			workspaceSeen[scope.WorkspaceID] = true
			out.WorkspaceIDs = append(out.WorkspaceIDs, scope.WorkspaceID)
		}
	}
	if err := activeRows.Err(); err != nil {
		return CodeGraphInspectionEvidence{}, fmt.Errorf("db: InspectOrgCodeGraph: active graph rows: %w", err)
	}
	out.SearchedRepoCount = int32(len(repoSeen))
	out.ActiveSnapshotCount = int32(len(out.ActiveScopes))
	if out.ActiveSnapshotCount == 0 {
		out.Warnings = append(out.Warnings, "No READY active code graph snapshots matched the requested repositories/revision.")
		return out, nil
	}
	if len(terms) == 0 {
		out.Warnings = append(out.Warnings, "Query had no searchable symbol, route, event, package, or config terms; returning active graph snapshot scope only.")
		return out, nil
	}

	if p.Compact {
		edges, err := q.inspectGraphSemanticEdges(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
		if err != nil {
			return CodeGraphInspectionEvidence{}, err
		}
		out.SemanticEdges = edges
		if len(out.SemanticEdges) == 0 {
			fallbackEdges, fallbackErr := q.inspectGraphSemanticEdgeSeeds(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, minCodeIntelInt32(limit, 12))
			if fallbackErr != nil {
				return CodeGraphInspectionEvidence{}, fallbackErr
			}
			out.SemanticEdges = fallbackEdges
			if len(out.SemanticEdges) == 0 {
				out.Warnings = append(out.Warnings, "No compact semantic graph edges matched the graph seeds; use full inspect_code_graph or grep/read_file for supplemental evidence.")
			} else {
				out.Warnings = append(out.Warnings, "Compact graph terms did not directly match semantic edge metadata; using high-confidence active graph edge seeds for the scoped repo/ref so Nebula traversal can still return structural context.")
			}
		}
		return out, nil
	}

	symbols, err := q.inspectGraphSymbols(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.Symbols = symbols

	occurrences, err := q.inspectGraphOccurrences(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.Occurrences = occurrences

	relationships, err := q.inspectGraphRelationships(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, symbols, occurrences, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.Relationships = relationships

	anchors, err := q.inspectGraphAnchors(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.Anchors = anchors

	facts, err := q.inspectGraphSemanticFacts(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.SemanticFacts = facts

	edges, err := q.inspectGraphSemanticEdges(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.SemanticEdges = edges

	hyperedges, err := q.inspectGraphSemanticHyperedges(ctx, p.OrgID, repos, p.RevisionCandidates, p.RepoRevisionScopes, patterns, limit)
	if err != nil {
		return CodeGraphInspectionEvidence{}, err
	}
	out.SemanticHyperedges = hyperedges

	if len(out.Symbols) == 0 && len(out.Relationships) == 0 && len(out.Occurrences) == 0 && len(out.Anchors) == 0 && len(out.SemanticFacts) == 0 && len(out.SemanticEdges) == 0 && len(out.SemanticHyperedges) == 0 {
		out.Warnings = append(out.Warnings, "No SCIP relationship, architecture anchor, or semantic fact matched the graph seeds; use grep/read_file for supplemental lexical evidence.")
	}
	return out, nil
}

func (q *Queries) inspectGraphSymbols(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, patterns []string, limit int32) ([]CodeGraphSymbolEvidence, error) {
	args := []any{orgID, patterns, limit}
	filters := activeIndexFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_indexes AS (
  SELECT DISTINCT ON (ci."repoId", ci.revision) ci.id, ci."repoId", ci.revision, ci."commitHash"
  FROM "CodeIntelIndex" ci
  JOIN "Repo" r ON r.id = ci."repoId" AND r."orgId" = ci."orgId"
  WHERE ci."orgId" = $1
    AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
    AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY ci."repoId", ci.revision, ci."updatedAt" DESC, ci."indexedAt" DESC, ci.id DESC
)
SELECT r.id, r.name, s."displayName", s.symbol, s.kind, s.language, s."filePath", ai.revision, ai."commitHash", ai.id
FROM "CodeIntelSymbol" s
JOIN active_indexes ai ON ai.id = s."codeIntelIndexId"
JOIN "CodeIntelLanguageIndex" li
  ON li.id = s."codeIntelLanguageIndexId"
 AND li."codeIntelIndexId" = ai.id
 AND li.status = 'READY'::"CodeIntelIndexStatus"
JOIN "Repo" r ON r.id = s."repoId" AND r."orgId" = s."orgId"
WHERE s."orgId" = $1
  AND EXISTS (SELECT 1 FROM unnest($2::text[]) pat WHERE s."displayName" ILIKE pat OR s.symbol ILIKE pat OR COALESCE(s."filePath", '') ILIKE pat)
ORDER BY r.name ASC, s."filePath" ASC NULLS LAST, s."displayName" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph symbols: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphSymbolEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphSymbolEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.DisplayName, &row.Symbol, &row.Kind, &row.Language, &row.FilePath, &row.Revision, &row.CommitHash, &row.CodeIntelIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph symbols scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph symbols rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphOccurrences(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, patterns []string, limit int32) ([]CodeGraphOccurrenceEvidence, error) {
	args := []any{orgID, patterns, limit}
	filters := activeIndexFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_indexes AS (
  SELECT DISTINCT ON (ci."repoId", ci.revision) ci.id, ci."repoId", ci.revision, ci."commitHash"
  FROM "CodeIntelIndex" ci
  JOIN "Repo" r ON r.id = ci."repoId" AND r."orgId" = ci."orgId"
  WHERE ci."orgId" = $1
    AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
    AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY ci."repoId", ci.revision, ci."updatedAt" DESC, ci."indexedAt" DESC, ci.id DESC
)
SELECT r.id, r.name, o.symbol, o."filePath", o."startLine", o.role::text, o."lineContent", o.language, ai.revision, ai."commitHash", ai.id
FROM "CodeIntelOccurrence" o
JOIN active_indexes ai ON ai.id = o."codeIntelIndexId"
JOIN "CodeIntelLanguageIndex" li
  ON li.id = o."codeIntelLanguageIndexId"
 AND li."codeIntelIndexId" = ai.id
 AND li.status = 'READY'::"CodeIntelIndexStatus"
JOIN "Repo" r ON r.id = o."repoId" AND r."orgId" = o."orgId"
WHERE o."orgId" = $1
  AND EXISTS (SELECT 1 FROM unnest($2::text[]) pat WHERE o.symbol ILIKE pat OR o."filePath" ILIKE pat)
ORDER BY r.name ASC, o."filePath" ASC, o."startLine" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph occurrences: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphOccurrenceEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphOccurrenceEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.Symbol, &row.FilePath, &row.StartLine, &row.Role, &row.LineContent, &row.Language, &row.Revision, &row.CommitHash, &row.CodeIntelIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph occurrences scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph occurrences rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphRelationships(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, symbols []CodeGraphSymbolEvidence, occurrences []CodeGraphOccurrenceEvidence, limit int32) ([]CodeGraphRelationshipEvidence, error) {
	seedSet := map[string]bool{}
	for _, symbol := range symbols {
		seedSet[symbol.Symbol] = true
	}
	for _, occurrence := range occurrences {
		seedSet[occurrence.Symbol] = true
	}
	seeds := make([]string, 0, len(seedSet))
	for seed := range seedSet {
		seeds = append(seeds, seed)
	}
	if len(seeds) == 0 {
		return []CodeGraphRelationshipEvidence{}, nil
	}
	args := []any{orgID, seeds, limit}
	filters := activeIndexFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_indexes AS (
  SELECT DISTINCT ON (ci."repoId", ci.revision) ci.id, ci."repoId", ci.revision, ci."commitHash"
  FROM "CodeIntelIndex" ci
  JOIN "Repo" r ON r.id = ci."repoId" AND r."orgId" = ci."orgId"
  WHERE ci."orgId" = $1
    AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
    AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY ci."repoId", ci.revision, ci."updatedAt" DESC, ci."indexedAt" DESC, ci.id DESC
)
SELECT r.id, r.name, rel."sourceSymbol", rel."targetSymbol", rel."isReference", rel."isImplementation", rel."isTypeDefinition", rel."isDefinition", ai.revision, ai."commitHash", ai.id
FROM "CodeIntelRelationship" rel
JOIN active_indexes ai ON ai.id = rel."codeIntelIndexId"
JOIN "CodeIntelLanguageIndex" li
  ON li.id = rel."codeIntelLanguageIndexId"
 AND li."codeIntelIndexId" = ai.id
 AND li.status = 'READY'::"CodeIntelIndexStatus"
JOIN "Repo" r ON r.id = rel."repoId" AND r."orgId" = rel."orgId"
WHERE rel."orgId" = $1
  AND (rel."sourceSymbol" = ANY($2::text[]) OR rel."targetSymbol" = ANY($2::text[]))
ORDER BY r.name ASC, rel."sourceSymbol" ASC, rel."targetSymbol" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph relationships: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphRelationshipEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphRelationshipEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.SourceSymbol, &row.TargetSymbol, &row.IsReference, &row.IsImplementation, &row.IsTypeDefinition, &row.IsDefinition, &row.Revision, &row.CommitHash, &row.CodeIntelIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph relationships scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph relationships rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphAnchors(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, patterns []string, limit int32) ([]CodeGraphAnchorEvidence, error) {
	args := []any{orgID, patterns, limit}
	filters := activeGraphFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", g."workspaceId", cgr.revision
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
)
SELECT r.id, r.name, a.kind, a.direction::text, a.key, a."normalizedKey", a."nodeVid", a."workspaceId", a."evidenceFilePath", a."startLine", a."endLine", a.confidence, a.source, ag.revision, ag."commitHash", ag.id
FROM "CodeGraphAnchor" a
JOIN active_graphs ag ON ag.id = a."graphIndexId"
JOIN "Repo" r ON r.id = a."repoId" AND r."orgId" = a."orgId"
WHERE a."orgId" = $1
  AND EXISTS (SELECT 1 FROM unnest($2::text[]) pat WHERE a.kind ILIKE pat OR a.key ILIKE pat OR a."normalizedKey" ILIKE pat OR COALESCE(a."evidenceFilePath", '') ILIKE pat OR a.source ILIKE pat)
ORDER BY r.name ASC, a.kind ASC, a."normalizedKey" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph anchors: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphAnchorEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphAnchorEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.Kind, &row.Direction, &row.Key, &row.NormalizedKey, &row.NodeVID, &row.WorkspaceID, &row.EvidenceFilePath, &row.StartLine, &row.EndLine, &row.Confidence, &row.Source, &row.Revision, &row.CommitHash, &row.GraphIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph anchors scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph anchors rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphSemanticFacts(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, patterns []string, limit int32) ([]CodeGraphSemanticFactEvidence, error) {
	args := []any{orgID, patterns, limit}
	filters := activeGraphFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", cgr.revision
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
)
SELECT r.id, r.name, f."workspaceId", f."externalId", f.kind, f.label, f."sourceFile", f."startLine", f."endLine", f.evidence, f.confidence, f."confidenceTier"::text, f.source, ag.revision, ag."commitHash", ag.id
FROM "CodeGraphSemanticFact" f
JOIN active_graphs ag ON ag.id = f."graphIndexId"
JOIN "Repo" r ON r.id = f."repoId" AND r."orgId" = f."orgId"
WHERE f."orgId" = $1
  AND EXISTS (SELECT 1 FROM unnest($2::text[]) pat WHERE f."externalId" ILIKE pat OR f.kind ILIKE pat OR f.label ILIKE pat OR f."sourceFile" ILIKE pat OR COALESCE(f.evidence, '') ILIKE pat)
ORDER BY r.name ASC,
  CASE lower(f.kind)
    WHEN 'route' THEN 0
    WHEN 'event' THEN 0
    WHEN 'service' THEN 0
    WHEN 'job' THEN 0
    WHEN 'function' THEN 1
    WHEN 'method' THEN 1
    WHEN 'class' THEN 1
    WHEN 'symbol' THEN 2
    WHEN 'field' THEN 3
    WHEN 'type' THEN 3
    WHEN 'file' THEN 8
    WHEN 'package' THEN 9
    WHEN 'import' THEN 10
    ELSE 4
  END ASC,
  CASE
    WHEN f."sourceFile" ILIKE '%_test.%' OR f."sourceFile" ILIKE '%/test/%' OR f."sourceFile" ILIKE 'tests/%' THEN 2
    ELSE 0
  END ASC,
  f.confidence DESC,
  f."sourceFile" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic facts: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphSemanticFactEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphSemanticFactEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.WorkspaceID, &row.ExternalID, &row.Kind, &row.Label, &row.SourceFile, &row.StartLine, &row.EndLine, &row.Evidence, &row.Confidence, &row.ConfidenceTier, &row.Source, &row.Revision, &row.CommitHash, &row.GraphIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic facts scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic facts rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphSemanticEdges(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, patterns []string, limit int32) ([]CodeGraphSemanticEdgeEvidence, error) {
	args := []any{orgID, patterns, limit}
	filters := activeGraphFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", g."workspaceId", cgr.revision
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
),
matched_edges AS (
  SELECT r.id AS "repoId", r.name AS "repoName", e."workspaceId", e."sourceExternalId", e."targetExternalId", e.relation, e."sourceFile", e."startLine", e."endLine", e.evidence, e.rationale, e.confidence, e."confidenceTier"::text AS "confidenceTier", e.source, ag.revision, ag."commitHash", ag.id AS "graphIndexId",
    CASE
      WHEN lower(e.source) = 'scip' THEN 'scip'
      WHEN lower(e.source) LIKE 'ast-%' OR lower(e.source) LIKE 'tree-sitter%' OR lower(e.source) LIKE 'tree_sitter%' THEN 'ast'
      ELSE 'other'
    END AS producer_class,
    (
      SELECT COALESCE(SUM(
        CASE
          WHEN (COALESCE(e."sourceExternalId", '') || ' ' || COALESCE(e."targetExternalId", '') || ' ' || COALESCE(e.relation, '') || ' ' || COALESCE(e."sourceFile", '') || ' ' || COALESCE(e.evidence, '') || ' ' || COALESCE(e.rationale, '')) ILIKE pat
          THEN GREATEST(1, LENGTH(REPLACE(REPLACE(pat, '%', ''), '\', '')))
          ELSE 0
        END
      ), 0)
      FROM unnest($2::text[]) pat
    )
    + CASE
        WHEN lower(e.source) = 'scip' THEN 100
        WHEN lower(e.source) LIKE 'ast-%' OR lower(e.source) LIKE 'tree-sitter%' OR lower(e.source) LIKE 'tree_sitter%' THEN 60
        ELSE 0
      END
    + CASE upper(e.relation)
        WHEN 'CALLS' THEN 80
        WHEN 'REFERENCES' THEN 50
        WHEN 'IMPLEMENTS' THEN 45
        WHEN 'TYPE_DEFINES' THEN 45
        WHEN 'DEFINES' THEN 20
        ELSE 0
      END
    + CASE
        WHEN lower(e."sourceFile") LIKE '%/sdk.%'
          OR lower(e."sourceFile") LIKE '%/nodejs.%'
          OR lower(e."sourceFile") LIKE '%/python.%'
          OR lower(e."sourceFile") LIKE '%/dotnet.%'
          OR lower(e."sourceFile") LIKE '%/podmutator.%'
          OR lower(e."sourceFile") LIKE '%/webhook/%'
        THEN 30 ELSE 0
      END
    - CASE
        WHEN e."sourceFile" ILIKE '%_test.%' OR e."sourceFile" ILIKE '%/test/%' OR e."sourceFile" ILIKE 'tests/%'
        THEN 50 ELSE 0
      END AS semantic_score,
    ROW_NUMBER() OVER (
      PARTITION BY upper(e.relation), CASE
        WHEN lower(e.source) = 'scip' THEN 'scip'
        WHEN lower(e.source) LIKE 'ast-%' OR lower(e.source) LIKE 'tree-sitter%' OR lower(e.source) LIKE 'tree_sitter%' THEN 'ast'
        ELSE 'other'
      END
      ORDER BY
        (
          SELECT COALESCE(SUM(
            CASE
              WHEN (COALESCE(e."sourceExternalId", '') || ' ' || COALESCE(e."targetExternalId", '') || ' ' || COALESCE(e.relation, '') || ' ' || COALESCE(e."sourceFile", '') || ' ' || COALESCE(e.evidence, '') || ' ' || COALESCE(e.rationale, '')) ILIKE pat
              THEN GREATEST(1, LENGTH(REPLACE(REPLACE(pat, '%', ''), '\', '')))
              ELSE 0
            END
          ), 0)
          FROM unnest($2::text[]) pat
        ) DESC,
        CASE WHEN lower(e.source) = 'scip' THEN 0 ELSE 1 END ASC,
        CASE upper(e.relation)
          WHEN 'CALLS' THEN 0
          WHEN 'REFERENCES' THEN 1
          WHEN 'IMPLEMENTS' THEN 2
          WHEN 'TYPE_DEFINES' THEN 2
          WHEN 'DEFINES' THEN 3
          ELSE 4
        END ASC,
        CASE
          WHEN lower(e."sourceFile") LIKE '%/sdk.%'
            OR lower(e."sourceFile") LIKE '%/nodejs.%'
            OR lower(e."sourceFile") LIKE '%/python.%'
            OR lower(e."sourceFile") LIKE '%/dotnet.%'
            OR lower(e."sourceFile") LIKE '%/podmutator.%'
            OR lower(e."sourceFile") LIKE '%/webhook/%'
          THEN 0 ELSE 1
        END ASC,
        CASE
          WHEN e."sourceFile" ILIKE '%_test.%' OR e."sourceFile" ILIKE '%/test/%' OR e."sourceFile" ILIKE 'tests/%' THEN 2
          ELSE 0
        END ASC,
        e.confidence DESC,
        e."sourceFile" ASC,
        e."startLine" ASC
	    ) AS relation_rank
	  FROM "CodeGraphSemanticEdge" e
  JOIN active_graphs ag ON ag.id = e."graphIndexId"
  JOIN "Repo" r ON r.id = e."repoId" AND r."orgId" = e."orgId"
  WHERE e."orgId" = $1
    AND (COALESCE(e."sourceExternalId", '') || ' ' || COALESCE(e."targetExternalId", '') || ' ' || COALESCE(e.relation, '') || ' ' || COALESCE(e."sourceFile", '') || ' ' || COALESCE(e.evidence, '') || ' ' || COALESCE(e.rationale, '')) ILIKE ANY($2::text[])
    AND NOT (e."sourceExternalId" ~* '(^|[ /#.])local [0-9]+\\.?$' OR e."targetExternalId" ~* '(^|[ /#.])local [0-9]+\\.?$')
    AND NOT (COALESCE(e.evidence, '') ~* '(CALLS|REFERENCES|DEFINES) [0-9]+\\.?$')
),
balanced_edges AS (
  SELECT *,
    ROW_NUMBER() OVER (
      PARTITION BY producer_class
      ORDER BY
        semantic_score DESC,
        CASE upper(relation)
          WHEN 'HANDLES' THEN 0
          WHEN 'EMITS' THEN 0
          WHEN 'PROVIDES' THEN 0
          WHEN 'CONSUMES' THEN 0
          WHEN 'CALLS' THEN 1
          WHEN 'REFERENCES' THEN 1
          WHEN 'IMPLEMENTS' THEN 2
          WHEN 'TYPE_DEFINES' THEN 2
          WHEN 'EXTENDS' THEN 2
          WHEN 'DEFINES' THEN 3
          WHEN 'IMPORTS' THEN 4
          WHEN 'IMPORTS_FROM' THEN 4
          WHEN 'CONTAINS' THEN 6
          ELSE 5
        END ASC,
        confidence DESC,
        "sourceFile" ASC,
        "startLine" ASC
    ) AS producer_rank
  FROM matched_edges
  WHERE relation_rank <= CASE
      WHEN upper(relation) IN ('CALLS', 'REFERENCES') THEN GREATEST(8, ($3::int / 2))
      WHEN upper(relation) IN ('IMPORTS', 'IMPORTS_FROM') THEN GREATEST(3, ($3::int / 8))
      ELSE GREATEST(4, ($3::int / 4))
    END
)
SELECT "repoId", "repoName", "workspaceId", "sourceExternalId", "targetExternalId", relation, "sourceFile", "startLine", "endLine", evidence, rationale, confidence, "confidenceTier", source, revision, "commitHash", "graphIndexId"
FROM balanced_edges
ORDER BY producer_rank ASC,
  CASE producer_class
    WHEN 'scip' THEN 0
    WHEN 'ast' THEN 1
    ELSE 2
  END ASC,
  "repoName" ASC,
  semantic_score DESC,
  CASE upper(relation)
    WHEN 'HANDLES' THEN 0
    WHEN 'EMITS' THEN 0
    WHEN 'PROVIDES' THEN 0
    WHEN 'CONSUMES' THEN 0
    WHEN 'CALLS' THEN 1
    WHEN 'REFERENCES' THEN 1
    WHEN 'IMPLEMENTS' THEN 2
    WHEN 'TYPE_DEFINES' THEN 2
    WHEN 'EXTENDS' THEN 2
    WHEN 'DEFINES' THEN 3
    WHEN 'IMPORTS' THEN 4
    WHEN 'IMPORTS_FROM' THEN 4
    WHEN 'CONTAINS' THEN 6
    ELSE 5
  END ASC,
  CASE
    WHEN "sourceFile" ILIKE '%_test.%' OR "sourceFile" ILIKE '%/test/%' OR "sourceFile" ILIKE 'tests/%' THEN 2
    ELSE 0
  END ASC,
  confidence DESC,
  "sourceFile" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic edges: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphSemanticEdgeEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphSemanticEdgeEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.WorkspaceID, &row.SourceExternalID, &row.TargetExternalID, &row.Relation, &row.SourceFile, &row.StartLine, &row.EndLine, &row.Evidence, &row.Rationale, &row.Confidence, &row.ConfidenceTier, &row.Source, &row.Revision, &row.CommitHash, &row.GraphIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic edges scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic edges rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphSemanticEdgeSeeds(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, limit int32) ([]CodeGraphSemanticEdgeEvidence, error) {
	args := []any{orgID, limit}
	filters := activeGraphFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", g."workspaceId", cgr.revision
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
),
ranked_edges AS (
  SELECT r.id AS "repoId", r.name AS "repoName", e."workspaceId", e."sourceExternalId", e."targetExternalId", e.relation, e."sourceFile", e."startLine", e."endLine", e.evidence, e.rationale, e.confidence, e."confidenceTier"::text AS "confidenceTier", e.source, ag.revision, ag."commitHash", ag.id AS "graphIndexId",
    CASE
      WHEN lower(e.source) = 'scip' THEN 0
      WHEN lower(e.source) LIKE 'ast-%' OR lower(e.source) LIKE 'tree-sitter%' OR lower(e.source) LIKE 'tree_sitter%' THEN 1
      ELSE 2
    END AS producer_rank,
    CASE upper(e.relation)
      WHEN 'CALLS' THEN 0
      WHEN 'REFERENCES' THEN 1
      WHEN 'HANDLES' THEN 2
      WHEN 'EMITS' THEN 2
      WHEN 'PROVIDES' THEN 2
      WHEN 'CONSUMES' THEN 2
      WHEN 'IMPLEMENTS' THEN 3
      WHEN 'TYPE_DEFINES' THEN 3
      WHEN 'EXTENDS' THEN 3
      WHEN 'DEFINES' THEN 4
      WHEN 'IMPORTS' THEN 5
      WHEN 'IMPORTS_FROM' THEN 5
      WHEN 'CONTAINS' THEN 7
      ELSE 6
    END AS relation_rank,
    ROW_NUMBER() OVER (
      PARTITION BY r.id,
        CASE
          WHEN lower(e.source) = 'scip' THEN 'scip'
          WHEN lower(e.source) LIKE 'ast-%' OR lower(e.source) LIKE 'tree-sitter%' OR lower(e.source) LIKE 'tree_sitter%' THEN 'ast'
          ELSE 'other'
        END,
        upper(e.relation)
      ORDER BY e.confidence DESC, e."sourceFile" ASC, e."startLine" ASC
    ) AS bucket_rank
  FROM "CodeGraphSemanticEdge" e
  JOIN active_graphs ag ON ag.id = e."graphIndexId"
  JOIN "Repo" r ON r.id = e."repoId" AND r."orgId" = e."orgId"
  WHERE e."orgId" = $1
    AND NOT (e."sourceExternalId" ~* '(^|[ /#.])local [0-9]+\\.?$' OR e."targetExternalId" ~* '(^|[ /#.])local [0-9]+\\.?$')
    AND NOT (COALESCE(e.evidence, '') ~* '(CALLS|REFERENCES|DEFINES) [0-9]+\\.?$')
)
SELECT "repoId", "repoName", "workspaceId", "sourceExternalId", "targetExternalId", relation, "sourceFile", "startLine", "endLine", evidence, rationale, confidence, "confidenceTier", source, revision, "commitHash", "graphIndexId"
FROM ranked_edges
WHERE bucket_rank <= 4
ORDER BY producer_rank ASC,
  relation_rank ASC,
  CASE
    WHEN "sourceFile" ILIKE '%_test.%' OR "sourceFile" ILIKE '%/test/%' OR "sourceFile" ILIKE 'tests/%' THEN 2
    ELSE 0
  END ASC,
  confidence DESC,
  "repoName" ASC,
  "sourceFile" ASC,
  "startLine" ASC
LIMIT $2`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic edge seeds: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphSemanticEdgeEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphSemanticEdgeEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.WorkspaceID, &row.SourceExternalID, &row.TargetExternalID, &row.Relation, &row.SourceFile, &row.StartLine, &row.EndLine, &row.Evidence, &row.Rationale, &row.Confidence, &row.ConfidenceTier, &row.Source, &row.Revision, &row.CommitHash, &row.GraphIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic edge seeds scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic edge seeds rows: %w", err)
	}
	return out, nil
}

func (q *Queries) inspectGraphSemanticHyperedges(ctx context.Context, orgID int32, repos, revisions []string, scopes []RepoRevisionScope, patterns []string, limit int32) ([]CodeGraphSemanticHyperedgeEvidence, error) {
	args := []any{orgID, patterns, limit}
	filters := activeGraphFilters(&args, repos, revisions, scopes, `r`)
	query := `WITH active_graphs AS (
  SELECT DISTINCT ON (g."repoId", cgr.revision) g.id, g."repoId", g."commitHash", cgr.revision
  FROM "CodeGraphIndex" g
  JOIN "Repo" r ON r.id = g."repoId" AND r."orgId" = g."orgId"
  JOIN "CodeGraphRevision" cgr ON cgr."codeGraphIndexId" = g.id AND cgr."orgId" = g."orgId" AND cgr."repoId" = g."repoId"
  WHERE g."orgId" = $1
    AND g.status = 'READY'::"CodeGraphIndexStatus"
    AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")` + filters + `
  ORDER BY g."repoId", cgr.revision, cgr."activatedAt" DESC, g."updatedAt" DESC, g.id DESC
)
SELECT r.id, r.name, h."externalId", h.label, h.relation, h."nodeExternalIds", h."sourceFile", h."startLine", h."endLine", h.evidence, h.confidence, h."confidenceTier"::text, ag.revision, ag."commitHash", ag.id
FROM "CodeGraphSemanticHyperedge" h
JOIN active_graphs ag ON ag.id = h."graphIndexId"
JOIN "Repo" r ON r.id = h."repoId" AND r."orgId" = h."orgId"
WHERE h."orgId" = $1
  AND EXISTS (SELECT 1 FROM unnest($2::text[]) pat WHERE h."externalId" ILIKE pat OR h.label ILIKE pat OR h.relation ILIKE pat OR h."sourceFile" ILIKE pat OR COALESCE(h.evidence, '') ILIKE pat)
ORDER BY r.name ASC, h.confidence DESC, h."sourceFile" ASC
LIMIT $3`
	rows, err := q.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic hyperedges: %w", err)
	}
	defer rows.Close()
	out := make([]CodeGraphSemanticHyperedgeEvidence, 0, limit)
	for rows.Next() {
		var row CodeGraphSemanticHyperedgeEvidence
		if err := rows.Scan(&row.RepoID, &row.RepoName, &row.ExternalID, &row.Label, &row.Relation, &row.NodeExternalIDs, &row.SourceFile, &row.StartLine, &row.EndLine, &row.Evidence, &row.Confidence, &row.ConfidenceTier, &row.Revision, &row.CommitHash, &row.GraphIndexID); err != nil {
			return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic hyperedges scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: InspectOrgCodeGraph semantic hyperedges rows: %w", err)
	}
	return out, nil
}

func activeIndexFilters(args *[]any, repos, revisions []string, scopes []RepoRevisionScope, repoAlias string) string {
	if filter := repoRevisionScopeFilter(args, scopes, repoAlias, `ci.revision`); filter != "" {
		return filter
	}
	var b strings.Builder
	if len(repos) > 0 {
		*args = append(*args, repos)
		b.WriteString(fmt.Sprintf(` AND %s.name = ANY($%d::text[])`, repoAlias, len(*args)))
	}
	if len(revisions) > 0 {
		*args = append(*args, cleanStringList(revisions))
		b.WriteString(fmt.Sprintf(` AND ci.revision = ANY($%d::text[])`, len(*args)))
	}
	return b.String()
}

func activeGraphFilters(args *[]any, repos, revisions []string, scopes []RepoRevisionScope, repoAlias string) string {
	if filter := repoRevisionScopeFilter(args, scopes, repoAlias, `cgr.revision`); filter != "" {
		return filter
	}
	var b strings.Builder
	if len(repos) > 0 {
		*args = append(*args, repos)
		b.WriteString(fmt.Sprintf(` AND %s.name = ANY($%d::text[])`, repoAlias, len(*args)))
	}
	if len(revisions) > 0 {
		*args = append(*args, cleanStringList(revisions))
		b.WriteString(fmt.Sprintf(` AND cgr.revision = ANY($%d::text[])`, len(*args)))
	}
	return b.String()
}

func repoRevisionScopeFilter(args *[]any, scopes []RepoRevisionScope, repoAlias, revisionExpr string) string {
	if len(scopes) == 0 {
		return ""
	}
	clauses := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		repo := strings.TrimSpace(scope.Repo)
		if repo == "" {
			continue
		}
		revisions := cleanStringList(scope.RevisionCandidates)
		*args = append(*args, repo)
		repoArg := len(*args)
		if len(revisions) == 0 {
			clauses = append(clauses, fmt.Sprintf(`%s.name = $%d`, repoAlias, repoArg))
			continue
		}
		*args = append(*args, revisions)
		revisionArg := len(*args)
		clauses = append(clauses, fmt.Sprintf(`(%s.name = $%d AND %s = ANY($%d::text[]))`, repoAlias, repoArg, revisionExpr, revisionArg))
	}
	if len(clauses) == 0 {
		return ""
	}
	return ` AND (` + strings.Join(clauses, ` OR `) + `)`
}

func normalizeCodeIntelLimit(limit int32) int32 {
	if limit <= 0 {
		return defaultCodeIntelQueryLimit
	}
	if limit > maxCodeIntelQueryLimit {
		return maxCodeIntelQueryLimit
	}
	return limit
}

func minCodeIntelInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func scipSymbolSuffixPatterns(symbol string) []string {
	symbol = strings.TrimSpace(symbol)
	suffixes := []string{
		"/" + symbol + "().",
		"#" + symbol + "().",
		"." + symbol + "().",
		symbol + "().",
		"/" + symbol + ".",
		"#" + symbol + ".",
		"." + symbol + ".",
		symbol + ".",
		"/" + symbol + "#",
		"#" + symbol + "#",
		"." + symbol + "#",
		symbol + "#",
	}
	out := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		out = append(out, "%"+escapeLikePattern(suffix))
	}
	return out
}

func tokenizeCodeIntelQuery(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '.' || r == '$' || r == '@' || r == ':' || r == '/' || r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	})
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "from": true, "with": true, "this": true,
		"that": true, "how": true, "what": true, "where": true, "when": true, "which": true,
		"code": true, "flow": true, "path": true, "repo": true, "service": true,
	}
	seen := map[string]bool{}
	out := make([]string, 0, 12)
	for _, field := range fields {
		term := strings.TrimSpace(field)
		if len(term) < 3 || stop[strings.ToLower(term)] || seen[strings.ToLower(term)] {
			continue
		}
		seen[strings.ToLower(term)] = true
		out = append(out, term)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func compactCodeGraphTerms(terms []string) []string {
	stop := map[string]bool{
		"architecture": true, "architectural": true, "repository": true, "repositories": true,
		"indexed": true, "relevant": true, "include": true, "exact": true, "files": true,
		"symbols": true, "runtime": true, "runtimes": true, "relates": true, "explain": true,
		"across": true, "auto-instrumentation": true,
		"opentelemetry": true,
	}
	specific := map[string]bool{}
	for _, term := range terms {
		lower := strings.ToLower(strings.TrimSpace(term))
		switch lower {
		case "injectnodejs", "injectpython", "injectdotnet", "injectcommonsdkconfig", "injectcommonenvvar":
			specific[lower] = true
		}
	}
	hasSpecificInject := len(specific) > 0
	preferred := []string{
		"injectnodejs", "injectpython", "injectdotnet", "injectcommonsdkconfig", "injectcommonenvvar",
		"sdkinjector", "nodejs", "python", "dotnet", "env", "var",
		"container", "volume", "mount", "webhook", "mutat", "annotation",
	}
	seen := map[string]bool{}
	out := make([]string, 0, 6)
	add := func(term string) {
		key := strings.ToLower(strings.TrimSpace(term))
		if key == "" || stop[key] || seen[key] || compactCodeGraphIsBroadHubTerm(key, hasSpecificInject) {
			return
		}
		seen[key] = true
		out = append(out, term)
	}
	for _, term := range terms {
		lower := strings.ToLower(term)
		for _, marker := range preferred {
			if strings.Contains(lower, marker) {
				add(term)
				break
			}
		}
		if len(out) >= 6 {
			return out
		}
	}
	for _, term := range terms {
		add(term)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

func compactCodeGraphIsBroadHubTerm(term string, hasSpecificInject bool) bool {
	if hasSpecificInject {
		switch term {
		case "inject", "sdk", "instrumentation", "instrumentationspec":
			return true
		}
	}
	return false
}

func likePatterns(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		out = append(out, "%"+escapeLikePattern(term)+"%")
	}
	return out
}

func escapeLikePattern(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

func cleanStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}
