package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/db"
	"codeintel/internal/graphreader"
	"codeintel/pkg/llmproxy"
	"codeintel/pkg/repoindexstatus"
	"codeintel/pkg/repopaths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	jsonRPCVersion = "2.0"

	readFileMaxLines       = 800
	readFileMaxBytes       = 16 * 1024
	readFileMaxSource      = 4 << 20
	searchResponseMaxBytes = 4 << 20
	maxLineLength          = 2000
	maxRepoPathLength      = 4096
	defaultTreeDepth       = 1
	maxTreeDepth           = 10
	defaultMaxTreeEntries  = 1000
	maxTreeEntries         = 10000
	defaultReadConcurrency = 8
	defaultReadTimeout     = 8 * time.Second
)

var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,255}$`)

type Querier interface {
	ListOrgRepos(ctx context.Context, p db.ListOrgReposParams) ([]db.RepoListRow, error)
	CountOrgRepos(ctx context.Context, p db.CountOrgReposParams) (int32, error)
	ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]db.OrgLanguageModelRow, error)
	GetOrgRepoForRead(ctx context.Context, orgID int32, repoName string) (db.RepoReadRow, error)
	GetOrgSecretCiphertext(ctx context.Context, orgID int32, key string) (db.OrgSecretCiphertext, error)
	FindOrgSymbolOccurrences(ctx context.Context, p db.FindOrgSymbolOccurrencesParams) ([]db.CodeIntelOccurrenceEvidence, error)
	ListActiveCodeGraphScopes(ctx context.Context, p db.ListActiveCodeGraphScopesParams) ([]db.CodeGraphActiveScope, error)
	InspectOrgCodeGraph(ctx context.Context, p db.InspectOrgCodeGraphParams) (db.CodeGraphInspectionEvidence, error)
}

type Config struct {
	Queries             Querier
	SearchBackend       api.SearchBackend
	GraphReader         graphreader.Inspector
	LanguageModelClient LanguageModelClient
	Paths               repopaths.Config
	Logger              *slog.Logger
	EncryptionKey       string
	HTTPClient          *http.Client
	ReadConcurrency     int
	ReadTimeout         time.Duration
	AskMaxSteps         int
	AskTimeout          time.Duration
	AskMaxAttempts      int
	GraphEvidenceCache  GraphEvidenceCache
	GraphEvidenceTTL    time.Duration
	CompactGraphTimeout time.Duration

	// AllowedModelBaseURLs is an optional egress allow-list for
	// OpenAI-compatible model endpoints. Entries may be origins
	// (https://api.example.com) or path prefixes. When empty, only
	// public HTTPS endpoints are accepted; private/internal endpoints
	// must be explicitly allow-listed by deployment config.
	AllowedModelBaseURLs []string
}

type LanguageModelClient interface {
	CompleteChat(ctx context.Context, req llmproxy.ChatRequest) (llmproxy.ChatResponse, error)
}

type asyncLanguageModelClient interface {
	StartChat(ctx context.Context, req llmproxy.ChatRequest) (llmproxy.ChatResponse, error)
	GetChat(ctx context.Context, orgID int32, requestID string) (llmproxy.ChatResponse, error)
}

type Backend struct {
	cfg     Config
	logger  *slog.Logger
	readSem chan struct{}
}

func NewBackend(cfg Config) *Backend {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ReadConcurrency <= 0 {
		cfg.ReadConcurrency = defaultReadConcurrency
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.GraphEvidenceCache != nil && cfg.GraphEvidenceTTL <= 0 {
		cfg.GraphEvidenceTTL = 2 * time.Minute
	}
	return &Backend{
		cfg:     cfg,
		logger:  logger.With("component", "mcp"),
		readSem: make(chan struct{}, cfg.ReadConcurrency),
	}
}

func (b *Backend) graphEvidenceCacheTTL() time.Duration {
	if b == nil || b.cfg.GraphEvidenceTTL <= 0 {
		return 2 * time.Minute
	}
	return b.cfg.GraphEvidenceTTL
}

func (b *Backend) compactGraphTimeout() time.Duration {
	if b == nil {
		return 0
	}
	return b.cfg.CompactGraphTimeout
}

func (b *Backend) Handle(ctx context.Context, req api.MCPRequest) (api.MCPResponse, error) {
	if b == nil {
		return api.MCPResponse{}, api.ErrMCPBackendNotConfigured
	}
	if req.Method == http.MethodGet {
		return b.jsonResponse(http.StatusMethodNotAllowed, rpcError(nil, -32000, "GET sessions are not enabled in stateless mode", nil)), nil
	}
	if req.Method != http.MethodPost {
		return b.jsonResponse(http.StatusMethodNotAllowed, rpcError(nil, -32000, "method not allowed", nil)), nil
	}
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return b.jsonResponse(http.StatusBadRequest, rpcError(nil, -32700, "empty JSON-RPC body", nil)), nil
	}

	trimmed := bytes.TrimSpace(req.Body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return b.jsonResponse(http.StatusOK, rpcError(nil, -32600, "JSON-RPC batch requests are not supported by this MCP endpoint", nil)), nil
	}
	var call rpcRequest
	if err := json.Unmarshal(trimmed, &call); err != nil {
		return b.jsonResponse(http.StatusOK, rpcError(nil, -32700, "parse error", nil)), nil
	}
	resp, ok := b.handleCall(ctx, req, call)
	if !ok {
		return api.MCPResponse{StatusCode: http.StatusAccepted, ContentType: "application/json", Body: []byte{}}, nil
	}
	return b.jsonResponse(http.StatusOK, resp), nil
}

func (b *Backend) handleCall(ctx context.Context, req api.MCPRequest, call rpcRequest) (rpcResponse, bool) {
	if call.JSONRPC != jsonRPCVersion {
		return rpcError(call.ID, -32600, "invalid JSON-RPC version", nil), call.hasID()
	}
	switch call.Method {
	case "initialize":
		protocolVersion := negotiateProtocolVersion(call.Params)
		if protocolVersion == "" {
			return rpcError(call.ID, -32602, "unsupported MCP protocol version", nil), call.hasID()
		}
		return rpcResult(call.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{
					"listChanged": false,
				},
			},
			"serverInfo": map[string]any{
				"name":    "codeintel-mcp",
				"version": "0.1.0",
			},
		}), call.hasID()
	case "notifications/initialized":
		return rpcResponse{}, false
	case "tools/list":
		return rpcResult(call.ID, map[string]any{"tools": b.tools(ctx, req.OrgID, true)}), call.hasID()
	case "tools/call":
		result, err := b.callTool(ctx, req, call.Params)
		if err != nil {
			var protoErr *protocolError
			if errors.As(err, &protoErr) {
				return rpcError(call.ID, protoErr.Code, protoErr.Message, nil), call.hasID()
			}
			return rpcResult(call.ID, toolError(publicToolError(err))), call.hasID()
		}
		return rpcResult(call.ID, result), call.hasID()
	default:
		return rpcError(call.ID, -32601, "method not found", nil), call.hasID()
	}
}

func (b *Backend) callTool(ctx context.Context, req api.MCPRequest, params json.RawMessage) (toolResult, error) {
	var body struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return toolResult{}, &protocolError{Code: -32602, Message: "invalid tools/call params"}
	}
	if body.Name == "" {
		return toolResult{}, &protocolError{Code: -32602, Message: "tools/call requires a tool name"}
	}
	if len(body.Arguments) == 0 || bytes.Equal(bytes.TrimSpace(body.Arguments), []byte("null")) {
		body.Arguments = []byte(`{}`)
	}
	switch body.Name {
	case "list_repos":
		return b.toolListRepos(ctx, req, body.Arguments)
	case "list_language_models":
		return b.toolListLanguageModels(ctx, req)
	case "grep":
		return b.toolGrep(ctx, req, body.Arguments)
	case "read_file":
		return b.toolReadFile(ctx, req, body.Arguments)
	case "list_tree":
		return b.toolListTree(ctx, req, body.Arguments)
	case "compare_branches":
		return b.toolCompareBranches(ctx, req, body.Arguments)
	case "find_symbol_definitions":
		return b.toolFindSymbolDefinitions(ctx, req, body.Arguments)
	case "find_symbol_references":
		return b.toolFindSymbolReferences(ctx, req, body.Arguments)
	case "inspect_code_graph":
		return b.toolInspectCodeGraph(ctx, req, body.Arguments)
	case "codegraph_context":
		return b.toolCodegraphContext(ctx, req, body.Arguments)
	case "graph_callers":
		return b.toolGraphCallers(ctx, req, body.Arguments)
	case "graph_callees":
		return b.toolGraphCallees(ctx, req, body.Arguments)
	case "graph_impact":
		return b.toolGraphImpact(ctx, req, body.Arguments)
	case "graph_path":
		return b.toolGraphPath(ctx, req, body.Arguments)
	case "graph_minimal_context":
		return b.toolGraphMinimalContext(ctx, req, body.Arguments)
	case "graph_status":
		return b.toolGraphStatus(ctx, req, body.Arguments)
	case "ask_codebase":
		result, err := b.toolAskCodebase(ctx, req, body.Arguments)
		if err != nil {
			return toolResult{}, fmt.Errorf("Failed to ask codebase: %w", err)
		}
		return result, nil
	case "get_ask_codebase_result":
		result, err := b.toolGetAskCodebaseResult(ctx, req, body.Arguments)
		if err != nil {
			return toolResult{}, fmt.Errorf("Failed to get ask_codebase result: %w", err)
		}
		return result, nil
	default:
		return toolResult{}, &protocolError{Code: -32602, Message: "unknown tool " + strconv.Quote(body.Name)}
	}
}

func (b *Backend) tools(ctx context.Context, orgID int32, includeAsk bool) []toolInfo {
	var tools []toolInfo
	if b.cfg.Queries != nil {
		tools = append(tools, toolInfo{
			Name:        "list_repos",
			Description: "List repositories available in the authenticated organization.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"query":     stringSchema("Filter repositories by name."),
				"page":      integerSchema("Page number. Defaults to 1."),
				"perPage":   integerSchema("Page size, maximum 100. Defaults to 30."),
				"sort":      enumSchema([]string{"name", "indexedAt", "pushed"}, "Sort field."),
				"direction": enumSchema([]string{"asc", "desc"}, "Sort direction."),
			}, nil),
		})
		tools = append(tools, toolInfo{
			Name:        "list_language_models",
			Description: "List language models configured for the authenticated organization.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(nil, nil),
		})
		if includeAsk && b.canRunFullAskCodebase(ctx, orgID) {
			tools = append(tools, toolInfo{
				Name:        "ask_codebase",
				Description: "Ask a development-grade natural-language question about indexed code. The agent uses MCP tools to search code, read files, compare indexed branches, and synthesize exact implementation context.",
				Annotations: readOnlyAnnotations(),
				InputSchema: objectSchema(map[string]any{
					"query":         stringSchema("The development question to ask about the codebase."),
					"repos":         arraySchema(stringSchema("Repository name."), "Optional repositories accessible to the agent. Omit to allow all org repositories."),
					"ref":           stringSchema("Optional indexed branch, tag, or commit. When omitted, each repository default branch is used."),
					"languageModel": objectSchema(map[string]any{"provider": stringSchema("Provider id."), "model": stringSchema("Model id."), "displayName": stringSchema("Optional display name.")}, nil),
					"async":         boolSchema("When true, enqueue durable backend synthesis and return a request id immediately; poll get_ask_codebase_result for completion. Durable gateways default to async unless explicitly set to false."),
				}, []string{"query"}),
			})
			if _, ok := b.cfg.LanguageModelClient.(asyncLanguageModelClient); ok {
				tools = append(tools, toolInfo{
					Name:        "get_ask_codebase_result",
					Description: "Fetch a durable ask_codebase synthesis request by id. Returns IN_PROGRESS, FAILED, or the final answer.",
					Annotations: readOnlyAnnotations(),
					InputSchema: objectSchema(map[string]any{
						"requestId": stringSchema("Durable ask_codebase request id returned by async ask_codebase."),
					}, []string{"requestId"}),
				})
			}
		}
	}
	if b.cfg.SearchBackend != nil {
		tools = append(tools, toolInfo{
			Name:        "grep",
			Description: "Search indexed code with broad text recall.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"pattern":     stringSchema("Regular expression to search for."),
				"path":        stringSchema("Directory or file path filter."),
				"include":     stringSchema("Glob of files to include."),
				"repo":        stringSchema("Repository name."),
				"repos":       arraySchema(stringSchema("Repository name."), "Optional repository names to search as one repo set."),
				"ref":         stringSchema("Branch, tag, or commit."),
				"limit":       integerSchema("Maximum matches."),
				"groupByRepo": boolSchema("Group results by repository."),
			}, []string{"pattern"}),
		})
	}
	if b.cfg.Queries != nil {
		tools = append(tools, toolInfo{
			Name:        "read_file",
			Description: "Read a file from an indexed repository checkout.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"path":   stringSchema("Path to the file."),
				"repo":   stringSchema("Repository name."),
				"ref":    stringSchema("Branch, tag, or commit. Defaults to HEAD."),
				"offset": integerSchema("One-based line offset."),
				"limit":  integerSchema("Maximum lines."),
			}, []string{"path", "repo"}),
		})
		tools = append(tools, toolInfo{
			Name:        "list_tree",
			Description: "List files and directories from an indexed repository path.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"repo":               stringSchema("Repository name."),
				"path":               stringSchema("Directory path relative to the repository root. Defaults to repo root."),
				"ref":                stringSchema("Branch, tag, or commit. Defaults to HEAD/default indexed branch."),
				"depth":              integerSchema("Directory levels to traverse, maximum 10. Defaults to 1."),
				"includeFiles":       boolSchema("Whether to include file entries. Defaults to true."),
				"includeDirectories": boolSchema("Whether to include directory entries. Defaults to true."),
				"maxEntries":         integerSchema("Maximum entries, maximum 10000. Defaults to 1000."),
			}, []string{"repo"}),
		})
		tools = append(tools, toolInfo{
			Name:        "compare_branches",
			Description: "Compare indexed branches or revisions in one repository. Returns explicit not-indexed status when a requested branch is unavailable.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"repo":          stringSchema("Repository name."),
				"baseRef":       stringSchema("Base branch, tag, or commit. Defaults to HEAD/default indexed branch."),
				"headRef":       stringSchema("Head branch, tag, or commit to compare."),
				"headRefs":      arraySchema(stringSchema("Head branch, tag, or commit to compare."), "Multiple head branches, tags, or commits to compare against baseRef."),
				"includeDiff":   boolSchema("Include a bounded unified diff excerpt."),
				"maxFiles":      integerSchema("Maximum changed files to list. Defaults to 50, maximum 200."),
				"maxPatchBytes": integerSchema("Maximum unified diff bytes when includeDiff is true. Defaults to 12000, maximum 64000."),
			}, []string{"repo"}),
		})
	}
	if b.cfg.Queries != nil && b.cfg.SearchBackend != nil {
		tools = append(tools, toolInfo{
			Name:        "find_symbol_definitions",
			Description: "Find precise SCIP-backed symbol definitions, fused with supplemental Zoekt text matches for broad recall.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"symbol":         stringSchema("The symbol to find definitions of."),
				"repo":           stringSchema("Optional repository name to scope the search to."),
				"repos":          arraySchema(stringSchema("Repository name."), "Optional repository names to search with each repository's default indexed branch."),
				"revision":       stringSchema("Optional indexed branch, tag, or revision. Defaults to the repo default indexed branch when repo is set."),
				"definitionFile": stringSchema("Optional file path expected to define this symbol; use for same-name symbols."),
				"limit":          integerSchema("Maximum precise SCIP occurrences to return. Defaults to 25, maximum 100."),
			}, []string{"symbol"}),
		})
		tools = append(tools, toolInfo{
			Name:        "find_symbol_references",
			Description: "Find precise SCIP-backed symbol references, fused with supplemental Zoekt text matches for broad recall.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"symbol":         stringSchema("The symbol to find references to."),
				"repo":           stringSchema("Optional repository name to scope the search to."),
				"repos":          arraySchema(stringSchema("Repository name."), "Optional repository names to search with each repository's default indexed branch."),
				"revision":       stringSchema("Optional indexed branch, tag, or revision. Defaults to the repo default indexed branch when repo is set."),
				"definitionFile": stringSchema("Optional definition file path to disambiguate same-name symbols."),
				"limit":          integerSchema("Maximum precise SCIP occurrences to return. Defaults to 25, maximum 100."),
			}, []string{"symbol"}),
		})
	}
	if b.cfg.Queries != nil && b.cfg.GraphReader != nil {
		if b.cfg.SearchBackend != nil {
			tools = append(tools, toolInfo{
				Name:        "codegraph_context",
				Description: "Return one fused development context pack for a code question: Zoekt broad recall, SCIP definitions/references, AST/tree-sitter graph facts, and NebulaGraph traversal.",
				Annotations: readOnlyAnnotations(),
				InputSchema: objectSchema(map[string]any{
					"query":   stringSchema("Natural-language development question, symbol, route, event, package, or file flow to investigate."),
					"repo":    stringSchema("Optional repository name to scope context to."),
					"repos":   arraySchema(stringSchema("Repository name."), "Optional repository names to scope context across multiple selected repositories."),
					"ref":     stringSchema("Optional indexed branch, tag, or revision. Defaults to default indexed branch for single-repo scope."),
					"depth":   integerSchema("Optional traversal depth hint, maximum 6."),
					"limit":   integerSchema("Maximum evidence rows per layer. Defaults to 25, maximum 100."),
					"compact": boolSchema("When true, return a compact ranked proof pack for ask_codebase/chat synthesis instead of the full debug evidence dump."),
				}, []string{"query"}),
			})
		}
		tools = append(tools, toolInfo{
			Name:        "inspect_code_graph",
			Description: "Inspect active code graph evidence for architecture, lifecycle, impact, dependency, route, event, package, and cross-repository questions using Postgres graph metadata plus NebulaGraph traversal.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"query": stringSchema("Natural-language or symbol/event/package/config query to inspect in the active graph."),
				"repo":  stringSchema("Optional repository name to scope graph inspection to."),
				"repos": arraySchema(stringSchema("Repository name."), "Optional repository names to scope graph inspection across multiple selected repositories."),
				"ref":   stringSchema("Optional indexed branch, tag, or revision. Defaults to default indexed branch for single-repo scope."),
				"depth": integerSchema("Optional traversal depth hint, maximum 6."),
				"limit": integerSchema("Maximum graph evidence rows per category. Defaults to 25, maximum 100."),
			}, []string{"query"}),
		})
		graphScopeSchema := map[string]any{
			"query":  stringSchema("Symbol, route, event, file, package, or natural-language seed for the graph task."),
			"symbol": stringSchema("Optional symbol seed. Alias for query for compatibility with caller/callee graph clients."),
			"seed":   stringSchema("Optional graph seed. Alias for query for impact/status clients."),
			"depth":  integerSchema("Optional traversal depth hint. Current bounded graph traversal chooses safe intent-specific limits."),
			"repo":   stringSchema("Optional repository name to scope graph inspection to."),
			"repos":  arraySchema(stringSchema("Repository name."), "Optional repository names to scope graph inspection across multiple selected repositories."),
			"ref":    stringSchema("Optional indexed branch, tag, or revision. Defaults to default indexed branch for single-repo scope."),
			"limit":  integerSchema("Maximum graph evidence rows per category. Defaults to 25, maximum 100."),
		}
		tools = append(tools, toolInfo{
			Name:        "graph_callers",
			Description: "Find graph-backed incoming callers/references for a symbol, route, event, file, or package using active Postgres graph metadata plus NebulaGraph traversal.",
			Annotations: readOnlyAnnotations(),
			InputSchema: graphTaskSchema(graphScopeSchema),
		})
		tools = append(tools, toolInfo{
			Name:        "graph_callees",
			Description: "Find graph-backed outgoing callees/dependencies from a symbol, route, event, file, or package using active Postgres graph metadata plus NebulaGraph traversal.",
			Annotations: readOnlyAnnotations(),
			InputSchema: graphTaskSchema(graphScopeSchema),
		})
		tools = append(tools, toolInfo{
			Name:        "graph_impact",
			Description: "Trace graph-backed blast radius and impacted files/services/tests for a change seed using active Postgres graph metadata plus NebulaGraph traversal.",
			Annotations: readOnlyAnnotations(),
			InputSchema: graphTaskSchema(graphScopeSchema),
		})
		tools = append(tools, toolInfo{
			Name:        "graph_path",
			Description: "Trace a graph-backed implementation path or sequence between services, routes, events, packages, files, or symbols using active Postgres graph metadata plus NebulaGraph traversal.",
			Annotations: readOnlyAnnotations(),
			InputSchema: graphTaskSchema(graphScopeSchema),
		})
		tools = append(tools, toolInfo{
			Name:        "graph_minimal_context",
			Description: "Return a compact graph-backed development context pack: ranked flows, critical files, symbols, and next reads for agentic code changes.",
			Annotations: readOnlyAnnotations(),
			InputSchema: graphTaskSchema(graphScopeSchema),
		})
		tools = append(tools, toolInfo{
			Name:        "graph_status",
			Description: "Report active graph snapshot coverage for the selected organization, repositories, and indexed revision scope.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"repo":  stringSchema("Optional repository name to scope graph status to."),
				"repos": arraySchema(stringSchema("Repository name."), "Optional repository names to inspect as one selected set."),
				"ref":   stringSchema("Optional indexed branch, tag, or revision. Defaults to default indexed branch for single-repo scope."),
				"limit": integerSchema("Maximum status rows. Defaults to 25, maximum 100."),
			}, nil),
		})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

func (b *Backend) deterministicTools(ctx context.Context, orgID int32) []toolInfo {
	tools := b.tools(ctx, orgID, false)
	out := tools[:0]
	for _, tool := range tools {
		if tool.Name == "list_language_models" {
			continue
		}
		out = append(out, tool)
	}
	return out
}

func (b *Backend) hasEnabledLanguageModel(ctx context.Context, orgID int32) bool {
	if b.cfg.Queries == nil {
		return false
	}
	rows, err := b.cfg.Queries.ListEnabledOrgLanguageModels(ctx, orgID)
	return err == nil && len(rows) > 0
}

func (b *Backend) canRunFullAskCodebase(ctx context.Context, orgID int32) bool {
	return b.cfg.Queries != nil &&
		b.cfg.SearchBackend != nil &&
		b.cfg.GraphReader != nil &&
		b.cfg.LanguageModelClient != nil &&
		b.hasEnabledLanguageModel(ctx, orgID)
}

func (b *Backend) toolListRepos(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	var args struct {
		Query     string `json:"query"`
		Page      *int32 `json:"page"`
		PerPage   *int32 `json:"perPage"`
		Sort      string `json:"sort"`
		Direction string `json:"direction"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid list_repos arguments")
	}
	page := int32(1)
	if args.Page != nil {
		if *args.Page <= 0 {
			return toolResult{}, fmt.Errorf("page must be a positive integer")
		}
		page = *args.Page
	}
	perPage := int32(30)
	if args.PerPage != nil {
		if *args.PerPage <= 0 || *args.PerPage > 100 {
			return toolResult{}, fmt.Errorf("perPage must be a positive integer no greater than 100")
		}
		perPage = *args.PerPage
	}
	sortField := db.ReposSortName
	switch args.Sort {
	case "", "name":
		sortField = db.ReposSortName
	case "pushed":
		sortField = db.ReposSortPushedAt
	case "indexedAt":
		sortField = db.ReposSortIndexedAt
	default:
		return toolResult{}, fmt.Errorf("sort must be one of: name, indexedAt, pushed")
	}
	direction := db.ReposSortAsc
	switch args.Direction {
	case "", "asc":
		direction = db.ReposSortAsc
	case "desc":
		direction = db.ReposSortDesc
	default:
		return toolResult{}, fmt.Errorf("direction must be one of: asc, desc")
	}
	rows, err := b.cfg.Queries.ListOrgRepos(ctx, db.ListOrgReposParams{
		OrgID:     req.OrgID,
		Query:     args.Query,
		Skip:      (page - 1) * perPage,
		Take:      perPage,
		Sort:      sortField,
		Direction: direction,
	})
	if err != nil {
		return toolResult{}, err
	}
	total, err := b.cfg.Queries.CountOrgRepos(ctx, db.CountOrgReposParams{OrgID: req.OrgID, Query: args.Query})
	if err != nil {
		return toolResult{}, err
	}
	type repoItem struct {
		Name          string  `json:"name"`
		URL           *string `json:"url"`
		PushedAt      *string `json:"pushedAt"`
		DefaultBranch *string `json:"defaultBranch"`
		IsFork        bool    `json:"isFork"`
		IsArchived    bool    `json:"isArchived"`
	}
	out := struct {
		Repos      []repoItem `json:"repos"`
		TotalCount int32      `json:"totalCount"`
	}{Repos: make([]repoItem, 0, len(rows)), TotalCount: total}
	for _, row := range rows {
		var pushed *string
		if row.PushedAt != nil {
			s := formatMillis(*row.PushedAt)
			pushed = &s
		}
		out.Repos = append(out.Repos, repoItem{
			Name:          row.RepoName,
			URL:           optionalNonBlankString(row.WebUrl),
			PushedAt:      pushed,
			DefaultBranch: row.DefaultBranch,
			IsFork:        row.IsFork,
			IsArchived:    row.IsArchived,
		})
	}
	return jsonToolResult(out)
}

