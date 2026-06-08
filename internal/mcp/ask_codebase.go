package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/pkg/llmproxy"
)

const (
	answerTag                      = "<!--answer-->"
	defaultAskTimeout              = 5 * time.Minute
	defaultAskMaxAttempts          = 2
	defaultAskRepoLimit            = 250
	maxAskToolOutputBytes          = 24000
	maxAskFusedContextBytes        = 160000
	maxAskConversationContextBytes = 12000
	maxAskAnswerBytes              = 256000
	askSynthesisPromptVersion      = "evidence-complete-v7"
)

func (b *Backend) toolAskCodebase(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("model query backend is not configured")
	}
	var args struct {
		Query               string                `json:"query"`
		Repos               []string              `json:"repos"`
		Ref                 string                `json:"ref"`
		LanguageModel       *languageModelRequest `json:"languageModel"`
		Async               *bool                 `json:"async"`
		AnswerBudget        string                `json:"answerBudget"`
		AnswerMode          string                `json:"answerMode"`
		ConversationContext string                `json:"conversationContext"`
		RetrievalQuery      string                `json:"retrievalQuery"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid ask_codebase arguments")
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return toolResult{}, fmt.Errorf("ask_codebase query is required")
	}
	answerBudget, err := normalizeAskAnswerBudget(firstNonEmpty(args.AnswerBudget, args.AnswerMode))
	if err != nil {
		return toolResult{}, err
	}
	requestedRepos := cleanStrings(args.Repos)
	for _, repoName := range requestedRepos {
		if _, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, repoName); err != nil {
			if errors.Is(err, db.ErrRepoNotFound) {
				return toolResult{}, fmt.Errorf("repository %q not found", repoName)
			}
			return toolResult{}, err
		}
	}

	modelConfig, modelInfo, err := b.selectLanguageModel(ctx, req.OrgID, args.LanguageModel)
	if err != nil {
		return toolResult{}, err
	}
	if modelConfig.Provider != "openai-compatible" {
		return toolResult{}, fmt.Errorf("ask_codebase provider %q is not supported by the Go MCP runtime yet", modelConfig.Provider)
	}
	clientCfg, err := b.resolveOpenAICompatibleConfig(ctx, req.OrgID, modelConfig)
	if err != nil {
		return toolResult{}, err
	}
	if b.cfg.LanguageModelClient == nil {
		return toolResult{}, errors.New("language model gateway is not configured")
	}

	askCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		timeout := b.cfg.AskTimeout
		if timeout <= 0 {
			timeout = defaultAskTimeout
		}
		var cancel context.CancelFunc
		askCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	selectedRepos, err := b.resolveAskRepoScope(askCtx, req.OrgID, requestedRepos)
	if err != nil {
		return toolResult{}, err
	}

	tools := b.deterministicTools(askCtx, req.OrgID)
	retrievalQuery := strings.TrimSpace(args.RetrievalQuery)
	if retrievalQuery == "" {
		retrievalQuery = args.Query
	}
	preflight, err := b.collectMandatoryEvidencePack(askCtx, req, retrievalQuery, selectedRepos, args.Ref, tools)
	if err != nil {
		return toolResult{}, err
	}
	if clientCfg.MaxTokens <= 0 {
		clientCfg.MaxTokens = answerBudget.MaxOutputTokens
	}
	messages := []openAIChatMessage{
		{Role: "system", Content: createAskSynthesisPrompt(selectedRepos, answerBudget)},
		{Role: "user", Content: preflight.ModelMessage()},
	}
	if strings.TrimSpace(args.ConversationContext) != "" {
		messages = append(messages, openAIChatMessage{Role: "user", Content: "Conversation context for this follow-up. Use it only to preserve continuity; code claims must still come from the retrieved evidence pack.\n\n" + truncateForModel(strings.TrimSpace(args.ConversationContext), maxAskConversationContextBytes)})
	}
	messages = append(messages, openAIChatMessage{Role: "user", Content: "Latest user question: " + args.Query})
	maxAttempts := b.cfg.AskMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultAskMaxAttempts
	}
	trace := append([]askToolTrace{}, preflight.Trace...)
	sessionID := deterministicAskSessionID(req, args.Query, retrievalQuery, selectedRepos, args.Ref, preflight.ScopeFingerprint, modelInfo, answerBudget)
	metadata := map[string]any{
		"tool":             "ask_codebase",
		"orgDomain":        req.OrgDomain,
		"repos":            selectedRepos,
		"answerBudget":     answerBudget.Mode,
		"promptVersion":    askSynthesisPromptVersion,
		"evidenceVersion":  askSynthesisPromptVersion,
		"scopeFingerprint": preflight.ScopeFingerprint,
		"toolTrace":        trace,
		"retrievalHarness": buildAskHarnessEvidence("", trace),
	}
	if retrievalQuery != args.Query {
		metadata["retrievalQuery"] = retrievalQuery
	}
	chatReq := llmproxy.ChatRequest{
		RequestID:   sessionID,
		OrgID:       req.OrgID,
		Provider:    "openai-compatible",
		Model:       llmproxy.LanguageModelInfo{Provider: modelInfo.Provider, Model: modelInfo.Model, DisplayName: modelInfo.DisplayName},
		OpenAI:      clientCfg,
		Messages:    messages,
		MaxAttempts: maxAttempts,
		Stream:      args.Async == nil || *args.Async,
		Budget:      answerBudget,
		Metadata:    metadata,
	}
	if asyncClient, ok := b.cfg.LanguageModelClient.(asyncLanguageModelClient); ok && (args.Async == nil || *args.Async) {
		started, err := asyncClient.StartChat(askCtx, chatReq)
		if err != nil {
			return toolResult{}, err
		}
		if started.Status == "SUCCEEDED" && strings.TrimSpace(started.Content) != "" {
			return b.formatAskCompletion(sessionID, modelInfo, trace, selectedRepos, started.Content, answerBudget, chatReq.Metadata)
		}
		text := fmt.Sprintf("ask_codebase synthesis is %s.\n\nrequestId: `%s`\nmodel: `%s`\n\nCall `get_ask_codebase_result` with this requestId to retrieve the durable backend result.", valueOrString(started.Status, "IN_PROGRESS"), sessionID, modelInfo.Model)
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			Meta: map[string]any{
				"sessionId":     sessionID,
				"status":        valueOrString(started.Status, "IN_PROGRESS"),
				"languageModel": modelInfo,
				"toolTrace":     trace,
				"repos":         selectedRepos,
			},
		}, nil
	}

	completion, err := b.cfg.LanguageModelClient.CompleteChat(askCtx, chatReq)
	if err != nil {
		return toolResult{}, err
	}
	if len(completion.ToolCalls) > 0 {
		return toolResult{}, fmt.Errorf("ask_codebase model requested tools even though retrieval already completed")
	}
	return b.formatAskCompletion(sessionID, modelInfo, trace, selectedRepos, completion.Content, answerBudget, chatReq.Metadata)
}

func (b *Backend) toolGetAskCodebaseResult(ctx context.Context, req api.MCPRequest, raw json.RawMessage) (toolResult, error) {
	var args struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("invalid get_ask_codebase_result arguments")
	}
	args.RequestID = strings.TrimSpace(args.RequestID)
	if args.RequestID == "" {
		return toolResult{}, fmt.Errorf("requestId is required")
	}
	asyncClient, ok := b.cfg.LanguageModelClient.(asyncLanguageModelClient)
	if !ok {
		return toolResult{}, fmt.Errorf("durable language model gateway is not configured")
	}
	resp, err := asyncClient.GetChat(ctx, req.OrgID, args.RequestID)
	if err != nil {
		return toolResult{}, err
	}
	model := languageModelInfo{Provider: resp.Model.Provider, Model: resp.Model.Model, DisplayName: resp.Model.DisplayName}
	selectedRepos := metadataStringSlice(resp.Metadata, "repos")
	trace := metadataAskToolTrace(resp.Metadata, "toolTrace")
	if resp.Status != "SUCCEEDED" {
		text := fmt.Sprintf("ask_codebase synthesis is %s for requestId `%s`.", valueOrString(resp.Status, "IN_PROGRESS"), args.RequestID)
		if strings.TrimSpace(resp.Error) != "" {
			text += "\nError: " + strings.TrimSpace(resp.Error)
		}
		if resp.Partial && strings.TrimSpace(resp.Content) != "" {
			text += "\n\nPartial streamed answer so far:\n\n" + normalizeAskFinalAnswer(resp.Content)
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			IsError: resp.Status == "FAILED",
			Meta: map[string]any{
				"sessionId":     args.RequestID,
				"status":        valueOrString(resp.Status, "IN_PROGRESS"),
				"languageModel": model,
				"partial":       resp.Partial,
				"budget":        resp.Budget,
				"toolTrace":     trace,
				"repos":         selectedRepos,
				"metadata":      resp.Metadata,
			},
		}, nil
	}
	return b.formatAskCompletion(args.RequestID, model, trace, selectedRepos, resp.Content, resp.Budget, resp.Metadata)
}

func metadataStringSlice(meta map[string]any, key string) []string {
	if len(meta) == 0 {
		return nil
	}
	switch value := meta[key].(type) {
	case []string:
		return cleanStrings(value)
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return cleanStrings(out)
	case string:
		return cleanStrings(strings.Split(value, ","))
	default:
		return nil
	}
}

func metadataAskToolTrace(meta map[string]any, key string) []askToolTrace {
	if len(meta) == 0 {
		return nil
	}
	switch value := meta[key].(type) {
	case []askToolTrace:
		return append([]askToolTrace{}, value...)
	case []any:
		out := make([]askToolTrace, 0, len(value))
		for _, item := range value {
			if row, ok := item.(map[string]any); ok {
				trace := askToolTrace{
					Step:      metadataInt(row["step"]),
					ToolName:  metadataString(row["toolName"]),
					IsError:   metadataBool(row["isError"]),
					Arguments: metadataString(row["arguments"]),
					Output:    metadataString(row["output"]),
				}
				if trace.ToolName != "" {
					out = append(out, trace)
				}
			}
		}
		return out
	default:
		return nil
	}
}

func metadataAskHarnessEvidence(meta map[string]any, key string) (askHarnessEvidence, bool) {
	if len(meta) == 0 {
		return askHarnessEvidence{}, false
	}
	value, ok := meta[key]
	if !ok || value == nil {
		return askHarnessEvidence{}, false
	}
	if typed, ok := value.(askHarnessEvidence); ok {
		return typed, true
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return askHarnessEvidence{}, false
	}
	var out askHarnessEvidence
	if err := json.Unmarshal(raw, &out); err != nil {
		return askHarnessEvidence{}, false
	}
	return out, true
}

func metadataString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func metadataBool(value any) bool {
	if flag, ok := value.(bool); ok {
		return flag
	}
	return false
}

func metadataInt(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int32:
		return int(number)
	case int64:
		return int(number)
	case float64:
		return int(number)
	default:
		return 0
	}
}

func (b *Backend) formatAskCompletion(sessionID string, modelInfo languageModelInfo, trace []askToolTrace, selectedRepos []string, content string, budget llmproxy.AnswerBudget, metadata map[string]any) (toolResult, error) {
	final := strings.TrimSpace(content)
	if final == "" {
		return toolResult{}, fmt.Errorf("ask_codebase did not produce a final assistant answer")
	}
	final = normalizeAskFinalAnswer(final)
	harness := buildAskHarnessEvidence(final, trace)
	if retrievalHarness, ok := metadataAskHarnessEvidence(metadata, "retrievalHarness"); ok {
		harness = mergeAskHarnessEvidence(retrievalHarness, harness)
	}
	maxBytes := budget.MaxAnswerBytes
	if maxBytes <= 0 {
		maxBytes = maxAskAnswerBytes
	}
	if len(final) > maxBytes {
		final = final[:maxBytes] + "\n\n(Answer capped by MCP output budget.)"
	}

	var out strings.Builder
	out.WriteString(final)
	out.WriteString("\n\n")
	out.WriteString(formatAskHarnessAppendix(harness))
	out.WriteString("\n\n---\n")
	out.WriteString("**Research session id:** ")
	out.WriteString(sessionID)
	out.WriteString("\n")
	out.WriteString("**Model used:** ")
	out.WriteString(modelInfo.Model)
	if len(trace) > 0 {
		out.WriteString("\n\n**Tool trace:**\n")
		for _, item := range trace {
			status := "ok"
			if item.IsError {
				status = "error"
			}
			out.WriteString(fmt.Sprintf("- step %d `%s` %s\n", item.Step, item.ToolName, status))
		}
	}
	meta := map[string]any{
		"sessionId":     sessionID,
		"status":        "SUCCEEDED",
		"languageModel": modelInfo,
		"toolTrace":     trace,
		"repos":         selectedRepos,
		"budget":        budget,
		"provenance":    harness.Provenance,
		"harness":       harness,
	}
	if len(metadata) > 0 {
		meta["metadata"] = metadata
		if value := strings.TrimSpace(metadataString(metadata["scopeFingerprint"])); value != "" {
			meta["scopeFingerprint"] = value
		}
		if value := strings.TrimSpace(metadataString(metadata["evidenceVersion"])); value != "" {
			meta["evidenceVersion"] = value
		}
		if value := strings.TrimSpace(metadataString(metadata["promptVersion"])); value != "" {
			meta["promptVersion"] = value
		}
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: out.String()}},
		Meta:    meta,
	}, nil
}

type askHarnessEvidence struct {
	Provenance    map[string]int      `json:"provenance"`
	FilesToRead   []askHarnessFileRef `json:"filesToRead"`
	FilesToModify []askHarnessFileRef `json:"filesToModify"`
	Symbols       []string            `json:"symbols"`
	CallEdges     []string            `json:"callEdges"`
	TestsToRun    []askHarnessFileRef `json:"testsToRun"`
	Gaps          []string            `json:"gaps"`
	Confidence    string              `json:"confidence"`
	EvidenceIDs   []string            `json:"evidenceIds"`
}

type askHarnessFileRef struct {
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"`
	Role   string `json:"role"`
	Source string `json:"source,omitempty"`
}

