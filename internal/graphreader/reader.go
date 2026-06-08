package graphreader

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	nebula "github.com/vesoft-inc/nebula-go/v3"
)

const (
	nodeTag  = "code_graph_node"
	edgeType = "code_graph_edge"
)

type Executor interface {
	Execute(ctx context.Context, stmt string) (*nebula.ResultSet, error)
}

type Inspector interface {
	Inspect(ctx context.Context, params InspectParams) (InspectResult, error)
}

type InspectParams struct {
	OrgID         int32
	Query         string
	WorkspaceIDs  []string
	Seeds         []Seed
	AllowedScopes []ActiveScope
	Limit         int32
	MaxDepth      int
	Strict        bool
	MaxSeedTokens int
	SeedRowLimit  int32
	SeedVIDLimit  int32
	TraversalRows int32
}

type Seed struct {
	WorkspaceID string
	NodeVID     string
}

type ActiveScope struct {
	WorkspaceID    string `json:"workspaceId"`
	RepoID         int32  `json:"repoId"`
	Revision       string `json:"revision"`
	CommitHash     string `json:"commitHash"`
	SchemaVersion  int32  `json:"schemaVersion"`
	BuilderVersion string `json:"builderVersion"`
}

type QueryPlan struct {
	Strategy  string   `json:"strategy"`
	Intent    string   `json:"intent"`
	Direction string   `json:"direction"`
	MaxDepth  int      `json:"maxDepth"`
	EdgeKinds []string `json:"edgeKinds,omitempty"`
	// Q.B — context filter inferred from the user's NL query
	// via inferContextFilters. When non-empty the traversal
	// statement restricts edges to those whose `context`
	// column matches one of these values. This is the
	// "graphify" pattern: a small enum of edge contexts plus a
	// LUT that maps NL hints (e.g. "what calls X" → ["call"])
	// makes BFS retrieval drastically less noisy because the
	// hop universe shrinks to relationships the user actually
	// asked about.
	ContextFilters []string `json:"contextFilters,omitempty"`
}

type traversalBudget struct {
	MaxSeedTokens int
	SeedRows      int
	SeedVIDs      int
	TraversalRows int
}