func optionalNonBlankString(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return value
}

func (b *Backend) toolListLanguageModels(ctx context.Context, req api.MCPRequest) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("model query backend is not configured")
	}
	rows, err := b.cfg.Queries.ListEnabledOrgLanguageModels(ctx, req.OrgID)
	if err != nil {
		return toolResult{}, err
	}
	type modelInfo struct {
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		DisplayName string `json:"displayName,omitempty"`
	}
	models := make([]modelInfo, 0, len(rows))
	for _, row := range rows {
		var cfg modelInfo
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return toolResult{}, fmt.Errorf("decode model config: %w", err)
		}
		models = append(models, cfg)
	}
	return jsonToolResult(models)
}

func (b *Backend) toolGrep(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.SearchBackend == nil {
		return toolResult{}, api.ErrSearchBackendNotConfigured
	}
	var args struct {
		Pattern     string   `json:"pattern"`
		Path        string   `json:"path"`
		Include     string   `json:"include"`
		Repo        string   `json:"repo"`
		Repos       []string `json:"repos"`
		Ref         string   `json:"ref"`
		Limit       *int64   `json:"limit"`
		GroupByRepo bool     `json:"groupByRepo"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid grep arguments")
	}
	if args.Pattern == "" {
		return toolResult{}, fmt.Errorf("grep pattern is required")
	}
	if args.Repo != "" && len(cleanStrings(args.Repos)) > 0 {
		return toolResult{}, fmt.Errorf("grep accepts either repo or repos, not both")
	}
	if args.Repo == "" && args.Ref == "" {
		if repos := cleanStrings(args.Repos); len(repos) > 0 {
			output, err := b.toolGrepDefaultRefsPerRepo(ctx, req, args.Pattern, args.Path, args.Include, repos, args.Limit)
			if err != nil {
				return toolResult{}, err
			}
			return textToolResult(output), nil
		}
		if b.cfg.Queries != nil {
			rows, err := b.cfg.Queries.ListOrgRepos(ctx, db.ListOrgReposParams{
				OrgID:     req.OrgID,
				Take:      defaultAskRepoLimit + 1,
				Sort:      db.ReposSortName,
				Direction: db.ReposSortAsc,
			})
			if err != nil {
				return toolResult{}, err
			}
			if len(rows) > defaultAskRepoLimit {
				return toolResult{}, fmt.Errorf("grep without repo spans more than %d active repositories; pass repo or repos for branch-coherent search", defaultAskRepoLimit)
			}
			repos := make([]string, 0, len(rows))
			for _, row := range rows {
				repos = append(repos, row.RepoName)
			}
			if len(repos) > 0 {
				output, err := b.toolGrepDefaultRefsPerRepo(ctx, req, args.Pattern, args.Path, args.Include, repos, args.Limit)
				if err != nil {
					return toolResult{}, err
				}
				return textToolResult(output), nil
			}
		}
	}
	limit := int64(100)
	if args.GroupByRepo {
		limit = 10000
	}
	if args.Limit != nil {
		if *args.Limit <= 0 {
			return toolResult{}, fmt.Errorf("limit must be a positive integer")
		}
		limit = *args.Limit
	}
	query := `"` + strings.ReplaceAll(args.Pattern, `"`, `\"`) + `"`
	if args.Path != "" {
		query += " file:" + regexp.QuoteMeta(args.Path)
	}
	if args.Include != "" {
		query += " file:" + globToRegex(args.Include)
	}
	if args.Repo != "" {
		resolved, err := b.resolveGrepRepoRef(ctx, req.OrgID, args.Repo, args.Ref)
		if err != nil {
			return toolResult{}, err
		}
		query += " repo:" + regexp.QuoteMeta(args.Repo)
		query += " branch:" + zoektQueryRef(resolved.GitRef)
	} else if repos := cleanStrings(args.Repos); len(repos) > 0 {
		if b.cfg.Queries == nil {
			return toolResult{}, errors.New("repository query backend is not configured")
		}
		var resolvedRef string
		for _, repo := range repos {
			if !validRepoSetName(repo) {
				return toolResult{}, fmt.Errorf("repository %q is not valid for a repo-set search", repo)
			}
			row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, repo)
			if err != nil {
				if errors.Is(err, db.ErrRepoNotFound) {
					return toolResult{}, fmt.Errorf("repository %q not found", repo)
				}
				return toolResult{}, err
			}
			if args.Ref != "" {
				if isImplicitRef(args.Ref) {
					output, err := b.toolGrepDefaultRefsPerRepo(ctx, req, args.Pattern, args.Path, args.Include, repos, args.Limit)
					if err != nil {
						return toolResult{}, err
					}
					return textToolResult(output), nil
				}
				resolved, err := resolveReadableGitRef(row, args.Ref)
				if err != nil {
					return toolResult{}, err
				}
				if resolvedRef == "" {
					resolvedRef = zoektQueryRef(resolved.GitRef)
				}
			}
		}
		query += " reposet:" + strings.Join(repos, ",")
		if args.Ref != "" {
			query += " branch:" + resolvedRef
		}
	} else if args.Ref != "" {
		return toolResult{}, fmt.Errorf("grep ref requires repo or repos so indexed branch status can be verified")
	}

	body, err := b.cfg.SearchBackend.Search(ctx, api.SearchRequest{
		OrgID:     req.OrgID,
		OrgDomain: req.OrgDomain,
		Query:     query,
		Options: map[string]any{
			"matches":                  limit,
			"contextLines":             0,
			"isRegexEnabled":           true,
			"isCaseSensitivityEnabled": true,
		},
	})
	if err != nil {
		return toolResult{}, err
	}
	if len(body) > searchResponseMaxBytes {
		return toolResult{}, errors.New("search response exceeded MCP output budget")
	}
	var search searchResponse
	if err := json.Unmarshal(body, &search); err != nil {
		return toolResult{}, fmt.Errorf("decode search response: %w", err)
	}
	output := formatGrepOutput(search, args.GroupByRepo)
	return textToolResult(output), nil
}

func (b *Backend) toolGrepDefaultRefsPerRepo(ctx context.Context, req api.MCPRequest, pattern, path, include string, repos []string, limit *int64) (string, error) {
	type defaultRefGroup struct {
		ref        string
		displayRef string
		repos      []string
	}
	groupsByRef := map[string]*defaultRefGroup{}
	groupOrder := make([]string, 0)
	for _, repo := range cleanStrings(repos) {
		if !validRepoSetName(repo) {
			return "", fmt.Errorf("repository %q is not valid for a repo-set search", repo)
		}
		row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, repo)
		if err != nil {
			if errors.Is(err, db.ErrRepoNotFound) {
				return "", fmt.Errorf("repository %q not found", repo)
			}
			return "", err
		}
		resolved, err := resolveReadableGitRef(row, "")
		if err != nil {
			return "", err
		}
		key := resolved.GitRef
		group := groupsByRef[key]
		if group == nil {
			group = &defaultRefGroup{ref: resolved.GitRef, displayRef: resolved.DisplayRef}
			groupsByRef[key] = group
			groupOrder = append(groupOrder, key)
		}
		group.repos = append(group.repos, row.Name)
	}
	var lines []string
	lines = append(lines, "Per-repository default-branch Zoekt recall:")
	for _, key := range groupOrder {
		group := groupsByRef[key]
		body := map[string]any{
			"pattern": pattern,
			"ref":     group.ref,
		}
		if len(group.repos) == 1 {
			body["repo"] = group.repos[0]
		} else {
			body["repos"] = group.repos
		}
		if path != "" {
			body["path"] = path
		}
		if include != "" {
			body["include"] = include
		}
		if limit != nil {
			body["limit"] = *limit
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		result, err := b.toolGrep(ctx, req, raw)
		if err != nil {
			return "", err
		}
		if len(group.repos) == 1 {
			lines = append(lines, "", fmt.Sprintf("Repository %s (%s):", group.repos[0], group.displayRef), toolResultText(result))
		} else {
			lines = append(lines, "", fmt.Sprintf("Default branch group %s (%d repositories): %s", group.displayRef, len(group.repos), strings.Join(group.repos, ", ")), toolResultText(result))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

func (b *Backend) resolveGrepRepoRef(ctx context.Context, orgID int32, repoName, requestedRef string) (resolvedGitRef, error) {
	if b.cfg.Queries == nil {
		return resolvedGitRef{}, errors.New("repository query backend is not configured")
	}
	row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, orgID, repoName)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			return resolvedGitRef{}, fmt.Errorf("repository %q not found", repoName)
		}
		return resolvedGitRef{}, err
	}
	return resolveReadableGitRef(row, requestedRef)
}

func (b *Backend) toolReadFile(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	var args struct {
		Path   string `json:"path"`
		Repo   string `json:"repo"`
		Ref    string `json:"ref"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid read_file arguments")
	}
	if args.Path == "" || args.Repo == "" {
		return toolResult{}, fmt.Errorf("read_file requires repo and path")
	}
	cleanPath, err := cleanRepoPath(args.Path)
	if err != nil {
		return toolResult{}, err
	}
	row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, args.Repo)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			return toolResult{}, fmt.Errorf("repository %q not found", args.Repo)
		}
		return toolResult{}, err
	}
	repoPath, _, err := b.cfg.Paths.RepoPath(repopaths.Repo{
		OrgID:        row.OrgID,
		RepoID:       row.ID,
		CloneURL:     row.CloneURL,
		CodeHostType: row.CodeHostType,
	})
	if err != nil {
		return toolResult{}, err
	}
	if err := ensureRepoPathAllowed(repoPath, b.cfg.Paths); err != nil {
		return toolResult{}, err
	}
	resolved, err := resolveReadableGitRef(row, args.Ref)
	if err != nil {
		return toolResult{}, err
	}
	readCtx, cancel := context.WithTimeout(ctx, b.cfg.ReadTimeout)
	defer cancel()
	if err := b.acquireRead(readCtx); err != nil {
		return toolResult{}, err
	}
	defer b.releaseRead()
	content, _, sourceTruncated, err := readGitFile(readCtx, repoPath, resolved.GitRef, cleanPath)
	if err != nil {
		return toolResult{}, err
	}
	offset := 1
	if args.Offset != nil {
		if *args.Offset <= 0 {
			return toolResult{}, fmt.Errorf("offset must be a positive integer")
		}
		offset = *args.Offset
	}
	limit := readFileMaxLines
	if args.Limit != nil {
		if *args.Limit <= 0 || *args.Limit > readFileMaxLines {
			return toolResult{}, fmt.Errorf("limit must be a positive integer no greater than %d", readFileMaxLines)
		}
		limit = *args.Limit
	}
	output, start, end, truncated := formatFileOutput(row.Name, cleanPath, content, offset, limit, sourceTruncated)
	meta := map[string]any{
		"path":        cleanPath,
		"repo":        row.Name,
		"startLine":   start,
		"endLine":     end,
		"isTruncated": truncated,
		"ref":         resolved.DisplayRef,
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: output}},
		Meta:    meta,
	}, nil
}

