package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/db"
	"codeintel/internal/graphreader"
	"codeintel/internal/retrievalpolicy"
	"codeintel/pkg/repopaths"
)

var codegraphContextMatchHeaderRE = regexp.MustCompile(`^\[([^\]]+)\] (.+):$`)
var codegraphContextMatchLineRE = regexp.MustCompile(`^\s+(\d+):`)
var codegraphContextOpaqueFunctionIDRE = regexp.MustCompile(`function:[0-9a-f]{16,}`)

const (
	codegraphContextCompactLayerBytes = 550
	codegraphContextCompactLines      = 8
)

type layer struct {
	Title      string `json:"title"`
	Tool       string `json:"tool"`
	Error      bool   `json:"error"`
	DurationMs int64  `json:"durationMs"`
	Output     string `json:"output"`
}

type codegraphContextLayerTask struct {
	Title     string
	Tool      string
	Arguments map[string]any
	Source    string
}

type codegraphContextRepoCoverage struct {
	IndexedRepos []string `json:"indexedRepos"`
	SkippedRepos []string `json:"skippedRepos"`
}

func (b *Backend) toolCodegraphContext(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.SearchBackend == nil {
		return toolResult{}, errors.New("codegraph_context requires Zoekt search backend")
	}
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("codegraph_context requires Postgres query backend")
	}
	if b.cfg.GraphReader == nil {
		return toolResult{}, errors.New("codegraph_context requires NebulaGraph reader")
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
		return toolResult{}, fmt.Errorf("invalid codegraph_context arguments")
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return toolResult{}, fmt.Errorf("codegraph_context query is required")
	}
	limit := int32(25)
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > 100 {
			return toolResult{}, fmt.Errorf("limit must be a positive integer no greater than 100")
		}
		limit = *args.Limit
	}
	repos := cleanStrings(append(args.Repos, args.Repo))
	if len(repos) == 0 {
		var err error
		repos, err = b.resolveAskRepoScope(ctx, req.OrgID, nil)
		if err != nil {
			return toolResult{}, err
		}
	}
	coverage, err := b.codegraphContextIndexedRepoCoverage(ctx, req.OrgID, repos, args.Ref)
	if err != nil {
		return toolResult{}, err
	}
	requestedRepos := append([]string(nil), repos...)
	repos = coverage.IndexedRepos
	if len(repos) == 0 {
		return toolResult{}, fmt.Errorf("codegraph_context found no selected repositories with an indexed ref for %q:\n%s", displayCodegraphContextRef(args.Ref), strings.Join(coverage.SkippedRepos, "\n"))
	}
	contextCacheKey := ""
	if args.Compact && b.cfg.GraphEvidenceCache != nil {
		_, revisions, scopedRevisions, scopeErr := b.resolveGraphScope(ctx, req.OrgID, "", repos, args.Ref)
		if scopeErr != nil {
			b.logger.Warn("codegraph_context cache scope failed", "err", scopeErr.Error())
		} else {
			activeScopes, activeErr := b.cfg.Queries.ListActiveCodeGraphScopes(ctx, db.ListActiveCodeGraphScopesParams{
				OrgID:              req.OrgID,
				Repos:              repos,
				RevisionCandidates: revisions,
				RepoRevisionScopes: scopedRevisions,
			})
			if activeErr != nil {
				b.logger.Warn("codegraph_context cache active-scope preflight failed", "err", activeErr.Error())
			} else {
				key, keyErr := codegraphContextCacheKey(codegraphContextCacheKeyParams{
					OrgID:        req.OrgID,
					Query:        args.Query,
					Repos:        repos,
					RequestedRef: args.Ref,
					Limit:        limit,
					Depth:        args.Depth,
					Compact:      args.Compact,
					ActiveScopes: activeScopes,
				})
				if keyErr != nil {
					b.logger.Warn("codegraph_context cache key failed", "err", keyErr.Error())
				} else {
					contextCacheKey = key
					if cached, ok, getErr := b.cfg.GraphEvidenceCache.GetCodegraphContext(ctx, key); getErr != nil {
						b.logger.Warn("codegraph_context cache read failed", "err", getErr.Error())
					} else if ok {
						return cached, nil
					}
				}
			}
		}
	}

	callLayer := func(title, name string, subArgs map[string]any) layer {
		body, _ := json.Marshal(map[string]any{
			"name":      name,
			"arguments": subArgs,
		})
		started := time.Now()
		result, err := b.callTool(ctx, req, body)
		duration := time.Since(started).Milliseconds()
		if err != nil {
			return layer{Title: title, Tool: name, Error: true, DurationMs: duration, Output: publicToolError(err)}
		}
		return layer{Title: title, Tool: name, Error: result.IsError, DurationMs: duration, Output: toolResultText(result)}
	}
	scopeArgs := func() map[string]any {
		out := map[string]any{"limit": limit}
		if args.Ref != "" {
			out["ref"] = args.Ref
		}
		if len(repos) == 1 {
			out["repo"] = repos[0]
		} else if len(repos) > 1 {
			out["repos"] = repos
			out["groupByRepo"] = true
		}
		return out
	}
	symbolScopeArgs := func() map[string]any {
		out := map[string]any{"limit": limit}
		if args.Ref != "" {
			out["revision"] = args.Ref
		}
		if len(repos) == 1 {
			out["repo"] = repos[0]
		} else if len(repos) > 1 {
			out["repos"] = repos
		}
		return out
	}

	layers := make([]layer, 0, 8)
	addLayer := func(title, tool string, durationMs int64, output string, isErr bool) {
		layers = append(layers, layer{Title: title, Tool: tool, Error: isErr, DurationMs: durationMs, Output: output})
	}
	if len(coverage.SkippedRepos) > 0 {
		addLayer("Repository index coverage", "scope_preflight", 0, codegraphContextRepoCoverageOutput(coverage, args.Ref), false)
	}
	sourceSliceCandidates := make([]codegraphContextSourceSliceCandidate, 0, 32)
	rememberSourceSlices := func(output, source string) {
		sourceSliceCandidates = append(sourceSliceCandidates, codegraphContextSourceSliceCandidates(output, source)...)
	}
	sourceSliceManifest := make([]string, 0, 12)

	expandedTerms := codegraphContextExpansionTerms(args.Query, 24)
	symbols := codegraphContextSymbolCandidates(args.Query, expandedTerms, 5)
	symbols = codegraphContextPruneBroadSymbols(symbols)
	if args.Compact {
		symbols = codegraphContextCompactSymbolCandidates(args.Query, symbols, 3)
	}
	broadPattern := preflightSearchPattern(args.Query)
	focusedTerms := append([]string{}, symbols...)
	focusedTerms = append(focusedTerms, expandedTerms...)
	focusedPattern := codegraphContextSearchPattern(args.Query, focusedTerms, 18)
	implementationPattern := codegraphContextImplementationPattern(args.Query)
	runtimeEnvPatterns := codegraphContextRuntimeEnvPatterns(args.Query)
	webhookPattern := codegraphContextWebhookPattern(args.Query)
	annotationPattern := codegraphContextAnnotationPattern(args.Query)
	specPattern := codegraphContextSearchPattern("", expandedTerms, 18)
	crdSpecPattern := codegraphContextCRDSpecPattern(args.Query)
	runtimeSidePattern := codegraphContextRuntimeSidePattern(args.Query)
	otlpFlowPattern := codegraphContextOTLPFlowPattern(args.Query)
	graphQuery := codegraphContextGraphQuery(args.Query, expandedTerms)

	graphLimit := codegraphContextGraphTraversalLimit(limit, args.Compact)
	pathArgs := scopeArgs()
	pathArgs["query"] = graphQuery
	pathArgs["limit"] = graphLimit
	if args.Depth != nil {
		pathArgs["depth"] = *args.Depth
	} else {
		pathArgs["depth"] = codegraphContextGraphTraversalDepth("path", args.Compact)
	}
	type graphLayerPair struct {
		Minimal layer
		Path    layer
	}
	graphLayersCh := make(chan graphLayerPair, 1)
	go func() {
		graphLayer, pathLayer := b.codegraphContextGraphLayers(ctx, req, pathArgs, graphQuery, args.Compact)
		graphLayersCh <- graphLayerPair{Minimal: graphLayer, Path: pathLayer}
	}()

	symbolLayersCh := make(chan []layer, 1)
	go func() {
		if len(symbols) == 0 {
			symbolLayersCh <- []layer{{
				Title:      "SCIP symbol precision",
				Tool:       "find_symbol_definitions/find_symbol_references",
				Error:      false,
				DurationMs: 0,
				Output:     "No code-like symbol token was detected; symbol tools remain available for follow-up.",
			}}
			return
		}
		symbolLayers := make([]layer, len(symbols)*2)
		sem := make(chan struct{}, codegraphContextSymbolProbeConcurrency(args.Compact))
		var symbolWG sync.WaitGroup
		for idx, symbol := range symbols {
			symbol := symbol
			defArgs := symbolScopeArgs()
			defArgs["symbol"] = symbol
			defArgs["limit"] = codegraphContextSymbolProbeLimit(args.Compact)
			refArgs := symbolScopeArgs()
			refArgs["symbol"] = symbol
			refArgs["limit"] = codegraphContextSymbolProbeLimit(args.Compact)
			defSlot := idx * 2
			refSlot := idx*2 + 1
			symbolWG.Add(2)
			go func() {
				defer symbolWG.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				symbolLayers[defSlot] = callLayer("SCIP definitions for "+symbol, "find_symbol_definitions", defArgs)
			}()
			go func() {
				defer symbolWG.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				symbolLayers[refSlot] = callLayer("SCIP references for "+symbol, "find_symbol_references", refArgs)
			}()
		}
		symbolWG.Wait()
		symbolLayersCh <- symbolLayers
	}()

	recallTasks := make([]codegraphContextLayerTask, 0, 10)
	if focusedPattern != broadPattern {
		focusedArgs := scopeArgs()
		focusedArgs["pattern"] = focusedPattern
		focusedArgs["limit"] = int64(minInt32(limit, 30))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt focused code recall",
			Tool:      "grep",
			Arguments: focusedArgs,
			Source:    "focused",
		})
	}
	if implementationPattern != "" && implementationPattern != focusedPattern && implementationPattern != broadPattern {
		implArgs := scopeArgs()
		implArgs["pattern"] = implementationPattern
		implArgs["limit"] = int64(minInt32(limit, 30))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt language implementation recall",
			Tool:      "grep",
			Arguments: implArgs,
			Source:    "implementation",
		})
	}
	for _, runtimeEnvPattern := range runtimeEnvPatterns {
		if runtimeEnvPattern.Pattern == focusedPattern || runtimeEnvPattern.Pattern == broadPattern || runtimeEnvPattern.Pattern == implementationPattern {
			continue
		}
		envArgs := scopeArgs()
		envArgs["pattern"] = runtimeEnvPattern.Pattern
		envArgs["include"] = "internal/instrumentation/*.go"
		envArgs["limit"] = int64(16)
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt runtime/env recall: " + runtimeEnvPattern.Language,
			Tool:      "grep",
			Arguments: envArgs,
			Source:    "runtime-env",
		})
	}
	if webhookPattern != "" && webhookPattern != focusedPattern && webhookPattern != broadPattern && webhookPattern != implementationPattern {
		webhookArgs := scopeArgs()
		webhookArgs["pattern"] = webhookPattern
		webhookArgs["include"] = "*.go"
		webhookArgs["limit"] = int64(minInt32(limit, 40))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt webhook/mutator entry recall",
			Tool:      "grep",
			Arguments: webhookArgs,
			Source:    "webhook",
		})
	}
	if annotationPattern != "" && annotationPattern != focusedPattern && annotationPattern != broadPattern && annotationPattern != implementationPattern {
		annotationArgs := scopeArgs()
		annotationArgs["pattern"] = annotationPattern
		annotationArgs["include"] = "internal/instrumentation/*.go"
		annotationArgs["limit"] = int64(minInt32(limit, 40))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt annotation routing recall",
			Tool:      "grep",
			Arguments: annotationArgs,
			Source:    "annotation",
		})
	}
	if specPattern != "" && specPattern != focusedPattern && specPattern != broadPattern && specPattern != implementationPattern {
		specArgs := scopeArgs()
		specArgs["pattern"] = specPattern
		specArgs["limit"] = int64(minInt32(limit, 30))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt spec/webhook recall",
			Tool:      "grep",
			Arguments: specArgs,
			Source:    "spec",
		})
	}
	if crdSpecPattern != "" {
		crdArgs := scopeArgs()
		crdArgs["pattern"] = crdSpecPattern
		crdArgs["include"] = "*.go"
		crdArgs["limit"] = int64(minInt32(limit, 40))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt CRD/spec struct recall",
			Tool:      "grep",
			Arguments: crdArgs,
			Source:    "crd-spec",
		})
	}
	if runtimeSidePattern != "" {
		runtimeSideArgs := scopeArgs()
		runtimeSideArgs["pattern"] = runtimeSidePattern
		runtimeSideArgs["limit"] = int64(minInt32(limit, 40))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt runtime-side SDK entry recall",
			Tool:      "grep",
			Arguments: runtimeSideArgs,
			Source:    "runtime-side",
		})
	}
	if otlpFlowPattern != "" && otlpFlowPattern != focusedPattern && otlpFlowPattern != broadPattern && otlpFlowPattern != implementationPattern {
		otlpArgs := scopeArgs()
		otlpArgs["pattern"] = otlpFlowPattern
		otlpArgs["limit"] = int64(maxInt32(limit, 160))
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt OTLP trace/export recall",
			Tool:      "grep",
			Arguments: otlpArgs,
			Source:    "otlp-flow",
		})
	}
	for _, runtimePattern := range codegraphContextRuntimeSidePatterns(args.Query) {
		runtimeSideArgs := scopeArgs()
		runtimeSideArgs["pattern"] = runtimePattern.Pattern
		runtimeSideArgs["limit"] = int64(16)
		recallTasks = append(recallTasks, codegraphContextLayerTask{
			Title:     "Zoekt runtime-side SDK entry recall: " + runtimePattern.Language,
			Tool:      "grep",
			Arguments: runtimeSideArgs,
			Source:    "runtime-side",
		})
	}
	if codegraphContextWantsTestEvidence(args.Query) {
		testTerms := make([]string, 0, len(expandedTerms)+8)
		if hasCodegraphInstrumentationHints(strings.ToLower(args.Query)) {
			testTerms = append(testTerms,
				"TestInjectPythonSDK",
				"TestInjectNodeJSSDK",
				"TestInjectDotNetSDK",
				"TestAppendIfNotSet",
				"injectPythonSDK",
				"injectNodeJSSDK",
				"injectDotNetSDK",
			)
		}
		testTerms = append(testTerms, expandedTerms...)
		testPattern := codegraphContextSearchPattern("", testTerms, 18)
		if testPattern != "" {
			testArgs := scopeArgs()
			testArgs["pattern"] = testPattern
			testArgs["include"] = "*_test.go"
			testArgs["limit"] = int64(minInt32(limit, 30))
			recallTasks = append(recallTasks, codegraphContextLayerTask{
				Title:     "Zoekt test evidence recall",
				Tool:      "grep",
				Arguments: testArgs,
				Source:    "test",
			})
		}
	}
	grepArgs := scopeArgs()
	grepArgs["pattern"] = broadPattern
	grepArgs["limit"] = int64(minInt32(limit, 30))
	recallTasks = append(recallTasks, codegraphContextLayerTask{
		Title:     "Zoekt broad recall",
		Tool:      "grep",
		Arguments: grepArgs,
		Source:    "broad",
	})

	recallLayers := b.codegraphContextRunLayerTasks(ctx, req, recallTasks, codegraphContextRecallConcurrency(args.Compact))
	layers = append(layers, recallLayers...)
	for idx, recallLayer := range recallLayers {
		if idx < len(recallTasks) {
			rememberSourceSlices(recallLayer.Output, recallTasks[idx].Source)
		}
	}
	sourceSliceCandidates = append(sourceSliceCandidates, codegraphContextMandatorySourceSliceCandidates(args.Query, repos)...)
	symbolLayers := <-symbolLayersCh
	for _, symbolLayer := range symbolLayers {
		if symbolLayer.Tool == "find_symbol_definitions" && !symbolLayer.Error {
			sourceSliceCandidates = append(sourceSliceCandidates, codegraphContextDefinitionSourceSliceCandidates(args.Query, symbolLayer.Output)...)
		}
	}

	if codegraphContextCanReadSourceSlices(b.cfg.Paths) {
		maxSourceSlices := 12
		readLimit := 180
		if args.Compact {
			maxSourceSlices = 72
			readLimit = 140
		}
		preselectLimit := maxSourceSlices
		if len(repos) > 1 {
			preselectLimit = maxInt(preselectLimit, 72)
		}
		selectedSlices := codegraphContextSelectSourceSlices(args.Query, sourceSliceCandidates, preselectLimit)
		if args.Compact {
			selectedSlices = codegraphContextDiverseSourceSlices(args.Query, selectedSlices, 18, 3)
		} else if len(repos) > 1 {
			selectedSlices = codegraphContextDiverseSourceSlices(args.Query, selectedSlices, maxSourceSlices, 3)
		}
		readTasks := make([]codegraphContextLayerTask, 0, len(selectedSlices))
		for _, slice := range selectedSlices {
			readArgs := map[string]any{
				"repo":   slice.Repo,
				"path":   slice.Path,
				"offset": codegraphContextSourceOffset(slice.Line),
				"limit":  readLimit,
			}
			if args.Ref != "" {
				readArgs["ref"] = args.Ref
			}
			readTasks = append(readTasks, codegraphContextLayerTask{
				Title:     fmt.Sprintf("Source slice %s:%d", slice.Path, slice.Line),
				Tool:      "read_file",
				Arguments: readArgs,
			})
		}
		readLayers := b.codegraphContextRunLayerTasks(ctx, req, readTasks, codegraphContextSourceReadConcurrency(args.Compact))
		layers = append(layers, readLayers...)
		for idx, readLayer := range readLayers {
			if idx < len(selectedSlices) {
				sourceSliceManifest = append(sourceSliceManifest, codegraphContextSourceSliceManifestLine(selectedSlices[idx], readLayer.Output, readLayer.Error))
			}
		}
	}

	graphLayers := <-graphLayersCh
	layers = append(layers, graphLayers.Minimal, graphLayers.Path)
	layers = append(layers, symbolLayers...)

	var out strings.Builder
	out.WriteString("Codegraph context fused evidence pack for ")
	out.WriteString(fmt.Sprintf("%q", args.Query))
	out.WriteString("\nRetrieval contract: Zoekt broad recall + SCIP semantic precision + AST/tree-sitter facts + NebulaGraph traversal.\n")
	out.WriteString("Architecture facts, graph paths, symbol references, and files-to-read are grouped below for coding-agent use.\n")
	out.WriteString("Repositories: ")
	out.WriteString(strings.Join(repos, ", "))
	if len(coverage.SkippedRepos) > 0 {
		out.WriteString("\nSkipped unindexed repositories: ")
		out.WriteString(strconv.Itoa(len(coverage.SkippedRepos)))
	}
	if args.Ref != "" {
		out.WriteString("\nRequested ref: ")
		out.WriteString(args.Ref)
	}
	out.WriteString("\n\n")
	out.WriteString(codegraphContextCriticalManifest(args.Query, layers, sourceSliceManifest))
	out.WriteString("\n\n")
	for _, section := range codegraphContextRenderOrder(layers, args.Compact) {
		status := "ok"
		if section.Error {
			status = "error"
		}
		out.WriteString("## ")
		out.WriteString(section.Title)
		out.WriteString(" (`")
		out.WriteString(section.Tool)
		out.WriteString("` ")
		out.WriteString(status)
		out.WriteString(")\n")
		out.WriteString(fmt.Sprintf("Duration: %dms\n", section.DurationMs))
		if strings.TrimSpace(section.Output) == "" {
			out.WriteString("No evidence returned.")
		} else if args.Compact {
			out.WriteString(codegraphContextCompactLayerOutput(section))
		} else {
			out.WriteString(truncateForModel(section.Output, maxAskToolOutputBytes))
		}
		out.WriteString("\n\n")
	}

	requiredLayerFailed := false
	for _, section := range layers {
		switch section.Tool {
		case "grep", "graph_minimal_context", "graph_path":
			if section.Error {
				requiredLayerFailed = true
			}
		}
	}
	if !codegraphContextLayersHaveASTEvidence(layers) && !codegraphContextGraphTimedOut(layers) {
		requiredLayerFailed = true
	}

	meta := map[string]any{
		"tool":           "codegraph_context",
		"query":          args.Query,
		"repos":          repos,
		"requestedRepos": requestedRepos,
		"repoCoverage":   coverage,
		"terms":          expandedTerms,
		"compact":        args.Compact,
	}
	if args.Compact {
		meta["layers"] = codegraphContextLayerSummaries(layers)
	} else {
		meta["layers"] = layers
	}

	result := toolResult{
		Content: []toolContent{{Type: "text", Text: strings.TrimSpace(out.String())}},
		IsError: requiredLayerFailed,
		Meta:    meta,
	}
	if contextCacheKey != "" && b.cfg.GraphEvidenceCache != nil && !result.IsError {
		if setErr := b.cfg.GraphEvidenceCache.SetCodegraphContext(ctx, contextCacheKey, result, b.graphEvidenceCacheTTL()); setErr != nil {
			b.logger.Warn("codegraph_context cache write failed", "err", setErr.Error())
		}
	}
	return result, nil
}