type Endpoint struct {
	VID    string `json:"vid,omitempty"`
	RepoID *int32 `json:"repoId,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Key    string `json:"key,omitempty"`
	Label  string `json:"label,omitempty"`
	Path   string `json:"path,omitempty"`
}

type Edge struct {
	EdgeSourceVID  string   `json:"edgeSourceVid"`
	EdgeTargetVID  string   `json:"edgeTargetVid"`
	Depth          int      `json:"depth"`
	Relation       string   `json:"relation"`
	Confidence     *float64 `json:"confidence,omitempty"`
	ConfidenceTier string   `json:"confidenceTier,omitempty"`
	Source         string   `json:"source,omitempty"`
	// Q.A — producer class. "scip" outranks "heuristic" in
	// edge ranking; retrieval can suppress heuristic-only
	// edges when an explicit precision threshold is set.
	Provenance string `json:"provenance,omitempty"`
	// Q.B — syntactic context. Filters traversal universe to
	// relationships the user actually asked about.
	Context          string   `json:"context,omitempty"`
	EvidenceFilePath string   `json:"evidenceFilePath,omitempty"`
	StartLine        *int32   `json:"startLine,omitempty"`
	EndLine          *int32   `json:"endLine,omitempty"`
	EdgeRepoID       *int32   `json:"edgeRepoId,omitempty"`
	EdgeRevision     string   `json:"edgeRevision,omitempty"`
	EdgeCommitHash   string   `json:"edgeCommitHash,omitempty"`
	Start            Endpoint `json:"start"`
	Neighbor         Endpoint `json:"neighbor"`
}

type InspectResult struct {
	Plan     QueryPlan `json:"plan"`
	Edges    []Edge    `json:"edges"`
	Warnings []string  `json:"warnings,omitempty"`
}

type Reader struct {
	executor Executor
	logger   *slog.Logger
}

func New(executor Executor, logger *slog.Logger) *Reader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reader{executor: executor, logger: logger.With("component", "graphreader")}
}

func (r *Reader) Inspect(ctx context.Context, params InspectParams) (InspectResult, error) {
	plan := BuildQueryPlan(params.Query)
	if params.MaxDepth > 0 {
		plan.MaxDepth = clamp(params.MaxDepth, 1, 6)
	}
	out := InspectResult{Plan: plan, Warnings: make([]string, 0)}
	if r == nil || r.executor == nil {
		if params.Strict {
			return InspectResult{}, fmt.Errorf("NebulaGraph traversal skipped because the graph reader is not configured")
		}
		out.Warnings = append(out.Warnings, "NebulaGraph traversal skipped because the graph reader is not configured.")
		return out, nil
	}
	if params.OrgID <= 0 {
		return InspectResult{}, fmt.Errorf("graphreader: org id is required")
	}
	limit := int(params.Limit)
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	budget := traversalBudgetFromInspectParams(params, limit)

	seedGroups := groupSeeds(params.Seeds)
	if len(seedGroups) == 0 {
		tokens := searchableTokens(params.Query, budget.MaxSeedTokens)
		if len(tokens) == 0 {
			if params.Strict {
				return InspectResult{}, fmt.Errorf("NebulaGraph seed lookup skipped because the query had no searchable graph tokens")
			}
			out.Warnings = append(out.Warnings, "NebulaGraph seed lookup skipped because the query had no searchable graph tokens.")
			return out, nil
		}
		for _, workspaceID := range cleanUnique(params.WorkspaceIDs) {
			stmt := renderSeedLookupStatement(params.OrgID, workspaceID, tokens, seedKindsForPlan(plan), params.AllowedScopes, budget.SeedRows)
			rs, err := r.executor.Execute(ctx, stmt)
			if err != nil {
				r.logger.Warn("seed lookup failed", "workspaceId", workspaceID, "err", err)
				if params.Strict {
					return InspectResult{}, fmt.Errorf("NebulaGraph seed lookup failed for workspace %s: %w", workspaceID, err)
				}
				out.Warnings = append(out.Warnings, fmt.Sprintf("NebulaGraph seed lookup failed for workspace %s; continuing with persisted evidence. %s", workspaceID, trimMessage(err.Error(), 240)))
				continue
			}
			for _, row := range parseSeedLookupResult(rs, budget.SeedRows) {
				if row.NodeVID != "" {
					seedGroups[row.WorkspaceID] = append(seedGroups[row.WorkspaceID], row.NodeVID)
				}
			}
		}
	}
	if len(seedGroups) == 0 {
		if params.Strict {
			return InspectResult{}, fmt.Errorf("NebulaGraph traversal had no tenant-scoped seed vertices")
		}
		out.Warnings = append(out.Warnings, "NebulaGraph traversal had no tenant-scoped seed vertices.")
		return out, nil
	}

	seenEdges := map[string]bool{}
	for _, workspaceID := range sortedSeedWorkspaceIDs(seedGroups) {
		seeds := seedGroups[workspaceID]
		current := uniqueStrings(seeds)
		visited := map[string]bool{}
		for _, seed := range current {
			visited[seed] = true
		}
		for depth := 1; depth <= plan.MaxDepth && len(current) > 0; depth++ {
			stmt := renderTraversalStepStatement(params.OrgID, workspaceID, current, limit, plan, params.AllowedScopes, depth, budget)
			rs, err := r.executor.Execute(ctx, stmt)
			if err != nil {
				r.logger.Warn("traversal step failed", "workspaceId", workspaceID, "depth", depth, "err", err)
				if params.Strict {
					return InspectResult{}, fmt.Errorf("NebulaGraph traversal failed for workspace %s depth %d: %w", workspaceID, depth, err)
				}
				out.Warnings = append(out.Warnings, fmt.Sprintf("NebulaGraph traversal failed for workspace %s depth %d; continuing with persisted evidence. %s", workspaceID, depth, trimMessage(err.Error(), 240)))
				break
			}
			edges := parseTraversalResult(rs, budget.TraversalRows)
			next := make([]string, 0)
			for _, edge := range edges {
				if !edgeBelongsToOrg(edge, params.OrgID) {
					out.Warnings = append(out.Warnings, "NebulaGraph returned an edge outside the requested org scope; it was discarded.")
					continue
				}
				if !edgeMatchesAllowedScopes(edge, params.AllowedScopes) {
					out.Warnings = append(out.Warnings, "NebulaGraph returned an edge outside the requested repo/revision scope; it was discarded.")
					continue
				}
				if isSCIPLocalEndpoint(edge.Start) || isSCIPLocalEndpoint(edge.Neighbor) {
					continue
				}
				if strings.EqualFold(edge.Relation, "REFERENCES") && (isExternalDependencyGraphEndpoint(edge.Start) || isExternalDependencyGraphEndpoint(edge.Neighbor)) {
					continue
				}
				key := edge.EdgeSourceVID + "\x00" + edge.EdgeTargetVID + "\x00" + edge.Relation + "\x00" + strconv.Itoa(edge.Depth)
				if seenEdges[key] {
					continue
				}
				seenEdges[key] = true
				out.Edges = append(out.Edges, edge)
				for _, candidate := range []string{edge.Neighbor.VID, edge.EdgeTargetVID, edge.EdgeSourceVID} {
					if candidate == "" || visited[candidate] {
						continue
					}
					visited[candidate] = true
					next = append(next, candidate)
				}
			}
			current = uniqueStrings(next)
			if len(out.Edges) >= limit {
				break
			}
		}
		if len(out.Edges) >= limit {
			break
		}
	}
	// Q.B — hub-aware post-filter. The BFS expands through
	// god-vertices like `Logger`/`String`/utility classes that
	// dominate any unconstrained subgraph; without this pass,
	// every traversal of any depth collapses through those
	// hubs and the answer becomes "utility X uses 200 other
	// utility Ys". The filter identifies p99-degree vertices
	// in the RESULT SET (floored at 50) and drops edges that
	// transit OUT OF a hub at depth > 1. Hubs themselves still
	// appear as terminals — the user can see "this calls
	// Logger.log" — but the BFS does not waste depth budget
	// listing everything Logger.log itself touches.
	out.Edges = applyHubAwareFilter(out.Edges, hubDegreeFloor)
	terms := searchableTokens(params.Query, 12)
	rankEdges(out.Edges, terms)
	out.Edges = diversifyEdgesByRequestedLanguages(out.Edges, requestedLanguageGraphTerms(terms))
	if len(out.Edges) > limit {
		out.Edges = out.Edges[:limit]
	}
	if len(out.Edges) == 0 {
		if params.Strict {
			return InspectResult{}, fmt.Errorf("NebulaGraph traversal returned no native edges for the matched graph seeds")
		}
		out.Warnings = append(out.Warnings, "NebulaGraph traversal returned no native edges for the matched graph seeds.")
	}
	return out, nil
}

// hubDegreeFloor is the minimum degree at which a result-set
// vertex is treated as a hub even when the dataset's p99 of
// degree is lower. Mirrors graphify's `max(50, p99)` rule.
const hubDegreeFloor = 50

// applyHubAwareFilter implements the graphify hub-aware BFS
// rule at the result-set level: identify high-degree VIDs
// (degree > p99 of result-set degree distribution, floored at
// `floorDegree`), then drop edges where the hub is the SOURCE
// at depth > 1. Hubs remain visible as terminals at any depth;
// only "transit out of hub" hops are suppressed.
//
// Why this matters (graphify architecture rules echoed): an
// unrestricted BFS through a code-graph collapses through
// utility vertices like loggers, error types, and stdlib
// helpers — every traversal balloons into the union of every
// neighbour those hubs reach. The LLM consumer drowns in the
// noise. Bounding the transit set to non-hubs is the single
// biggest precision lift in NL-driven graph retrieval.
//
// Tiny result sets (< 4 edges) skip filtering — there's no
// meaningful distribution to compute p99 of, and any hub at
// that scale is intentional (the user explicitly asked about
// it).
func applyHubAwareFilter(edges []Edge, floorDegree int) []Edge {
	if len(edges) < 4 {
		return edges
	}
	degree := make(map[string]int, len(edges)*2)
	for _, e := range edges {
		degree[e.EdgeSourceVID]++
		degree[e.EdgeTargetVID]++
	}
	threshold := computeHubThreshold(degree, floorDegree)
	if threshold <= 0 {
		return edges
	}
	hubs := make(map[string]bool, len(degree))
	for vid, d := range degree {
		if d >= threshold {
			hubs[vid] = true
		}
	}
	if len(hubs) == 0 {
		return edges
	}
	out := make([]Edge, 0, len(edges))
	for _, e := range edges {
		// "Visit-only-as-terminal" rule: hubs surviving as
		// targets of depth-1 edges are kept; subsequent
		// depth-N edges whose source is a hub are dropped so
		// the hub doesn't transit the traversal forward.
		if e.Depth > 1 && hubs[e.EdgeSourceVID] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// computeHubThreshold returns the degree at or above which a
// vertex is classified a hub. Floor at `floorDegree` so tiny
// result sets (where p99 might be 1 or 2) don't accidentally
// label every neighbour as a hub.
func computeHubThreshold(degree map[string]int, floorDegree int) int {
	if len(degree) == 0 {
		return floorDegree
	}
	counts := make([]int, 0, len(degree))
	for _, d := range degree {
		counts = append(counts, d)
	}
	sort.Ints(counts)
	p99Idx := len(counts) * 99 / 100
	if p99Idx >= len(counts) {
		p99Idx = len(counts) - 1
	}
	threshold := counts[p99Idx]
	if threshold < floorDegree {
		threshold = floorDegree
	}
	return threshold
}

func BuildQueryPlan(query string) QueryPlan {
	lower := strings.ToLower(query)
	plan := QueryPlan{
		Strategy:  "bounded-nebula-bfs",
		Intent:    "default",
		Direction: "bidirectional",
		MaxDepth:  3,
	}
	switch {
	case containsAny(lower, "import", "imports", "imported by"):
		plan.Intent = "imports"
		plan.Direction = "bidirectional"
		plan.EdgeKinds = []string{"IMPORTS", "IMPORTS_FROM"}
	case containsAny(lower, "caller", "who calls", "references to"):
		plan.Intent = "callers"
		plan.Direction = "incoming"
		plan.EdgeKinds = []string{"CALLS", "REFERENCES", "USES"}
	case containsAny(lower, "callee", "calls from", "what does", "depends on"):
		plan.Intent = "callees"
		plan.Direction = "outgoing"
		plan.EdgeKinds = []string{"CALLS", "DEPENDS_ON", "CONTAINS"}
	case containsAny(lower, "impact", "blast radius", "affected", "risk"):
		plan.Intent = "impact"
		plan.Direction = "bidirectional"
		plan.MaxDepth = 4
		plan.EdgeKinds = []string{"CALLS", "DEPENDS_ON", "REFERENCES", "IMPORTS", "IMPORTS_FROM", "DEFINES", "EXTENDS", "IMPLEMENTS", "TYPE_DEFINES", "HANDLES", "EMITS"}
	case containsAny(lower, "path", "sequence", "chain") || (strings.Contains(lower, "from ") && strings.Contains(lower, " to ")):
		plan.Intent = "path"
		plan.Direction = "bidirectional"
		plan.MaxDepth = 5
		plan.EdgeKinds = []string{"CALLS", "DEPENDS_ON", "REFERENCES", "IMPORTS", "IMPORTS_FROM", "CONTAINS", "DEFINES", "EXTENDS", "IMPLEMENTS", "TYPE_DEFINES", "PROVIDES", "CONSUMES", "HANDLES", "EMITS", "ANCHOR_LINK"}
	case containsAny(lower, "architecture", "flow", "lifecycle", "data flow", "cross-repo", "route", "event"):
		plan.Intent = "architecture"
		plan.Direction = "bidirectional"
		plan.MaxDepth = 4
		plan.EdgeKinds = []string{"CALLS", "DEPENDS_ON", "REFERENCES", "IMPORTS", "IMPORTS_FROM", "CONTAINS", "DEFINES", "EXTENDS", "IMPLEMENTS", "TYPE_DEFINES", "PROVIDES", "CONSUMES", "HANDLES", "EMITS", "ANCHOR_LINK"}
	}
	// Q.B — infer edge `context` filter from the NL query
	// independent of the intent classifier above. The two
	// run separately so a query like "what are the impacts of
	// changing X's call signature" still infers ["call",
	// "type"] context filters even though intent="impact"
	// keeps the broader EdgeKinds list. The two filters AND
	// together in the traversal NGQL.
	plan.ContextFilters = inferContextFilters(lower)
	return plan
}

func traversalBudgetFromInspectParams(params InspectParams, limit int) traversalBudget {
	budget := traversalBudget{
		MaxSeedTokens: 8,
		SeedRows:      clamp(limit, 1, 200),
		SeedVIDs:      clamp(limit*4, 50, 200),
		TraversalRows: clamp(limit*6, 100, 500),
	}
	if params.MaxSeedTokens > 0 {
		budget.MaxSeedTokens = clamp(params.MaxSeedTokens, 1, 16)
	}
	if params.SeedRowLimit > 0 {
		budget.SeedRows = clamp(int(params.SeedRowLimit), 1, 200)
	}
	if params.SeedVIDLimit > 0 {
		budget.SeedVIDs = clamp(int(params.SeedVIDLimit), 1, 200)
	}
	if params.TraversalRows > 0 {
		budget.TraversalRows = clamp(int(params.TraversalRows), 1, 500)
	}
	return budget
}

func renderTraversalStepStatement(orgID int32, workspaceID string, seeds []string, limit int, plan QueryPlan, allowedScopes []ActiveScope, depth int, budget traversalBudget) string {
	if budget.SeedVIDs <= 0 || budget.TraversalRows <= 0 {
		budget = traversalBudgetFromInspectParams(InspectParams{}, limit)
	}
	vids := renderSeedVids(seeds, budget.SeedVIDs)
	boundedLimit := budget.TraversalRows
	clauses := []string{
		"GO 1 STEP FROM " + strings.Join(vids, ", "),
		"OVER " + quoteIdentifier(edgeType) + renderDirectionClause(plan.Direction),
		fmt.Sprintf("WHERE properties(edge).orgId == %d", orgID),
		"AND properties(edge).workspaceId == " + ngqlValue(workspaceID),
	}
	if filter := renderEdgeKindFilter(plan.EdgeKinds); filter != "" {
		clauses = append(clauses, "AND "+filter)
	}
	if filter := renderContextFilter(plan.ContextFilters); filter != "" {
		clauses = append(clauses, "AND "+filter)
	}
	if filter := renderScopeFilter("properties(edge)", allowedScopes); filter != "" {
		clauses = append(clauses, "AND "+filter)
	}
	clauses = append(clauses,
		"YIELD DISTINCT",
		strings.Join([]string{
			"src(edge) AS edgeSourceVid",
			"dst(edge) AS edgeTargetVid",
			fmt.Sprintf("%d AS depth", clamp(depth, 1, 10)),
			edgeProperty("kind", "relation"),
			edgeProperty("confidence", "confidence"),
			edgeProperty("confidenceTier", "confidenceTier"),
			edgeProperty("source", "source"),
			// Q.A — provenance + Q.B — context yielded so the
			// post-processor can rank by tier and the caller
			// can surface them in the MCP / chat response.
			edgeProperty("provenance", "provenance"),
			edgeProperty("context", "context"),
			edgeProperty("evidenceFilePath", "evidenceFilePath"),
			edgeProperty("startLine", "startLine"),
			edgeProperty("endLine", "endLine"),
			edgeProperty("repoId", "edgeRepoId"),
			edgeProperty("revision", "edgeRevision"),
			edgeProperty("commitHash", "edgeCommitHash"),
			"id($^) AS startVid",
			vertexProperty("$^", "repoId", "startRepoId"),
			vertexProperty("$^", "kind", "startKind"),
			vertexProperty("$^", "key", "startKey"),
			vertexProperty("$^", "label", "startLabel"),
			vertexProperty("$^", "path", "startPath"),
			"id($$) AS neighborVid",
			vertexProperty("$$", "repoId", "neighborRepoId"),
			vertexProperty("$$", "kind", "neighborKind"),
			vertexProperty("$$", "key", "neighborKey"),
			vertexProperty("$$", "label", "neighborLabel"),
			vertexProperty("$$", "path", "neighborPath"),
		}, ", "),
		fmt.Sprintf("| LIMIT %d;", boundedLimit),
	)
	return strings.Join(clauses, " ")
}

func renderSeedLookupStatement(orgID int32, workspaceID string, tokens, kinds []string, allowedScopes []ActiveScope, limit int) string {
	tagProp := func(prop string) string {
		return quoteIdentifier(nodeTag) + "." + quoteIdentifier(prop)
	}
	tokenConditions := make([]string, 0, len(tokens)*8)
	for _, token := range uniqueStrings(tokens) {
		literal := ngqlValue(token)
		tokenConditions = append(tokenConditions,
			tagProp("key")+" == "+literal,
			tagProp("label")+" == "+literal,
			tagProp("path")+" == "+literal,
			tagProp("routePath")+" == "+literal,
			tagProp("key")+" STARTS WITH "+literal,
			tagProp("label")+" STARTS WITH "+literal,
			tagProp("path")+" STARTS WITH "+literal,
			tagProp("routePath")+" STARTS WITH "+literal,
		)
	}
	kindFilter := ""
	if len(kinds) > 0 {
		values := make([]string, 0, len(kinds))
		for _, kind := range uniqueStrings(kinds) {
			values = append(values, ngqlValue(kind))
		}
		kindFilter = " AND " + tagProp("kind") + " IN [" + strings.Join(values, ", ") + "]"
	}
	scopeFilter := ""
	if filter := renderScopeFilter(quoteIdentifier(nodeTag), allowedScopes); filter != "" {
		scopeFilter = " AND " + filter
	}
	return strings.Join([]string{
		"LOOKUP ON " + quoteIdentifier(nodeTag),
		fmt.Sprintf("WHERE %s == %d", tagProp("orgId"), orgID),
		"AND " + tagProp("workspaceId") + " == " + ngqlValue(workspaceID),
		"AND (" + strings.Join(tokenConditions, " OR ") + ")" + kindFilter + scopeFilter,
		"YIELD",
		strings.Join([]string{
			"id(vertex) AS " + quoteIdentifier("vid"),
			tagProp("kind") + " AS " + quoteIdentifier("kind"),
			tagProp("label") + " AS " + quoteIdentifier("label"),
			tagProp("key") + " AS " + quoteIdentifier("key"),
			tagProp("path") + " AS " + quoteIdentifier("path"),
			tagProp("repoId") + " AS " + quoteIdentifier("repoId"),
			tagProp("workspaceId") + " AS " + quoteIdentifier("workspaceId"),
		}, ", "),
		fmt.Sprintf("| LIMIT %d;", clamp(limit, 1, 200)),
	}, " ")
}

func parseTraversalResult(rs *nebula.ResultSet, limit int) []Edge {
	if rs == nil {
		return nil
	}
	maxRows := rs.GetRowSize()
	if maxRows > limit {
		maxRows = limit
	}
	out := make([]Edge, 0, maxRows)
	for i := 0; i < maxRows; i++ {
		record, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			continue
		}
		source := stringColumn(record, "edgeSourceVid")
		target := stringColumn(record, "edgeTargetVid")
		if source == "" || target == "" {
			continue
		}
		// Nebula's BIDIRECTLY traversal can yield vertex property
		// aliases ($^/$$) in traversal direction, while src(edge) /
		// dst(edge) remain the stored edge direction. Align the
		// endpoint payload with the stored direction before ranking
		// or formatting, otherwise callers see reversed CALLS edges.
		rawStart := Endpoint{
			VID:    stringColumn(record, "startVid"),
			RepoID: int32Column(record, "startRepoId"),
			Kind:   stringColumn(record, "startKind"),
			Key:    stringColumn(record, "startKey"),
			Label:  stringColumn(record, "startLabel"),
			Path:   stringColumn(record, "startPath"),
		}
		rawNeighbor := Endpoint{
			VID:    stringColumn(record, "neighborVid"),
			RepoID: int32Column(record, "neighborRepoId"),
			Kind:   stringColumn(record, "neighborKind"),
			Key:    stringColumn(record, "neighborKey"),
			Label:  stringColumn(record, "neighborLabel"),
			Path:   stringColumn(record, "neighborPath"),
		}
		if rawStart.VID == "" {
			rawStart.VID = source
		}
		if rawNeighbor.VID == "" {
			rawNeighbor.VID = target
		}
		start, neighbor := alignTraversalEndpoints(source, target, rawStart, rawNeighbor)
		edge := Edge{
			EdgeSourceVID:  source,
			EdgeTargetVID:  target,
			Depth:          intColumn(record, "depth", 1),
			Relation:       stringColumnDefault(record, "relation", "RELATES_TO"),
			Confidence:     floatColumn(record, "confidence"),
			ConfidenceTier: stringColumn(record, "confidenceTier"),
			Source:         stringColumn(record, "source"),
			// Q.A/Q.B — yielded by the new NGQL projection so
			// retrieval can rank by tier + filter by context.
			Provenance:       stringColumn(record, "provenance"),
			Context:          stringColumn(record, "context"),
			EvidenceFilePath: stringColumn(record, "evidenceFilePath"),
			StartLine:        int32Column(record, "startLine"),
			EndLine:          int32Column(record, "endLine"),
			EdgeRepoID:       int32Column(record, "edgeRepoId"),
			EdgeRevision:     stringColumn(record, "edgeRevision"),
			EdgeCommitHash:   stringColumn(record, "edgeCommitHash"),
			Start:            start,
			Neighbor:         neighbor,
		}
		out = append(out, edge)
	}
	return out
}

func alignTraversalEndpoints(sourceVID, targetVID string, start, neighbor Endpoint) (Endpoint, Endpoint) {
	if start.VID == sourceVID && neighbor.VID == targetVID {
		return start, neighbor
	}
	if start.VID == targetVID && neighbor.VID == sourceVID {
		return neighbor, start
	}
	if start.VID == sourceVID {
		neighbor.VID = targetVID
		return start, neighbor
	}
	if neighbor.VID == targetVID {
		start.VID = sourceVID
		return start, neighbor
	}
	start.VID = sourceVID
	neighbor.VID = targetVID
	return start, neighbor
}

func parseSeedLookupResult(rs *nebula.ResultSet, limit int) []Seed {
	if rs == nil {
		return nil
	}
	maxRows := rs.GetRowSize()
	if maxRows > limit {
		maxRows = limit
	}
	out := make([]Seed, 0, maxRows)
	for i := 0; i < maxRows; i++ {
		record, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			continue
		}
		vid := stringColumn(record, "vid")
		workspaceID := stringColumn(record, "workspaceId")
		if vid == "" || workspaceID == "" {
			continue
		}
		out = append(out, Seed{WorkspaceID: workspaceID, NodeVID: vid})
	}
	return out
}

func groupSeeds(seeds []Seed) map[string][]string {
	out := map[string][]string{}
	for _, seed := range seeds {
		if seed.WorkspaceID == "" || seed.NodeVID == "" {
			continue
		}
		out[seed.WorkspaceID] = append(out[seed.WorkspaceID], seed.NodeVID)
	}
	return out
}

func sortedSeedWorkspaceIDs(seedGroups map[string][]string) []string {
	out := make([]string, 0, len(seedGroups))
	for workspaceID := range seedGroups {
		out = append(out, workspaceID)
	}
	sort.Strings(out)
	return out
}

func rankEdges(edges []Edge, terms []string) {
	sort.SliceStable(edges, func(i, j int) bool {
		left := edgeScore(edges[i], terms)
		right := edgeScore(edges[j], terms)
		if left != right {
			return left > right
		}
		return edgeStableSortKey(edges[i]) < edgeStableSortKey(edges[j])
	})
}

func edgeStableSortKey(edge Edge) string {
	repoID := ""
	if edge.EdgeRepoID != nil {
		repoID = strconv.Itoa(int(*edge.EdgeRepoID))
	}
	startLine := ""
	if edge.StartLine != nil {
		startLine = strconv.Itoa(int(*edge.StartLine))
	}
	return strings.Join([]string{
		repoID,
		edge.EdgeRevision,
		edge.EdgeCommitHash,
		fmt.Sprintf("%02d", edge.Depth),
		strings.ToUpper(edge.Relation),
		edge.Source,
		edge.Provenance,
		edge.Context,
		edge.EvidenceFilePath,
		startLine,
		edge.EdgeSourceVID,
		edge.EdgeTargetVID,
		edge.Start.Kind,
		edge.Start.Label,
		edge.Start.Path,
		edge.Neighbor.Kind,
		edge.Neighbor.Label,
		edge.Neighbor.Path,
	}, "\x00")
}

func edgeScore(edge Edge, terms []string) int {
	score := 0
	relation := strings.ToUpper(edge.Relation)
	switch relation {
	case "HANDLES", "EMITS", "PROVIDES", "CONSUMES", "ANCHOR_LINK":
		score += 35
	case "CALLS":
		score += 50
	case "DEPENDS_ON":
		score += 25
	case "REFERENCES":
		score += 18
	case "IMPORTS", "IMPORTS_FROM":
		score += 6
	case "IMPLEMENTS", "TYPE_DEFINES", "EXTENDS":
		score += 22
	case "CONTAINS", "DEFINES":
		score += 10
	default:
		score += 5
	}
	switch strings.ToLower(firstNonEmpty(edge.Provenance, edge.Source)) {
	case "scip":
		score += 35
	case "tree-sitter", "tree-sitter-go", "tree-sitter-typescript", "tree-sitter-python", "tree-sitter-java", "tree-sitter-rust":
		score += 18
	case "heuristic":
		score -= 8
	}
	if edge.Start.RepoID != nil && edge.Neighbor.RepoID != nil && *edge.Start.RepoID != *edge.Neighbor.RepoID {
		score += 25
	}
	matchedEndpoint := false
	languageBoostTerms := requestedLanguageGraphTerms(terms)
	score += edgeRequestedLanguageScore(edge, languageBoostTerms)
	for _, endpoint := range []Endpoint{edge.Start, edge.Neighbor} {
		kind := strings.ToLower(endpoint.Kind)
		if kind == "route" || kind == "event" || kind == "service" || kind == "job" || kind == "function" || kind == "method" || kind == "class" {
			score += 18
		} else if kind == "package" || kind == "import" || kind == "parameter" {
			score -= 10
		}
		if isLowSignalGraphEndpoint(endpoint) {
			score -= 28
		}
		if isExternalDependencyGraphEndpoint(endpoint) {
			score -= 35
		}
		if endpoint.Kind == "symbol" && endpoint.Path == "" {
			score -= 14
		}
		text := strings.ToLower(endpoint.Kind + " " + endpoint.Key + " " + endpoint.Label + " " + endpoint.Path)
		if languageBoostTerms["node"] && (strings.Contains(text, "nodejs") || strings.Contains(text, "node.js")) {
			score += 30
			matchedEndpoint = true
		}
		if languageBoostTerms["python"] && strings.Contains(text, "python") {
			score += 30
			matchedEndpoint = true
		}
		if languageBoostTerms["dotnet"] && (strings.Contains(text, "dotnet") || strings.Contains(text, ".net")) {
			score += 30
			matchedEndpoint = true
		}
		if !languageBoostTerms["go"] && len(languageBoostTerms) > 0 && (strings.Contains(text, "injectgo") || strings.Contains(text, "golang")) {
			score -= 24
		}
		for _, term := range terms {
			if strings.Contains(text, strings.ToLower(term)) {
				score += 8
				matchedEndpoint = true
			}
		}
	}
	if edge.EvidenceFilePath != "" {
		pathText := strings.ToLower(edge.EvidenceFilePath)
		for _, term := range terms {
			if strings.Contains(pathText, strings.ToLower(term)) {
				score += 10
				matchedEndpoint = true
			}
		}
		if isLowValueGraphPath(pathText) {
			score -= 20
		}
	}
	if (relation == "IMPORTS" || relation == "IMPORTS_FROM") && !matchedEndpoint {
		score -= 18
	}
	if edge.Confidence != nil {
		score += int(*edge.Confidence * 10)
	}
	return score
}

func edgeRequestedLanguageScore(edge Edge, requested map[string]bool) int {
	if len(requested) == 0 {
		return 0
	}
	score := 0
	bucket := edgeLanguageBucket(edge)
	switch {
	case bucket != "" && requested[bucket]:
		score += 42
	case bucket != "" && !requested[bucket]:
		score -= 48
	}
	for _, topic := range edgeInstrumentationTopicBuckets(edge) {
		if requested[topic] {
			score += 26
			continue
		}
		if isLanguageOrRuntimeTopic(topic) {
			score -= 42
		}
	}
	if edgeContainsCoreFlowAnchor(edge) {
		score += 24
	}
	return score
}

func requestedLanguageGraphTerms(terms []string) map[string]bool {
	out := map[string]bool{}
	for _, term := range terms {
		lower := strings.ToLower(term)
		switch {
		case strings.Contains(lower, "node") || lower == "js" || strings.Contains(lower, "javascript"):
			out["node"] = true
		case strings.Contains(lower, "python"):
			out["python"] = true
		case strings.Contains(lower, "dotnet") || lower == ".net" || lower == "net":
			out["dotnet"] = true
		case lower == "go" || strings.Contains(lower, "golang"):
			out["go"] = true
		}
	}
	return out
}

func diversifyEdgesByRequestedLanguages(edges []Edge, requested map[string]bool) []Edge {
	if len(edges) < 2 || len(requested) == 0 {
		return edges
	}
	order := []string{"node", "python", "dotnet", "go"}
	groups := map[string][]Edge{}
	general := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		lang := edgeLanguageBucket(edge)
		if requested[lang] {
			groups[lang] = append(groups[lang], edge)
			continue
		}
		general = append(general, edge)
	}
	if len(groups) <= 1 {
		return edges
	}
	out := make([]Edge, 0, len(edges))
	for {
		progress := false
		for _, lang := range order {
			queue := groups[lang]
			if len(queue) == 0 {
				continue
			}
			out = append(out, queue[0])
			groups[lang] = queue[1:]
			progress = true
		}
		if !progress {
			break
		}
	}
	out = append(out, general...)
	return out
}

func edgeLanguageBucket(edge Edge) string {
	text := strings.ToLower(strings.Join([]string{
		edge.EvidenceFilePath,
		edge.Start.Kind,
		edge.Start.Key,
		edge.Start.Label,
		edge.Start.Path,
		edge.Neighbor.Kind,
		edge.Neighbor.Key,
		edge.Neighbor.Label,
		edge.Neighbor.Path,
	}, " "))
	switch {
	case strings.Contains(text, "nodejs") || strings.Contains(text, "node.js") || strings.Contains(text, "javascript"):
		return "node"
	case strings.Contains(text, "python"):
		return "python"
	case strings.Contains(text, "dotnet") || strings.Contains(text, ".net"):
		return "dotnet"
	case strings.Contains(text, "golang") || strings.Contains(text, "/go/") || strings.Contains(text, " go "):
		return "go"
	case strings.Contains(text, "apache") || strings.Contains(text, "httpd"):
		return "apache"
	case strings.Contains(text, "nginx"):
		return "nginx"
	case strings.Contains(text, "java") && !strings.Contains(text, "javascript"):
		return "java"
	case strings.Contains(text, "ruby"):
		return "ruby"
	case strings.Contains(text, "php"):
		return "php"
	default:
		return ""
	}
}

func edgeInstrumentationTopicBuckets(edge Edge) []string {
	text := edgeSearchText(edge)
	topics := []string{}
	add := func(topic string) {
		for _, existing := range topics {
			if existing == topic {
				return
			}
		}
		topics = append(topics, topic)
	}
	markers := []struct {
		topic   string
		markers []string
	}{
		{"node", []string{"nodejs", "node.js", "javascript", "injectnode"}},
		{"python", []string{"python", "injectpython"}},
		{"dotnet", []string{"dotnet", ".net", "injectdotnet"}},
		{"go", []string{"golang", "injectgo", "/go/"}},
		{"apache", []string{"apache", "httpd", "injectapache"}},
		{"nginx", []string{"nginx", "injectnginx"}},
		{"java", []string{" java", "java/", "injectjava"}},
		{"ruby", []string{"ruby", "injectruby"}},
		{"php", []string{"php", "injectphp"}},
	}
	for _, candidate := range markers {
		for _, marker := range candidate.markers {
			if strings.Contains(text, marker) {
				add(candidate.topic)
				break
			}
		}
	}
	return topics
}

func isLanguageOrRuntimeTopic(topic string) bool {
	switch topic {
	case "node", "python", "dotnet", "go", "apache", "nginx", "java", "ruby", "php":
		return true
	default:
		return false
	}
}

func edgeContainsCoreFlowAnchor(edge Edge) bool {
	text := edgeSearchText(edge)
	for _, marker := range []string{
		"webhook", "mutate", "podmutator", "sdkinjector", "instrumentationspec",
		"injectcommon", "languageinstrumentations", "annotationvalue",
		"modifiedpod", "initcontainer", "volume mount", "envotel",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func edgeSearchText(edge Edge) string {
	return strings.ToLower(strings.Join([]string{
		edge.Relation,
		edge.Source,
		edge.Provenance,
		edge.Context,
		edge.EvidenceFilePath,
		edge.Start.Kind,
		edge.Start.Key,
		edge.Start.Label,
		edge.Start.Path,
		edge.Neighbor.Kind,
		edge.Neighbor.Key,
		edge.Neighbor.Label,
		edge.Neighbor.Path,
	}, " "))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isLowValueGraphPath(path string) bool {
	for _, marker := range []string{"/vendor/", "/node_modules/", "/dist/", "/build/", "/target/", "/generated/", "/.git/", "/test/", "/tests/", "_test.go", "/hack/"} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return false
}

func isLowSignalGraphEndpoint(endpoint Endpoint) bool {
	text := strings.ToLower(endpoint.Key + " " + endpoint.Label)
	for _, marker := range []string{
		"assert.", "require.", "fmt.", "errors.", "log.", "logger.", "t.run",
		"err.error", "string(", "strconv.", "slices.", "append(", "len(",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	if isSCIPLocalSymbol(text) {
		return true
	}
	return false
}

func isSCIPLocalEndpoint(endpoint Endpoint) bool {
	return isSCIPLocalSymbol(endpoint.Key) || isSCIPLocalSymbol(endpoint.Label)
}

func isExternalDependencyGraphEndpoint(endpoint Endpoint) bool {
	text := strings.ToLower(endpoint.Key + " " + endpoint.Label)
	if endpoint.Path != "" {
		return false
	}
	for _, marker := range []string{" npm ", " pypi ", " maven ", " nuget ", " cargo "} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	for _, marker := range []string{"k8s.io/", "github.com/go-", "google.golang.org/", "sigs.k8s.io/"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isSCIPLocalSymbol(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return false
	}
	fields := strings.Fields(text)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "local" && isAllDigits(strings.Trim(fields[i+1], ".#`()[]{}")) {
			return true
		}
	}
	return false
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func edgeBelongsToOrg(edge Edge, orgID int32) bool {
	prefix := fmt.Sprintf("cg:o%d:", orgID)
	return strings.HasPrefix(edge.EdgeSourceVID, prefix) && strings.HasPrefix(edge.EdgeTargetVID, prefix)
}

