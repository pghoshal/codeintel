package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"codeintel/internal/api"
	"codeintel/internal/db"
	"codeintel/internal/graphreader"
)

func (b *Backend) toolFindSymbolDefinitions(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFindSymbolOccurrences(ctx, req, raw, db.SymbolOccurrenceDefinitions)
}

func (b *Backend) toolFindSymbolReferences(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFindSymbolOccurrences(ctx, req, raw, db.SymbolOccurrenceReferences)
}

func (b *Backend) toolFindSymbolOccurrences(ctx context.Context, req api.MCPRequest, raw json.RawMessage, mode db.SymbolOccurrenceMode) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	if b.cfg.SearchBackend == nil {
		return toolResult{}, errors.New("symbol tools require Zoekt search backend for fused SCIP + lexical recall")
	}
	var args struct {
		Symbol         string   `json:"symbol"`
		Repo           string   `json:"repo"`
		Repos          []string `json:"repos"`
		Revision       string   `json:"revision"`
		DefinitionFile string   `json:"definitionFile"`
		Limit          *int32   `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid symbol tool arguments")
	}
	args.Symbol = strings.TrimSpace(args.Symbol)
	if args.Symbol == "" {
		if mode == db.SymbolOccurrenceDefinitions {
			return toolResult{}, fmt.Errorf("find_symbol_definitions requires symbol")
		}
		return toolResult{}, fmt.Errorf("find_symbol_references requires symbol")
	}
	if args.DefinitionFile != "" {
		clean, err := cleanRepoPath(args.DefinitionFile)
		if err != nil {
			return toolResult{}, err
		}
		args.DefinitionFile = clean
	}
	limit := int32(25)
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > 100 {
			return toolResult{}, fmt.Errorf("limit must be a positive integer no greater than 100")
		}
		limit = *args.Limit
	}
	repoNames, revisionCandidates, scopedRevisions, displayRef, err := b.resolveSemanticScopes(ctx, req.OrgID, args.Repo, args.Repos, args.Revision)
	if err != nil {
		return toolResult{}, err
	}
	precise, err := b.cfg.Queries.FindOrgSymbolOccurrences(ctx, db.FindOrgSymbolOccurrencesParams{
		OrgID:              req.OrgID,
		Symbol:             args.Symbol,
		Repos:              repoNames,
		RevisionCandidates: revisionCandidates,
		RepoRevisionScopes: scopedRevisions,
		DefinitionFile:     args.DefinitionFile,
		Mode:               mode,
		Limit:              limit,
	})
	if err != nil {
		return toolResult{}, err
	}

	singular, plural := "definition", "definitions"
	toolName := "find_symbol_definitions"
	if mode == db.SymbolOccurrenceReferences {
		singular, plural = "reference", "references"
		toolName = "find_symbol_references"
	}

	var supplemental string
	grepArgs := map[string]any{"pattern": args.Symbol, "limit": int64(limit)}
	if args.Repo != "" {
		grepArgs["repo"] = args.Repo
		if args.Revision != "" {
			grepArgs["ref"] = args.Revision
		}
	} else if len(args.Repos) > 0 {
		grepArgs["repos"] = cleanStrings(args.Repos)
		if args.Revision != "" {
			grepArgs["ref"] = args.Revision
		}
	}
	if body, marshalErr := json.Marshal(grepArgs); marshalErr == nil {
		grepResult, grepErr := b.toolGrep(ctx, req, body)
		if grepErr != nil {
			return toolResult{}, fmt.Errorf("symbol tools require Zoekt supplemental recall: %w", grepErr)
		}
		if grepResult.IsError {
			return toolResult{}, errors.New("symbol tools require Zoekt supplemental recall")
		}
		supplemental = toolResultText(grepResult)
	}

	output := formatSymbolEvidenceOutput(args.Symbol, singular, plural, precise, supplemental)
	meta := map[string]any{
		"symbol":          args.Symbol,
		"repos":           repoNames,
		"revision":        displayRef,
		"definitionFile":  args.DefinitionFile,
		"preciseMatches":  len(precise),
		"supplementalRaw": supplemental != "",
		"mode":            string(mode),
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: output}},
		Meta: map[string]any{
			"tool":     toolName,
			"metadata": meta,
			"matches":  precise,
		},
	}, nil
}

func (b *Backend) toolInspectCodeGraph(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	if b.cfg.GraphReader == nil {
		return toolResult{}, errors.New("NebulaGraph reader is required for code graph MCP tools")
	}
	var args struct {
		Query   string   `json:"query"`
		Repo    string   `json:"repo"`
		Repos   []string `json:"repos"`
		Ref     string   `json:"ref"`
		Depth   *int32   `json:"depth"`
		Limit   *int32   `json:"limit"`
		Compact bool     `json:"compact"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid inspect_code_graph arguments")
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return toolResult{}, fmt.Errorf("inspect_code_graph query is required")
	}
	limit := int32(25)
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > 100 {
			return toolResult{}, fmt.Errorf("limit must be a positive integer no greater than 100")
		}
		limit = *args.Limit
	}
	maxDepth := 0
	if args.Depth != nil {
		if *args.Depth <= 0 || *args.Depth > 6 {
			return toolResult{}, fmt.Errorf("depth must be a positive integer no greater than 6")
		}
		maxDepth = int(*args.Depth)
	}
	repos, revisions, scopedRevisions, err := b.resolveGraphScope(ctx, req.OrgID, args.Repo, args.Repos, args.Ref)
	if err != nil {
		return toolResult{}, err
	}
	inspectParams := graphreader.InspectParams{
		OrgID:    req.OrgID,
		Query:    args.Query,
		Limit:    limit,
		MaxDepth: maxDepth,
		Strict:   true,
	}
	if args.Compact {
		inspectParams.MaxSeedTokens = 5
		inspectParams.SeedRowLimit = 8
		inspectParams.SeedVIDLimit = 24
		inspectParams.TraversalRows = 48
	}
	cacheKey := ""
	if args.Compact && b.cfg.GraphEvidenceCache != nil {
		activeScopes, scopeErr := b.cfg.Queries.ListActiveCodeGraphScopes(ctx, db.ListActiveCodeGraphScopesParams{
			OrgID:              req.OrgID,
			Repos:              repos,
			RevisionCandidates: revisions,
			RepoRevisionScopes: scopedRevisions,
		})
		if scopeErr != nil {
			b.logger.Warn("graph evidence cache preflight failed", "err", scopeErr.Error())
		} else {
			key, keyErr := graphInspectionCacheKey(graphInspectionCacheKeyParams{
				OrgID:         req.OrgID,
				Query:         args.Query,
				Repos:         repos,
				Revisions:     revisions,
				Scopes:        scopedRevisions,
				ActiveScopes:  activeScopes,
				Limit:         limit,
				MaxDepth:      maxDepth,
				Compact:       args.Compact,
				MaxSeedTokens: inspectParams.MaxSeedTokens,
				SeedRowLimit:  inspectParams.SeedRowLimit,
				SeedVIDLimit:  inspectParams.SeedVIDLimit,
				TraversalRows: inspectParams.TraversalRows,
			})
			if keyErr != nil {
				b.logger.Warn("graph evidence cache key failed", "err", keyErr.Error())
			} else {
				cacheKey = key
				if cached, ok, getErr := b.cfg.GraphEvidenceCache.GetGraphInspection(ctx, cacheKey); getErr != nil {
					b.logger.Warn("graph evidence cache read failed", "err", getErr.Error())
				} else if ok {
					return graphInspectionToolResult(cached), nil
				}
			}
		}
	}
	evidence, err := b.cfg.Queries.InspectOrgCodeGraph(ctx, db.InspectOrgCodeGraphParams{
		OrgID:              req.OrgID,
		Query:              args.Query,
		Repos:              repos,
		RevisionCandidates: revisions,
		RepoRevisionScopes: scopedRevisions,
		Limit:              limit,
		Compact:            args.Compact,
	})
	if err != nil {
		return toolResult{}, err
	}

	seeds := graphSeedsFromEvidence(evidence)
	if args.Compact {
		seeds = compactGraphTraversalSeeds(seeds, 4, 8)
	}
	inspectParams.WorkspaceIDs = evidence.WorkspaceIDs
	inspectParams.Seeds = seeds
	inspectParams.AllowedScopes = graphAllowedScopes(evidence.ActiveScopes)
	graphCtx := ctx
	cancelGraph := func() {}
	if args.Compact {
		if timeout := b.compactGraphTimeout(); timeout > 0 {
			graphCtx, cancelGraph = context.WithTimeout(ctx, timeout)
		}
	}
	defer cancelGraph()
	graphResult, err := b.cfg.GraphReader.Inspect(graphCtx, inspectParams)
	if err != nil {
		if args.Compact && (errors.Is(err, context.DeadlineExceeded) || graphCtx.Err() != nil) {
			graphResult = graphreader.InspectResult{
				Plan: graphreader.QueryPlan{
					Strategy: "compact-nebula-timeout",
					Intent:   "architecture",
				},
			}
			output := strings.TrimSpace(formatGraphInspectionOutput(evidence, graphResult))
			output += "\n\nNebulaGraph traversal timeout: returning Postgres semantic graph evidence only. Native traversal can be retried with a narrower graph query or served from cache after warmup."
			if cacheKey != "" && b.cfg.GraphEvidenceCache != nil {
				if setErr := b.cfg.GraphEvidenceCache.SetGraphInspection(ctx, cacheKey, cachedGraphInspection{
					Output:      output,
					Evidence:    evidence,
					GraphResult: graphResult,
				}, b.graphEvidenceCacheTTL()); setErr != nil {
					b.logger.Warn("graph evidence cache write failed", "err", setErr.Error())
				}
			}
			return toolResult{
				Content: []toolContent{{Type: "text", Text: output}},
				Meta: map[string]any{
					"mode":        "active-code-graph",
					"evidence":    evidence,
					"graphReader": graphResult,
				},
			}, nil
		}
		if args.Compact && codeGraphInspectionHasPostgresEvidence(evidence) {
			graphResult = graphreader.InspectResult{
				Plan: graphreader.BuildQueryPlan(args.Query),
				Warnings: []string{
					"NebulaGraph native traversal returned no edges for the selected seeds; returning scoped Postgres SCIP/AST/tree-sitter graph evidence instead of failing the fused response.",
				},
			}
			if maxDepth > 0 {
				graphResult.Plan.MaxDepth = maxDepth
			}
			output := strings.TrimSpace(formatGraphInspectionOutput(evidence, graphResult))
			output += "\n\nNebulaGraph traversal degraded: returning Postgres semantic graph evidence only. Native traversal should be repaired or warmed, but the MCP response remains useful and tenant-scoped."
			if cacheKey != "" && b.cfg.GraphEvidenceCache != nil {
				if setErr := b.cfg.GraphEvidenceCache.SetGraphInspection(ctx, cacheKey, cachedGraphInspection{
					Output:      output,
					Evidence:    evidence,
					GraphResult: graphResult,
				}, b.graphEvidenceCacheTTL()); setErr != nil {
					b.logger.Warn("graph evidence cache write failed", "err", setErr.Error())
				}
			}
			return toolResult{
				Content: []toolContent{{Type: "text", Text: output}},
				Meta: map[string]any{
					"mode":        "active-code-graph",
					"evidence":    evidence,
					"graphReader": graphResult,
				},
			}, nil
		}
		return toolResult{}, fmt.Errorf("NebulaGraph traversal failed: %s", trimForTool(err.Error(), 240))
	}

	output := formatGraphInspectionOutput(evidence, graphResult)
	if cacheKey != "" && b.cfg.GraphEvidenceCache != nil {
		if setErr := b.cfg.GraphEvidenceCache.SetGraphInspection(ctx, cacheKey, cachedGraphInspection{
			Output:      output,
			Evidence:    evidence,
			GraphResult: graphResult,
		}, b.graphEvidenceCacheTTL()); setErr != nil {
			b.logger.Warn("graph evidence cache write failed", "err", setErr.Error())
		}
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: output}},
		Meta: map[string]any{
			"mode":        "active-code-graph",
			"evidence":    evidence,
			"graphReader": graphResult,
		},
	}, nil
}