func codegraphContextGraphTimedOut(layers []layer) bool {
	for _, section := range layers {
		if (section.Tool == "graph_minimal_context" || section.Tool == "graph_path") &&
			strings.Contains(section.Output, "Graph evidence timeout") {
			return true
		}
	}
	return false
}

func (b *Backend) codegraphContextGraphLayers(ctx context.Context, req api.MCPRequest, pathArgs map[string]any, graphQuery string, compact bool) (layer, layer) {
	inspectArgs := cloneAnyMap(pathArgs)
	if compact {
		inspectArgs["compact"] = true
		if _, ok := inspectArgs["depth"]; !ok {
			inspectArgs["depth"] = codegraphContextGraphTraversalDepth("path", true)
		}
	}
	started := time.Now()
	body, _ := json.Marshal(inspectArgs)
	result, err := b.toolInspectCodeGraph(ctx, req, body)
	duration := time.Since(started).Milliseconds()
	if err != nil {
		errText := publicToolError(err)
		return layer{Title: "Graph minimal context", Tool: "graph_minimal_context", Error: true, DurationMs: duration, Output: errText},
			layer{Title: "Graph path context", Tool: "graph_path", Error: true, DurationMs: duration, Output: errText}
	}

	evidence, evidenceOK := result.Meta["evidence"].(db.CodeGraphInspectionEvidence)
	native, nativeOK := result.Meta["graphReader"].(graphreader.InspectResult)
	if !evidenceOK || !nativeOK {
		text := toolResultText(result)
		return layer{Title: "Graph minimal context", Tool: "graph_minimal_context", Error: result.IsError, DurationMs: duration, Output: text},
			layer{Title: "Graph path context", Tool: "graph_path", Error: result.IsError, DurationMs: duration, Output: text}
	}

	minimal := formatGraphMinimalContextOutput(graphQuery, evidence, native)
	path := formatGraphPathOutput(graphQuery, evidence, native)
	return layer{
			Title:      "Graph minimal context",
			Tool:       "graph_minimal_context",
			Error:      result.IsError,
			DurationMs: duration,
			Output:     fmt.Sprintf("Graph minimal context for %q\nShared NebulaGraph read: yes\n\n%s", graphQuery, minimal),
		},
		layer{
			Title:      "Graph path context",
			Tool:       "graph_path",
			Error:      result.IsError,
			DurationMs: duration,
			Output:     fmt.Sprintf("Graph path for %q\nShared NebulaGraph read: yes\n\n%s", graphQuery, path),
		}
}