func edgeMatchesAllowedScopes(edge Edge, scopes []ActiveScope) bool {
	if len(scopes) == 0 {
		return true
	}
	allowedRepos := map[int32]bool{}
	for _, scope := range scopes {
		if scope.RepoID > 0 {
			allowedRepos[scope.RepoID] = true
		}
	}
	for _, scope := range scopes {
		if edge.EdgeRepoID == nil || scope.RepoID != *edge.EdgeRepoID {
			continue
		}
		if scope.Revision != "" && edge.EdgeRevision != scope.Revision {
			continue
		}
		if scope.CommitHash != "" && edge.EdgeCommitHash != scope.CommitHash {
			continue
		}
		if edge.Start.RepoID == nil || !allowedRepos[*edge.Start.RepoID] {
			return false
		}
		if edge.Neighbor.RepoID == nil || !allowedRepos[*edge.Neighbor.RepoID] {
			return false
		}
		return true
	}
	return false
}

func searchableTokens(query string, max int) []string {
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "from": true, "with": true, "this": true,
		"that": true, "how": true, "what": true, "where": true, "when": true, "which": true,
		"code": true, "flow": true, "path": true, "repo": true, "service": true,
		"explain": true, "show": true, "important": true, "workloads": true, "pieces": true,
		"auto-instrumentation": true, "opentelemetry": true, "operator": true, "otel": true,
	}
	seen := map[string]bool{}
	lowerQuery := strings.ToLower(query)
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '.' || r == '$' || r == '@' || r == ':' || r == '/' || r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	})
	out := make([]string, 0, max)
	for _, term := range graphDomainExpansionTokens(lowerQuery) {
		lower := strings.ToLower(term)
		if len(out) >= max {
			break
		}
		if stop[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, term)
	}
	for _, field := range fields {
		term := strings.TrimSpace(field)
		lower := strings.ToLower(term)
		if len(term) < 3 || stop[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, term)
		if len(out) >= max {
			break
		}
	}
	return out
}