func codeGraphInspectionHasPostgresEvidence(e db.CodeGraphInspectionEvidence) bool {
	return len(e.Symbols) > 0 ||
		len(e.Relationships) > 0 ||
		len(e.Occurrences) > 0 ||
		len(e.Anchors) > 0 ||
		len(e.SemanticFacts) > 0 ||
		len(e.SemanticEdges) > 0 ||
		len(e.SemanticHyperedges) > 0
}

func compactGraphTraversalSeeds(seeds []graphreader.Seed, maxWorkspaces, maxPerWorkspace int) []graphreader.Seed {
	if maxWorkspaces <= 0 || maxPerWorkspace <= 0 || len(seeds) == 0 {
		return nil
	}
	out := make([]graphreader.Seed, 0, minInt(len(seeds), maxWorkspaces*maxPerWorkspace))
	workspaceOrder := make([]string, 0, maxWorkspaces)
	workspaceSeen := map[string]bool{}
	perWorkspace := map[string]int{}
	seedSeen := map[string]bool{}
	for _, seed := range seeds {
		if seed.WorkspaceID == "" || seed.NodeVID == "" {
			continue
		}
		if !workspaceSeen[seed.WorkspaceID] {
			if len(workspaceOrder) >= maxWorkspaces {
				continue
			}
			workspaceSeen[seed.WorkspaceID] = true
			workspaceOrder = append(workspaceOrder, seed.WorkspaceID)
		}
		if !workspaceSeen[seed.WorkspaceID] || perWorkspace[seed.WorkspaceID] >= maxPerWorkspace {
			continue
		}
		key := seed.WorkspaceID + "\x00" + seed.NodeVID
		if seedSeen[key] {
			continue
		}
		seedSeen[key] = true
		perWorkspace[seed.WorkspaceID]++
		out = append(out, seed)
	}
	return out
}

func (b *Backend) toolGraphCallers(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFocusedCodeGraph(ctx, req, raw, focusedGraphIntent{
		ToolName: "graph_callers",
		Prefix:   "callers references to ",
		Title:    "Graph callers",
	})
}

func (b *Backend) toolGraphCallees(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFocusedCodeGraph(ctx, req, raw, focusedGraphIntent{
		ToolName: "graph_callees",
		Prefix:   "callees calls from depends on ",
		Title:    "Graph callees",
	})
}

func (b *Backend) toolGraphImpact(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFocusedCodeGraph(ctx, req, raw, focusedGraphIntent{
		ToolName: "graph_impact",
		Prefix:   "impact blast radius affected risk ",
		Title:    "Graph impact",
	})
}

func (b *Backend) toolGraphPath(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFocusedCodeGraph(ctx, req, raw, focusedGraphIntent{
		ToolName:  "graph_path",
		Prefix:    "path sequence chain from ",
		Title:     "Graph path",
		Formatter: formatGraphPathOutput,
	})
}

func (b *Backend) toolGraphMinimalContext(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	return b.toolFocusedCodeGraph(ctx, req, raw, focusedGraphIntent{
		ToolName:  "graph_minimal_context",
		Prefix:    "minimal context architecture flow impact ",
		Title:     "Graph minimal context",
		Formatter: formatGraphMinimalContextOutput,
	})
}

func (b *Backend) toolGraphStatus(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	var args struct {
		Repo  string   `json:"repo"`
		Repos []string `json:"repos"`
		Ref   string   `json:"ref"`
		Limit *int32   `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid graph_status arguments")
	}
	limit := int32(25)
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > 100 {
			return toolResult{}, fmt.Errorf("limit must be a positive integer no greater than 100")
		}
		limit = *args.Limit
	}
	repos, revisions, scopedRevisions, err := b.resolveGraphScope(ctx, req.OrgID, args.Repo, args.Repos, args.Ref)
	if err != nil {
		return toolResult{}, err
	}
	evidence, err := b.cfg.Queries.InspectOrgCodeGraph(ctx, db.InspectOrgCodeGraphParams{
		OrgID:              req.OrgID,
		Query:              "",
		Repos:              repos,
		RevisionCandidates: revisions,
		RepoRevisionScopes: scopedRevisions,
		Limit:              limit,
	})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: formatGraphStatusOutput(evidence)}},
		Meta: map[string]any{
			"tool":     "graph_status",
			"evidence": evidence,
		},
	}, nil
}

type focusedGraphIntent struct {
	ToolName  string
	Prefix    string
	Title     string
	Formatter func(string, db.CodeGraphInspectionEvidence, graphreader.InspectResult) string
}

func (b *Backend) toolFocusedCodeGraph(ctx context.Context, req api.MCPRequest, raw json.RawMessage, intent focusedGraphIntent) (toolResult, error) {
	var args struct {
		Query  string   `json:"query"`
		Symbol string   `json:"symbol"`
		Seed   string   `json:"seed"`
		Repo   string   `json:"repo"`
		Repos  []string `json:"repos"`
		Ref    string   `json:"ref"`
		Depth  *int32   `json:"depth"`
		Limit  *int32   `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid %s arguments", intent.ToolName)
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		args.Query = strings.TrimSpace(args.Symbol)
	}
	if args.Query == "" {
		args.Query = strings.TrimSpace(args.Seed)
	}
	if args.Query == "" {
		return toolResult{}, fmt.Errorf("%s requires query", intent.ToolName)
	}
	type inspectArgs struct {
		Query   string   `json:"query"`
		Repo    string   `json:"repo"`
		Repos   []string `json:"repos"`
		Ref     string   `json:"ref"`
		Depth   *int32   `json:"depth"`
		Limit   *int32   `json:"limit"`
		Compact bool     `json:"compact"`
	}
	rewritten := inspectArgs{
		Query:   intent.Prefix + args.Query,
		Repo:    args.Repo,
		Repos:   args.Repos,
		Ref:     args.Ref,
		Depth:   args.Depth,
		Limit:   args.Limit,
		Compact: true,
	}
	body, err := json.Marshal(rewritten)
	if err != nil {
		return toolResult{}, err
	}
	result, err := b.toolInspectCodeGraph(ctx, req, body)
	if err != nil {
		return toolResult{}, err
	}
	text := toolResultText(result)
	if intent.Formatter != nil {
		evidence, _ := result.Meta["evidence"].(db.CodeGraphInspectionEvidence)
		native, _ := result.Meta["graphReader"].(graphreader.InspectResult)
		text = intent.Formatter(args.Query, evidence, native)
	}
	result.Content = []toolContent{{Type: "text", Text: fmt.Sprintf("%s for %q\n\n%s", intent.Title, args.Query, text)}}
	if result.Meta == nil {
		result.Meta = map[string]any{}
	}
	result.Meta["tool"] = intent.ToolName
	result.Meta["originalQuery"] = args.Query
	result.Meta["graphQuery"] = rewritten.Query
	return result, nil
}