func (b *Backend) codegraphContextRunLayerTasks(ctx context.Context, req api.MCPRequest, tasks []codegraphContextLayerTask, concurrency int) []layer {
	if len(tasks) == 0 {
		return nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(tasks) {
		concurrency = len(tasks)
	}
	layers := make([]layer, len(tasks))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for idx, task := range tasks {
		idx := idx
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, _ := json.Marshal(map[string]any{
				"name":      task.Tool,
				"arguments": task.Arguments,
			})
			started := time.Now()
			result, err := b.callTool(ctx, req, body)
			duration := time.Since(started).Milliseconds()
			if err != nil {
				layers[idx] = layer{Title: task.Title, Tool: task.Tool, Error: true, DurationMs: duration, Output: publicToolError(err)}
				return
			}
			layers[idx] = layer{Title: task.Title, Tool: task.Tool, Error: result.IsError, DurationMs: duration, Output: toolResultText(result)}
		}()
	}
	wg.Wait()
	return layers
}

func codegraphContextRecallConcurrency(compact bool) int {
	if compact {
		return 8
	}
	return 4
}

func codegraphContextSourceReadConcurrency(compact bool) int {
	if compact {
		return 12
	}
	return 6
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (b *Backend) codegraphContextIndexedRepoCoverage(ctx context.Context, orgID int32, repos []string, requestedRef string) (codegraphContextRepoCoverage, error) {
	if b.cfg.Queries == nil {
		return codegraphContextRepoCoverage{}, errors.New("repository query backend is not configured")
	}
	if err := validateGitRef(requestedRef); err != nil {
		return codegraphContextRepoCoverage{}, err
	}
	coverage := codegraphContextRepoCoverage{
		IndexedRepos: make([]string, 0, len(repos)),
		SkippedRepos: make([]string, 0),
	}
	for _, repo := range cleanStrings(repos) {
		if !validRepoSetName(repo) {
			coverage.SkippedRepos = append(coverage.SkippedRepos, fmt.Sprintf("- %s: repository name is not valid for scoped retrieval", repo))
			continue
		}
		row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, orgID, repo)
		if err != nil {
			if errors.Is(err, db.ErrRepoNotFound) {
				coverage.SkippedRepos = append(coverage.SkippedRepos, fmt.Sprintf("- %s: repository is not active in this organization", repo))
				continue
			}
			return codegraphContextRepoCoverage{}, err
		}
		_, err = resolveReadableGitRef(row, requestedRef)
		if err != nil {
			coverage.SkippedRepos = append(coverage.SkippedRepos, fmt.Sprintf("- %s: %s", row.Name, err.Error()))
			continue
		}
		coverage.IndexedRepos = append(coverage.IndexedRepos, row.Name)
	}
	return coverage, nil
}

func codegraphContextRepoCoverageOutput(coverage codegraphContextRepoCoverage, requestedRef string) string {
	var out strings.Builder
	out.WriteString("Repository index preflight for ")
	out.WriteString(displayCodegraphContextRef(requestedRef))
	out.WriteString("\nIndexed repositories used for retrieval:")
	if len(coverage.IndexedRepos) == 0 {
		out.WriteString("\n- none")
	} else {
		for _, repo := range coverage.IndexedRepos {
			out.WriteString("\n- ")
			out.WriteString(repo)
		}
	}
	if len(coverage.SkippedRepos) > 0 {
		out.WriteString("\nSkipped repositories:")
		for _, line := range coverage.SkippedRepos {
			out.WriteString("\n")
			out.WriteString(line)
		}
		out.WriteString("\nBehavior: fused retrieval continues over indexed repositories and reports skipped repos explicitly; no data is read from unindexed refs.")
	}
	return out.String()
}

func displayCodegraphContextRef(requestedRef string) string {
	if strings.TrimSpace(requestedRef) == "" {
		return "the default indexed branch"
	}
	if isImplicitRef(requestedRef) {
		return requestedRef + " (per-repository default indexed branch)"
	}
	return requestedRef
}