func graphDomainExpansionTokens(lowerQuery string) []string {
	out := make([]string, 0, 12)
	runtimes := requestedRuntimeFamilies(lowerQuery)
	if strings.Contains(lowerQuery, "instrument") || strings.Contains(lowerQuery, "workload") || strings.Contains(lowerQuery, "otel") || strings.Contains(lowerQuery, "opentelemetry") {
		out = append(out, "inject")
		if len(runtimes) == 0 {
			out = append(out, "injectNodeJS", "injectPython", "injectDotNet")
		}
	}
	if strings.Contains(lowerQuery, "node") || strings.Contains(lowerQuery, "javascript") || strings.Contains(lowerQuery, "js") {
		out = append(out, "NodeJS", "injectNodeJS")
	}
	if strings.Contains(lowerQuery, "python") {
		out = append(out, "Python", "injectPython")
	}
	if strings.Contains(lowerQuery, ".net") || strings.Contains(lowerQuery, "dotnet") || strings.Contains(lowerQuery, "c#") {
		out = append(out, "DotNet", "injectDotNet")
	}
	if strings.Contains(lowerQuery, "instrument") || strings.Contains(lowerQuery, "workload") || strings.Contains(lowerQuery, "otel") || strings.Contains(lowerQuery, "opentelemetry") {
		out = append(out, "sdkInjector", "InstrumentationSpec", "instrumentation")
	}
	if strings.Contains(lowerQuery, "webhook") || strings.Contains(lowerQuery, "admission") || strings.Contains(lowerQuery, "mutat") {
		out = append(out, "webhook", "admission", "mutate")
	}
	if strings.Contains(lowerQuery, "controller") || strings.Contains(lowerQuery, "reconcile") {
		out = append(out, "controller", "Reconcile")
	}
	return out
}