func (b *Backend) toolListTree(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	var args struct {
		Repo               string `json:"repo"`
		Path               string `json:"path"`
		Ref                string `json:"ref"`
		Depth              *int   `json:"depth"`
		IncludeFiles       *bool  `json:"includeFiles"`
		IncludeDirectories *bool  `json:"includeDirectories"`
		MaxEntries         *int   `json:"maxEntries"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid list_tree arguments")
	}
	if strings.TrimSpace(args.Repo) == "" {
		return toolResult{}, fmt.Errorf("list_tree requires repo")
	}
	cleanPath, err := cleanTreePath(args.Path)
	if err != nil {
		return toolResult{}, err
	}
	depth := defaultTreeDepth
	if args.Depth != nil {
		if *args.Depth <= 0 || *args.Depth > maxTreeDepth {
			return toolResult{}, fmt.Errorf("depth must be a positive integer no greater than %d", maxTreeDepth)
		}
		depth = *args.Depth
	}
	maxEntries := defaultMaxTreeEntries
	if args.MaxEntries != nil {
		if *args.MaxEntries <= 0 || *args.MaxEntries > maxTreeEntries {
			return toolResult{}, fmt.Errorf("maxEntries must be a positive integer no greater than %d", maxTreeEntries)
		}
		maxEntries = *args.MaxEntries
	}
	includeFiles := true
	if args.IncludeFiles != nil {
		includeFiles = *args.IncludeFiles
	}
	includeDirectories := true
	if args.IncludeDirectories != nil {
		includeDirectories = *args.IncludeDirectories
	}

	row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, args.Repo)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			return toolResult{}, fmt.Errorf("repository %q not found", args.Repo)
		}
		return toolResult{}, err
	}
	resolved, err := resolveReadableGitRef(row, args.Ref)
	if err != nil {
		return toolResult{}, err
	}
	if !includeFiles && !includeDirectories {
		return jsonToolResult(map[string]any{
			"repo":          row.Name,
			"ref":           resolved.DisplayRef,
			"path":          cleanPath,
			"entries":       []listTreeEntry{},
			"totalReturned": 0,
			"truncated":     false,
		})
	}
	repoPath, _, err := b.cfg.Paths.RepoPath(repopaths.Repo{
		OrgID:        row.OrgID,
		RepoID:       row.ID,
		CloneURL:     row.CloneURL,
		CodeHostType: row.CodeHostType,
	})
	if err != nil {
		return toolResult{}, err
	}
	if err := ensureRepoPathAllowed(repoPath, b.cfg.Paths); err != nil {
		return toolResult{}, err
	}

	readCtx, cancel := context.WithTimeout(ctx, b.cfg.ReadTimeout)
	defer cancel()
	if err := b.acquireRead(readCtx); err != nil {
		return toolResult{}, err
	}
	defer b.releaseRead()
	entries, truncated, err := listGitTree(readCtx, repoPath, resolved.GitRef, cleanPath, depth, includeFiles, includeDirectories, maxEntries)
	if err != nil {
		return toolResult{}, err
	}
	return jsonToolResult(map[string]any{
		"repo":          row.Name,
		"ref":           resolved.DisplayRef,
		"path":          cleanPath,
		"entries":       entries,
		"totalReturned": len(entries),
		"truncated":     truncated,
	})
}

func (b *Backend) toolCompareBranches(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	var args struct {
		Repo          string   `json:"repo"`
		BaseRef       string   `json:"baseRef"`
		HeadRef       string   `json:"headRef"`
		HeadRefs      []string `json:"headRefs"`
		IncludeDiff   bool     `json:"includeDiff"`
		MaxFiles      *int     `json:"maxFiles"`
		MaxPatchBytes *int     `json:"maxPatchBytes"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid compare_branches arguments")
	}
	if args.Repo == "" {
		return toolResult{}, fmt.Errorf("compare_branches requires repo")
	}
	headRefs := cleanStrings(append(args.HeadRefs, args.HeadRef))
	if len(headRefs) == 0 {
		return toolResult{}, fmt.Errorf("compare_branches requires headRef or headRefs")
	}
	maxFiles := 50
	if args.MaxFiles != nil {
		if *args.MaxFiles <= 0 || *args.MaxFiles > 200 {
			return toolResult{}, fmt.Errorf("maxFiles must be a positive integer no greater than 200")
		}
		maxFiles = *args.MaxFiles
	}
	maxPatchBytes := 12000
	if args.MaxPatchBytes != nil {
		if *args.MaxPatchBytes <= 0 || *args.MaxPatchBytes > 64000 {
			return toolResult{}, fmt.Errorf("maxPatchBytes must be a positive integer no greater than 64000")
		}
		maxPatchBytes = *args.MaxPatchBytes
	}

	row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, args.Repo)
	if err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			return toolResult{}, fmt.Errorf("repository %q not found", args.Repo)
		}
		return toolResult{}, err
	}
	if readBlockedByRemoveIndex(row) {
		return toolResult{
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Branch comparison for %s cannot run because remove-index is active or failed for this repository. Reindex the selected branch from Atom before requesting checkout-backed MCP code access.", row.Name)}},
			IsError: true,
			Meta: map[string]any{
				"repo":   row.Name,
				"status": "blocked_by_remove_index",
			},
		}, nil
	}
	repoPath, _, err := b.cfg.Paths.RepoPath(repopaths.Repo{
		OrgID:        row.OrgID,
		RepoID:       row.ID,
		CloneURL:     row.CloneURL,
		CodeHostType: row.CodeHostType,
	})
	if err != nil {
		return toolResult{}, err
	}
	if err := ensureRepoPathAllowed(repoPath, b.cfg.Paths); err != nil {
		return toolResult{}, err
	}

	requestedBase := args.BaseRef
	if requestedBase == "" {
		requestedBase = "HEAD"
	}
	type resolvedHeadRef struct {
		Requested string
		Resolved  resolvedGitRef
	}
	headResolved := make([]resolvedHeadRef, 0, len(headRefs))
	unavailable := make([]string, 0)
	baseResolved, err := resolveComparisonGitRef(row, args.BaseRef)
	if err != nil {
		unavailable = append(unavailable, fmt.Sprintf("- baseRef %q: %s", requestedBase, err.Error()))
	}
	for _, headRef := range headRefs {
		resolved, err := resolveComparisonGitRef(row, headRef)
		if err != nil {
			unavailable = append(unavailable, fmt.Sprintf("- headRef %q: %s", headRef, err.Error()))
			continue
		}
		headResolved = append(headResolved, resolvedHeadRef{Requested: headRef, Resolved: resolved})
	}
	if len(unavailable) > 0 {
		return branchUnavailableResult(row.Name, unavailable), nil
	}

	readCtx, cancel := context.WithTimeout(ctx, b.cfg.ReadTimeout)
	defer cancel()
	if err := b.acquireRead(readCtx); err != nil {
		return toolResult{}, err
	}
	defer b.releaseRead()

	gitRepo, err := git.PlainOpen(repoPath)
	if err != nil {
		return toolResult{}, fmt.Errorf("open repository: %w", err)
	}
	baseHash, baseResolvedName, err := resolveRevision(gitRepo, baseResolved.GitRef)
	if err != nil {
		return branchUnavailableResult(row.Name, []string{fmt.Sprintf("- baseRef %q: %s", requestedBase, err.Error())}), nil
	}
	baseCommit, err := gitRepo.CommitObject(*baseHash)
	if err != nil {
		return toolResult{}, fmt.Errorf("load base commit: %w", err)
	}
	type resolvedHeadCommit struct {
		Requested    string
		Resolved     resolvedGitRef
		ResolvedName string
		Hash         plumbing.Hash
		Commit       *object.Commit
	}
	headCommits := make([]resolvedHeadCommit, 0, len(headResolved))
	unavailable = unavailable[:0]
	for _, head := range headResolved {
		headHash, headResolvedName, err := resolveRevision(gitRepo, head.Resolved.GitRef)
		if err != nil {
			unavailable = append(unavailable, fmt.Sprintf("- headRef %q: %s", head.Requested, err.Error()))
			continue
		}
		headCommit, err := gitRepo.CommitObject(*headHash)
		if err != nil {
			return toolResult{}, fmt.Errorf("load head commit: %w", err)
		}
		headCommits = append(headCommits, resolvedHeadCommit{
			Requested:    head.Requested,
			Resolved:     head.Resolved,
			ResolvedName: headResolvedName,
			Hash:         *headHash,
			Commit:       headCommit,
		})
	}
	if len(unavailable) > 0 {
		return branchUnavailableResult(row.Name, unavailable), nil
	}

	lines := []string{
		fmt.Sprintf("Branch comparison for %s", row.Name),
		fmt.Sprintf("Base: requested %q -> %s (%s)", requestedBase, baseResolvedName, baseHash.String()),
		fmt.Sprintf("Base index coverage: %s", formatIndexCoverage(row, args.BaseRef, baseResolved)),
		"Diff source: managed git checkout. Zoekt/SCIP/graph indexed search remains limited to revisions marked indexed below.",
	}
	metaComparisons := make([]map[string]any, 0, len(headResolved))
	for _, head := range headCommits {
		comparison, err := compareCommits(readCtx, baseCommit, head.Commit, maxFiles, args.IncludeDiff, maxPatchBytes)
		if err != nil {
			return toolResult{}, err
		}
		requestedHead := head.Requested
		lines = append(lines,
			"",
			fmt.Sprintf("Head: requested %q -> %s (%s)", requestedHead, head.ResolvedName, head.Hash.String()),
			fmt.Sprintf("Head index coverage: %s", formatIndexCoverage(row, requestedHead, head.Resolved)),
			"Summary:",
			fmt.Sprintf("- commits ahead: %d; commits behind: %d", comparison.CommitsAhead, comparison.CommitsBehind),
			fmt.Sprintf("- files changed: %d; additions: %d; deletions: %d", comparison.TotalFiles, comparison.Additions, comparison.Deletions),
			"Changed files:",
		)
		if len(comparison.Files) == 0 {
			lines = append(lines, "- (no file changes)")
		} else {
			for _, file := range comparison.Files {
				lines = append(lines, fmt.Sprintf("- %s (+%d -%d)", file.Path, file.Additions, file.Deletions))
			}
			if comparison.TotalFiles > len(comparison.Files) {
				lines = append(lines, fmt.Sprintf("- ... %d more files omitted by maxFiles", comparison.TotalFiles-len(comparison.Files)))
			}
		}
		if args.IncludeDiff {
			lines = append(lines, "", "Unified diff excerpt:", "```diff", comparison.PatchExcerpt, "```")
			if comparison.PatchTruncated {
				lines = append(lines, fmt.Sprintf("(Diff excerpt capped at %d bytes.)", maxPatchBytes))
			}
		}
		metaComparisons = append(metaComparisons, map[string]any{
			"requestedHead":  requestedHead,
			"resolvedHead":   head.ResolvedName,
			"headCommit":     head.Hash.String(),
			"headIndexed":    isRefIndexed(row, requestedHead, head.Resolved),
			"commitsAhead":   comparison.CommitsAhead,
			"commitsBehind":  comparison.CommitsBehind,
			"filesChanged":   comparison.TotalFiles,
			"additions":      comparison.Additions,
			"deletions":      comparison.Deletions,
			"patchTruncated": comparison.PatchTruncated,
		})
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: strings.Join(lines, "\n")}},
		Meta: map[string]any{
			"repo":         row.Name,
			"status":       "ok",
			"baseRef":      requestedBase,
			"resolvedBase": baseResolvedName,
			"baseCommit":   baseHash.String(),
			"baseIndexed":  isRefIndexed(row, args.BaseRef, baseResolved),
			"comparisons":  metaComparisons,
		},
	}, nil
}