func (b *Backend) resolveSemanticScopes(ctx context.Context, orgID int32, repoName string, repoNames []string, requestedRef string) ([]string, []string, []db.RepoRevisionScope, string, error) {
	repos := cleanStrings(append(repoNames, repoName))
	if len(repos) > 0 {
		displayRefs := make([]string, 0, len(repos))
		scopes := make([]db.RepoRevisionScope, 0, len(repos))
		for _, repo := range repos {
			row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, orgID, repo)
			if err != nil {
				if errors.Is(err, db.ErrRepoNotFound) {
					return nil, nil, nil, "", fmt.Errorf("repository %q not found", repo)
				}
				return nil, nil, nil, "", err
			}
			resolved, err := resolveReadableGitRef(row, requestedRef)
			if err != nil {
				return nil, nil, nil, "", err
			}
			displayRefs = append(displayRefs, fmt.Sprintf("%s:%s", row.Name, resolved.DisplayRef))
			scopes = append(scopes, db.RepoRevisionScope{
				Repo:               row.Name,
				RevisionCandidates: []string{resolved.GitRef},
			})
		}
		return repos, nil, scopes, strings.Join(displayRefs, ", "), nil
	}
	if requestedRef == "" || isImplicitRef(requestedRef) {
		defaultRepos, err := b.resolveAskRepoScope(ctx, orgID, nil)
		if err != nil {
			return nil, nil, nil, "", err
		}
		if len(defaultRepos) == 0 {
			return nil, nil, nil, "active indexed revisions", nil
		}
		return b.resolveSemanticScopes(ctx, orgID, "", defaultRepos, "")
	}
	if requestedRef != "" && !isImplicitRef(requestedRef) {
		if err := validateGitRef(requestedRef); err != nil {
			return nil, nil, nil, "", err
		}
		return nil, nil, nil, "", fmt.Errorf("symbol revision requires repo or repos so indexed branch status can be verified")
	}
	return nil, nil, nil, "active indexed revisions", nil
}

func (b *Backend) resolveGraphScope(ctx context.Context, orgID int32, repoName string, repoNames []string, requestedRef string) ([]string, []string, []db.RepoRevisionScope, error) {
	repos := cleanStrings(append(repoNames, repoName))
	if requestedRef != "" && !isImplicitRef(requestedRef) {
		if err := validateGitRef(requestedRef); err != nil {
			return nil, nil, nil, err
		}
	}
	revisions := make([]string, 0)
	if len(repos) == 0 {
		if requestedRef == "" || isImplicitRef(requestedRef) {
			defaultRepos, err := b.resolveAskRepoScope(ctx, orgID, nil)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(defaultRepos) == 0 {
				return repos, nil, nil, nil
			}
			return b.resolveGraphScope(ctx, orgID, "", defaultRepos, "")
		}
		if requestedRef != "" && !isImplicitRef(requestedRef) {
			return nil, nil, nil, fmt.Errorf("graph ref requires repo or repos so indexed branch status can be verified")
		}
		return repos, cleanStrings(revisions), nil, nil
	}
	scopes := make([]db.RepoRevisionScope, 0, len(repos))
	for _, repo := range repos {
		row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, orgID, repo)
		if err != nil {
			if errors.Is(err, db.ErrRepoNotFound) {
				return nil, nil, nil, fmt.Errorf("repository %q not found", repo)
			}
			return nil, nil, nil, err
		}
		resolved, err := resolveReadableGitRef(row, requestedRef)
		if err != nil {
			return nil, nil, nil, err
		}
		scopes = append(scopes, db.RepoRevisionScope{
			Repo:               row.Name,
			RevisionCandidates: []string{resolved.GitRef},
		})
	}
	return repos, nil, scopes, nil
}

func formatSymbolEvidenceOutput(symbol, singular, plural string, precise []db.CodeIntelOccurrenceEvidence, supplemental string) string {
	scipMatchCount := len(precise)
	var lines []string
	if scipMatchCount > 0 && supplemental != "" && supplemental != "No files found" {
		lines = append(lines, fmt.Sprintf("Found %d precise SCIP %s and supplemental Zoekt text matches", scipMatchCount, plural), "")
		lines = append(lines, symbolAmbiguityWarning(symbol, precise)...)
		lines = append(lines, fmt.Sprintf("Precise SCIP %s:", plural))
		lines = append(lines, formatPreciseOccurrences(precise)...)
		lines = append(lines, "", "Supplemental Zoekt text matches (lexical; may include definitions, docs, comments, or same-name symbols):", supplemental)
		return strings.Join(lines, "\n")
	}
	if scipMatchCount > 0 {
		label := plural
		if scipMatchCount == 1 {
			label = singular
		}
		lines = append(lines, fmt.Sprintf("Found %d precise SCIP %s", scipMatchCount, label))
		lines = append(lines, symbolAmbiguityWarning(symbol, precise)...)
		lines = append(lines, formatPreciseOccurrences(precise)...)
		return strings.Join(lines, "\n")
	}
	if supplemental != "" && supplemental != "No files found" {
		return fmt.Sprintf("No precise SCIP %s found for %q.\n\nSupplemental Zoekt text matches (lexical; may include definitions, docs, comments, or same-name symbols):\n%s", plural, symbol, supplemental)
	}
	return fmt.Sprintf("No %s found", plural)
}

func symbolAmbiguityWarning(query string, rows []db.CodeIntelOccurrenceEvidence) []string {
	symbols := make(map[string]bool, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.Symbol) != "" {
			symbols[row.Symbol] = true
		}
	}
	if len(symbols) <= 1 {
		return nil
	}
	return []string{
		fmt.Sprintf("Ambiguity: query %q matched %d distinct SCIP symbols. Treat each `SCIP symbol:` block independently; do not merge references across blocks. Use `definitionFile` to disambiguate follow-up calls.", query, len(symbols)),
		"",
	}
}