func buildAskHarnessEvidence(final string, trace []askToolTrace) askHarnessEvidence {
	var combinedBuilder strings.Builder
	combinedBuilder.Grow(minInt(len(final)+4096, maxAskFusedContextBytes+maxAskAnswerBytes))
	combinedBuilder.WriteString(final)
	for _, item := range trace {
		if strings.TrimSpace(item.Output) != "" {
			combinedBuilder.WriteByte('\n')
			combinedBuilder.WriteString(item.Output)
		}
	}
	combined := combinedBuilder.String()
	provenance := map[string]int{
		"zoekt":      0,
		"scip":       0,
		"ast":        0,
		"treeSitter": 0,
		"graph":      0,
		"heuristic":  0,
	}
	for _, line := range strings.Split(combined, "\n") {
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "source=scip") || strings.Contains(lower, "provenance=scip") || strings.Contains(lower, "precise scip"):
			provenance["scip"]++
		case strings.Contains(lower, "source=tree-sitter") || strings.Contains(lower, "source=tree_sitter") || strings.Contains(lower, "tree-sitter"):
			provenance["treeSitter"]++
		case strings.Contains(lower, "source=ast") || strings.Contains(lower, "source=syntactic-ast"):
			provenance["ast"]++
		}
		if strings.Contains(lower, "zoekt") || strings.Contains(lower, "grep") {
			provenance["zoekt"]++
		}
		if strings.Contains(lower, "graph") || strings.Contains(line, "-->") {
			provenance["graph"]++
		}
		if strings.Contains(lower, "heuristic") || strings.Contains(lower, "confidence=0.6") {
			provenance["heuristic"]++
		}
	}

	files := askHarnessFilesFromText(combined, 80)
	filesToRead := make([]askHarnessFileRef, 0, minInt(32, len(files)))
	filesToModify := make([]askHarnessFileRef, 0, 16)
	testsToRun := make([]askHarnessFileRef, 0, 12)
	for _, file := range files {
		if strings.Contains(file.Path, "_test.") || strings.Contains(file.Path, "/test/") || strings.Contains(file.Path, "/tests/") {
			file.Role = "test"
			testsToRun = append(testsToRun, file)
			continue
		}
		filesToRead = append(filesToRead, file)
		lower := strings.ToLower(file.Role + " " + file.Source + " " + file.Path)
		if strings.Contains(lower, "touchpoint") || strings.Contains(lower, "definition") || strings.Contains(lower, "implementation") || strings.Contains(lower, "runtime") || strings.Contains(lower, "crd") || strings.Contains(lower, "webhook") {
			edit := file
			edit.Role = "candidate-edit-touchpoint"
			filesToModify = append(filesToModify, edit)
		}
		if len(filesToRead) >= 32 && len(filesToModify) >= 16 && len(testsToRun) >= 12 {
			break
		}
	}

	h := askHarnessEvidence{
		Provenance:    provenance,
		FilesToRead:   capHarnessFiles(filesToRead, 32),
		FilesToModify: capHarnessFiles(filesToModify, 16),
		Symbols:       cleanStrings(preflightSymbols(combined, 40)),
		CallEdges:     askHarnessCallEdges(combined, 24),
		TestsToRun:    capHarnessFiles(testsToRun, 12),
		Gaps:          askHarnessGaps(combined, 16),
		EvidenceIDs:   askHarnessEvidenceIDs(trace),
	}
	if provenance["scip"] > 0 && provenance["graph"] > 0 && provenance["zoekt"] > 0 {
		h.Confidence = "mixed-high-when-file-lines-match; verify heuristic graph edges"
	} else if provenance["zoekt"] > 0 {
		h.Confidence = "lexical-plus-partial-structure"
	} else {
		h.Confidence = "insufficient-evidence"
	}
	return h
}