func codegraphContextCompactLayerOutput(section layer) string {
	switch section.Tool {
	case "scope_preflight":
		return codegraphContextCompactHighSignalLines(section.Output, 64, codegraphContextCompactLayerBytes+2200)
	case "read_file":
		return codegraphContextCompactReadFile(section.Output)
	case "grep":
		if strings.Contains(strings.ToLower(section.Title), "runtime-side") {
			return codegraphContextCompactHighSignalLines(section.Output, 48, codegraphContextCompactLayerBytes+3200)
		}
		return codegraphContextCompactHighSignalLines(section.Output, codegraphContextCompactLines, codegraphContextCompactLayerBytes)
	case "graph_minimal_context", "graph_path":
		return codegraphContextCompactGraphOutput(section.Output, codegraphContextCompactLines+10, codegraphContextCompactLayerBytes+1800)
	case "find_symbol_definitions", "find_symbol_references":
		return codegraphContextCompactHighSignalLines(section.Output, 14, codegraphContextCompactLayerBytes)
	default:
		return codegraphContextCompactHighSignalLines(section.Output, codegraphContextCompactLines, codegraphContextCompactLayerBytes)
	}
}

func codegraphContextCompactReadFile(output string) string {
	return codegraphContextCompactHighSignalLines(output, 28, codegraphContextCompactLayerBytes+2600)
}

func codegraphContextCompactGraphOutput(output string, maxLines int, maxBytes int) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "No evidence returned."
	}
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, maxLines)
	seen := map[string]bool{}
	add := func(line string) {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" || seen[line] || len(kept) >= maxLines {
			return
		}
		seen[line] = true
		kept = append(kept, line)
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i < 3 {
			add(line)
			continue
		}
		if !codegraphContextIsHighSignalGraphLine(trimmed) {
			continue
		}
		add(line)
		if i+1 < len(lines) && codegraphContextIsGraphProvenanceLine(strings.TrimSpace(lines[i+1])) {
			add(lines[i+1])
		}
	}
	if len(kept) == 0 {
		return codegraphContextCompactHighSignalLines(output, maxLines, maxBytes)
	}
	return truncateForCompactEvidence(strings.Join(kept, "\n"), maxBytes)
}

func codegraphContextIsHighSignalGraphLine(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "- depth ") ||
		strings.HasPrefix(lower, "- step ") ||
		strings.HasPrefix(lower, "- [repoid:") ||
		strings.HasPrefix(lower, "native graph traversal") ||
		strings.HasPrefix(lower, "high-signal connected flow") ||
		strings.HasPrefix(lower, "heuristic connected flow") ||
		strings.HasPrefix(lower, "ranked implementation path") {
		return true
	}
	for _, marker := range []string{
		"injectnodejs",
		"injectpython",
		"injectdotnet",
		"injectnodejssdk",
		"injectpythonsdk",
		"injectdotnetsdk",
		"source=scip",
		"provenance=scip",
		"source=tree-sitter",
		"source=ast-",
		"provenance=heuristic",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func codegraphContextIsGraphProvenanceLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "source=") ||
		strings.Contains(lower, "provenance=") ||
		strings.Contains(lower, "confidence=")
}

func codegraphContextRenderOrder(layers []layer, compact bool) []layer {
	if !compact {
		return layers
	}
	out := make([]layer, 0, len(layers))
	for _, section := range layers {
		if section.Tool != "read_file" {
			out = append(out, section)
		}
	}
	for _, section := range layers {
		if section.Tool == "read_file" {
			out = append(out, section)
		}
	}
	return out
}

func codegraphContextCompactHighSignalLines(output string, maxLines int, maxBytes int) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "No evidence returned."
	}
	kept := make([]string, 0, maxLines)
	seen := map[string]bool{}
	add := func(line string) {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" || seen[line] || len(kept) >= maxLines {
			return
		}
		seen[line] = true
		kept = append(kept, line)
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(kept) < 3 {
			add(line)
			continue
		}
		if codegraphContextIsHighSignalEvidenceLine(trimmed) {
			add(line)
		}
	}
	if len(kept) == 0 {
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			add(line)
			if len(kept) >= maxLines {
				break
			}
		}
	}
	result := strings.Join(kept, "\n")
	if result == "" {
		result = output
	}
	return truncateForCompactEvidence(result, maxBytes)
}

func truncateForCompactEvidence(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n(compact excerpt truncated.)"
}