func branchUnavailableResult(repoName string, failures []string) toolResult {
	lines := []string{
		fmt.Sprintf("Branch comparison for %s cannot run because one or more requested revisions are not available in the managed git checkout.", repoName),
		"",
		"Git checkout failures:",
	}
	lines = append(lines, failures...)
	lines = append(lines, "", "No diff was computed. Sync/fetch the missing branch or revision from Atom, then retry this MCP tool. Indexing is not required for branch diff itself; it is required for indexed search and semantic tools on that revision.")
	return toolResult{
		Content: []toolContent{{Type: "text", Text: strings.Join(lines, "\n")}},
		IsError: true,
		Meta: map[string]any{
			"repo":   repoName,
			"status": "not_available",
		},
	}
}

func (b *Backend) jsonResponse(status int, value any) api.MCPResponse {
	body, err := json.Marshal(value)
	if err != nil {
		b.logger.Error("marshal mcp response", "err", err)
		body = []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal error"},"id":null}`)
	}
	return api.MCPResponse{
		StatusCode:  status,
		ContentType: "application/json",
		Body:        body,
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r rpcRequest) hasID() bool {
	return len(bytes.TrimSpace(r.ID)) > 0
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErrorObject `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

type rpcErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type protocolError struct {
	Code    int
	Message string
}

func (e *protocolError) Error() string {
	return e.Message
}

func negotiateProtocolVersion(params json.RawMessage) string {
	const latest = "2025-06-18"
	if len(bytes.TrimSpace(params)) == 0 {
		return latest
	}
	var body struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return ""
	}
	switch body.ProtocolVersion {
	case "", "2025-06-18":
		return latest
	case "2025-03-26":
		return "2025-03-26"
	default:
		return ""
	}
}

func rpcResult(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: jsonRPCVersion, Result: result, ID: normalizeID(id)}
}

func rpcError(id json.RawMessage, code int, message string, data any) rpcResponse {
	return rpcResponse{JSONRPC: jsonRPCVersion, Error: &rpcErrorObject{Code: code, Message: message, Data: data}, ID: normalizeID(id)}
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(id)) == 0 {
		return json.RawMessage("null")
	}
	return id
}

type toolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

type toolResult struct {
	Content []toolContent  `json:"content"`
	IsError bool           `json:"isError,omitempty"`
	Meta    map[string]any `json:"_meta,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func toolError(message string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: message}}, IsError: true}
}

func publicToolError(err error) string {
	if err == nil {
		return "Tool execution failed."
	}
	msg := err.Error()
	allowedPrefixes := []string{
		"grep pattern is required",
		"grep accepts either repo or repos",
		"grep ref requires repo or repos",
		"grep without repo spans",
		"invalid file path",
		"invalid git reference",
		"page must be",
		"perPage must be",
		"sort must be",
		"direction must be",
		"limit must be",
		"offset must be",
		"read_file requires repo and path",
		"list_tree requires repo",
		"depth must be",
		"maxEntries must be",
		"compare_branches requires",
		"find_symbol_definitions requires",
		"find_symbol_references requires",
		"symbol revision requires repo or repos",
		"symbol tools require",
		"inspect_code_graph query is required",
		"graph ref requires repo or repos",
		"NebulaGraph traversal",
		"graph_callers requires query",
		"graph_callees requires query",
		"graph_impact requires query",
		"Failed to ask codebase:",
		"ask_codebase query is required",
		"retrieval layer incomplete",
		"No language models are configured",
		"Language model ",
		"ask_codebase provider ",
		"openai-compatible model ",
		"invalid model baseUrl",
		"model baseUrl ",
		"language model request failed",
		"ask_codebase did not produce",
		"secret ",
		"secret value must",
		"language model credentials must use",
		"environment secret references are not allowed",
		"secret reference belongs",
		"encryption key is not configured",
		"environment secret ",
		"Atom VCS token references",
		"googleCloudSecret model credentials",
		"maxFiles must be",
		"maxPatchBytes must be",
		"repository ",
		"file ",
		"search backend is not configured",
		"search response exceeded MCP output budget",
		"repository path is outside managed storage",
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return msg
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "Tool execution timed out."
	}
	if errors.Is(err, api.ErrSearchBackendNotConfigured) {
		return "search backend is not configured"
	}
	if errors.Is(err, api.ErrSearchBackendUnavailable) {
		return "search backend is unavailable"
	}
	if errors.Is(err, api.ErrSearchInvalidQuery) {
		return "search query is invalid"
	}
	return "Tool execution failed."
}

func (b *Backend) acquireRead(ctx context.Context) error {
	select {
	case b.readSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) releaseRead() {
	select {
	case <-b.readSem:
	default:
	}
}

func textToolResult(text string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}}
}