func mergeAskHarnessEvidence(primary, secondary askHarnessEvidence) askHarnessEvidence {
	out := askHarnessEvidence{
		Provenance:    map[string]int{},
		FilesToRead:   mergeHarnessFiles(primary.FilesToRead, secondary.FilesToRead, 32),
		FilesToModify: mergeHarnessFiles(primary.FilesToModify, secondary.FilesToModify, 16),
		Symbols:       mergeHarnessStrings(primary.Symbols, secondary.Symbols, 40),
		CallEdges:     mergeHarnessStrings(primary.CallEdges, secondary.CallEdges, 24),
		TestsToRun:    mergeHarnessFiles(primary.TestsToRun, secondary.TestsToRun, 12),
		Gaps:          mergeHarnessStrings(primary.Gaps, secondary.Gaps, 16),
		Confidence:    firstNonEmpty(primary.Confidence, secondary.Confidence),
		EvidenceIDs:   mergeHarnessStrings(primary.EvidenceIDs, secondary.EvidenceIDs, 32),
	}
	for _, key := range []string{"zoekt", "scip", "ast", "treeSitter", "graph", "heuristic"} {
		out.Provenance[key] = primary.Provenance[key] + secondary.Provenance[key]
	}
	if out.Confidence == "" {
		out.Confidence = "insufficient-evidence"
	}
	return out
}

func mergeHarnessFiles(primary, secondary []askHarnessFileRef, max int) []askHarnessFileRef {
	if max <= 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]askHarnessFileRef, 0, max)
	add := func(values []askHarnessFileRef) {
		for _, value := range values {
			if strings.TrimSpace(value.Path) == "" {
				continue
			}
			key := value.Repo + "\x00" + value.Path + "\x00" + strconv.Itoa(value.Line) + "\x00" + value.Role
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, value)
			if len(out) >= max {
				return
			}
		}
	}
	add(primary)
	if len(out) < max {
		add(secondary)
	}
	return out
}

func mergeHarnessStrings(primary, secondary []string, max int) []string {
	if max <= 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, max)
	add := func(values []string) {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := strings.ToLower(value)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, value)
			if len(out) >= max {
				return
			}
		}
	}
	add(primary)
	if len(out) < max {
		add(secondary)
	}
	return out
}

func formatAskHarnessAppendix(h askHarnessEvidence) string {
	payload, _ := json.Marshal(h)
	var b strings.Builder
	b.WriteString("**Evidence provenance summary:** ")
	b.WriteString(fmt.Sprintf("Zoekt=%d, SCIP=%d, AST=%d, tree-sitter=%d, graph=%d, heuristic=%d.",
		h.Provenance["zoekt"], h.Provenance["scip"], h.Provenance["ast"], h.Provenance["treeSitter"], h.Provenance["graph"], h.Provenance["heuristic"]))
	labels := make([]string, 0, 6)
	for _, item := range []struct {
		key   string
		label string
	}{
		{key: "scip", label: "source=scip"},
		{key: "ast", label: "source=ast"},
		{key: "treeSitter", label: "source=tree-sitter"},
		{key: "graph", label: "graph"},
		{key: "heuristic", label: "provenance=heuristic"},
	} {
		if h.Provenance[item.key] > 0 {
			labels = append(labels, item.label)
		}
	}
	if len(labels) > 0 {
		b.WriteString(" Labels: ")
		b.WriteString(strings.Join(labels, ", "))
		b.WriteString(".")
	}
	b.WriteString("\n\n**Harness JSON:**\n```json\n")
	b.Write(payload)
	b.WriteString("\n```")
	return b.String()
}

func askHarnessFilesFromText(text string, max int) []askHarnessFileRef {
	if max <= 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]askHarnessFileRef, 0, max)
	add := func(repo, path string, line int, role, source string) {
		path = strings.Trim(path, " \t\r\n`.,:;")
		if path == "" || !strings.Contains(path, ".") {
			return
		}
		key := repo + "\x00" + path + "\x00" + fmt.Sprintf("%d", line)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, askHarnessFileRef{Repo: repo, Path: path, Line: line, Role: valueOrString(role, "read-before-edit"), Source: source})
	}
	var repo, path, source string
	for _, rawLine := range strings.Split(text, "\n") {
		if match := codegraphContextMatchHeaderRE.FindStringSubmatch(rawLine); match != nil {
			repo = strings.TrimSpace(match[1])
			path = strings.TrimSpace(match[2])
			source = "tool-output"
			add(repo, path, 0, "read-before-edit", source)
			if len(out) >= max {
				return out
			}
			continue
		}
		if path != "" {
			if match := codegraphContextMatchLineRE.FindStringSubmatch(rawLine); match != nil {
				lineNo, _ := strconv.Atoi(match[1])
				add(repo, path, lineNo, "read-before-edit", source)
				if len(out) >= max {
					return out
				}
			}
		}
	}
	for _, path := range preflightFilePaths(text, max) {
		add("", path, 0, "read-before-edit", "answer")
		if len(out) >= max {
			break
		}
	}
	return out
}

func askHarnessCallEdges(text string, max int) []string {
	if max <= 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "<!--") {
			continue
		}
		if line == "" || (!strings.Contains(line, "-->") && !strings.Contains(line, " CALLS ") && !strings.Contains(line, " REFERENCES ")) {
			continue
		}
		if len(line) > 420 {
			line = line[:420]
		}
		lower := strings.ToLower(line)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, line)
		if len(out) >= max {
			break
		}
	}
	return out
}

func askHarnessGaps(text string, max int) []string {
	if max <= 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimLeft(rawLine, "-* "))
		lower := strings.ToLower(line)
		if line == "" || !(strings.Contains(lower, "gap") || strings.Contains(lower, "missing") || strings.Contains(lower, "insufficient") || strings.Contains(lower, "no precise")) {
			continue
		}
		if len(line) > 360 {
			line = line[:360]
		}
		if seen[strings.ToLower(line)] {
			continue
		}
		seen[strings.ToLower(line)] = true
		out = append(out, line)
		if len(out) >= max {
			break
		}
	}
	return out
}

func askHarnessEvidenceIDs(trace []askToolTrace) []string {
	out := make([]string, 0, len(trace))
	for idx, item := range trace {
		name := strings.TrimSpace(item.ToolName)
		if name == "" {
			continue
		}
		status := "ok"
		if item.IsError {
			status = "error"
		}
		out = append(out, fmt.Sprintf("step:%d:%s:%s", idx, name, status))
	}
	return out
}

func capHarnessFiles(in []askHarnessFileRef, max int) []askHarnessFileRef {
	if max <= 0 || len(in) == 0 {
		return nil
	}
	if len(in) <= max {
		return in
	}
	return in[:max]
}