func requestedRuntimeFamilies(lowerQuery string) map[string]bool {
	out := map[string]bool{}
	if strings.Contains(lowerQuery, "node") || strings.Contains(lowerQuery, "javascript") || strings.Contains(lowerQuery, "js") {
		out["node"] = true
	}
	if strings.Contains(lowerQuery, "python") {
		out["python"] = true
	}
	if strings.Contains(lowerQuery, ".net") || strings.Contains(lowerQuery, "dotnet") || strings.Contains(lowerQuery, "c#") {
		out["dotnet"] = true
	}
	if lowerQuery == "go" || strings.Contains(lowerQuery, " golang") || strings.Contains(lowerQuery, " go ") {
		out["go"] = true
	}
	return out
}

func seedKindsForPlan(plan QueryPlan) []string {
	if plan.Intent == "architecture" || plan.Intent == "impact" {
		return []string{"route", "event", "package", "service", "job", "symbol", "function", "class", "file"}
	}
	return []string{"symbol", "function", "class", "method", "file", "package"}
}

func renderSeedVids(seeds []string, limit int) []string {
	seeds = uniqueStrings(seeds)
	if len(seeds) > limit {
		seeds = seeds[:limit]
	}
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		out = append(out, ngqlValue(seed))
	}
	return out
}