func jsonToolResult(value any) (toolResult, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return toolResult{}, err
	}
	return textToolResult(string(body)), nil
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func graphTaskSchema(properties map[string]any) map[string]any {
	schema := objectSchema(properties, nil)
	schema["anyOf"] = []map[string]any{
		{"required": []string{"query"}},
		{"required": []string{"symbol"}},
		{"required": []string{"seed"}},
	}
	return schema
}

func readOnlyAnnotations() map[string]any {
	return map[string]any{
		"readOnlyHint":   true,
		"idempotentHint": true,
	}
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerSchema(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func enumSchema(values []string, description string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}

func arraySchema(items map[string]any, description string) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": description}
}

func formatMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

type searchResponse struct {
	Stats struct {
		ActualMatchCount int64 `json:"actualMatchCount"`
	} `json:"stats"`
	Files              []searchFile `json:"files"`
	IsSearchExhaustive bool         `json:"isSearchExhaustive"`
}

type searchFile struct {
	FileName struct {
		Text string `json:"text"`
	} `json:"fileName"`
	Repository string        `json:"repository"`
	Chunks     []searchChunk `json:"chunks"`
}

type searchChunk struct {
	Content      string `json:"content"`
	ContentStart struct {
		LineNumber uint32 `json:"lineNumber"`
	} `json:"contentStart"`
	MatchRanges []any `json:"matchRanges"`
}

func formatGrepOutput(search searchResponse, groupByRepo bool) string {
	if len(search.Files) == 0 {
		return "No files found"
	}
	if groupByRepo {
		type counts struct {
			matches int
			files   int
		}
		byRepo := map[string]counts{}
		for _, file := range search.Files {
			c := byRepo[file.Repository]
			c.files++
			for _, chunk := range file.Chunks {
				c.matches += len(chunk.MatchRanges)
			}
			byRepo[file.Repository] = c
		}
		repos := make([]string, 0, len(byRepo))
		for repo := range byRepo {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		lines := []string{fmt.Sprintf("Found matches in %d repositories:", len(repos))}
		for _, repo := range repos {
			c := byRepo[repo]
			lines = append(lines, fmt.Sprintf("  %s: %d matches in %d files", repo, c.matches, c.files))
		}
		if !search.IsSearchExhaustive {
			lines = append(lines, "", "(Results truncated. Consider using a more specific query.)")
		}
		return strings.Join(lines, "\n")
	}

	lines := []string{fmt.Sprintf("Found %d matches in %d files", search.Stats.ActualMatchCount, len(search.Files))}
	for _, file := range search.Files {
		lines = append(lines, "", fmt.Sprintf("[%s] %s:", file.Repository, file.FileName.Text))
		for _, chunk := range file.Chunks {
			start := int(chunk.ContentStart.LineNumber)
			if start <= 0 {
				start = 1
			}
			chunkLines := strings.Split(strings.TrimRight(chunk.Content, "\n"), "\n")
			for i, line := range chunkLines {
				lines = append(lines, fmt.Sprintf("  %d: %s", start+i, truncateLine(line)))
			}
		}
	}
	if !search.IsSearchExhaustive {
		lines = append(lines, "", "(Results truncated. Consider using a more specific query.)")
	}
	return strings.Join(lines, "\n")
}

func truncateLine(line string) string {
	if len(line) <= maxLineLength {
		return line
	}
	return line[:maxLineLength] + "... (line truncated)"
}

func globToRegex(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		r := rune(glob[i])
		switch r {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '{':
			end := strings.IndexByte(glob[i+1:], '}')
			if end >= 0 {
				body := glob[i+1 : i+1+end]
				alts := strings.Split(body, ",")
				b.WriteByte('(')
				for j, alt := range alts {
					if j > 0 {
						b.WriteByte('|')
					}
					b.WriteString(regexp.QuoteMeta(alt))
				}
				b.WriteByte(')')
				i += end + 1
			} else {
				b.WriteString(`\{`)
			}
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '}', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cleanRepoPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || len(path) > maxRepoPathLength || strings.HasPrefix(path, "/") || hasControl(path) {
		return "", fmt.Errorf("invalid file path")
	}
	clean := filepath.Clean(path)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("invalid file path")
	}
	return filepath.ToSlash(clean), nil
}

func cleanTreePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if len(path) > maxRepoPathLength || hasControl(path) {
		return "", fmt.Errorf("invalid file path")
	}
	path = strings.TrimLeft(path, "/")
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "", nil
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return "", fmt.Errorf("invalid file path")
		}
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid file path")
	}
	return filepath.ToSlash(clean), nil
}

func validateGitRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !gitRefPattern.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, ".lock") {
		return fmt.Errorf("invalid git reference %q", ref)
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

func validRepoSetName(repo string) bool {
	repo = strings.TrimSpace(repo)
	return repo != "" && !strings.ContainsAny(repo, " \t\r\n,") && !hasControl(repo)
}

func ensureRepoPathAllowed(repoPath string, cfg repopaths.Config) error {
	if repoPath == "" {
		return errors.New("repository path is outside managed storage")
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return errors.New("repository path is outside managed storage")
	}
	allowedRoots := []string{cfg.DataCacheDir}
	if cfg.ZoektEFSRoot != "" {
		allowedRoots = append(allowedRoots, cfg.ZoektEFSRoot)
	}
	for _, root := range allowedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absRoot, absRepo)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return errors.New("repository path is outside managed storage")
}

type resolvedGitRef struct {
	GitRef     string
	DisplayRef string
}

func resolveReadableGitRef(repo db.RepoReadRow, requestedRef string) (resolvedGitRef, error) {
	if err := validateGitRef(requestedRef); err != nil {
		return resolvedGitRef{}, err
	}
	if readBlockedByRemoveIndex(repo) {
		ref := requestedRef
		if ref == "" {
			ref = "HEAD"
		}
		return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not indexed for repository %q", ref, repo.Name)
	}
	if repo.IndexedAt == nil {
		ref := requestedRef
		if ref == "" {
			ref = "HEAD"
		}
		return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not indexed for repository %q", ref, repo.Name)
	}

	defaultBranch := repo.DefaultBranch
	repoInput := repoindexstatus.RepoInput{
		Metadata:      repo.Metadata,
		DefaultBranch: defaultBranch,
	}
	indexedAt := formatMillis(*repo.IndexedAt)
	repoInput.IndexedAt = &indexedAt
	indexedRevisions := repoindexstatus.GetPolicyVisibleIndexedRevisions(repoInput)

	if len(indexedRevisions) > 0 {
		if isImplicitRef(requestedRef) {
			defaultRevision := ""
			if defaultBranch != nil && *defaultBranch != "" {
				defaultRevision = repoindexstatus.BranchRevision(*defaultBranch)
			}
			if defaultRevision != "" && containsString(indexedRevisions, defaultRevision) {
				return resolvedGitRef{GitRef: defaultRevision, DisplayRef: stripRefPrefix(defaultRevision)}, nil
			}
			for _, revision := range indexedRevisions {
				if strings.HasPrefix(revision, "refs/heads/") {
					return resolvedGitRef{GitRef: revision, DisplayRef: stripRefPrefix(revision)}, nil
				}
			}
			return resolvedGitRef{GitRef: indexedRevisions[0], DisplayRef: stripRefPrefix(indexedRevisions[0])}, nil
		}
		for _, candidate := range revisionCandidates(requestedRef) {
			if containsString(indexedRevisions, candidate) {
				return resolvedGitRef{GitRef: candidate, DisplayRef: stripRefPrefix(candidate)}, nil
			}
		}
		return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not indexed for repository %q", requestedRef, repo.Name)
	}

	// Older single-branch rows may have IndexedAt without an
	// indexedRevisions manifest. Treat only the default branch as
	// proven in that compatibility path. Branch policy (for example
	// branches:["*"]) is a selection rule, not evidence that a
	// concrete branch was actually indexed.
	if isImplicitRef(requestedRef) {
		if defaultBranch != nil && *defaultBranch != "" {
			return resolvedGitRef{GitRef: repoindexstatus.BranchRevision(*defaultBranch), DisplayRef: *defaultBranch}, nil
		}
		return resolvedGitRef{GitRef: "HEAD", DisplayRef: "HEAD"}, nil
	}

	refName := stripRefPrefix(requestedRef)
	if defaultBranch != nil && refName == *defaultBranch {
		return resolvedGitRef{GitRef: repoindexstatus.BranchRevision(refName), DisplayRef: refName}, nil
	}
	return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not indexed for repository %q", requestedRef, repo.Name)
}