func deterministicAskSessionID(req api.MCPRequest, query string, retrievalQuery string, repos []string, ref string, scopeFingerprint string, model languageModelInfo, budget llmproxy.AnswerBudget) string {
	h := sha256.New()
	_, _ = h.Write([]byte(fmt.Sprintf("org:%d\n", req.OrgID)))
	_, _ = h.Write([]byte("domain:" + req.OrgDomain + "\n"))
	_, _ = h.Write([]byte("query:" + strings.TrimSpace(query) + "\n"))
	if strings.TrimSpace(retrievalQuery) != "" && strings.TrimSpace(retrievalQuery) != strings.TrimSpace(query) {
		_, _ = h.Write([]byte("retrievalQuery:" + strings.TrimSpace(retrievalQuery) + "\n"))
	}
	_, _ = h.Write([]byte("ref:" + strings.TrimSpace(ref) + "\n"))
	_, _ = h.Write([]byte("model:" + model.Provider + "/" + model.Model + "\n"))
	repos = cleanStrings(repos)
	sort.Strings(repos)
	for _, repo := range repos {
		_, _ = h.Write([]byte("repo:" + repo + "\n"))
	}
	_, _ = h.Write([]byte("scope:" + strings.TrimSpace(scopeFingerprint) + "\n"))
	_, _ = h.Write([]byte(fmt.Sprintf("budget:%s:%d:%d\n", budget.Mode, budget.MaxOutputTokens, budget.MaxAnswerBytes)))
	_, _ = h.Write([]byte("prompt:" + askSynthesisPromptVersion + "\n"))
	sum := h.Sum(nil)
	return "mcp-" + hex.EncodeToString(sum[:16])
}

func normalizeAskFinalAnswer(final string) string {
	final = strings.TrimSpace(final)
	if idx := strings.Index(final, answerTag); idx >= 0 {
		return strings.TrimSpace(final[idx:])
	}
	return answerTag + "\n" + final
}

func (b *Backend) askEvidenceScopeFingerprint(ctx context.Context, orgID int32, repos []string, ref string) (string, error) {
	if b == nil || b.cfg.Queries == nil {
		return "", errors.New("repository query backend is not configured")
	}
	resolvedRepos, revisions, scopedRevisions, err := b.resolveGraphScope(ctx, orgID, "", repos, ref)
	if err != nil {
		return "", err
	}
	activeScopes, err := b.cfg.Queries.ListActiveCodeGraphScopes(ctx, db.ListActiveCodeGraphScopesParams{
		OrgID:              orgID,
		Repos:              resolvedRepos,
		RevisionCandidates: revisions,
		RepoRevisionScopes: scopedRevisions,
	})
	if err != nil {
		return "", err
	}
	type payloadScope struct {
		Repo               string   `json:"repo"`
		RevisionCandidates []string `json:"revisionCandidates"`
	}
	type payloadActiveScope struct {
		GraphIndexID   string `json:"graphIndexId"`
		RepoID         int32  `json:"repoId"`
		Revision       string `json:"revision"`
		CommitHash     string `json:"commitHash"`
		WorkspaceID    string `json:"workspaceId"`
		SchemaVersion  int32  `json:"schemaVersion"`
		BuilderVersion string `json:"builderVersion"`
	}
	type payload struct {
		OrgID             int32                `json:"orgId"`
		Repos             []string             `json:"repos"`
		RequestedRef      string               `json:"requestedRef"`
		Revisions         []string             `json:"revisions"`
		RepoRevisionScope []payloadScope       `json:"repoRevisionScope"`
		ActiveScopes      []payloadActiveScope `json:"activeScopes"`
	}
	resolvedRepos = cleanStrings(resolvedRepos)
	sort.Strings(resolvedRepos)
	revisions = cleanStrings(revisions)
	sort.Strings(revisions)
	scopes := make([]payloadScope, 0, len(scopedRevisions))
	for _, scope := range scopedRevisions {
		candidates := cleanStrings(scope.RevisionCandidates)
		sort.Strings(candidates)
		scopes = append(scopes, payloadScope{
			Repo:               scope.Repo,
			RevisionCandidates: candidates,
		})
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Repo != scopes[j].Repo {
			return scopes[i].Repo < scopes[j].Repo
		}
		return stringsJoinForKey(scopes[i].RevisionCandidates) < stringsJoinForKey(scopes[j].RevisionCandidates)
	})
	active := make([]payloadActiveScope, 0, len(activeScopes))
	for _, scope := range activeScopes {
		active = append(active, payloadActiveScope{
			GraphIndexID:   scope.GraphIndexID,
			RepoID:         scope.RepoID,
			Revision:       scope.Revision,
			CommitHash:     scope.CommitHash,
			WorkspaceID:    scope.WorkspaceID,
			SchemaVersion:  scope.SchemaVersion,
			BuilderVersion: scope.BuilderVersion,
		})
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].WorkspaceID != active[j].WorkspaceID {
			return active[i].WorkspaceID < active[j].WorkspaceID
		}
		if active[i].RepoID != active[j].RepoID {
			return active[i].RepoID < active[j].RepoID
		}
		if active[i].Revision != active[j].Revision {
			return active[i].Revision < active[j].Revision
		}
		if active[i].CommitHash != active[j].CommitHash {
			return active[i].CommitHash < active[j].CommitHash
		}
		return active[i].GraphIndexID < active[j].GraphIndexID
	})
	raw, err := json.Marshal(payload{
		OrgID:             orgID,
		Repos:             resolvedRepos,
		RequestedRef:      strings.TrimSpace(ref),
		Revisions:         revisions,
		RepoRevisionScope: scopes,
		ActiveScopes:      active,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (b *Backend) resolveAskRepoScope(ctx context.Context, orgID int32, requestedRepos []string) ([]string, error) {
	if len(requestedRepos) > 0 {
		return requestedRepos, nil
	}
	if b.cfg.Queries == nil {
		return nil, errors.New("repository query backend is not configured")
	}
	rows, err := b.cfg.Queries.ListOrgRepos(ctx, db.ListOrgReposParams{
		OrgID:     orgID,
		Take:      defaultAskRepoLimit + 1,
		Sort:      db.ReposSortName,
		Direction: db.ReposSortAsc,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("ask_codebase requires at least one active repository in the organization")
	}
	if len(rows) > defaultAskRepoLimit {
		return nil, fmt.Errorf("ask_codebase without explicit repos spans more than %d active repositories; Atom must pass a repo scope for branch-coherent fused retrieval", defaultAskRepoLimit)
	}
	repos := make([]string, 0, len(rows))
	for _, row := range rows {
		repos = append(repos, row.RepoName)
	}
	return repos, nil
}

func (b *Backend) executeAskTool(ctx context.Context, req api.MCPRequest, call openAIToolCall, selectedRepos []string) (string, bool) {
	name := call.Function.Name
	if name == "ask_codebase" {
		return "Recursive ask_codebase calls are not allowed.", true
	}
	if name == "list_repos" && len(selectedRepos) > 0 {
		result, err := b.toolListSelectedRepos(ctx, req, selectedRepos)
		if err != nil {
			return publicToolError(err), true
		}
		return toolResultText(result), result.IsError
	}
	args := json.RawMessage(call.Function.Arguments)
	if len(bytes.TrimSpace(args)) == 0 {
		args = []byte(`{}`)
	}
	var scopeErr string
	args, scopeErr = constrainToolArgsToSelectedRepos(name, args, selectedRepos)
	if scopeErr != "" {
		return scopeErr, true
	}
	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": json.RawMessage(args),
	})
	if err != nil {
		return "Tool arguments could not be encoded.", true
	}
	result, err := b.callTool(ctx, req, params)
	if err != nil {
		return publicToolError(err), true
	}
	return toolResultText(result), result.IsError
}

func constrainToolArgsToSelectedRepos(toolName string, raw json.RawMessage, selectedRepos []string) (json.RawMessage, string) {
	if len(selectedRepos) == 0 {
		return raw, ""
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return raw, ""
	}
	hasRepo := false
	if repo, present := args["repo"].(string); present {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			delete(args, "repo")
		} else {
			hasRepo = true
			args["repo"] = repo
			if !containsString(selectedRepos, repo) {
				return raw, fmt.Sprintf("Repository %q is outside the selected repository scope for this ask_codebase call.", repo)
			}
		}
	}
	hasRepos := false
	if _, present := args["repos"]; present {
		repos, ok := stringSliceFromAny(args["repos"])
		if !ok {
			return raw, "repos must be an array of repository names."
		}
		repos = cleanStrings(repos)
		if len(repos) == 0 {
			delete(args, "repos")
		} else {
			hasRepos = true
			args["repos"] = repos
		}
		for _, repo := range repos {
			if !containsString(selectedRepos, repo) {
				return raw, fmt.Sprintf("Repository %q is outside the selected repository scope for this ask_codebase call.", repo)
			}
		}
	}
	if toolRequiresSelectedRepoScope(toolName) && !hasRepo && !hasRepos {
		args["repos"] = append([]string(nil), selectedRepos...)
	}
	out, err := json.Marshal(args)
	if err != nil {
		return raw, ""
	}
	return out, ""
}

