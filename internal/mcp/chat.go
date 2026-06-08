package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"codeintel/internal/api"
)

// Ask exposes the same retrieval/agent loop used by MCP
// ask_codebase to the headless HTTP chat API. The API layer owns chat
// rows and auth; this layer owns model/tool execution.
func (b *Backend) Ask(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	query := strings.TrimSpace(req.Query)
	conversationContext := buildChatConversationContext(req.PriorMessages)
	retrievalQuery := buildChatRetrievalQuery(query, conversationContext)

	var lm *languageModelRequest
	if req.LanguageModel != nil {
		lm = &languageModelRequest{
			Provider:    req.LanguageModel.Provider,
			Model:       req.LanguageModel.Model,
			DisplayName: req.LanguageModel.DisplayName,
		}
	}
	body, err := json.Marshal(map[string]any{
		"query":               query,
		"repos":               req.Repos,
		"languageModel":       lm,
		"async":               req.Async,
		"answerBudget":        req.AnswerBudget,
		"conversationContext": conversationContext,
		"retrievalQuery":      retrievalQuery,
	})
	if err != nil {
		return api.ChatResponse{}, err
	}
	result, err := b.toolAskCodebase(ctx, api.MCPRequest{
		OrgID:     req.OrgID,
		OrgDomain: req.OrgDomain,
		Method:    "POST",
	}, body)
	if err != nil {
		return api.ChatResponse{}, err
	}
	out := api.ChatResponse{
		Answer: toolResultText(result),
		ChatID: req.ChatID,
		Status: "SUCCEEDED",
	}
	if v, ok := result.Meta["sessionId"].(string); ok {
		out.SessionID = v
	}
	if v, ok := result.Meta["status"].(string); ok && strings.TrimSpace(v) != "" {
		out.Status = strings.ToUpper(strings.TrimSpace(v))
	}
	if result.IsError {
		out.Status = "FAILED"
		out.Error = toolResultText(result)
	}
	if v, ok := result.Meta["toolTrace"]; ok {
		out.ToolTrace = v
	}
	if v, ok := result.Meta["languageModel"].(languageModelInfo); ok {
		out.LanguageModel = api.ChatLanguageModel{Provider: v.Provider, Model: v.Model, DisplayName: v.DisplayName}
	}
	return out, nil
}

func buildChatConversationContext(messages []api.ChatMessage) string {
	const (
		maxMessages     = 8
		maxMessageBytes = 1400
		maxContextBytes = maxAskConversationContextBytes
	)
	if len(messages) == 0 {
		return ""
	}
	start := 0
	if len(messages) > maxMessages {
		start = len(messages) - maxMessages
	}
	var history strings.Builder
	history.WriteString("Recent prior chat turns, oldest to newest:\n")
	for _, msg := range messages[start:] {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		role := strings.ToUpper(strings.TrimSpace(msg.Role))
		if role == "" {
			role = "MESSAGE"
		}
		history.WriteString(role)
		history.WriteString(": ")
		history.WriteString(truncateForTrace(content, maxMessageBytes))
		history.WriteString("\n")
		if history.Len() >= maxContextBytes {
			return truncateForModel(history.String(), maxContextBytes)
		}
	}
	return strings.TrimSpace(history.String())
}

func buildChatRetrievalQuery(query string, conversationContext string) string {
	const maxRetrievalQueryBytes = 4000
	query = strings.TrimSpace(query)
	anchors := extractChatRetrievalAnchors(conversationContext, 32)
	if len(anchors) == 0 {
		return query
	}
	var out strings.Builder
	out.WriteString(query)
	out.WriteString("\n\nRelevant prior code anchors for retrieval only:\n")
	for _, anchor := range anchors {
		out.WriteString("- ")
		out.WriteString(anchor)
		out.WriteString("\n")
		if out.Len() >= maxRetrievalQueryBytes {
			return truncateForModel(out.String(), maxRetrievalQueryBytes)
		}
	}
	return strings.TrimSpace(out.String())
}

func extractChatRetrievalAnchors(conversationContext string, max int) []string {
	if max <= 0 || strings.TrimSpace(conversationContext) == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, max)
	add := func(anchor string) {
		anchor = strings.Trim(anchor, " \t\r\n.,:;()[]{}<>\"'`")
		if len(anchor) < 3 {
			return
		}
		lower := strings.ToLower(anchor)
		if seen[lower] || isChatRetrievalAnchorStopword(lower) {
			return
		}
		seen[lower] = true
		out = append(out, anchor)
	}
	for _, path := range preflightFilePaths(conversationContext, max) {
		add(path)
		if len(out) >= max {
			return out
		}
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*\b`),
		regexp.MustCompile(`\b[A-Z][A-Z0-9_]{3,}\b`),
		regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*(?:[A-Z][A-Za-z0-9_]*)+\b`),
		regexp.MustCompile("`([^`]{3,120})`"),
	}
	for _, pattern := range patterns {
		matches := pattern.FindAllStringSubmatch(conversationContext, -1)
		for _, match := range matches {
			anchor := match[0]
			if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
				anchor = match[1]
			}
			if strings.Contains(anchor, "\n") {
				continue
			}
			add(anchor)
			if len(out) >= max {
				return out
			}
		}
	}
	return out
}

func isChatRetrievalAnchorStopword(lower string) bool {
	if isCodegraphStopTerm(lower) {
		return true
	}
	switch lower {
	case "assistant", "conversation", "evidence", "latest", "previous", "prior", "question", "retrieval", "using":
		return true
	default:
		return false
	}
}

func (b *Backend) Get(ctx context.Context, req api.ChatResultRequest) (api.ChatResponse, error) {
	body, err := json.Marshal(map[string]any{
		"requestId": req.SessionID,
	})
	if err != nil {
		return api.ChatResponse{}, err
	}
	result, err := b.toolGetAskCodebaseResult(ctx, api.MCPRequest{
		OrgID:     req.OrgID,
		OrgDomain: req.OrgDomain,
		Method:    "GET",
	}, body)
	if err != nil {
		return api.ChatResponse{}, err
	}
	out := api.ChatResponse{
		Answer:    toolResultText(result),
		ChatID:    req.ChatID,
		SessionID: req.SessionID,
		Status:    "SUCCEEDED",
	}
	if v, ok := result.Meta["sessionId"].(string); ok && strings.TrimSpace(v) != "" {
		out.SessionID = v
	}
	if v, ok := result.Meta["status"].(string); ok && strings.TrimSpace(v) != "" {
		out.Status = strings.ToUpper(strings.TrimSpace(v))
	}
	if result.IsError {
		out.Status = "FAILED"
		out.Error = toolResultText(result)
	}
	if v, ok := result.Meta["languageModel"].(languageModelInfo); ok {
		out.LanguageModel = api.ChatLanguageModel{Provider: v.Provider, Model: v.Model, DisplayName: v.DisplayName}
	}
	return out, nil
}