func formatPreciseOccurrences(rows []db.CodeIntelOccurrenceEvidence) []string {
	type symbolKey struct {
		repo      string
		symbol    string
		display   string
		kind      string
		language  string
		signature string
	}
	type fileKey struct {
		repo string
		path string
	}
	grouped := map[symbolKey]map[fileKey][]db.CodeIntelOccurrenceEvidence{}
	symbolKeys := make([]symbolKey, 0)
	for _, row := range rows {
		symKey := symbolKey{
			repo:      row.RepoName,
			symbol:    row.Symbol,
			display:   row.DisplayName,
			kind:      valueOrEmpty(row.Kind),
			language:  valueOrEmpty(row.Language),
			signature: strings.TrimSpace(valueOrEmpty(row.Signature)),
		}
		if _, ok := grouped[symKey]; !ok {
			grouped[symKey] = map[fileKey][]db.CodeIntelOccurrenceEvidence{}
			symbolKeys = append(symbolKeys, symKey)
		}
		file := fileKey{repo: row.RepoName, path: row.FilePath}
		grouped[symKey][file] = append(grouped[symKey][file], row)
	}
	sort.Slice(symbolKeys, func(i, j int) bool {
		if symbolKeys[i].repo != symbolKeys[j].repo {
			return symbolKeys[i].repo < symbolKeys[j].repo
		}
		if symbolKeys[i].display != symbolKeys[j].display {
			return symbolKeys[i].display < symbolKeys[j].display
		}
		if symbolKeys[i].kind != symbolKeys[j].kind {
			return symbolKeys[i].kind < symbolKeys[j].kind
		}
		if symbolKeys[i].signature != symbolKeys[j].signature {
			return symbolKeys[i].signature < symbolKeys[j].signature
		}
		return symbolKeys[i].symbol < symbolKeys[j].symbol
	})
	lines := make([]string, 0, len(rows)+len(symbolKeys)*3)
	for _, symKey := range symbolKeys {
		details := make([]string, 0, 4)
		if symKey.language != "" {
			details = append(details, "language="+symKey.language)
		}
		if symKey.kind != "" {
			details = append(details, "kind="+symKey.kind)
		}
		if symKey.signature != "" {
			details = append(details, "signature="+symKey.signature)
		}
		if symKey.symbol != "" {
			details = append(details, "symbolID="+shortSymbolID(symKey.symbol))
		}
		lines = append(lines, "", fmt.Sprintf("SCIP symbol: %s", symKey.display))
		if len(details) > 0 {
			lines = append(lines, "  "+strings.Join(details, "; "))
		}
		fileKeys := make([]fileKey, 0, len(grouped[symKey]))
		for key := range grouped[symKey] {
			fileKeys = append(fileKeys, key)
		}
		sort.Slice(fileKeys, func(i, j int) bool {
			if fileKeys[i].repo == fileKeys[j].repo {
				return fileKeys[i].path < fileKeys[j].path
			}
			return fileKeys[i].repo < fileKeys[j].repo
		})
		for _, key := range fileKeys {
			lines = append(lines, fmt.Sprintf("[%s] %s:", key.repo, key.path))
			for _, row := range grouped[symKey][key] {
				lineNo := row.StartLine + 1
				content := strings.TrimSpace(valueOrEmpty(row.LineContent))
				if content == "" {
					content = row.DisplayName
				}
				lines = append(lines, fmt.Sprintf("  %d:%d %s %s", lineNo, row.StartCharacter+1, row.Role, truncateLine(content)))
			}
		}
	}
	return lines
}

func shortSymbolID(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if len(symbol) <= 96 {
		return symbol
	}
	return symbol[:96] + "..."
}