func toolRequiresSelectedRepoScope(toolName string) bool {
	switch toolName {
	case "grep", "codegraph_context", "inspect_code_graph", "graph_callers", "graph_callees", "graph_impact", "graph_path", "graph_minimal_context", "graph_status", "find_symbol_definitions", "find_symbol_references":
		return true
	default:
		return false
	}
}

func (b *Backend) toolListSelectedRepos(ctx context.Context, req api.MCPRequest, selectedRepos []string) (toolResult, error) {
	if b.cfg.Queries == nil {
		return toolResult{}, errors.New("repository query backend is not configured")
	}
	type repoItem struct {
		Name          string  `json:"name"`
		URL           *string `json:"url"`
		DefaultBranch *string `json:"defaultBranch"`
		Scoped        bool    `json:"scoped"`
	}
	out := struct {
		Repos      []repoItem `json:"repos"`
		TotalCount int        `json:"totalCount"`
		Scoped     bool       `json:"scoped"`
	}{Repos: make([]repoItem, 0, len(selectedRepos)), Scoped: true}
	for _, repoName := range selectedRepos {
		row, err := b.cfg.Queries.GetOrgRepoForRead(ctx, req.OrgID, repoName)
		if err != nil {
			if errors.Is(err, db.ErrRepoNotFound) {
				return toolResult{}, fmt.Errorf("repository %q not found", repoName)
			}
			return toolResult{}, err
		}
		out.Repos = append(out.Repos, repoItem{
			Name:          row.Name,
			URL:           optionalNonBlankString(row.WebURL),
			DefaultBranch: row.DefaultBranch,
			Scoped:        true,
		})
	}
	out.TotalCount = len(out.Repos)
	return jsonToolResult(out)
}

func stringSliceFromAny(value any) ([]string, bool) {
	switch v := value.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

func toolResultText(result toolResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if content.Type == "text" && content.Text != "" {
			parts = append(parts, content.Text)
		}
	}
	if len(parts) == 0 {
		return "(tool returned no text content)"
	}
	return strings.Join(parts, "\n")
}

type languageModelRequest struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName,omitempty"`
}

type languageModelConfig struct {
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	DisplayName  string         `json:"displayName,omitempty"`
	Token        any            `json:"token,omitempty"`
	APIKey       any            `json:"apiKey,omitempty"`
	BaseURL      string         `json:"baseUrl,omitempty"`
	Headers      map[string]any `json:"headers,omitempty"`
	QueryParams  map[string]any `json:"queryParams,omitempty"`
	ReasoningTag string         `json:"reasoningTag,omitempty"`
}

type languageModelInfo struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName,omitempty"`
}

func (b *Backend) selectLanguageModel(ctx context.Context, orgID int32, requested *languageModelRequest) (languageModelConfig, languageModelInfo, error) {
	rows, err := b.cfg.Queries.ListEnabledOrgLanguageModels(ctx, orgID)
	if err != nil {
		return languageModelConfig{}, languageModelInfo{}, err
	}
	if len(rows) == 0 {
		return languageModelConfig{}, languageModelInfo{}, fmt.Errorf("No language models are configured.")
	}
	for _, row := range rows {
		var cfg languageModelConfig
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return languageModelConfig{}, languageModelInfo{}, fmt.Errorf("decode model config: %w", err)
		}
		if requested != nil {
			if cfg.Provider != requested.Provider || cfg.Model != requested.Model {
				continue
			}
			if requested.DisplayName != "" && cfg.DisplayName != requested.DisplayName {
				continue
			}
		}
		info := languageModelInfo{Provider: cfg.Provider, Model: cfg.Model, DisplayName: cfg.DisplayName}
		return cfg, info, nil
	}
	return languageModelConfig{}, languageModelInfo{}, fmt.Errorf("Language model '%s/%s' is not configured.", requested.Provider, requested.Model)
}

type openAICompatibleConfig = llmproxy.OpenAICompatibleConfig

func (b *Backend) resolveOpenAICompatibleConfig(ctx context.Context, orgID int32, cfg languageModelConfig) (openAICompatibleConfig, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		return openAICompatibleConfig{}, fmt.Errorf("openai-compatible model %q is missing baseUrl", cfg.Model)
	}
	if err := validateModelBaseURL(baseURL, b.cfg.AllowedModelBaseURLs); err != nil {
		return openAICompatibleConfig{}, err
	}
	tokenValue := cfg.Token
	if tokenValue == nil {
		tokenValue = cfg.APIKey
	}
	token, err := b.resolveCredentialValue(ctx, orgID, tokenValue)
	if err != nil {
		return openAICompatibleConfig{}, err
	}
	headers, err := b.resolveStringMap(ctx, orgID, cfg.Headers)
	if err != nil {
		return openAICompatibleConfig{}, err
	}
	queryParams, err := b.resolveStringMap(ctx, orgID, cfg.QueryParams)
	if err != nil {
		return openAICompatibleConfig{}, err
	}
	return openAICompatibleConfig{
		Model:       cfg.Model,
		BaseURL:     baseURL,
		Token:       token,
		Headers:     headers,
		QueryParams: queryParams,
	}, nil
}

func (b *Backend) resolveStringMap(ctx context.Context, orgID int32, in map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for key, value := range in {
		resolved, err := b.resolveConfigStringValue(ctx, orgID, value)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", key, err)
		}
		out[key] = resolved
	}
	return out, nil
}

func (b *Backend) resolveCredentialValue(ctx context.Context, orgID int32, value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if s, ok := value.(string); ok {
		if strings.TrimSpace(s) == "" {
			return "", nil
		}
		return "", fmt.Errorf("language model credentials must use an org-scoped secretRef")
	}
	record, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("secret value must be an org-scoped secret reference")
	}
	if ref, _ := record["secretRef"].(string); ref != "" {
		if refOrgID, ok := numericOrgID(record["orgId"]); ok && refOrgID != orgID {
			return "", fmt.Errorf("secret reference belongs to a different org")
		}
		return b.decryptOrgSecret(ctx, orgID, ref)
	}
	legacySecretKey := "source" + "botSecret"
	if ref, _ := record[legacySecretKey].(string); ref != "" {
		if refOrgID, ok := numericOrgID(record["orgId"]); ok && refOrgID != orgID {
			return "", fmt.Errorf("secret reference belongs to a different org")
		}
		return b.decryptOrgSecret(ctx, orgID, ref)
	}
	if _, ok := record["env"].(string); ok {
		return "", fmt.Errorf("environment secret references are not allowed for tenant language-model credentials")
	}
	if _, ok := record["atomVcsConnection"]; ok {
		return "", fmt.Errorf("Atom VCS token references are not valid for language-model credentials")
	}
	if _, ok := record["googleCloudSecret"]; ok {
		return "", fmt.Errorf("googleCloudSecret model credentials are not supported by the Go MCP runtime yet")
	}
	return "", fmt.Errorf("unsupported secret reference")
}

func (b *Backend) resolveConfigStringValue(ctx context.Context, orgID int32, value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if s, ok := value.(string); ok {
		return s, nil
	}
	record, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("secret value must be a string or secret reference")
	}
	return b.resolveCredentialValue(ctx, orgID, record)
}

func numericOrgID(value any) (int32, bool) {
	switch v := value.(type) {
	case float64:
		return int32(v), true
	case int32:
		return v, true
	case int:
		return int32(v), true
	default:
		return 0, false
	}
}

func (b *Backend) decryptOrgSecret(ctx context.Context, orgID int32, key string) (string, error) {
	if b.cfg.EncryptionKey == "" {
		return "", fmt.Errorf("encryption key is not configured")
	}
	row, err := b.cfg.Queries.GetOrgSecretCiphertext(ctx, orgID, key)
	if err != nil {
		if errors.Is(err, db.ErrOrgSecretNotFound) {
			return "", fmt.Errorf("secret %q not found", key)
		}
		return "", err
	}
	value, err := auth.Decrypt(b.cfg.EncryptionKey, row.IV, row.EncryptedValue)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", key, err)
	}
	return value, nil
}

type openAIChatMessage = llmproxy.ChatMessage
type openAIToolCall = llmproxy.ToolCall
type openAIToolFunction = llmproxy.ToolFunction