func readBlockedByRemoveIndex(repo db.RepoReadRow) bool {
	if repo.LatestJobType == nil || repo.LatestJobStatus == nil {
		return false
	}
	if *repo.LatestJobType != "REMOVE_INDEX" {
		return false
	}
	switch *repo.LatestJobStatus {
	case "PENDING", "IN_PROGRESS", "FAILED":
		return true
	default:
		return false
	}
}

func resolveComparisonGitRef(repo db.RepoReadRow, requestedRef string) (resolvedGitRef, error) {
	if err := validateGitRef(requestedRef); err != nil {
		return resolvedGitRef{}, err
	}
	repoInput := repoindexstatus.RepoInput{
		Metadata:      repo.Metadata,
		DefaultBranch: repo.DefaultBranch,
	}
	if isImplicitRef(requestedRef) {
		displayRequested := requestedRef
		if displayRequested == "" {
			displayRequested = "HEAD"
		}
		if repo.DefaultBranch != nil && *repo.DefaultBranch != "" {
			candidate := repoindexstatus.BranchRevision(*repo.DefaultBranch)
			if !repoindexstatus.IsRevisionAllowedByIndexPolicy(repoInput, candidate) {
				return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not selected for repository %q", displayRequested, repo.Name)
			}
			return resolvedGitRef{GitRef: candidate, DisplayRef: *repo.DefaultBranch}, nil
		}
		md := repoindexstatus.ParseMetadata(repo.Metadata)
		if len(md.Branches) > 0 || len(md.Tags) > 0 || len(md.IndexedRevisions) > 0 {
			return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not selected for repository %q", displayRequested, repo.Name)
		}
		return resolvedGitRef{GitRef: "HEAD", DisplayRef: "HEAD"}, nil
	}
	for _, candidate := range comparisonPolicyCandidates(requestedRef) {
		if repoindexstatus.IsRevisionAllowedByIndexPolicy(repoInput, candidate) {
			return resolvedGitRef{GitRef: candidate, DisplayRef: stripRefPrefix(candidate)}, nil
		}
	}
	return resolvedGitRef{}, fmt.Errorf("invalid git reference %q: ref is not selected for repository %q", requestedRef, repo.Name)
}

func comparisonPolicyCandidates(ref string) []string {
	refName := stripRefPrefix(ref)
	if strings.HasPrefix(ref, "refs/tags/") {
		return []string{tagRevision(refName)}
	}
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/remotes/origin/") {
		return []string{repoindexstatus.BranchRevision(refName)}
	}
	return []string{repoindexstatus.BranchRevision(refName), tagRevision(refName)}
}

func formatIndexCoverage(repo db.RepoReadRow, requestedRef string, resolved resolvedGitRef) string {
	if isRefIndexed(repo, requestedRef, resolved) {
		return "indexed for Zoekt/SCIP/graph search"
	}
	return "not indexed for Zoekt/SCIP/graph search; branch diff still computed from git checkout"
}

func isRefIndexed(repo db.RepoReadRow, requestedRef string, resolved resolvedGitRef) bool {
	if repo.IndexedAt == nil {
		return false
	}
	defaultBranch := repo.DefaultBranch
	repoInput := repoindexstatus.RepoInput{
		Metadata:      repo.Metadata,
		DefaultBranch: defaultBranch,
	}
	indexedAt := formatMillis(*repo.IndexedAt)
	repoInput.IndexedAt = &indexedAt
	indexedRevisions := repoindexstatus.GetPolicyVisibleIndexedRevisions(repoInput)
	if len(indexedRevisions) > 0 {
		candidates := []string{resolved.GitRef}
		if !isImplicitRef(requestedRef) {
			candidates = append(candidates, revisionCandidates(requestedRef)...)
		}
		for _, candidate := range candidates {
			if containsString(indexedRevisions, candidate) {
				return true
			}
		}
		if isImplicitRef(requestedRef) && defaultBranch != nil && *defaultBranch != "" {
			return containsString(indexedRevisions, repoindexstatus.BranchRevision(*defaultBranch))
		}
		return false
	}
	if isImplicitRef(requestedRef) {
		return true
	}
	refName := stripRefPrefix(requestedRef)
	return defaultBranch != nil && refName == *defaultBranch
}

func isImplicitRef(ref string) bool {
	return strings.TrimSpace(ref) == "" || ref == "HEAD"
}