func renderDirectionClause(direction string) string {
	switch direction {
	case "incoming":
		return " REVERSELY"
	case "outgoing":
		return ""
	default:
		return " BIDIRECT"
	}
}

func renderEdgeKindFilter(kinds []string) string {
	kinds = uniqueStrings(kinds)
	if len(kinds) == 0 {
		return ""
	}
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		values = append(values, ngqlValue(kind))
	}
	return "properties(edge).kind IN [" + strings.Join(values, ", ") + "]"
}

// renderContextFilter is the Q.B WHERE-clause builder that
// restricts traversal to edges whose `context` matches one of
// the inferred-from-NL values. Returns "" when ContextFilters
// is empty so the traversal stays unrestricted (current
// behaviour for un-recognised queries).
//
// **NULL handling**: this clause does NOT include NULL edges
// because Nebula `IN` semantics drop NULL on the left side.
// That is the correct behaviour: an edge whose context is
// NULL has no canonical relationship label, and including it
// in a context-restricted traversal would re-introduce the
// "answer drowns in noise" problem this filter exists to
// solve. The caller can pass ContextFilters=nil to opt out
// of restriction entirely.
func renderContextFilter(contexts []string) string {
	contexts = uniqueStrings(contexts)
	if len(contexts) == 0 {
		return ""
	}
	values := make([]string, 0, len(contexts))
	for _, c := range contexts {
		values = append(values, ngqlValue(c))
	}
	return "properties(edge).context IN [" + strings.Join(values, ", ") + "]"
}