func formatGraphInspectionOutput(e db.CodeGraphInspectionEvidence, native graphreader.InspectResult) string {
	lines := []string{
		fmt.Sprintf("Active code graph evidence for %q", e.Query),
		fmt.Sprintf("Snapshots: %d; repositories searched: %d", e.ActiveSnapshotCount, e.SearchedRepoCount),
		fmt.Sprintf("Graph DB query plan: %s; intent=%s; direction=%s; maxDepth=%d%s", native.Plan.Strategy, native.Plan.Intent, native.Plan.Direction, native.Plan.MaxDepth, formatEdgeKinds(native.Plan.EdgeKinds)),
		"",
	}
	if len(e.Symbols) > 0 {
		lines = append(lines, "Symbols:")
		for _, row := range e.Symbols {
			location := ""
			if row.FilePath != nil && *row.FilePath != "" {
				location = " at " + *row.FilePath
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s (%s, %s)%s", row.RepoName, row.DisplayName, valueOrDefault(row.Kind, "symbol"), valueOrDefault(row.Language, "unknown"), location))
		}
		lines = append(lines, "")
	}
	if len(e.Relationships) > 0 {
		lines = append(lines, "SCIP relationships:")
		for _, row := range e.Relationships {
			lines = append(lines, fmt.Sprintf("- [%s] %s -> %s (%s)", row.RepoName, formatScipSymbol(row.SourceSymbol), formatScipSymbol(row.TargetSymbol), relationshipFlags(row)))
		}
		lines = append(lines, "")
	}
	if len(e.Anchors) > 0 {
		lines = append(lines, "Architecture facts (routes/events/packages/config anchors):")
		for _, row := range e.Anchors {
			location := ""
			if row.EvidenceFilePath != nil && *row.EvidenceFilePath != "" {
				location = fmt.Sprintf(" at %s%s", *row.EvidenceFilePath, optionalGraphLine(row.StartLine))
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s %s: %s%s", row.RepoName, row.Direction, row.Kind, row.Key, location))
			lines = append(lines, fmt.Sprintf("  source=%s; confidence=%.2f; normalized=%s", row.Source, row.Confidence, row.NormalizedKey))
		}
		lines = append(lines, "")
	}
	if len(native.Edges) > 0 {
		trusted, heuristic := splitGraphEdgesByEvidenceTier(e.Query, native.Edges)
		if len(trusted) > 0 {
			lines = append(lines, "Native graph traversal (ranked NebulaGraph BFS neighborhood):")
			appendGraphEdges(&lines, trusted, 0)
			lines = append(lines, "")
		}
		if len(heuristic) > 0 {
			lines = append(lines, "Heuristic AST graph traversal (low-confidence; verify against source before treating as proven):")
			appendGraphEdges(&lines, heuristic, 0)
			lines = append(lines, "")
		}
	}
	if len(e.SemanticFacts) > 0 || len(e.SemanticEdges) > 0 || len(e.SemanticHyperedges) > 0 {
		lines = append(lines, "Semantic architecture facts (evidence-bound AST/tree-sitter/semantic extraction):")
		for _, row := range e.SemanticFacts {
			lines = append(lines, fmt.Sprintf("- [%s] %s %s (%s, %.2f) at %s%s", row.RepoName, row.Kind, row.Label, row.ConfidenceTier, row.Confidence, row.SourceFile, optionalGraphLine(row.StartLine)))
			if strings.TrimSpace(row.Source) != "" {
				lines = append(lines, "  source="+strings.TrimSpace(row.Source))
			}
		}
		for _, row := range e.SemanticEdges {
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s (%s, %.2f) at %s%s", row.RepoName, row.Relation, formatSemanticEdgeDisplay(row), row.ConfidenceTier, row.Confidence, row.SourceFile, optionalGraphLine(row.StartLine)))
			if strings.TrimSpace(row.Source) != "" {
				lines = append(lines, "  source="+strings.TrimSpace(row.Source))
			}
			if row.Rationale != nil && strings.TrimSpace(*row.Rationale) != "" {
				lines = append(lines, "  rationale="+strings.TrimSpace(*row.Rationale))
			}
		}
		for _, row := range e.SemanticHyperedges {
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s (%s, %.2f) at %s%s", row.RepoName, row.Relation, row.Label, row.ConfidenceTier, row.Confidence, row.SourceFile, optionalGraphLine(row.StartLine)))
			if len(row.NodeExternalIDs) > 0 {
				lines = append(lines, "  nodes="+strings.Join(row.NodeExternalIDs, ", "))
			}
		}
		lines = append(lines, "")
	}
	if len(e.Occurrences) > 0 {
		lines = append(lines, "Occurrences:")
		for _, row := range e.Occurrences {
			lineNo := row.StartLine + 1
			lines = append(lines, fmt.Sprintf("- [%s] %s:%d %s %s", row.RepoName, row.FilePath, lineNo, row.Role, formatScipSymbol(row.Symbol)))
			if row.LineContent != nil && strings.TrimSpace(*row.LineContent) != "" {
				lines = append(lines, "  "+truncateLine(strings.TrimSpace(*row.LineContent)))
			}
		}
		lines = append(lines, "")
	}
	files := graphFilesToRead(e, native)
	if len(files) > 0 {
		lines = append(lines, "Files to read next:")
		for _, file := range files {
			lines = append(lines, "- "+file)
		}
		lines = append(lines, "")
	}
	warnings := append([]string{}, e.Warnings...)
	warnings = append(warnings, native.Warnings...)
	if len(warnings) > 0 {
		lines = append(lines, "Warnings:")
		for _, warning := range cleanStrings(warnings) {
			lines = append(lines, "- "+warning)
		}
	}
	if len(e.Symbols) == 0 && len(e.Relationships) == 0 && len(e.Anchors) == 0 && len(native.Edges) == 0 && len(e.SemanticFacts) == 0 && len(e.SemanticEdges) == 0 && len(e.SemanticHyperedges) == 0 && len(e.Occurrences) == 0 {
		lines = append(lines, "No graph evidence found.")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func formatGraphPathOutput(query string, e db.CodeGraphInspectionEvidence, native graphreader.InspectResult) string {
	lines := []string{
		fmt.Sprintf("Path-oriented graph evidence for %q", query),
		fmt.Sprintf("Snapshots: %d; repositories searched: %d", e.ActiveSnapshotCount, e.SearchedRepoCount),
		fmt.Sprintf("Graph DB query plan: %s; intent=%s; direction=%s; maxDepth=%d%s", native.Plan.Strategy, native.Plan.Intent, native.Plan.Direction, native.Plan.MaxDepth, formatEdgeKinds(native.Plan.EdgeKinds)),
		"",
	}
	if len(native.Edges) > 0 {
		trusted, heuristic := splitGraphEdgesByEvidenceTier(query, native.Edges)
		if len(trusted) > 0 {
			lines = append(lines, "Ranked implementation path / sequence edges (native NebulaGraph traversal):")
			appendGraphPathEdges(&lines, trusted, 20)
			lines = append(lines, "")
		}
		if len(heuristic) > 0 {
			lines = append(lines, "Ranked heuristic AST path edges (low-confidence; source verification required):")
			appendGraphPathEdges(&lines, heuristic, 20)
			lines = append(lines, "")
		}
	}
	if len(e.Relationships) > 0 {
		lines = append(lines, "Precise SCIP relationships related to the path:")
		for i, row := range e.Relationships {
			if i >= 12 {
				lines = append(lines, fmt.Sprintf("- ... %d additional SCIP relationships omitted", len(e.Relationships)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s -> %s (%s)", row.RepoName, formatScipSymbol(row.SourceSymbol), formatScipSymbol(row.TargetSymbol), relationshipFlags(row)))
		}
		lines = append(lines, "")
	}
	if len(e.SemanticEdges) > 0 {
		lines = append(lines, "AST/tree-sitter semantic edges related to the path:")
		for i, row := range e.SemanticEdges {
			if i >= 12 {
				lines = append(lines, fmt.Sprintf("- ... %d additional semantic edges omitted", len(e.SemanticEdges)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s at %s%s", row.RepoName, row.Relation, formatSemanticEdgeDisplay(row), row.SourceFile, optionalGraphLine(row.StartLine)))
			if strings.TrimSpace(row.Source) != "" {
				lines = append(lines, "  source="+strings.TrimSpace(row.Source))
			}
		}
		lines = append(lines, "")
	}
	files := graphFilesToRead(e, native)
	if len(files) > 0 {
		lines = append(lines, "Files to verify next:")
		for i, file := range files {
			if i >= 12 {
				lines = append(lines, fmt.Sprintf("- ... %d additional files omitted", len(files)-i))
				break
			}
			lines = append(lines, "- "+file)
		}
		lines = append(lines, "")
	}
	warnings := append([]string{}, e.Warnings...)
	warnings = append(warnings, native.Warnings...)
	if len(warnings) > 0 {
		lines = append(lines, "Warnings:")
		for _, warning := range cleanStrings(warnings) {
			lines = append(lines, "- "+warning)
		}
	}
	if len(native.Edges) == 0 && len(e.Relationships) == 0 && len(e.SemanticEdges) == 0 {
		lines = append(lines, "No path evidence found.")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func formatGraphMinimalContextOutput(query string, e db.CodeGraphInspectionEvidence, native graphreader.InspectResult) string {
	lines := []string{
		fmt.Sprintf("Minimal graph development context for %q", query),
		fmt.Sprintf("Coverage: %d active snapshots across %d repositories", e.ActiveSnapshotCount, e.SearchedRepoCount),
		fmt.Sprintf("Graph DB plan: intent=%s; direction=%s; maxDepth=%d%s", native.Plan.Intent, native.Plan.Direction, native.Plan.MaxDepth, formatEdgeKinds(native.Plan.EdgeKinds)),
		"",
	}
	if len(native.Edges) > 0 {
		trusted, heuristic := splitGraphEdgesByEvidenceTier(query, native.Edges)
		if len(trusted) > 0 {
			lines = append(lines, "High-signal connected flow (native NebulaGraph traversal edges):")
			appendGraphMinimalEdges(&lines, trusted, 10)
			lines = append(lines, "")
		}
		if len(heuristic) > 0 {
			lines = append(lines, "Heuristic connected flow (low-confidence AST edges; verify in source):")
			appendGraphMinimalEdges(&lines, heuristic, 10)
			lines = append(lines, "")
		}
	}
	if len(e.Symbols) > 0 || len(e.Occurrences) > 0 {
		lines = append(lines, "Precise symbol context:")
		for i, row := range e.Symbols {
			if i >= 8 {
				lines = append(lines, fmt.Sprintf("- ... %d additional symbol rows omitted", len(e.Symbols)-i))
				break
			}
			location := ""
			if row.FilePath != nil && *row.FilePath != "" {
				location = " at " + *row.FilePath
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s (%s/%s)%s", row.RepoName, row.DisplayName, valueOrDefault(row.Kind, "symbol"), valueOrDefault(row.Language, "unknown"), location))
		}
		for i, row := range e.Occurrences {
			if i >= 8 {
				lines = append(lines, fmt.Sprintf("- ... %d additional occurrence rows omitted", len(e.Occurrences)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s:%d %s %s", row.RepoName, row.FilePath, row.StartLine+1, row.Role, formatScipSymbol(row.Symbol)))
		}
		lines = append(lines, "")
	}
	if len(e.Anchors) > 0 {
		lines = append(lines, "Architecture anchors:")
		for i, row := range e.Anchors {
			if i >= 8 {
				lines = append(lines, fmt.Sprintf("- ... %d additional anchors omitted", len(e.Anchors)-i))
				break
			}
			location := ""
			if row.EvidenceFilePath != nil && *row.EvidenceFilePath != "" {
				location = " at " + *row.EvidenceFilePath + optionalGraphLine(row.StartLine)
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s %s %s%s", row.RepoName, row.Direction, row.Kind, row.Key, location))
			lines = append(lines, fmt.Sprintf("  source=%s; confidence=%.2f", row.Source, row.Confidence))
		}
		lines = append(lines, "")
	}
	if len(e.SemanticFacts) > 0 || len(e.SemanticEdges) > 0 {
		lines = append(lines, "AST/tree-sitter facts:")
		for i, row := range e.SemanticFacts {
			if i >= 8 {
				lines = append(lines, fmt.Sprintf("- ... %d additional semantic facts omitted", len(e.SemanticFacts)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s %s at %s%s", row.RepoName, row.Kind, row.Label, row.SourceFile, optionalGraphLine(row.StartLine)))
			if strings.TrimSpace(row.Source) != "" {
				lines = append(lines, "  source="+strings.TrimSpace(row.Source))
			}
		}
		for i, row := range e.SemanticEdges {
			if i >= 8 {
				lines = append(lines, fmt.Sprintf("- ... %d additional semantic edges omitted", len(e.SemanticEdges)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s at %s%s", row.RepoName, row.Relation, formatSemanticEdgeDisplay(row), row.SourceFile, optionalGraphLine(row.StartLine)))
			if strings.TrimSpace(row.Source) != "" {
				lines = append(lines, "  source="+strings.TrimSpace(row.Source))
			}
		}
		lines = append(lines, "")
	}
	files := graphFilesToRead(e, native)
	if len(files) > 0 {
		lines = append(lines, "Next files for the coding agent:")
		for i, file := range files {
			if i >= 10 {
				lines = append(lines, fmt.Sprintf("- ... %d additional files omitted", len(files)-i))
				break
			}
			lines = append(lines, "- "+file)
		}
		lines = append(lines, "")
	}
	warnings := append([]string{}, e.Warnings...)
	warnings = append(warnings, native.Warnings...)
	if len(warnings) > 0 {
		lines = append(lines, "Warnings:")
		for _, warning := range cleanStrings(warnings) {
			lines = append(lines, "- "+warning)
		}
	}
	if len(native.Edges) == 0 && len(e.Symbols) == 0 && len(e.Occurrences) == 0 && len(e.Anchors) == 0 && len(e.SemanticFacts) == 0 && len(e.SemanticEdges) == 0 {
		lines = append(lines, "No minimal graph context found.")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func splitGraphEdgesByEvidenceTier(query string, edges []graphreader.Edge) ([]graphreader.Edge, []graphreader.Edge) {
	trusted := make([]graphreader.Edge, 0, len(edges))
	heuristic := make([]graphreader.Edge, 0)
	for _, edge := range edges {
		if isTrustedGraphEdgeForQuery(query, edge) {
			trusted = append(trusted, edge)
		} else {
			heuristic = append(heuristic, edge)
		}
	}
	return trusted, heuristic
}

func isTrustedGraphEdgeForQuery(query string, edge graphreader.Edge) bool {
	edgeText := graphEdgeEvidenceText(edge)
	requestedBuckets := graphEvidenceRequestedLanguageBuckets(query)
	relevanceScore := graphTextRequestedLanguageScore(edgeText, requestedBuckets)
	if relevanceScore < -30 {
		return false
	}
	if len(requestedBuckets) > 0 && !graphEdgeMatchesRequestedRuntimeOrCoreFlow(edgeText, requestedBuckets) {
		return false
	}
	if strings.EqualFold(edge.Provenance, "scip") {
		return true
	}
	source := strings.ToLower(strings.TrimSpace(edge.Source))
	if strings.HasPrefix(source, "ast-") || strings.EqualFold(edge.Provenance, "heuristic") {
		return false
	}
	return edge.Confidence != nil && *edge.Confidence >= 0.8
}

func graphEdgeMatchesRequestedRuntimeOrCoreFlow(edgeText string, requested map[string]bool) bool {
	if len(requested) == 0 {
		return true
	}
	hasRequested := false
	hasUnrequestedRuntime := false
	for _, bucket := range graphEvidenceTopicBuckets(edgeText) {
		if requested[bucket] {
			hasRequested = true
			continue
		}
		if graphEvidenceIsLanguageOrRuntimeBucket(bucket) {
			hasUnrequestedRuntime = true
		}
	}
	if hasRequested && !hasUnrequestedRuntime {
		return true
	}
	if hasRequested && hasUnrequestedRuntime && !graphEvidenceContainsCoreFlowAnchor(edgeText) {
		return false
	}
	if hasRequested {
		return true
	}
	return graphEvidenceContainsCoreFlowAnchor(edgeText)
}

func appendGraphEdges(lines *[]string, edges []graphreader.Edge, limit int) {
	for i, edge := range edges {
		if limit > 0 && i >= limit {
			*lines = append(*lines, fmt.Sprintf("- ... %d additional graph edges omitted", len(edges)-i))
			break
		}
		location := ""
		if edge.EvidenceFilePath != "" {
			location = " at " + edge.EvidenceFilePath + optionalGraphLine(edge.StartLine)
		}
		*lines = append(*lines, fmt.Sprintf("- depth %d %s: %s -> %s%s", edge.Depth, edge.Relation, formatNativeEndpoint(edge.Start), formatNativeEndpoint(edge.Neighbor), location))
		if edge.Source != "" || edge.Confidence != nil || edge.Provenance != "" {
			conf := "unknown"
			if edge.Confidence != nil {
				conf = fmt.Sprintf("%.2f", *edge.Confidence)
			}
			*lines = append(*lines, fmt.Sprintf("  source=%s; provenance=%s; confidence=%s", valueOrString(edge.Source, "unknown"), valueOrString(edge.Provenance, "unknown"), conf))
		}
	}
}

func appendGraphPathEdges(lines *[]string, edges []graphreader.Edge, limit int) {
	for i, edge := range edges {
		if limit > 0 && i >= limit {
			*lines = append(*lines, fmt.Sprintf("- ... %d additional graph edges omitted by the path context budget", len(edges)-i))
			break
		}
		location := ""
		if edge.EvidenceFilePath != "" {
			location = " at " + edge.EvidenceFilePath + optionalGraphLine(edge.StartLine)
		}
		*lines = append(*lines, fmt.Sprintf("- step %d depth %d: %s --%s--> %s%s", i+1, edge.Depth, formatNativeEndpoint(edge.Start), edge.Relation, formatNativeEndpoint(edge.Neighbor), location))
		if edge.Source != "" || edge.Confidence != nil || edge.Provenance != "" {
			conf := "unknown"
			if edge.Confidence != nil {
				conf = fmt.Sprintf("%.2f", *edge.Confidence)
			}
			*lines = append(*lines, fmt.Sprintf("  source=%s; provenance=%s; confidence=%s", valueOrString(edge.Source, "unknown"), valueOrString(edge.Provenance, "unknown"), conf))
		}
	}
}

func appendGraphMinimalEdges(lines *[]string, edges []graphreader.Edge, limit int) {
	for i, edge := range edges {
		if limit > 0 && i >= limit {
			*lines = append(*lines, fmt.Sprintf("- ... %d additional graph edges omitted", len(edges)-i))
			break
		}
		location := ""
		if edge.EvidenceFilePath != "" {
			location = " @ " + edge.EvidenceFilePath + optionalGraphLine(edge.StartLine)
		}
		*lines = append(*lines, fmt.Sprintf("- %s --%s--> %s%s", formatNativeEndpoint(edge.Start), edge.Relation, formatNativeEndpoint(edge.Neighbor), location))
		if edge.Source != "" || edge.Confidence != nil || edge.Provenance != "" {
			conf := "unknown"
			if edge.Confidence != nil {
				conf = fmt.Sprintf("%.2f", *edge.Confidence)
			}
			*lines = append(*lines, fmt.Sprintf("  source=%s; provenance=%s; confidence=%s", valueOrString(edge.Source, "unknown"), valueOrString(edge.Provenance, "unknown"), conf))
		}
	}
}

func formatGraphStatusOutput(e db.CodeGraphInspectionEvidence) string {
	lines := []string{
		"Graph status",
		fmt.Sprintf("Active snapshots: %d; repositories covered: %d", e.ActiveSnapshotCount, e.SearchedRepoCount),
		"",
	}
	if len(e.ActiveScopes) > 0 {
		lines = append(lines, "Active snapshot scopes:")
		for _, scope := range e.ActiveScopes {
			lines = append(lines, fmt.Sprintf("- repoId=%d revision=%s commit=%s workspace=%s schema=%d builder=%s graphIndex=%s", scope.RepoID, valueOrString(scope.Revision, "unknown"), valueOrString(scope.CommitHash, "unknown"), valueOrString(scope.WorkspaceID, "unknown"), scope.SchemaVersion, valueOrString(scope.BuilderVersion, "unknown"), scope.GraphIndexID))
		}
		lines = append(lines, "")
	} else {
		lines = append(lines, "No READY active graph snapshots matched the requested repository/revision scope.", "")
	}
	lines = append(lines, "Matched evidence counts:")
	lines = append(lines, fmt.Sprintf("- SCIP symbols: %d", len(e.Symbols)))
	lines = append(lines, fmt.Sprintf("- SCIP relationships: %d", len(e.Relationships)))
	lines = append(lines, fmt.Sprintf("- SCIP occurrences: %d", len(e.Occurrences)))
	lines = append(lines, fmt.Sprintf("- architecture anchors: %d", len(e.Anchors)))
	lines = append(lines, fmt.Sprintf("- semantic facts: %d", len(e.SemanticFacts)))
	lines = append(lines, fmt.Sprintf("- semantic edges: %d", len(e.SemanticEdges)))
	lines = append(lines, fmt.Sprintf("- semantic hyperedges: %d", len(e.SemanticHyperedges)))
	if len(e.Warnings) > 0 {
		lines = append(lines, "", "Warnings:")
		for _, warning := range cleanStrings(e.Warnings) {
			lines = append(lines, "- "+warning)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func graphSeedsFromEvidence(e db.CodeGraphInspectionEvidence) []graphreader.Seed {
	type scoredSeed struct {
		seed   graphreader.Seed
		score  int
		order  int
		bucket string
	}
	scored := make([]scoredSeed, 0, len(e.Symbols)+len(e.Relationships)*2+len(e.Anchors)+len(e.SemanticFacts)+len(e.SemanticEdges)*2)
	seen := map[string]bool{}
	orgPrefix := ""
	if e.OrgID > 0 {
		orgPrefix = fmt.Sprintf("cg:o%d:", e.OrgID)
	}
	requestedBuckets := graphEvidenceRequestedLanguageBuckets(e.Query)
	addBucket := func(workspaceID, vid string, score int, bucket string) {
		if workspaceID == "" || vid == "" || !strings.HasPrefix(vid, "cg:o") {
			return
		}
		if orgPrefix != "" && !strings.HasPrefix(vid, orgPrefix) {
			return
		}
		key := workspaceID + "\x00" + vid
		if seen[key] {
			return
		}
		seen[key] = true
		scored = append(scored, scoredSeed{
			seed:   graphreader.Seed{WorkspaceID: workspaceID, NodeVID: vid},
			score:  score,
			order:  len(scored),
			bucket: bucket,
		})
	}
	add := func(workspaceID, vid string, score int) {
		addBucket(workspaceID, vid, score, "")
	}
	scopeFor := func(repoID int32, revision, commitHash string) (db.CodeGraphActiveScope, bool) {
		for _, scope := range e.ActiveScopes {
			if scope.RepoID != repoID {
				continue
			}
			if revision != "" && scope.Revision != "" && scope.Revision != revision {
				continue
			}
			if commitHash != "" && scope.CommitHash != "" && scope.CommitHash != commitHash {
				continue
			}
			if scope.WorkspaceID == "" || scope.CommitHash == "" {
				continue
			}
			return scope, true
		}
		return db.CodeGraphActiveScope{}, false
	}
	if e.OrgID > 0 {
		for _, edge := range e.SemanticEdges {
			edgeText := graphSemanticEdgeEvidenceText(edge)
			score := graphSeedScoreForSemanticEdge(edge) + graphTextRequestedLanguageScore(edgeText, requestedBuckets)
			bucket := graphEvidenceLanguageBucket(edgeText)
			addBucket(edge.WorkspaceID, edge.SourceExternalID, score, bucket)
			addBucket(edge.WorkspaceID, edge.TargetExternalID, score, bucket)
		}
		for _, fact := range e.SemanticFacts {
			add(fact.WorkspaceID, fact.ExternalID, graphSeedScoreForFact(fact))
		}
		for _, anchor := range e.Anchors {
			add(anchor.WorkspaceID, anchor.NodeVID, graphSeedScoreForAnchor(anchor))
		}
		for _, symbol := range e.Symbols {
			if scope, ok := scopeFor(symbol.RepoID, symbol.Revision, symbol.CommitHash); ok {
				symbolText := valueOrEmpty(symbol.FilePath) + " " + symbol.DisplayName + " " + symbol.Symbol
				addBucket(scope.WorkspaceID, mcpCodeGraphVID(e.OrgID, scope, "symbol", symbol.Symbol), graphSeedScoreForSymbol(symbol)+graphTextRequestedLanguageScore(symbolText, requestedBuckets), graphEvidenceLanguageBucket(symbolText))
			}
		}
		for _, relationship := range e.Relationships {
			if scope, ok := scopeFor(relationship.RepoID, relationship.Revision, relationship.CommitHash); ok {
				relationshipText := relationship.SourceSymbol + " " + relationship.TargetSymbol
				score := graphSeedScoreForRelationship(relationship) + graphTextRequestedLanguageScore(relationshipText, requestedBuckets)
				bucket := graphEvidenceLanguageBucket(relationshipText)
				addBucket(scope.WorkspaceID, mcpCodeGraphVID(e.OrgID, scope, "symbol", relationship.SourceSymbol), score, bucket)
				addBucket(scope.WorkspaceID, mcpCodeGraphVID(e.OrgID, scope, "symbol", relationship.TargetSymbol), score, bucket)
			}
		}
	}
	for _, edge := range e.SemanticEdges {
		edgeText := graphSemanticEdgeEvidenceText(edge)
		score := graphSeedScoreForSemanticEdge(edge) + graphTextRequestedLanguageScore(edgeText, requestedBuckets)
		bucket := graphEvidenceLanguageBucket(edgeText)
		addBucket(edge.WorkspaceID, edge.SourceExternalID, score, bucket)
		addBucket(edge.WorkspaceID, edge.TargetExternalID, score, bucket)
	}
	for _, fact := range e.SemanticFacts {
		add(fact.WorkspaceID, fact.ExternalID, graphSeedScoreForFact(fact))
	}
	for _, anchor := range e.Anchors {
		add(anchor.WorkspaceID, anchor.NodeVID, graphSeedScoreForAnchor(anchor))
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].order < scored[j].order
	})
	if requested := requestedBuckets; len(requested) > 1 {
		groups := map[string][]scoredSeed{}
		rest := make([]scoredSeed, 0, len(scored))
		for _, item := range scored {
			if requested[item.bucket] {
				groups[item.bucket] = append(groups[item.bucket], item)
				continue
			}
			rest = append(rest, item)
		}
		if len(groups) > 1 {
			diverse := make([]scoredSeed, 0, len(scored))
			for {
				progress := false
				for _, bucket := range []string{"node", "python", "dotnet", "go"} {
					queue := groups[bucket]
					if len(queue) == 0 {
						continue
					}
					diverse = append(diverse, queue[0])
					groups[bucket] = queue[1:]
					progress = true
				}
				if !progress {
					break
				}
			}
			diverse = append(diverse, rest...)
			scored = diverse
		}
	}
	out := make([]graphreader.Seed, 0, len(scored))
	for _, item := range scored {
		out = append(out, item.seed)
	}
	return out
}

func graphSeedScoreForSemanticEdge(edge db.CodeGraphSemanticEdgeEvidence) int {
	score := 80 + int(edge.Confidence*10)
	if strings.EqualFold(edge.Source, "scip") {
		score += 30
	}
	switch strings.ToUpper(strings.TrimSpace(edge.Relation)) {
	case "CALLS", "REFERENCES", "HANDLES", "EMITS", "PROVIDES", "CONSUMES":
		score += 25
	case "IMPLEMENTS", "TYPE_DEFINES", "EXTENDS":
		score += 15
	case "IMPORTS", "IMPORTS_FROM":
		score -= 12
	}
	if isPreferredGraphSeedPath(edge.SourceFile) {
		score += 12
	}
	return score
}

func graphEvidenceRequestedLanguageBuckets(query string) map[string]bool {
	lower := strings.ToLower(query)
	out := map[string]bool{}
	if strings.Contains(lower, "node") || strings.Contains(lower, "javascript") || strings.Contains(lower, "nodejs") {
		out["node"] = true
	}
	if strings.Contains(lower, "python") {
		out["python"] = true
	}
	if strings.Contains(lower, "dotnet") || strings.Contains(lower, ".net") {
		out["dotnet"] = true
	}
	if strings.Contains(lower, "golang") || strings.Contains(lower, " go ") {
		out["go"] = true
	}
	return out
}

func graphEvidenceLanguageBucket(value string) string {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "nodejs") || strings.Contains(lower, "node.js") || strings.Contains(lower, "javascript"):
		return "node"
	case strings.Contains(lower, "python"):
		return "python"
	case strings.Contains(lower, "dotnet") || strings.Contains(lower, ".net"):
		return "dotnet"
	case strings.Contains(lower, "golang") || strings.Contains(lower, "/go/") || strings.Contains(lower, " go "):
		return "go"
	case strings.Contains(lower, "apache") || strings.Contains(lower, "httpd"):
		return "apache"
	case strings.Contains(lower, "nginx"):
		return "nginx"
	case strings.Contains(lower, "java") && !strings.Contains(lower, "javascript"):
		return "java"
	case strings.Contains(lower, "ruby"):
		return "ruby"
	case strings.Contains(lower, "php"):
		return "php"
	default:
		return ""
	}
}

func graphTextRequestedLanguageScore(value string, requested map[string]bool) int {
	if len(requested) == 0 {
		return 0
	}
	score := 0
	buckets := graphEvidenceTopicBuckets(value)
	for _, bucket := range buckets {
		if requested[bucket] {
			score += 42
			continue
		}
		if graphEvidenceIsLanguageOrRuntimeBucket(bucket) {
			score -= 72
		}
	}
	if len(buckets) == 0 && graphEvidenceContainsCoreFlowAnchor(value) {
		score += 24
	}
	return score
}

func graphEvidenceHasMixedRequestedAndUnrequestedRuntime(value string, requested map[string]bool) bool {
	if len(requested) == 0 {
		return false
	}
	hasRequested := false
	hasUnrequested := false
	for _, bucket := range graphEvidenceTopicBuckets(value) {
		if requested[bucket] {
			hasRequested = true
			continue
		}
		if graphEvidenceIsLanguageOrRuntimeBucket(bucket) {
			hasUnrequested = true
		}
	}
	return hasRequested && hasUnrequested
}

func graphEvidenceTopicBuckets(value string) []string {
	lower := strings.ToLower(value)
	out := []string{}
	add := func(bucket string) {
		for _, existing := range out {
			if existing == bucket {
				return
			}
		}
		out = append(out, bucket)
	}
	for _, candidate := range []struct {
		bucket  string
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
	} {
		for _, marker := range candidate.markers {
			if strings.Contains(lower, marker) {
				add(candidate.bucket)
				break
			}
		}
	}
	return out
}

func graphEvidenceIsLanguageOrRuntimeBucket(bucket string) bool {
	switch bucket {
	case "node", "python", "dotnet", "go", "apache", "nginx", "java", "ruby", "php":
		return true
	default:
		return false
	}
}

func graphEvidenceContainsCoreFlowAnchor(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"webhook", "mutate", "podmutator", "sdkinjector", "instrumentationspec",
		"injectcommon", "languageinstrumentations", "annotationvalue",
		"modifiedpod", "initcontainer", "volume mount", "envotel",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func graphSemanticEdgeEvidenceText(edge db.CodeGraphSemanticEdgeEvidence) string {
	return strings.Join([]string{
		edge.Relation,
		edge.Source,
		edge.SourceFile,
		valueOrEmpty(edge.Evidence),
		edge.SourceExternalID,
		edge.TargetExternalID,
	}, " ")
}

func graphEdgeEvidenceText(edge graphreader.Edge) string {
	return strings.Join([]string{
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
	}, " ")
}

func graphSeedScoreForFact(fact db.CodeGraphSemanticFactEvidence) int {
	score := 65 + int(fact.Confidence*10)
	if strings.EqualFold(fact.Source, "scip") {
		score += 25
	}
	switch strings.ToLower(strings.TrimSpace(fact.Kind)) {
	case "route", "event", "service", "job", "function", "method", "class", "symbol":
		score += 15
	case "file", "package", "import", "parameter":
		score -= 10
	}
	if isPreferredGraphSeedPath(fact.SourceFile) {
		score += 12
	}
	return score
}

func graphSeedScoreForAnchor(anchor db.CodeGraphAnchorEvidence) int {
	score := 60 + int(anchor.Confidence*10)
	switch strings.ToLower(strings.TrimSpace(anchor.Kind)) {
	case "route", "event", "service", "job":
		score += 20
	}
	if anchor.EvidenceFilePath != nil && isPreferredGraphSeedPath(*anchor.EvidenceFilePath) {
		score += 12
	}
	return score
}

func graphSeedScoreForSymbol(symbol db.CodeGraphSymbolEvidence) int {
	score := 45
	if symbol.Kind != nil {
		switch strings.ToLower(strings.TrimSpace(*symbol.Kind)) {
		case "function", "method", "class", "struct", "interface":
			score += 15
		case "parameter", "package", "namespace":
			score -= 10
		}
	}
	if symbol.FilePath != nil && isPreferredGraphSeedPath(*symbol.FilePath) {
		score += 12
	}
	return score
}

func graphSeedScoreForRelationship(relationship db.CodeGraphRelationshipEvidence) int {
	score := 50
	if relationship.IsReference || relationship.IsDefinition {
		score += 15
	}
	if relationship.IsImplementation || relationship.IsTypeDefinition {
		score += 10
	}
	return score
}

func isPreferredGraphSeedPath(path string) bool {
	path = strings.ToLower(path)
	if path == "" {
		return false
	}
	if strings.Contains(path, "/vendor/") || strings.Contains(path, "/node_modules/") || strings.Contains(path, "/dist/") || strings.Contains(path, "/generated/") {
		return false
	}
	for _, marker := range []string{"/internal/", "/cmd/", "/pkg/", "/apis/", "/api/", "/webhook/", "/controller/", "/instrumentation/", "/routes/", "/services/"} {
		if strings.Contains(path, marker) || strings.HasPrefix(path, strings.TrimPrefix(marker, "/")) {
			return true
		}
	}
	return false
}

const mcpDefaultGraphBuilder = "codeintel-code-graph-v7"

func mcpCodeGraphVID(orgID int32, scope db.CodeGraphActiveScope, kind, key string) string {
	builder := scope.BuilderVersion
	if builder == "" {
		builder = mcpDefaultGraphBuilder
	}
	schema := scope.SchemaVersion
	if schema <= 0 {
		schema = 1
	}
	commit := scope.CommitHash
	if len(commit) > 12 {
		commit = commit[:12]
	}
	keyHash := mcpHashParts([]string{
		fmt.Sprint(orgID),
		scope.WorkspaceID,
		fmt.Sprint(scope.RepoID),
		scope.CommitHash,
		fmt.Sprint(schema),
		builder,
		kind,
		key,
	}, 32)
	return fmt.Sprintf("cg:o%d:w%s:r%d:c%s:s%d:b%s:%s:%s",
		orgID,
		mcpHashParts([]string{scope.WorkspaceID}, 8),
		scope.RepoID,
		commit,
		schema,
		mcpHashParts([]string{builder}, 8),
		kind,
		keyHash,
	)
}

func mcpHashParts(parts []string, n int) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))
	if n > len(sum) {
		return sum
	}
	return sum[:n]
}

func graphAllowedScopes(scopes []db.CodeGraphActiveScope) []graphreader.ActiveScope {
	out := make([]graphreader.ActiveScope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.RepoID <= 0 {
			continue
		}
		out = append(out, graphreader.ActiveScope{
			WorkspaceID:    scope.WorkspaceID,
			RepoID:         scope.RepoID,
			Revision:       scope.Revision,
			CommitHash:     scope.CommitHash,
			SchemaVersion:  scope.SchemaVersion,
			BuilderVersion: scope.BuilderVersion,
		})
	}
	return out
}

func graphFilesToRead(e db.CodeGraphInspectionEvidence, native graphreader.InspectResult) []string {
	seen := map[string]bool{}
	var out []string
	add := func(repo, path, revision string) {
		if repo == "" || path == "" {
			return
		}
		if revision == "" {
			revision = "active"
		}
		key := repo + "\x00" + path + "\x00" + revision
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, fmt.Sprintf("[%s] %s (%s)", repo, path, revision))
	}
	for _, row := range e.Symbols {
		if row.FilePath != nil {
			add(row.RepoName, *row.FilePath, row.Revision)
		}
	}
	for _, row := range e.Occurrences {
		add(row.RepoName, row.FilePath, row.Revision)
	}
	for _, row := range e.Anchors {
		if row.EvidenceFilePath != nil {
			add(row.RepoName, *row.EvidenceFilePath, row.Revision)
		}
	}
	for _, row := range e.SemanticFacts {
		add(row.RepoName, row.SourceFile, row.Revision)
	}
	for _, row := range e.SemanticEdges {
		add(row.RepoName, row.SourceFile, row.Revision)
	}
	for _, row := range e.SemanticHyperedges {
		add(row.RepoName, row.SourceFile, row.Revision)
	}
	for _, edge := range native.Edges {
		repo := ""
		if edge.EdgeRepoID != nil {
			repo = fmt.Sprintf("repoId:%d", *edge.EdgeRepoID)
		}
		add(repo, edge.EvidenceFilePath, edge.EdgeRevision)
	}
	sort.Strings(out)
	if len(out) > 25 {
		out = out[:25]
	}
	return out
}

func formatScipSymbol(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if first := strings.Index(symbol, "`"); first >= 0 {
		if second := strings.Index(symbol[first+1:], "`"); second >= 0 {
			quotedEnd := first + 1 + second
			base := symbol[first+1 : quotedEnd]
			suffix := strings.TrimSpace(symbol[quotedEnd+1:])
			if suffix != "" && suffix != "/" {
				return base + suffix
			}
			return base
		}
	}
	if idx := strings.LastIndex(symbol, "/"); idx >= 0 && idx+1 < len(symbol) {
		return symbol[idx+1:]
	}
	return symbol
}

func relationshipFlags(row db.CodeGraphRelationshipEvidence) string {
	var flags []string
	if row.IsReference {
		flags = append(flags, "reference")
	}
	if row.IsImplementation {
		flags = append(flags, "implementation")
	}
	if row.IsTypeDefinition {
		flags = append(flags, "type-definition")
	}
	if row.IsDefinition {
		flags = append(flags, "definition")
	}
	if len(flags) == 0 {
		return "relationship"
	}
	return strings.Join(flags, ", ")
}

func formatSemanticEdgeDisplay(row db.CodeGraphSemanticEdgeEvidence) string {
	evidence := strings.TrimSpace(valueOrEmpty(row.Evidence))
	if evidence != "" {
		if formatted := formatSemanticEvidence(evidence); formatted != "" {
			return formatted
		}
	}
	return compactGraphExternalID(row.SourceExternalID) + " -> " + compactGraphExternalID(row.TargetExternalID)
}

func formatSemanticEvidence(evidence string) string {
	for _, sep := range []string{" CALLS ", " REFERENCES ", " DEFINES ", " TYPE_DEFINES ", " IMPLEMENTS ", " imports ", " -> "} {
		if !strings.Contains(evidence, sep) {
			continue
		}
		parts := strings.SplitN(evidence, sep, 2)
		if len(parts) != 2 {
			continue
		}
		return formatScipSymbol(parts[0]) + sep + formatScipSymbol(parts[1])
	}
	return formatScipSymbol(evidence)
}

func compactGraphExternalID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if strings.HasPrefix(value, "cg:") {
		parts := strings.Split(value, ":")
		if len(parts) >= 2 {
			return parts[len(parts)-2] + ":" + parts[len(parts)-1]
		}
	}
	return formatScipSymbol(value)
}

func formatNativeEndpoint(endpoint graphreader.Endpoint) string {
	name := endpoint.Label
	if name == "" {
		name = endpoint.Key
	}
	if name == "" {
		name = endpoint.Kind
	}
	if name == "" {
		name = endpoint.VID
	}
	location := ""
	if endpoint.Path != "" {
		location = " at " + endpoint.Path
	}
	repo := ""
	if endpoint.RepoID != nil {
		repo = fmt.Sprintf("[repoId:%d] ", *endpoint.RepoID)
	}
	return repo + valueOrString(endpoint.Kind, "node") + " " + name + location
}

func formatEdgeKinds(kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	return "; edgeKinds=" + strings.Join(kinds, ",")
}

func optionalLine(line *int32) string {
	if line == nil {
		return ""
	}
	return fmt.Sprintf(":%d", *line+1)
}

func optionalGraphLine(line *int32) string {
	if line == nil || *line <= 0 {
		return ""
	}
	return fmt.Sprintf(":%d", *line)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func valueOrDefault(value *string, fallback string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return fallback
	}
	return *value
}

func valueOrString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func trimForTool(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