func stripRefPrefix(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/remotes/origin/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

func zoektQueryRef(ref string) string {
	if ref == "" || ref == "HEAD" {
		return ref
	}
	return stripRefPrefix(ref)
}

func tagRevision(tag string) string {
	if strings.HasPrefix(tag, "refs/tags/") {
		return tag
	}
	return "refs/tags/" + tag
}

func revisionCandidates(ref string) []string {
	refName := stripRefPrefix(ref)
	return []string{ref, repoindexstatus.BranchRevision(refName), tagRevision(refName)}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validateModelBaseURL(raw string, allowed []string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid model baseUrl: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("model baseUrl must be an absolute URL")
	}
	if len(allowed) > 0 {
		for _, candidate := range allowed {
			if modelBaseURLMatches(raw, candidate) {
				return nil
			}
		}
		return fmt.Errorf("model baseUrl is not in the configured allow-list")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("model baseUrl must use https unless explicitly allow-listed")
	}
	host := modelURLHost(parsed)
	if isPrivateModelHost(host) {
		return fmt.Errorf("model baseUrl points to a private or internal host; add an explicit allow-list entry to use it")
	}
	return nil
}

func modelBaseURLMatches(raw, allowed string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	allowedURL, err := url.Parse(strings.TrimSpace(allowed))
	if err != nil || allowedURL.Scheme == "" || allowedURL.Host == "" {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, allowedURL.Scheme) || !strings.EqualFold(parsed.Host, allowedURL.Host) {
		return false
	}
	prefix := strings.TrimRight(allowedURL.EscapedPath(), "/")
	if prefix == "" {
		return true
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func modelURLHost(u *url.URL) string {
	host := u.Hostname()
	if host == "" {
		host = u.Host
	}
	return strings.Trim(strings.ToLower(host), "[]")
}

func isPrivateModelHost(host string) bool {
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, suffix := range []string{".local", ".internal", ".svc", ".cluster.local", ".svc.cluster.local"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if !strings.Contains(host, ".") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			return true
		}
		return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified()
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified()
	}
	return false
}

func cleanStrings(values []string) []string {
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

type listTreeEntry struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	ParentPath string `json:"parentPath"`
	Depth      int    `json:"depth"`
}

type treeQueueEntry struct {
	Path  string
	Depth int
}

func readGitFile(ctx context.Context, repoPath, ref, filePath string) (string, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return "", "", false, fmt.Errorf("open repository: %w", err)
	}
	if ref == "" {
		ref = "HEAD"
	}
	hash, resolved, err := resolveRevision(repo, ref)
	if err != nil {
		return "", "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return "", "", false, fmt.Errorf("load commit: %w", err)
	}
	file, err := commit.File(filePath)
	if err != nil {
		return "", "", false, fmt.Errorf("file %q not found at %s", filePath, resolved)
	}
	reader, err := file.Reader()
	if err != nil {
		return "", "", false, fmt.Errorf("open file: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, readFileMaxSource+1))
	if err != nil {
		return "", "", false, fmt.Errorf("read file: %w", err)
	}
	truncated := len(data) > readFileMaxSource
	if len(data) > readFileMaxSource {
		data = data[:readFileMaxSource]
	}
	return string(data), resolved, truncated, nil
}

func listGitTree(ctx context.Context, repoPath, ref, rootPath string, depth int, includeFiles, includeDirectories bool, maxEntries int) ([]listTreeEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, false, fmt.Errorf("open repository: %w", err)
	}
	if ref == "" {
		ref = "HEAD"
	}
	hash, _, err := resolveRevision(repo, ref)
	if err != nil {
		return nil, false, err
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, false, fmt.Errorf("load commit: %w", err)
	}
	root, err := commit.Tree()
	if err != nil {
		return nil, false, fmt.Errorf("load tree: %w", err)
	}

	queue := []treeQueueEntry{{Path: rootPath, Depth: 0}}
	queued := map[string]bool{rootPath: true}
	seen := map[string]bool{}
	entries := make([]listTreeEntry, 0)
	truncated := false
	for len(queue) > 0 && !truncated {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		current := queue[0]
		queue = queue[1:]
		tree := root
		if current.Path != "" {
			subtree, err := root.Tree(current.Path)
			if err != nil {
				continue
			}
			tree = subtree
		}
		children := append([]object.TreeEntry(nil), tree.Entries...)
		sort.Slice(children, func(i, j int) bool {
			return compareFold(children[i].Name, children[j].Name) < 0
		})
		for _, child := range children {
			childType := ""
			if child.Mode == filemode.Dir {
				childType = "tree"
			} else if child.Mode.IsFile() {
				childType = "blob"
			} else {
				continue
			}
			childPath := joinTreePath(current.Path, child.Name)
			childDepth := current.Depth + 1
			if childType == "tree" && childDepth < depth && !queued[childPath] {
				queue = append(queue, treeQueueEntry{Path: childPath, Depth: childDepth})
				queued[childPath] = true
			}
			if (childType == "blob" && !includeFiles) || (childType == "tree" && !includeDirectories) {
				continue
			}
			key := childType + ":" + childPath
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(entries) >= maxEntries {
				truncated = true
				break
			}
			entries = append(entries, listTreeEntry{
				Type:       childType,
				Path:       childPath,
				Name:       child.Name,
				ParentPath: current.Path,
				Depth:      childDepth,
			})
		}
	}
	sortTreeEntries(entries)
	return entries, truncated, nil
}

func joinTreePath(parentPath, name string) string {
	if parentPath == "" {
		return name
	}
	return parentPath + "/" + name
}

func sortTreeEntries(entries []listTreeEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a := entries[i]
		b := entries[j]
		if cmp := compareFold(a.ParentPath, b.ParentPath); cmp != 0 {
			return cmp < 0
		}
		if a.Type != b.Type {
			return a.Type == "tree"
		}
		if cmp := compareFold(a.Name, b.Name); cmp != 0 {
			return cmp < 0
		}
		return compareFold(a.Path, b.Path) < 0
	})
}

func compareFold(a, b string) int {
	la := strings.ToLower(a)
	lb := strings.ToLower(b)
	if la < lb {
		return -1
	}
	if la > lb {
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func resolveRevision(repo *git.Repository, ref string) (*plumbing.Hash, string, error) {
	if hash, err := repo.ResolveRevision(plumbing.Revision(ref)); err == nil && hash != nil {
		return hash, ref, nil
	}
	refName := stripRefPrefix(ref)
	candidates := []string{
		"refs/heads/" + refName,
		"refs/remotes/origin/" + refName,
		"refs/tags/" + refName,
	}
	for _, candidate := range candidates {
		if hash, err := repo.ResolveRevision(plumbing.Revision(candidate)); err == nil && hash != nil {
			return hash, candidate, nil
		}
	}
	return nil, "", fmt.Errorf("invalid git reference %q", ref)
}

func formatFileOutput(repoName, filePath, content string, offset, limit int, sourceTruncated bool) (string, int, int, bool) {
	lines := strings.Split(content, "\n")
	startIdx := offset - 1
	if startIdx > len(lines) {
		startIdx = len(lines)
	}
	endIdx := startIdx + limit
	if endIdx > len(lines) {
		endIdx = len(lines)
	}
	var out strings.Builder
	out.WriteString("<repo>")
	out.WriteString(repoName)
	out.WriteString("</repo>\n<path>")
	out.WriteString(filePath)
	out.WriteString("</path>\n<content>\n")

	bytesWritten := 0
	lastLine := offset - 1
	truncated := sourceTruncated || endIdx < len(lines)
	for i := startIdx; i < endIdx; i++ {
		line := truncateLine(lines[i])
		prefix := strconv.Itoa(i+1) + ": "
		size := len(prefix) + len(line) + 1
		if bytesWritten+size > readFileMaxBytes {
			truncated = true
			break
		}
		out.WriteString(prefix)
		out.WriteString(line)
		if i < endIdx-1 {
			out.WriteByte('\n')
		}
		bytesWritten += size
		lastLine = i + 1
	}
	if lastLine < offset {
		lastLine = offset
	}
	if sourceTruncated {
		out.WriteString(fmt.Sprintf("\n\n(File content capped at %d bytes before formatting. Showing lines %d-%d of the available prefix.)", readFileMaxSource, offset, lastLine))
	} else if truncated {
		out.WriteString(fmt.Sprintf("\n\n(Output capped. Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, lastLine, len(lines), lastLine+1))
	} else {
		out.WriteString(fmt.Sprintf("\n\n(End of file - %d lines total)", len(lines)))
	}
	out.WriteString("\n</content>")
	return out.String(), offset, lastLine, truncated
}

type branchComparison struct {
	Files          []changedFile
	TotalFiles     int
	Additions      int
	Deletions      int
	CommitsAhead   int
	CommitsBehind  int
	PatchExcerpt   string
	PatchTruncated bool
}

type changedFile struct {
	Path      string
	Additions int
	Deletions int
}

func compareCommits(ctx context.Context, baseCommit, headCommit *object.Commit, maxFiles int, includeDiff bool, maxPatchBytes int) (branchComparison, error) {
	if err := ctx.Err(); err != nil {
		return branchComparison{}, err
	}
	patch, err := baseCommit.Patch(headCommit)
	if err != nil {
		return branchComparison{}, fmt.Errorf("build branch patch: %w", err)
	}
	stats := patch.Stats()
	comparison := branchComparison{
		Files:      make([]changedFile, 0, minInt(len(stats), maxFiles)),
		TotalFiles: len(stats),
	}
	for i, stat := range stats {
		comparison.Additions += stat.Addition
		comparison.Deletions += stat.Deletion
		if i < maxFiles {
			comparison.Files = append(comparison.Files, changedFile{
				Path:      stat.Name,
				Additions: stat.Addition,
				Deletions: stat.Deletion,
			})
		}
	}
	comparison.CommitsAhead, comparison.CommitsBehind, err = aheadBehindCounts(ctx, baseCommit, headCommit, 50000)
	if err != nil {
		return branchComparison{}, err
	}
	if includeDiff {
		diff := patch.String()
		if len(diff) > maxPatchBytes {
			comparison.PatchExcerpt = diff[:maxPatchBytes]
			comparison.PatchTruncated = true
		} else {
			comparison.PatchExcerpt = diff
		}
		if comparison.PatchExcerpt == "" {
			comparison.PatchExcerpt = "(no diff)"
		}
	}
	return comparison, nil
}

func aheadBehindCounts(ctx context.Context, baseCommit, headCommit *object.Commit, cap int) (int, int, error) {
	baseSet, err := reachableCommitSet(ctx, baseCommit, cap)
	if err != nil {
		return 0, 0, err
	}
	headSet, err := reachableCommitSet(ctx, headCommit, cap)
	if err != nil {
		return 0, 0, err
	}
	ahead := 0
	for hash := range headSet {
		if _, ok := baseSet[hash]; !ok {
			ahead++
		}
	}
	behind := 0
	for hash := range baseSet {
		if _, ok := headSet[hash]; !ok {
			behind++
		}
	}
	return ahead, behind, nil
}

func reachableCommitSet(ctx context.Context, start *object.Commit, cap int) (map[plumbing.Hash]struct{}, error) {
	seen := map[plumbing.Hash]struct{}{}
	stack := []*object.Commit{start}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(seen) >= cap {
			return seen, nil
		}
		commit := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[commit.Hash]; ok {
			continue
		}
		seen[commit.Hash] = struct{}{}
		parents := commit.Parents()
		err := parents.ForEach(func(parent *object.Commit) error {
			if _, ok := seen[parent.Hash]; !ok {
				stack = append(stack, parent)
			}
			return nil
		})
		parents.Close()
		if err != nil {
			return nil, fmt.Errorf("walk commit parents: %w", err)
		}
	}
	return seen, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