func renderScopeFilter(propertySource string, scopes []ActiveScope) string {
	if len(scopes) == 0 {
		return ""
	}
	seen := map[string]bool{}
	clauses := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope.RepoID <= 0 {
			continue
		}
		parts := []string{scopedProperty(propertySource, "repoId") + " == " + strconv.FormatInt(int64(scope.RepoID), 10)}
		if scope.WorkspaceID != "" {
			parts = append(parts, scopedProperty(propertySource, "workspaceId")+" == "+ngqlValue(scope.WorkspaceID))
		}
		if scope.Revision != "" {
			parts = append(parts, scopedProperty(propertySource, "revision")+" == "+ngqlValue(scope.Revision))
		}
		if scope.CommitHash != "" {
			parts = append(parts, scopedProperty(propertySource, "commitHash")+" == "+ngqlValue(scope.CommitHash))
		}
		if scope.SchemaVersion > 0 {
			parts = append(parts, scopedProperty(propertySource, "schemaVersion")+" == "+strconv.FormatInt(int64(scope.SchemaVersion), 10))
		}
		if scope.BuilderVersion != "" {
			parts = append(parts, scopedProperty(propertySource, "builderVersion")+" == "+ngqlValue(scope.BuilderVersion))
		}
		clause := "(" + strings.Join(parts, " AND ") + ")"
		if seen[clause] {
			continue
		}
		seen[clause] = true
		clauses = append(clauses, clause)
	}
	if len(clauses) == 0 {
		return ""
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

func scopedProperty(source, property string) string {
	if strings.HasPrefix(source, "properties(") {
		return source + "." + quoteIdentifier(property)
	}
	return source + "." + quoteIdentifier(property)
}

func edgeProperty(property, alias string) string {
	return "properties(edge)." + quoteIdentifier(property) + " AS " + quoteIdentifier(alias)
}

func vertexProperty(vertexRef, property, alias string) string {
	return "properties(" + vertexRef + ")." + quoteIdentifier(property) + " AS " + quoteIdentifier(alias)
}

func quoteIdentifier(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}

func ngqlValue(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return "NULL"
	}
	return string(body)
}