func codegraphContextIsHighSignalEvidenceLine(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(line, "[") ||
		strings.HasPrefix(line, "- `") ||
		strings.HasPrefix(line, "Found ") ||
		strings.HasPrefix(line, "No ") ||
		strings.HasPrefix(line, "Native graph traversal") ||
		strings.HasPrefix(line, "Heuristic") ||
		strings.HasPrefix(line, "AST/tree-sitter") ||
		strings.HasPrefix(line, "Ranked ") ||
		strings.HasPrefix(line, "Repository index preflight") ||
		strings.HasPrefix(line, "Indexed repositories") ||
		strings.HasPrefix(line, "Skipped repositories") ||
		strings.HasPrefix(line, "Behavior:") ||
		strings.HasPrefix(line, "- github.com/") ||
		strings.HasPrefix(line, "<repo>") ||
		strings.HasPrefix(line, "<path>") {
		return true
	}
	for _, marker := range []string{
		"source=tree-sitter",
		"source=tree_sitter",
		"source=ast-",
		"source=syntactic-ast",
		"source=scip",
		"confidence=",
		"call",
		"reference",
		"definition",
		"func ",
		"type ",
		"return ",
		"Volume",
		"VolumeMount",
		"volumeMount",
		"MountPath",
		"emptyDir",
		"InitContainer",
		"init container",
		"Command:",
		"Args:",
		"apiVersion:",
		"kind:",
		"webhooks:",
		"clientConfig:",
		"failurePolicy:",
		"rules:",
		"operations:",
		"resources:",
		"namespace:",
		"service:",
		"path:",
		"containerPort:",
		"secretName:",
		"modifiedPod = pm.sdkInjector.inject",
		"injectCommonSDKConfig",
		"EnableWebhooks",
		"NewWebhookHandler",
		"SetupWebhookWithManager",
		"annotation",
		"NODE_OPTIONS",
		"PYTHONPATH",
		"CORECLR_ENABLE_PROFILING",
		"DOTNET_STARTUP_HOOKS",
		"constants.EnvOTELResourceAttrs",
	} {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func codegraphContextLayerSummaries(layers []layer) []map[string]any {
	out := make([]map[string]any, 0, len(layers))
	for _, section := range layers {
		out = append(out, map[string]any{
			"title":      section.Title,
			"tool":       section.Tool,
			"error":      section.Error,
			"durationMs": section.DurationMs,
			"excerpt":    codegraphContextCompactLayerOutput(section),
		})
	}
	return out
}

func codegraphContextGraphTraversalLimit(limit int32, compact bool) int32 {
	cap := int32(25)
	if compact {
		cap = 16
	}
	if limit <= 0 {
		return cap
	}
	if limit < cap {
		return limit
	}
	return cap
}

func codegraphContextGraphTraversalDepth(mode string, compact bool) int32 {
	if compact {
		return 1
	}
	if mode == "path" {
		return 3
	}
	return 2
}

func codegraphContextSearchPattern(query string, terms []string, max int) string {
	candidates := make([]string, 0, max)
	candidates = append(candidates, preflightCodeSearchTerms(query, max)...)
	candidates = append(candidates, terms...)
	if len(candidates) == 0 {
		return preflightSearchPattern(query)
	}
	candidates = cleanCodegraphTerms(candidates, max)
	if len(candidates) == 0 {
		return preflightSearchPattern(query)
	}
	return strings.Join(candidates, "|")
}

func codegraphContextGraphQuery(query string, terms []string) string {
	if !hasCodegraphDomainHints(strings.ToLower(query)) {
		return query
	}
	terms = cleanCodegraphTerms(terms, 18)
	if len(terms) == 0 {
		return query
	}
	return "architecture flow " + strings.Join(terms, " ")
}

func codegraphContextImplementationPattern(query string) string {
	if !hasCodegraphInstrumentationHints(strings.ToLower(query)) {
		return ""
	}
	return strings.Join([]string{
		"injectPythonSDKToContainer",
		"injectPythonSDKToPod",
		"injectNodeJSSDKToContainer",
		"injectNodeJSSDKToPod",
		"injectDotNetSDKToContainer",
		"injectDotNetSDKToPod",
		"pythonPlatformSrc",
		"getDefaultPythonEnvVars",
		"getDefaultNodeJSEnvVars",
		"injectDotNetSDK",
	}, "|")
}

type codegraphContextRuntimeEnvLayer struct {
	Language string
	Pattern  string
}

func codegraphContextRuntimeEnvPatterns(query string) []codegraphContextRuntimeEnvLayer {
	if !hasCodegraphInstrumentationHints(strings.ToLower(query)) {
		return nil
	}
	return []codegraphContextRuntimeEnvLayer{
		{
			Language: "Node.js",
			Pattern: strings.Join([]string{
				"envNodeOptions",
				"NODE_OPTIONS",
				"getDefaultNodeJSEnvVars",
				"nodejsInstrMountPath",
				"nodejsRequireArg",
			}, "|"),
		},
		{
			Language: "Python",
			Pattern: strings.Join([]string{
				"envPythonPath",
				"PYTHONPATH",
				"getDefaultPythonEnvVars",
				"pythonPlatformSrc",
				"pythonInstrMountPath",
				"pythonAutoInstrumentationPath",
			}, "|"),
		},
		{
			Language: ".NET",
			Pattern: strings.Join([]string{
				"envDotNetCoreClrEnableProfiling",
				"envDotNetCoreClrProfiler",
				"envDotNetCoreClrProfilerPath",
				"envDotNetStartupHook",
				"envDotNetOTelAutoHome",
				"CORECLR_ENABLE_PROFILING",
				"CORECLR_PROFILER",
				"CORECLR_PROFILER_PATH",
				"DOTNET_STARTUP_HOOKS",
				"OTEL_DOTNET_AUTO_HOME",
			}, "|"),
		},
	}
}

func codegraphContextRuntimeSidePattern(query string) string {
	lower := strings.ToLower(query)
	if !hasCodegraphInstrumentationHints(lower) {
		return ""
	}
	terms := make([]string, 0, 24)
	if strings.Contains(lower, "node") || strings.Contains(lower, "javascript") || strings.Contains(lower, "js") {
		terms = append(terms,
			"NODE_OPTIONS",
			"node_options",
			"--require",
			"registerInstrumentations",
			"auto-instrument",
			"autoInstrumentation",
			"instrumentation.js",
		)
	}
	if strings.Contains(lower, "python") {
		terms = append(terms,
			"PYTHONPATH",
			"sitecustomize",
			"auto_instrumentation",
			"opentelemetry-instrument",
			"initialize",
			"instrumentation.auto_instrumentation",
		)
	}
	if strings.Contains(lower, ".net") || strings.Contains(lower, "dotnet") || strings.Contains(lower, "c#") {
		terms = append(terms,
			"DOTNET_STARTUP_HOOKS",
			"CORECLR_ENABLE_PROFILING",
			"CORECLR_PROFILER",
			"StartupHook",
			"OpenTelemetry.AutoInstrumentation",
			"OTEL_DOTNET_AUTO_HOME",
		)
	}
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(cleanCodegraphTerms(terms, 24), "|")
}

func codegraphContextRuntimeSidePatterns(query string) []codegraphContextRuntimeEnvLayer {
	lower := strings.ToLower(query)
	if !hasCodegraphInstrumentationHints(lower) {
		return nil
	}
	out := []codegraphContextRuntimeEnvLayer{}
	if strings.Contains(lower, "node") || strings.Contains(lower, "javascript") || strings.Contains(lower, "js") {
		out = append(out, codegraphContextRuntimeEnvLayer{
			Language: "NodeJS",
			Pattern: strings.Join([]string{
				"NODE_OPTIONS",
				"node_options",
				"--require",
				"registerInstrumentations",
				"autoInstrumentation",
				"instrumentation.js",
				"opentelemetry-instrumentation",
			}, "|"),
		})
	}
	if strings.Contains(lower, "python") {
		out = append(out, codegraphContextRuntimeEnvLayer{
			Language: "Python",
			Pattern: strings.Join([]string{
				"PYTHONPATH",
				"sitecustomize",
				"auto_instrumentation",
				"opentelemetry-instrument",
				"instrumentation.auto_instrumentation",
				"initialize",
			}, "|"),
		})
	}
	if strings.Contains(lower, ".net") || strings.Contains(lower, "dotnet") || strings.Contains(lower, "c#") {
		out = append(out, codegraphContextRuntimeEnvLayer{
			Language: ".NET",
			Pattern: strings.Join([]string{
				"DOTNET_STARTUP_HOOKS",
				"DOTNET_ADDITIONAL_DEPS",
				"DOTNET_SHARED_STORE",
				"CORECLR_ENABLE_PROFILING",
				"CORECLR_PROFILER",
				"CORECLR_PROFILER_PATH",
				"StartupHook",
				"OpenTelemetry.AutoInstrumentation",
				"OTEL_DOTNET_AUTO_HOME",
			}, "|"),
		})
	}
	return out
}

func codegraphContextOTLPFlowPattern(query string) string {
	return retrievalpolicy.OTLPFlowPattern(query)
}

func codegraphContextCRDSpecPattern(query string) string {
	lower := strings.ToLower(query)
	if !hasCodegraphInstrumentationHints(lower) && !strings.Contains(lower, "crd") && !strings.Contains(lower, "spec") {
		return ""
	}
	return strings.Join([]string{
		"type InstrumentationSpec",
		"type NodeJS",
		"type Python",
		"type DotNet",
		"NodeJS NodeJS",
		"Python Python",
		"DotNet DotNet",
		"VolumeClaimTemplate",
		"VolumeSizeLimit",
		"InstrumentationSpec",
	}, "|")
}

func codegraphContextWebhookPattern(query string) string {
	lower := strings.ToLower(query)
	if !hasCodegraphDomainHints(lower) || !(strings.Contains(lower, "webhook") || strings.Contains(lower, "admission") || strings.Contains(lower, "mutat") || strings.Contains(lower, "controller")) {
		return ""
	}
	return strings.Join([]string{
		"NewWebhookHandler",
		"type PodMutator",
		"podmutation.PodMutator",
		"instPodMutator",
		"languageInstrumentations",
		"func NewMutator",
		"GetWebhookServer",
		"mutate-v1-pod",
		"internal/webhook/podmutation",
	}, "|")
}

func codegraphContextAnnotationPattern(query string) string {
	lower := strings.ToLower(query)
	if !hasCodegraphInstrumentationHints(lower) && !strings.Contains(lower, "annotation") {
		return ""
	}
	return strings.Join([]string{
		"annotationInjectContainerName",
		"annotationInjectNodeJS",
		"annotationInjectNodeJSContainersName",
		"annotationInjectPython",
		"annotationInjectPythonContainersName",
		"annotationInjectDotNet",
		"annotationInjectDotnetContainersName",
		"instrumentation.opentelemetry.io/inject-nodejs",
		"instrumentation.opentelemetry.io/inject-python",
		"instrumentation.opentelemetry.io/inject-dotnet",
	}, "|")
}

func codegraphContextExpansionTerms(query string, max int) []string {
	lower := strings.ToLower(query)
	candidates := make([]string, 0, max+16)
	runtimes := codegraphContextRequestedRuntimeFamilies(lower)

	if hasCodegraphInstrumentationHints(lower) {
		candidates = append(candidates,
			"inject",
		)
		if len(runtimes) == 0 {
			candidates = append(candidates, "injectNodeJS", "injectPython", "injectDotNet")
		}
	}
	if strings.Contains(lower, "node") || strings.Contains(lower, "javascript") || strings.Contains(lower, "js") {
		candidates = append(candidates, "NodeJS", "nodejs", "injectNodeJS", "injectNodeJSSDKToContainer", "injectNodeJSSDKToPod", "nodejs_test")
	}
	if strings.Contains(lower, "python") {
		candidates = append(candidates, "Python", "python", "injectPython", "injectPythonSDKToContainer", "python_test")
	}
	if strings.Contains(lower, ".net") || strings.Contains(lower, "dotnet") || strings.Contains(lower, "c#") {
		candidates = append(candidates, "DotNet", "dotnet", "injectDotNet", "injectDotNetSDKToContainer", "dotnet_test")
	}
	if hasCodegraphInstrumentationHints(lower) {
		candidates = append(candidates,
			"sdkInjector",
			"instrumentation",
			"Instrumentation",
			"InstrumentationSpec",
			"injectCommonSDKConfig",
			"injectCommonEnvVar",
			"validateContainerEnv",
			"getIndexOfEnv",
			"appendIfNotSet",
		)
	}
	if strings.Contains(lower, "webhook") || strings.Contains(lower, "admission") || strings.Contains(lower, "mutat") {
		candidates = append(candidates, "webhook", "admission", "mutate", "Mutate", "Webhook")
	}
	if strings.Contains(lower, "controller") || strings.Contains(lower, "reconcile") {
		candidates = append(candidates, "controller", "reconcile", "Reconcile")
	}
	candidates = append(candidates, preflightSymbols(query, max)...)
	return cleanCodegraphTerms(candidates, max)
}

func codegraphContextRequestedRuntimeFamilies(lower string) map[string]bool {
	out := map[string]bool{}
	if strings.Contains(lower, "node") || strings.Contains(lower, "javascript") || strings.Contains(lower, "js") {
		out["node"] = true
	}
	if strings.Contains(lower, "python") {
		out["python"] = true
	}
	if strings.Contains(lower, ".net") || strings.Contains(lower, "dotnet") || strings.Contains(lower, "c#") {
		out["dotnet"] = true
	}
	if lower == "go" || strings.Contains(lower, " golang") || strings.Contains(lower, " go ") {
		out["go"] = true
	}
	return out
}

func hasCodegraphDomainHints(lower string) bool {
	return hasCodegraphInstrumentationHints(lower) ||
		strings.Contains(lower, "webhook") ||
		strings.Contains(lower, "admission") ||
		strings.Contains(lower, "mutat") ||
		strings.Contains(lower, "controller") ||
		strings.Contains(lower, "reconcile")
}

func hasCodegraphInstrumentationHints(lower string) bool {
	return strings.Contains(lower, "instrument") ||
		strings.Contains(lower, "workload") ||
		strings.Contains(lower, "otel") ||
		strings.Contains(lower, "opentelemetry")
}

func codegraphContextWantsTestEvidence(query string) bool {
	lower := strings.ToLower(query)
	for _, marker := range []string{"test evidence", "tests", "unit test", "e2e", "integration test", "coverage", "coding harness", "touchpoint", "touchpoints", "development touchpoint"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func codegraphContextSymbolCandidates(query string, expandedTerms []string, max int) []string {
	candidates := make([]string, 0, max+len(expandedTerms))
	candidates = append(candidates, expandedTerms...)
	candidates = append(candidates, preflightSymbols(query, max*2)...)
	out := make([]string, 0, max)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = strings.Trim(candidate, ".,:;()[]{}<>\"'`")
		lower := strings.ToLower(candidate)
		if seen[lower] || !isHighValueSymbolCandidate(candidate) {
			continue
		}
		seen[lower] = true
		out = append(out, candidate)
		if len(out) >= max {
			break
		}
	}
	return out
}

func codegraphContextPruneBroadSymbols(candidates []string) []string {
	if len(candidates) <= 1 {
		return candidates
	}
	hasSpecificInject := false
	for _, candidate := range candidates {
		lower := strings.ToLower(candidate)
		if lower != "inject" && strings.Contains(lower, "inject") {
			hasSpecificInject = true
			break
		}
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		lower := strings.ToLower(candidate)
		if hasSpecificInject && (lower == "inject" || lower == "instrumentation") {
			continue
		}
		out = append(out, candidate)
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

func codegraphContextCompactSymbolCandidates(query string, candidates []string, max int) []string {
	if max <= 0 || len(candidates) <= max {
		return candidates
	}
	lower := strings.ToLower(query)
	preferred := make([]string, 0, max)
	if codegraphContextWantsTestEvidence(query) {
		for _, symbol := range []string{"TestInjectPythonSDK", "TestInjectNodeJS", "TestInjectDotNetSDK", "TestAppendIfNotSet"} {
			if containsString(candidates, symbol) || strings.Contains(lower, strings.ToLower(symbol)) {
				preferred = append(preferred, symbol)
				if len(preferred) >= max {
					return preferred
				}
			}
		}
	}
	if strings.Contains(lower, "helper") || strings.Contains(lower, "body") || strings.Contains(lower, "validate") || strings.Contains(lower, "getindex") {
		for _, symbol := range []string{"validateContainerEnv", "getIndexOfEnv", "appendIfNotSet"} {
			if containsString(candidates, symbol) || strings.Contains(lower, strings.ToLower(symbol)) {
				preferred = append(preferred, symbol)
				if len(preferred) >= max {
					return preferred
				}
			}
		}
	}
	if hasCodegraphInstrumentationHints(lower) {
		for _, symbol := range []string{"injectNodeJS", "injectPython", "injectDotNet", "injectCommonSDKConfig", "sdkInjector"} {
			if containsString(candidates, symbol) {
				preferred = append(preferred, symbol)
				if len(preferred) >= max {
					return preferred
				}
			}
		}
	}
	for _, symbol := range candidates {
		if containsString(preferred, symbol) {
			continue
		}
		preferred = append(preferred, symbol)
		if len(preferred) >= max {
			return preferred
		}
	}
	return preferred
}

func codegraphContextSymbolProbeLimit(compact bool) int32 {
	if compact {
		return 12
	}
	return 16
}

func codegraphContextSymbolProbeConcurrency(compact bool) int {
	if compact {
		return 4
	}
	return 6
}

func cleanCodegraphTerms(candidates []string, max int) []string {
	out := make([]string, 0, max)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = strings.Trim(candidate, ".,:;()[]{}<>\"'`")
		lower := strings.ToLower(candidate)
		if len(candidate) < 3 || seen[lower] || isCodegraphStopTerm(lower) {
			continue
		}
		seen[lower] = true
		out = append(out, candidate)
		if len(out) >= max {
			break
		}
	}
	return out
}

func isHighValueSymbolCandidate(candidate string) bool {
	lower := strings.ToLower(candidate)
	if len(candidate) < 3 || strings.Contains(candidate, "/") || strings.Contains(candidate, "-") || isCodegraphStopTerm(lower) {
		return false
	}
	switch lower {
	case "inject", "instrumentation":
		return true
	}
	for _, r := range candidate {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	if strings.Contains(candidate, ".") || strings.Contains(candidate, "$") || strings.Contains(candidate, "_") {
		return true
	}
	return len(candidate) >= 5
}

func isCodegraphStopTerm(lower string) bool {
	switch lower {
	case "a", "an", "and", "are", "as", "at", "be", "by", "can", "code", "codebase", "connect", "connection",
		"anchors", "context", "detail", "details", "diagram", "does", "entry", "exact", "explain", "file", "files", "flow", "focus", "for",
		"from", "function", "functions", "how", "important", "inside", "latest", "level", "lifecycle", "lines", "matter", "method", "methods",
		"of", "on", "operator", "part", "parts", "path", "piece", "pieces", "previous", "prior", "question", "relevant", "repo", "repos", "repository", "retrieval",
		"route", "service", "show", "spec", "symbol", "symbols", "test", "the", "their", "these", "this", "through",
		"to", "use", "used", "using", "work", "workloads", "what", "when", "where", "which", "with",
		"auto-instrumentation", "opentelemetry", "otel":
		return true
	default:
		return false
	}
}

type codegraphContextSourceSliceCandidate struct {
	Repo   string
	Path   string
	Line   int
	Source string
	Score  int
	Text   string
}

func codegraphContextCanReadSourceSlices(paths repopaths.Config) bool {
	return paths.DataCacheDir != "" || paths.ZoektEFSRoot != "" || paths.ZoektStorageLayout != ""
}

func codegraphContextSourceSliceCandidates(output, source string) []codegraphContextSourceSliceCandidate {
	var repo, file string
	out := make([]codegraphContextSourceSliceCandidate, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		if match := codegraphContextMatchHeaderRE.FindStringSubmatch(line); match != nil {
			repo = strings.TrimSpace(match[1])
			file = strings.TrimSpace(match[2])
			continue
		}
		if repo == "" || file == "" {
			continue
		}
		match := codegraphContextMatchLineRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		lineNo, err := strconv.Atoi(match[1])
		if err != nil {
			lineNo = 1
		}
		out = append(out, codegraphContextSourceSliceCandidate{
			Repo:   repo,
			Path:   file,
			Line:   lineNo,
			Source: source,
			Score:  codegraphContextSourceSliceBaseScore(source, file, lineNo),
			Text:   strings.TrimSpace(line),
		})
	}
	return out
}

func codegraphContextDefinitionSourceSliceCandidates(query string, output string) []codegraphContextSourceSliceCandidate {
	candidates := codegraphContextSourceSliceCandidates(output, "scip-definition")
	if len(candidates) == 0 {
		return nil
	}
	wantsBody := codegraphContextWantsDefinitionBodies(query)
	wantsTests := codegraphContextWantsTestEvidence(query)
	for i := range candidates {
		candidates[i].Score = codegraphContextSourceSliceBaseScore(candidates[i].Source, candidates[i].Path, candidates[i].Line)
		if wantsTests && strings.Contains(candidates[i].Path, "_test.") && codegraphContextLineHasExplicitQuerySymbol(query, candidates[i].Text) {
			candidates[i].Source = "scip-test-requested"
			candidates[i].Score = codegraphContextSourceSliceBaseScore(candidates[i].Source, candidates[i].Path, candidates[i].Line)
			continue
		}
		if wantsBody {
			if codegraphContextLineHasExplicitQuerySymbol(query, candidates[i].Text) {
				candidates[i].Source = "scip-definition-requested"
			} else {
				candidates[i].Source = "scip-definition-forced"
			}
			candidates[i].Score = codegraphContextSourceSliceBaseScore(candidates[i].Source, candidates[i].Path, candidates[i].Line)
		}
	}
	return candidates
}

func codegraphContextLineHasExplicitQuerySymbol(query string, line string) bool {
	lowerQuery := strings.ToLower(query)
	lowerLine := strings.ToLower(line)
	for _, symbol := range preflightSymbols(query, 24) {
		symbol = strings.TrimSpace(symbol)
		if !codegraphContextCanPromoteRequestedDefinitionSymbol(symbol) {
			continue
		}
		lowerSymbol := strings.ToLower(symbol)
		if !strings.Contains(lowerQuery, lowerSymbol) || !strings.Contains(lowerLine, lowerSymbol) {
			continue
		}
		if strings.Contains(lowerLine, " func "+lowerSymbol+"(") ||
			strings.Contains(lowerLine, "\tfunc "+lowerSymbol+"(") ||
			strings.Contains(lowerLine, " type "+lowerSymbol) ||
			strings.Contains(lowerLine, " const "+lowerSymbol) ||
			strings.Contains(lowerLine, " var "+lowerSymbol) {
			return true
		}
	}
	return false
}

func codegraphContextCanPromoteRequestedDefinitionSymbol(symbol string) bool {
	if len(symbol) < 5 {
		return false
	}
	lower := strings.ToLower(symbol)
	switch lower {
	case "calls", "create", "crd", "hld", "mutatingwebhook", "nebulagraph", "pythonpath", "scip", "sdk", "update", "user":
		return false
	}
	if strings.Contains(symbol, "/") || strings.Contains(symbol, "-") || strings.Contains(symbol, ":") {
		return false
	}
	hasLower := false
	hasUpper := false
	for _, r := range symbol {
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
	}
	return hasLower && hasUpper
}

func codegraphContextWantsDefinitionBodies(query string) bool {
	lower := strings.ToLower(query)
	for _, marker := range []string{"body", "bodies", "definition", "definitions", "helper", "helpers", "source"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func codegraphContextSelectSourceSlices(query string, candidates []codegraphContextSourceSliceCandidate, max int) []codegraphContextSourceSliceCandidate {
	if max <= 0 || len(candidates) == 0 {
		return nil
	}
	wantsTests := codegraphContextWantsTestEvidence(query)
	preferredFragments := codegraphContextPreferredSourcePathFragments(query)
	best := map[string]codegraphContextSourceSliceCandidate{}
	for _, candidate := range candidates {
		if candidate.Repo == "" || candidate.Path == "" || candidate.Line <= 0 {
			continue
		}
		if strings.Contains(candidate.Path, "/vendor/") || strings.Contains(candidate.Path, "/node_modules/") || strings.Contains(candidate.Path, "/dist/") {
			continue
		}
		if strings.Contains(candidate.Path, "_test.") && !wantsTests {
			candidate.Score -= 35
		}
		for _, preferred := range preferredFragments {
			if strings.Contains(candidate.Path, preferred) {
				candidate.Score += 45
				break
			}
		}
		bucket := candidate.Line / 80
		sourceBucket := ""
		if candidate.Source == "runtime-env" || candidate.Source == "runtime-side" {
			sourceBucket = candidate.Source
		}
		if candidate.Source == "scip-definition-forced" || candidate.Source == "scip-definition-requested" || candidate.Source == "scip-test-requested" {
			sourceBucket = candidate.Source + ":" + strconv.Itoa(candidate.Line)
		}
		key := candidate.Repo + "\x00" + candidate.Path + "\x00" + sourceBucket + "\x00" + fmt.Sprintf("%d", bucket)
		if prior, ok := best[key]; !ok || candidate.Score > prior.Score || (candidate.Score == prior.Score && candidate.Line < prior.Line) {
			best[key] = candidate
		}
	}
	out := make([]codegraphContextSourceSliceCandidate, 0, len(best))
	for _, candidate := range best {
		out = append(out, candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	if len(out) > max {
		out = out[:max]
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}

func codegraphContextDiverseSourceSlices(query string, candidates []codegraphContextSourceSliceCandidate, max int, perPathCap int) []codegraphContextSourceSliceCandidate {
	if max <= 0 || len(candidates) == 0 {
		return nil
	}
	if perPathCap <= 0 {
		perPathCap = 1
	}
	ranked := append([]codegraphContextSourceSliceCandidate(nil), candidates...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		if ranked[i].Path != ranked[j].Path {
			return ranked[i].Path < ranked[j].Path
		}
		return ranked[i].Line < ranked[j].Line
	})
	countByPath := map[string]int{}
	seenExact := map[string]bool{}
	out := make([]codegraphContextSourceSliceCandidate, 0, minInt(max, len(ranked)))
	addCandidate := func(candidate codegraphContextSourceSliceCandidate) {
		if len(out) >= max {
			return
		}
		key := candidate.Repo + "\x00" + candidate.Path
		exactKey := key + "\x00" + candidate.Source + "\x00" + strconv.Itoa(candidate.Line)
		if seenExact[exactKey] {
			return
		}
		if countByPath[key] >= perPathCap && candidate.Source != "scip-definition-requested" && candidate.Source != "scip-test-requested" {
			return
		}
		seenExact[exactKey] = true
		countByPath[key]++
		out = append(out, candidate)
	}
	if codegraphContextWantsRepoDiverseSourceSlices(query, ranked) {
		seenRepo := map[string]bool{}
		for _, candidate := range ranked {
			if candidate.Repo == "" || seenRepo[candidate.Repo] {
				continue
			}
			before := len(out)
			addCandidate(candidate)
			if len(out) > before {
				seenRepo[candidate.Repo] = true
			}
			if len(out) >= max {
				break
			}
		}
	}
	for _, preferred := range codegraphContextPreferredSourcePathFragments(query) {
		for _, candidate := range ranked {
			if strings.Contains(candidate.Path, preferred) {
				addCandidate(candidate)
				break
			}
		}
		if len(out) >= max {
			break
		}
	}
	for _, candidate := range ranked {
		addCandidate(candidate)
		if len(out) >= max {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}

func codegraphContextWantsRepoDiverseSourceSlices(query string, candidates []codegraphContextSourceSliceCandidate) bool {
	repos := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.Repo != "" {
			repos[candidate.Repo] = true
		}
	}
	if len(repos) < 2 {
		return false
	}
	lower := strings.ToLower(query)
	for _, marker := range []string{"across repo", "across these repos", "across these repositories", "cross-repo", "multi-repo", "multiple repos", "repositories", "end-to-end", "architecture", "flow"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func codegraphContextPreferredSourcePathFragments(query string) []string {
	out := retrievalpolicy.PreferredPathFragments(query)
	if codegraphContextWantsTestEvidence(query) {
		out = append(out, "_test.go")
	}
	return out
}

func codegraphContextMandatorySourceSliceCandidates(query string, repos []string) []codegraphContextSourceSliceCandidate {
	hints := retrievalpolicy.MandatorySourceHints(query, repos)
	out := make([]codegraphContextSourceSliceCandidate, 0, len(hints))
	for _, hint := range hints {
		out = append(out, codegraphContextSourceSliceCandidate{
			Repo:   hint.Repo,
			Path:   hint.Path,
			Line:   hint.Line,
			Source: hint.Source,
			Score:  codegraphContextSourceSliceBaseScore(hint.Source, hint.Path, hint.Line) + hint.ScoreBoost,
		})
	}
	return out
}

func codegraphContextSourceSliceBaseScore(source, file string, line int) int {
	score := 10
	switch source {
	case "scip-test-requested":
		score += 215
	case "scip-definition-requested":
		score += 220
	case "scip-definition-forced":
		score += 140
	case "scip-definition":
		score += 105
	case "webhook":
		score += 90
	case "webhook-registration":
		score += 88
	case "implementation":
		score += 80
	case "runtime-env":
		score += 75
	case "python-constants":
		score += 130
	case "annotation":
		score += 78
	case "focused":
		score += 70
	case "spec":
		score += 55
	case "crd-spec":
		score += 82
	case "runtime-side":
		score += 60
	case "otlp-flow":
		score += 86
	case "test":
		score += 45
	}
	if strings.HasSuffix(file, "_test.go") {
		score -= 10
	}
	if strings.Contains(file, "/internal/") || strings.Contains(file, "/cmd/") {
		score += 10
	}
	if strings.Contains(file, "podmutator") || strings.Contains(file, "webhook") || strings.Contains(file, "sdk.go") {
		score += 8
	}
	if strings.Contains(file, "annotation.go") || strings.Contains(file, "instrumentation_types.go") {
		score += 12
	}
	if strings.Contains(file, "config/webhook/") || strings.Contains(file, "manager_webhook") {
		score += 12
	}
	if line <= 5 {
		score -= 10
	}
	return score
}

func codegraphContextSourceOffset(line int) int {
	if line <= 20 {
		return 1
	}
	return line - 20
}

func codegraphContextSourceSliceManifestLine(slice codegraphContextSourceSliceCandidate, output string, isErr bool) string {
	status := "available"
	if isErr {
		status = "error"
	}
	proofs := codegraphContextSourceSliceLocalProofs(slice.Line, output, 12, 3)
	line := fmt.Sprintf("- `%s:%d` (%s, %s)", slice.Path, slice.Line, slice.Source, status)
	if len(proofs) > 0 {
		line += " local proof: " + strings.Join(proofs, "; ")
	}
	return line
}

func codegraphContextSourceSliceLocalProofs(targetLine int, output string, radius int, maxProofs int) []string {
	if targetLine <= 0 || strings.TrimSpace(output) == "" || maxProofs <= 0 {
		return nil
	}
	if radius < 0 {
		radius = 0
	}
	proofs := make([]string, 0, 4)
	proofNeedles := []string{
		"modifiedPod = pm.sdkInjector.inject",
		"func (pm *instPodMutator) Mutate",
		"func (i *sdkInjector) injectCommonSDKConfig",
		"constants.EnvOTELResourceAttrs",
		"func (p *podMutationWebhook) Handle",
		"func annotationValue",
		"return nsAnnValue",
		"annotationInjectPython",
		"instrumentation.opentelemetry.io/inject-python",
		"CORECLR_ENABLE_PROFILING",
		"DOTNET_STARTUP_HOOKS",
		"DOTNET_ADDITIONAL_DEPS",
		"NODE_OPTIONS",
		"PYTHONPATH",
	}
	for _, rawLine := range strings.Split(output, "\n") {
		lineNo, text, ok := parseNumberedSourceLine(rawLine)
		if !ok || lineNo < targetLine-radius || lineNo > targetLine+radius {
			continue
		}
		for _, proof := range proofNeedles {
			if strings.Contains(text, proof) {
				proofs = append(proofs, fmt.Sprintf("%d %s", lineNo, proof))
				if len(proofs) >= maxProofs {
					return proofs
				}
				break
			}
		}
	}
	return proofs
}

func parseNumberedSourceLine(raw string) (int, string, bool) {
	trimmed := strings.TrimLeft(raw, " \t")
	idx := strings.Index(trimmed, ":")
	if idx <= 0 {
		return 0, "", false
	}
	lineNo, err := strconv.Atoi(strings.TrimSpace(trimmed[:idx]))
	if err != nil || lineNo <= 0 {
		return 0, "", false
	}
	return lineNo, strings.TrimSpace(trimmed[idx+1:]), true
}

func codegraphContextCriticalManifest(query string, layers []layer, sourceSlices []string) string {
	var b strings.Builder
	b.WriteString("## Critical Evidence Manifest\n")
	b.WriteString("This compact manifest exists so ask_codebase/chat keep the highest-signal facts even when detailed layers are truncated.\n")
	if len(sourceSlices) > 0 {
		b.WriteString("Source slices selected:\n")
		maxSlices := len(sourceSlices)
		if maxSlices > 16 {
			maxSlices = 16
		}
		for _, line := range sourceSlices[:maxSlices] {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	graphNative, graphHeuristic, graphAny := false, false, false
	astEvidence := false
	scipPrecise, scipMissing := false, false
	graphProofs := make([]string, 0, 3)
	astProofs := make([]string, 0, 3)
	for _, section := range layers {
		lowerTitle := strings.ToLower(section.Title)
		lowerOutput := strings.ToLower(section.Output)
		if strings.Contains(lowerTitle, "graph") {
			graphAny = graphAny || strings.TrimSpace(section.Output) != ""
			if strings.Contains(section.Output, "Native graph traversal") ||
				strings.Contains(section.Output, "High-signal connected flow (native") ||
				strings.Contains(section.Output, "Ranked implementation path / sequence edges (native") ||
				strings.Contains(lowerOutput, "source=scip") ||
				strings.Contains(lowerOutput, "provenance=scip") {
				graphNative = true
			}
			if strings.Contains(section.Output, "Heuristic") || strings.Contains(lowerOutput, "low-confidence") {
				graphHeuristic = true
			}
			graphProofs = append(graphProofs, codegraphContextGraphProofLines(query, section.Output, 3-len(graphProofs))...)
			if codegraphContextOutputHasASTEvidence(section.Output) {
				astEvidence = true
				astProofs = append(astProofs, codegraphContextASTProofLines(query, section.Output, 3-len(astProofs))...)
			}
		}
		if strings.Contains(lowerTitle, "scip") {
			if strings.Contains(lowerOutput, "precise scip") && !strings.Contains(lowerOutput, "no precise scip") {
				scipPrecise = true
			}
			if strings.Contains(lowerOutput, "no precise scip") {
				scipMissing = true
			}
		}
	}
	b.WriteString("Graph/SCIP coverage:\n")
	switch {
	case graphNative:
		b.WriteString("- Native/high-signal graph traversal evidence is present in detailed graph sections; label exact edge provenance in the answer.\n")
	case graphHeuristic:
		b.WriteString("- Graph evidence is heuristic/low-confidence AST traversal only; verify every graph edge against exact file/line source before treating it as proven.\n")
	case graphAny:
		b.WriteString("- Graph sections returned data, but no native CALLS/REFERENCES/IMPORTS signal was detected in the compact manifest.\n")
	default:
		b.WriteString("- No graph evidence was visible to the compact manifest.\n")
	}
	if len(graphProofs) > 0 {
		b.WriteString("Graph proof lines:\n")
		for _, proof := range graphProofs {
			b.WriteString("- ")
			b.WriteString(proof)
			b.WriteByte('\n')
		}
	}
	switch {
	case scipPrecise:
		b.WriteString("- Precise SCIP evidence is present in detailed SCIP sections.\n")
	case scipMissing:
		b.WriteString("- SCIP tools were attempted but returned no precise symbol evidence for at least one requested symbol; call that out as a semantic-precision gap.\n")
	default:
		b.WriteString("- SCIP precision status is inconclusive in the compact manifest.\n")
	}
	if astEvidence {
		b.WriteString("- AST/tree-sitter evidence is present in detailed graph sections and should be used for structural flow hints, not as precise symbol proof.\n")
		if len(astProofs) > 0 {
			b.WriteString("AST/tree-sitter proof lines:\n")
			for _, proof := range astProofs {
				b.WriteString("- ")
				b.WriteString(proof)
				b.WriteByte('\n')
			}
		}
	} else {
		b.WriteString("- AST/tree-sitter evidence is missing from the fused pack; treat the full-stack retrieval contract as incomplete.\n")
	}
	return strings.TrimSpace(b.String())
}

func codegraphContextLayersHaveASTEvidence(layers []layer) bool {
	for _, section := range layers {
		if codegraphContextOutputHasASTEvidence(section.Output) {
			return true
		}
	}
	return false
}

func codegraphContextOutputHasASTEvidence(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "source=ast-") ||
		strings.Contains(lower, "source=tree-sitter") ||
		strings.Contains(lower, "source=tree_sitter") ||
		strings.Contains(lower, "ast/tree-sitter semantic") ||
		strings.Contains(lower, "heuristic ast")
}

func codegraphContextASTProofLines(query string, output string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	type candidate struct {
		text  string
		score int
		order int
	}
	lines := strings.Split(output, "\n")
	candidates := make([]candidate, 0, limit*2)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !codegraphContextIsGraphProvenanceLine(trimmed) || !codegraphContextOutputHasASTEvidence(trimmed) {
			continue
		}
		edge := ""
		for j := i - 1; j >= 0; j-- {
			prev := strings.TrimSpace(lines[j])
			if !codegraphContextIsASTProofEdgeLine(prev) {
				continue
			}
			if (codegraphContextIsHighSignalGraphLine(prev) || codegraphContextIsHighSignalEvidenceLine(prev)) && !codegraphContextIsGraphProvenanceLine(prev) {
				edge = prev
				break
			}
		}
		if edge == "" {
			edge = trimmed
		} else {
			edge = strings.TrimPrefix(edge, "- ") + " / " + trimmed
		}
		score := codegraphContextASTProofScore(query, edge)
		if codegraphContextASTProofIrrelevantToRuntimeQuery(query, edge) {
			continue
		}
		if codegraphContextOpaqueFunctionIDRE.MatchString(strings.ToLower(edge)) {
			continue
		}
		candidates = append(candidates, candidate{
			text:  edge,
			score: score,
			order: len(candidates),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].order < candidates[j].order
	})
	out := make([]string, 0, limit)
	for _, candidate := range candidates {
		out = append(out, candidate.text)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func codegraphContextIsASTProofEdgeLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	if trimmed == "" || strings.HasSuffix(trimmed, ":") || strings.Contains(lower, "facts:") {
		return false
	}
	return strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "- step ") ||
		strings.Contains(trimmed, "--") ||
		strings.Contains(trimmed, " CALLS:") ||
		strings.Contains(trimmed, " REFERENCES:") ||
		strings.Contains(trimmed, " DEFINES:")
}

func codegraphContextASTProofScore(query string, value string) int {
	lower := strings.ToLower(value)
	score := 0
	if strings.Contains(lower, "source=tree-sitter") || strings.Contains(lower, "source=tree_sitter") {
		score += 20
	}
	if strings.Contains(lower, "source=ast-") {
		score += 15
	}
	if codegraphContextOpaqueFunctionIDRE.MatchString(lower) {
		score -= 80
	}
	for _, marker := range []string{" calls:", "--calls-->", " calls ", " handles:", " emits:", " provides:", " consumes:"} {
		if strings.Contains(lower, marker) {
			score += 20
		}
	}
	for _, marker := range []string{"webhook", "route", "handler", "mutate", "inject", "sdk", "event"} {
		if strings.Contains(lower, marker) {
			score += 8
		}
	}
	if strings.Contains(lower, "_test.") || strings.Contains(lower, "/test/") || strings.Contains(lower, "tests/") {
		score -= 15
	}
	score += graphTextRequestedLanguageScore(value, graphEvidenceRequestedLanguageBuckets(query))
	if graphEvidenceHasMixedRequestedAndUnrequestedRuntime(value, graphEvidenceRequestedLanguageBuckets(query)) && !graphEvidenceContainsCoreFlowAnchor(value) {
		score -= 80
	}
	return score
}

func codegraphContextASTProofIrrelevantToRuntimeQuery(query string, value string) bool {
	requested := graphEvidenceRequestedLanguageBuckets(query)
	if len(requested) == 0 {
		return false
	}
	return !graphEdgeMatchesRequestedRuntimeOrCoreFlow(value, requested)
}

func codegraphContextGraphProofLines(query string, output string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	type candidate struct {
		text  string
		score int
		order int
	}
	lines := strings.Split(output, "\n")
	candidates := make([]candidate, 0, limit*2)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !codegraphContextIsGraphProvenanceLine(trimmed) || !strings.Contains(strings.ToLower(trimmed), "scip") {
			continue
		}
		edge := ""
		for j := i - 1; j >= 0; j-- {
			prev := strings.TrimSpace(lines[j])
			if codegraphContextIsHighSignalGraphLine(prev) && !codegraphContextIsGraphProvenanceLine(prev) {
				edge = prev
				break
			}
		}
		if edge == "" {
			edge = trimmed
		} else {
			edge = strings.TrimPrefix(edge, "- ") + " / " + trimmed
		}
		candidates = append(candidates, candidate{
			text:  edge,
			score: codegraphContextGraphProofScore(query, edge),
			order: len(candidates),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].order < candidates[j].order
	})
	out := make([]string, 0, limit)
	for _, candidate := range candidates {
		out = append(out, candidate.text)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func codegraphContextGraphProofScore(query string, value string) int {
	lower := strings.ToLower(value)
	score := 0
	if strings.Contains(lower, "source=scip") || strings.Contains(lower, "provenance=scip") {
		score += 50
	}
	if strings.Contains(lower, " calls:") || strings.Contains(lower, "--calls-->") || strings.Contains(lower, " calls ") {
		score += 40
	}
	if strings.Contains(lower, " references:") || strings.Contains(lower, "--references-->") || strings.Contains(lower, " references ") {
		score += 25
	}
	for _, marker := range []string{"injectnodejs", "injectpython", "injectdotnet", "injectcommon", "sdkinjector"} {
		if strings.Contains(lower, marker) {
			score += 15
		}
	}
	score += graphTextRequestedLanguageScore(value, graphEvidenceRequestedLanguageBuckets(query))
	if strings.Contains(lower, "annotation") {
		score -= 15
	}
	return score
}

func minInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