type askToolTrace struct {
	Step      int    `json:"step"`
	ToolName  string `json:"toolName"`
	IsError   bool   `json:"isError,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type mandatoryEvidencePack struct {
	Query            string
	ScopeFingerprint string
	Sections         []mandatoryEvidenceSection
	Trace            []askToolTrace
}

type mandatoryEvidenceSection struct {
	Layer     string
	ToolName  string
	Attempted bool
	Satisfied bool
	Output    string
	Error     string
}

func (p mandatoryEvidencePack) ModelMessage() string {
	var b strings.Builder
	b.WriteString("Mandatory fused retrieval evidence pack. You must synthesize from all attempted layers below and explicitly call out any missing layer evidence as a gap. Do not answer from memory.\n\n")
	b.WriteString("Graph evidence rules: `Native graph traversal`, `High-signal connected flow`, and `Ranked implementation path / sequence edges` are trusted graph evidence. `Heuristic AST graph traversal`, `Heuristic connected flow`, and `Ranked heuristic AST path edges` are low-confidence structural hints; use them only with verification language and do not call them proven. If the graph has only `DEFINES` edges, say it confirms definitions but no CALLS/REFERENCES/IMPORTS edges were found.\n\n")
	for _, section := range p.Sections {
		status := "missing"
		if section.Satisfied {
			status = "available"
		} else if section.Attempted {
			status = "attempted-no-evidence"
		}
		if section.Error != "" {
			status = "error"
		}
		b.WriteString("## ")
		b.WriteString(section.Layer)
		b.WriteString(" (")
		b.WriteString(status)
		b.WriteString(")\n")
		if section.Error != "" {
			b.WriteString("Error: ")
			b.WriteString(section.Error)
			b.WriteString("\n\n")
			continue
		}
		if strings.TrimSpace(section.Output) == "" {
			b.WriteString("No evidence returned.\n\n")
			continue
		}
		b.WriteString(section.ModelOutput())
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

func (s mandatoryEvidenceSection) ModelOutput() string {
	switch s.ToolName {
	case "codegraph_context":
		return truncateFusedContextForModel(s.Output, maxAskFusedContextBytes)
	default:
		return truncateForModel(s.Output, maxAskToolOutputBytes)
	}
}

func (b *Backend) collectMandatoryEvidencePack(ctx context.Context, req api.MCPRequest, query string, selectedRepos []string, ref string, tools []toolInfo) (mandatoryEvidencePack, error) {
	if b.cfg.SearchBackend == nil {
		return mandatoryEvidencePack{}, fmt.Errorf("retrieval layer unavailable: Zoekt search backend is required for ask_codebase/chat")
	}
	if b.cfg.Queries == nil {
		return mandatoryEvidencePack{}, fmt.Errorf("retrieval layer unavailable: Postgres query backend is required for ask_codebase/chat")
	}
	if b.cfg.GraphReader == nil {
		return mandatoryEvidencePack{}, fmt.Errorf("retrieval layer unavailable: NebulaGraph reader is required for ask_codebase/chat")
	}
	if !toolAvailable(tools, "codegraph_context") {
		return mandatoryEvidencePack{}, fmt.Errorf("retrieval layer unavailable: fused MCP tools are not fully configured")
	}

	pack := mandatoryEvidencePack{
		Query:    query,
		Sections: make([]mandatoryEvidenceSection, 0, 1),
		Trace:    make([]askToolTrace, 0, 8),
	}
	scopeFingerprint, scopeErr := b.askEvidenceScopeFingerprint(ctx, req.OrgID, selectedRepos, ref)
	if scopeErr != nil {
		return mandatoryEvidencePack{}, scopeErr
	}
	pack.ScopeFingerprint = scopeFingerprint
	call := func(layer, toolName string, args map[string]any) {
		raw, _ := json.Marshal(args)
		output, isErr := b.executeAskTool(ctx, req, openAIToolCall{
			ID: "mandatory_" + toolName,
			Function: openAIToolFunction{
				Name:      toolName,
				Arguments: string(raw),
			},
		}, selectedRepos)
		pack.Trace = append(pack.Trace, askToolTrace{
			Step:      0,
			ToolName:  toolName,
			IsError:   isErr,
			Arguments: truncateForTrace(string(raw), 4000),
			Output:    truncateForTrace(output, 4000),
		})
		section := mandatoryEvidenceSection{
			Layer:     layer,
			ToolName:  toolName,
			Attempted: true,
			Satisfied: mandatoryToolOutputHasEvidence(toolName, output) && !isErr,
			Output:    output,
		}
		if isErr {
			section.Error = output
		}
		pack.Sections = append(pack.Sections, section)
	}

	if len(selectedRepos) == 1 {
		for _, path := range preflightFilePaths(query, 5) {
			call("Direct file evidence for "+path, "read_file", map[string]any{
				"repo": selectedRepos[0],
				"path": path,
				"ref":  ref,
			})
		}
	}

	contextArgs := map[string]any{
		"query":   query,
		"limit":   int32(40),
		"compact": true,
	}
	if len(selectedRepos) == 1 {
		contextArgs["repo"] = selectedRepos[0]
	} else if len(selectedRepos) > 1 {
		contextArgs["repos"] = selectedRepos
	}
	if strings.TrimSpace(ref) != "" {
		contextArgs["ref"] = strings.TrimSpace(ref)
	}
	call("Primary fused codegraph context", "codegraph_context", contextArgs)
	if err := pack.RequireCoreEvidence(); err != nil {
		return pack, err
	}
	return pack, nil
}

func (p mandatoryEvidencePack) RequireCoreEvidence() error {
	var (
		zoektOK                  bool
		directFileOK             bool
		graphOK                  bool
		semanticGraphOK          bool
		scipDefinitionsAttempted bool
		scipReferencesAttempted  bool
		scipDefinitionsOK        bool
		scipReferencesOK         bool
		problems                 []string
	)
	for _, section := range p.Sections {
		if section.Error != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", section.Layer, section.Error))
			continue
		}
		switch section.ToolName {
		case "read_file":
			directFileOK = directFileOK || section.Satisfied
		case "codegraph_context":
			zoektSections := codegraphContextSections(section.Output, "Zoekt broad recall")
			for _, codegraphSection := range zoektSections {
				if codegraphSection.OK && !strings.Contains(codegraphSection.Body, "No files found") {
					zoektOK = true
					break
				}
			}
			graphSections := codegraphContextSections(section.Output, "Graph minimal context")
			for _, codegraphSection := range graphSections {
				if codegraphSection.OK &&
					!strings.Contains(codegraphSection.Body, "No minimal graph context found.") &&
					!strings.Contains(codegraphSection.Body, "No graph evidence found.") {
					graphOK = true
					break
				}
			}
			semanticGraphOK = graphOutputHasSemanticGraphEvidence(section.Output)
			definitionSections := codegraphContextSections(section.Output, "SCIP definitions")
			scipDefinitionsAttempted = scipDefinitionsAttempted || len(definitionSections) > 0
			for _, codegraphSection := range definitionSections {
				if codegraphSection.OK &&
					strings.Contains(codegraphSection.Body, "Found ") &&
					strings.Contains(codegraphSection.Body, "precise SCIP definition") {
					scipDefinitionsOK = true
					break
				}
			}
			referenceSections := codegraphContextSections(section.Output, "SCIP references")
			scipReferencesAttempted = scipReferencesAttempted || len(referenceSections) > 0
			for _, codegraphSection := range referenceSections {
				if codegraphSection.OK &&
					strings.Contains(codegraphSection.Body, "Found ") &&
					strings.Contains(codegraphSection.Body, "precise SCIP reference") {
					scipReferencesOK = true
					break
				}
			}
		case "grep":
			zoektOK = section.Satisfied
		case "inspect_code_graph":
			graphOK = section.Satisfied
			semanticGraphOK = graphOutputHasSemanticGraphEvidence(section.Output)
		case "find_symbol_definitions":
			scipDefinitionsAttempted = true
			scipDefinitionsOK = section.Satisfied
		case "find_symbol_references":
			scipReferencesAttempted = true
			scipReferencesOK = section.Satisfied
		}
	}
	if !zoektOK {
		problems = append(problems, "Zoekt broad recall returned no usable evidence")
	}
	if !graphOK {
		problems = append(problems, "Postgres graph metadata + Nebula traversal returned no usable native graph evidence")
	}
	if graphOK && !semanticGraphOK {
		problems = append(problems, "semantic graph evidence is missing from the graph inspection output")
	}
	requireDirectSCIP := p.requiresDirectSCIPPrecision()
	semanticGraphCanCoverScip := !requireDirectSCIP && zoektOK && graphOK && semanticGraphOK
	if !scipDefinitionsAttempted || !scipReferencesAttempted {
		if !semanticGraphCanCoverScip {
			problems = append(problems, "SCIP symbol precision was not attempted because the question had no code-like symbol token")
		}
	} else {
		if !scipDefinitionsOK {
			if !semanticGraphCanCoverScip {
				problems = append(problems, "SCIP symbol definitions returned no precise evidence")
			}
		}
		if !scipReferencesOK {
			if !semanticGraphCanCoverScip {
				problems = append(problems, "SCIP symbol references returned no precise evidence")
			}
		}
	}
	if directFileOK && len(problems) > 0 {
		if p.directFileEvidenceSatisfiesQuery() {
			return nil
		}
		problems = append(problems, "direct file evidence is present, but it does not replace required graph, semantic graph, and SCIP retrieval")
	}
	if len(problems) > 0 {
		return fmt.Errorf("retrieval layer incomplete: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (p mandatoryEvidencePack) requiresDirectSCIPPrecision() bool {
	query := strings.TrimSpace(p.Query)
	if query == "" {
		return true
	}
	lower := strings.ToLower(query)
	for _, marker := range []string{
		"architecture", "flow", "call chain", "call graph", "who calls",
		"references", "definition", "implementation", "impact", "blast radius",
		"sequence", "route", "event", "service", "database", "db flow",
		"cross-repo", "multi-repo", "dependency", "dependencies",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"marker", "literal", "string", "constant", "exact value", "value starts",
		"we discussed", "previous answer", "prior answer", "earlier answer",
		"without me restating", "same marker", "that marker", "the marker",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func (p mandatoryEvidencePack) directFileEvidenceSatisfiesQuery() bool {
	query := strings.TrimSpace(p.Query)
	if query == "" || len(preflightFilePaths(query, 2)) == 0 {
		return false
	}
	lower := strings.ToLower(query)
	for _, marker := range []string{
		"architecture", "flow", "call chain", "call graph", "who calls",
		"references", "impact", "blast radius", "sequence", "route",
		"event", "service", "database", "db flow", "cross-repo",
		"multi-repo", "dependency", "dependencies",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range []string{
		"read ", "inspect ", "show ", "open ", "exact ", "value",
		"marker", "string", "constant", "literal",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func toolAvailable(tools []toolInfo, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func mandatoryToolOutputHasEvidence(toolName, output string) bool {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return false
	}
	switch toolName {
	case "grep":
		return !strings.Contains(trimmed, "No files found")
	case "read_file":
		return strings.Contains(trimmed, "<content>") && !strings.Contains(trimmed, "(tool returned no text content)")
	case "codegraph_context":
		return codegraphContextHasUsableSection(trimmed, "Zoekt broad recall") &&
			codegraphContextHasUsableSection(trimmed, "Graph minimal context") &&
			codegraphContextHasUsableSection(trimmed, "SCIP definitions") &&
			codegraphContextHasUsableSection(trimmed, "SCIP references")
	case "inspect_code_graph":
		return strings.Contains(trimmed, "Native graph traversal (ranked NebulaGraph BFS neighborhood):") &&
			!strings.Contains(trimmed, "No graph evidence found.") &&
			!strings.Contains(trimmed, "No READY active code graph snapshots")
	case "find_symbol_definitions", "find_symbol_references":
		return strings.Contains(trimmed, "Found ") && strings.Contains(trimmed, "precise SCIP")
	default:
		return true
	}
}

type codegraphContextSection struct {
	Title string
	Body  string
	OK    bool
}

func codegraphContextSections(output, titlePrefix string) []codegraphContextSection {
	var sections []codegraphContextSection
	var current *codegraphContextSection
	flush := func() {
		if current != nil {
			sections = append(sections, *current)
			current = nil
		}
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if strings.HasPrefix(title, titlePrefix) {
				current = &codegraphContextSection{
					Title: title,
					Body:  line + "\n",
					OK:    strings.Contains(title, " ok)"),
				}
			}
			continue
		}
		if current != nil {
			current.Body += line + "\n"
		}
	}
	flush()
	return sections
}

func codegraphContextHasUsableSection(output, titlePrefix string) bool {
	for _, section := range codegraphContextSections(output, titlePrefix) {
		if section.OK && strings.TrimSpace(section.Body) != "" {
			return true
		}
	}
	return false
}

func graphOutputHasSemanticGraphEvidence(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{
		"source=scip",
		"provenance=scip",
		"precise scip",
		"scip relationships",
		"native graph traversal",
		"high-signal connected flow",
		"ranked implementation path",
		"semantic architecture facts",
		"source=syntactic-ast",
		"source=tree-sitter",
		"source=tree_sitter",
		"source=ast",
		"tree-sitter-",
		"tree_sitter",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func preflightSearchPattern(query string) string {
	terms := preflightCodeSearchTerms(query, 4)
	if len(terms) > 0 {
		return strings.Join(terms, "|")
	}
	symbols := preflightSymbols(query, 8)
	if len(symbols) > 0 {
		return strings.Join(symbols, "|")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "."
	}
	return query
}

func preflightCodeSearchTerms(query string, max int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, max)
	add := func(term string) {
		term = strings.Trim(term, ".,:;()[]{}<>\"'`")
		if len(term) < 3 {
			return
		}
		lower := strings.ToLower(term)
		if seen[lower] {
			return
		}
		seen[lower] = true
		out = append(out, term)
	}

	quoted := regexp.MustCompile(`["']([^"']+)["']`).FindAllStringSubmatch(query, -1)
	for _, match := range quoted {
		if len(match) < 2 {
			continue
		}
		term := strings.TrimSpace(match[1])
		if strings.Contains(term, "_") || strings.Contains(term, "$") {
			add(term)
		}
		if len(out) >= max {
			return out
		}
	}
	for _, path := range preflightFilePaths(query, max) {
		base := path
		if slash := strings.LastIndex(base, "/"); slash >= 0 {
			base = base[slash+1:]
		}
		if dot := strings.LastIndex(base, "."); dot > 0 {
			base = base[:dot]
		}
		add(base)
		if len(out) >= max {
			return out
		}
	}
	return out
}