func stringColumn(record *nebula.Record, column string) string {
	value, err := record.GetValueByColName(column)
	if err != nil || value == nil || value.IsNull() || value.IsEmpty() {
		return ""
	}
	if value.IsString() {
		s, err := value.AsString()
		if err == nil && strings.TrimSpace(s) != "" && strings.TrimSpace(s) != "__NULL__" {
			return strings.TrimSpace(s)
		}
	}
	if value.IsInt() {
		i, err := value.AsInt()
		if err == nil {
			return strconv.FormatInt(i, 10)
		}
	}
	return ""
}

func stringColumnDefault(record *nebula.Record, column, fallback string) string {
	if value := stringColumn(record, column); value != "" {
		return value
	}
	return fallback
}

func intColumn(record *nebula.Record, column string, fallback int) int {
	value, err := record.GetValueByColName(column)
	if err != nil || value == nil || value.IsNull() || value.IsEmpty() {
		return fallback
	}
	if value.IsInt() {
		i, err := value.AsInt()
		if err == nil {
			return int(i)
		}
	}
	if value.IsFloat() {
		f, err := value.AsFloat()
		if err == nil {
			return int(f)
		}
	}
	return fallback
}

func int32Column(record *nebula.Record, column string) *int32 {
	value := intColumn(record, column, -1)
	if value < 0 {
		return nil
	}
	out := int32(value)
	return &out
}

func floatColumn(record *nebula.Record, column string) *float64 {
	value, err := record.GetValueByColName(column)
	if err != nil || value == nil || value.IsNull() || value.IsEmpty() {
		return nil
	}
	if value.IsFloat() {
		f, err := value.AsFloat()
		if err == nil {
			return &f
		}
	}
	if value.IsInt() {
		i, err := value.AsInt()
		if err == nil {
			f := float64(i)
			return &f
		}
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cleanUnique(values []string) []string {
	return uniqueStrings(values)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func trimMessage(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