func preflightFilePaths(query string, max int) []string {
	matches := regexp.MustCompile(`[A-Za-z0-9_.@-]+(?:/[A-Za-z0-9_.@-]+)+`).FindAllString(query, -1)
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, candidate := range matches {
		candidate = strings.Trim(candidate, ".,:;()[]{}<>\"'`")
		if !looksLikeSourceFilePath(candidate) || seen[candidate] {
			continue
		}
		clean, err := cleanRepoPath(candidate)
		if err != nil || clean == "" || seen[clean] {
			continue
		}
		seen[candidate] = true
		seen[clean] = true
		out = append(out, clean)
		if len(out) >= max {
			break
		}
	}
	return out
}

func looksLikeSourceFilePath(path string) bool {
	lower := strings.ToLower(path)
	for _, suffix := range []string{
		".go", ".rs", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".py", ".java", ".kt", ".kts", ".scala", ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp",
		".cs", ".fs", ".vb", ".rb", ".dart", ".php", ".swift", ".sql", ".proto",
		".yaml", ".yml", ".json", ".toml", ".xml", ".md",
	} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func preflightSymbols(query string, max int) []string {
	stop := map[string]bool{
		"architecture": true, "between": true, "branch": true, "branches": true, "codebase": true,
		"compare": true, "difference": true, "explain": true, "feature": true, "files": true,
		"flows": true, "function": true, "implementation": true, "indexed": true, "inside": true,
		"lifecycle": true, "method": true, "repos": true, "repository": true, "service": true,
		"symbol": true, "through": true, "where": true, "which": true,
		"answer": true, "code": true, "exact": true, "inspect": true, "marker": true,
		"anchors": true, "avoid": true, "context": true, "focus": true, "latest": true, "lines": true, "matter": true, "only": true,
		"previous": true, "prior": true, "question": true, "read": true, "retrieval": true,
		"relevant": true, "risk": true, "starts": true, "string": true, "using": true,
		"tenant": true, "tools": true, "value": true, "whose": true, "with": true,
		"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
		"be": true, "by": true, "does": true, "for": true, "from": true, "how": true,
		"important": true, "of": true, "on": true, "show": true, "that": true,
		"the": true, "their": true, "these": true, "this": true, "to": true,
		"use": true, "used": true, "what": true, "when": true,
	}
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '.' || r == '$' || r == '@' || r == ':' || r == '/' || r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	})
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, field := range fields {
		token := strings.Trim(field, ".,:;()[]{}<>\"'")
		lower := strings.ToLower(token)
		if strings.Contains(token, "/") {
			continue
		}
		if len(token) < 3 || stop[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, token)
		if len(out) >= max {
			break
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeAskAnswerBudget(mode string) (llmproxy.AnswerBudget, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "full"
	}
	switch mode {
	case "full":
		return llmproxy.AnswerBudget{Mode: "full", MaxOutputTokens: 12000, MaxAnswerBytes: maxAskAnswerBytes}, nil
	case "standard":
		return llmproxy.AnswerBudget{Mode: "standard", MaxOutputTokens: 8000, MaxAnswerBytes: 160000}, nil
	case "compact":
		return llmproxy.AnswerBudget{Mode: "compact", MaxOutputTokens: 6000, MaxAnswerBytes: 96000}, nil
	case "brief":
		return llmproxy.AnswerBudget{Mode: "brief", MaxOutputTokens: 3000, MaxAnswerBytes: 64000}, nil
	default:
		return llmproxy.AnswerBudget{}, fmt.Errorf("answerBudget must be one of full, standard, compact, or brief")
	}
}

func createAskSynthesisPrompt(selectedRepos []string, budget llmproxy.AnswerBudget) string {
	var b strings.Builder
	b.WriteString("You are a senior agentic code-development assistant. Retrieval has already completed deterministically before this model call. Do not request tools. Synthesize only from the evidence pack and clearly separate proven facts from gaps.\n\n")
	b.WriteString("Answer budget mode: ")
	b.WriteString(valueOrString(budget.Mode, "full"))
	b.WriteString(". ")
	switch budget.Mode {
	case "brief":
		b.WriteString("Prioritize a short coding-harness handoff: decision, the highest-risk files/functions, the critical flow, and exact gaps. Keep prose tight, avoid repeating evidence rows, do not use markdown tables, and cap each section to one compact paragraph or bullet group so the answer cannot truncate mid-structure.\n\n")
	case "compact":
		b.WriteString("Prioritize compact but complete implementation guidance: keep all required sections, use concise bullets/tables, and include only the most actionable evidence per section.\n\n")
	case "standard":
		b.WriteString("Prioritize balanced detail: include every required section, but compress repeated files, duplicated graph edges, and similar test evidence.\n\n")
	default:
		b.WriteString("Use the full coding-harness format when evidence supports it, but avoid duplicate rows and filler.\n\n")
	}
	b.WriteString("Evidence interpretation rules:\n")
	b.WriteString("- Zoekt sections are broad text recall. They are useful for coverage, exact files, tests, config keys, and candidate code paths, but may include lexical noise.\n")
	b.WriteString("- SCIP sections are semantic precision. Use them to distinguish definitions and references when they are present. If they are partial or lexical fallback only, say so.\n")
	b.WriteString("- AST/tree-sitter and graph sections are connected code-structure evidence. Use them for routes, functions, services, event paths, call paths, impact analysis, and implementation maps.\n")
	b.WriteString("- If graph evidence returns native traversal edges, SCIP relationships, semantic facts, architecture anchors, graph paths, or minimal-context packs, include concrete `source -> target` edge/path lines in the final Symbol And Call Graph section and label the evidence type.\n")
	b.WriteString("- If a section is labelled heuristic or low-confidence, describe it as a hint and verify it against exact file/line evidence before using it as a conclusion.\n")
	b.WriteString("- Never collapse `DEFINES` graph edges into \"no graph evidence\". If the graph has native `DEFINES` edges but lacks `CALLS`, `REFERENCES`, or `IMPORTS`, say exactly that.\n")
	b.WriteString("- If evidence is only lexical, say so. If a flow cannot be proven from retrieved code, call it a gap instead of inventing behavior.\n\n")
	b.WriteString("- Distinguish operator-side injection evidence from runtime repository implementation evidence. Do not say a selected runtime repository has no evidence if source slices, grep hits, SCIP rows, or graph rows from that repository are present. If the runtime implementation lives in a different repository than the selected/indexed scope, say that exact repository-scope gap instead.\n")
	b.WriteString("- Separate retrieved facts from inferred implementation plans. For new files, new functions, or projected insertion points, do not attach exact line numbers unless the evidence contains those existing lines; use phrases like \"near the existing NodeJS/Python/DotNet branch\" or \"new file\" instead of pretending planned code already exists.\n")
	b.WriteString("- Do not hand off vague instructions such as \"verify this file\" when the evidence pack contains source slices or exact grep rows for that file. Convert those rows into concrete facts with paths, line numbers, symbols, constants, and edit touchpoints.\n\n")
	b.WriteString("Answer format:\n")
	b.WriteString("Return markdown that starts with ")
	b.WriteString(answerTag)
	b.WriteString(" and include sections in this order: Answer / Decision, HLD Architecture Map, LLD Execution Flow, Exact Development Touchpoints, Gaps / Verification Needed, Multi-Repo Navigation, Code Navigation Table, Symbol And Call Graph, Function / Variable / Data Flow, Sequence Diagram, Behavioral Notes. ")
	b.WriteString("The response is consumed by an automated coding harness, so make it directly actionable: name exact repos, files, line numbers, functions, methods, variables, config keys, routes, events, dependencies, tests, and edit touchpoints whenever the evidence provides them. ")
	b.WriteString("For Exact Development Touchpoints and Gaps / Verification Needed, keep the sections short but never omit them; place them before broad navigation/tables because coding agents depend on them for edit planning even when a stream is interrupted. ")
	b.WriteString("For Multi-Repo Navigation, explain which repository owns each responsibility and how evidence connects repositories or runtime families. Use \"not in the selected/indexed repository set\" for likely external repos that are outside scope; reserve \"unindexed\" or \"skipped\" only for repos explicitly selected by the user and explicitly reported that way by the evidence. ")
	if budget.Mode == "brief" {
		b.WriteString("For Code Navigation, do not use a markdown table; use compact bullets with repo, path, line, symbol, role, and why it matters. ")
	} else {
		b.WriteString("For Code Navigation Table, use markdown table rows with repo, path, line, symbol, role, and why it matters. ")
	}
	b.WriteString("For Function / Variable / Data Flow, trace inputs, outputs, env vars, constants, parameters, return values, persisted state, events, and side effects only when the evidence supports them. ")
	b.WriteString("For Sequence Diagram, include a Mermaid sequenceDiagram or flowchart when there are at least two proven steps; otherwise say the diagram is omitted because the evidence is insufficient. ")
	b.WriteString("Every code claim must include a concrete repo/path/line reference when the tool output provides one.")
	if len(selectedRepos) > 0 {
		b.WriteString("\n\nSelected repositories:\n")
		for _, repo := range selectedRepos {
			b.WriteString("- ")
			b.WriteString(repo)
			b.WriteByte('\n')
		}
		b.WriteString("Stay within these repositories unless the user explicitly asks to broaden scope.")
	}
	return b.String()
}

func shouldPreloadCodeGraph(query string, tools []toolInfo) bool {
	hasGraph := false
	for _, tool := range tools {
		if tool.Name == "inspect_code_graph" {
			hasGraph = true
			break
		}
	}
	if !hasGraph {
		return false
	}
	lower := strings.ToLower(query)
	triggers := []string{
		"architecture", "lifecycle", "data flow", "data-flow", "cross repo", "cross-repo",
		"impact", "blast radius", "dependency", "dependencies", "how does", "flow",
		"route", "event", "service", "pipeline", "trace", "call graph", "symbol graph",
		"feature difference", "release comparison", "regression",
	}
	for _, trigger := range triggers {
		if strings.Contains(lower, trigger) {
			return true
		}
	}
	return false
}

func truncateForModel(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n\n(Output truncated before sending back to the model.)"
}

func truncateFusedContextForModel(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit < 4096 {
		return truncateForModel(value, limit)
	}
	headLimit := limit * 3 / 4
	tailLimit := limit - headLimit
	return strings.TrimRight(value[:headLimit], "\n") +
		"\n\n(Output middle truncated before synthesis; preserved tail evidence below.)\n\n" +
		strings.TrimLeft(value[len(value)-tailLimit:], "\n")
}

func truncateForTrace(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "... (truncated)"
}
