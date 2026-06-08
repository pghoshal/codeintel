package llmproxy

type OpenAICompatibleConfig struct {
	Model       string            `json:"model"`
	BaseURL     string            `json:"baseUrl"`
	Token       string            `json:"token,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	QueryParams map[string]string `json:"queryParams,omitempty"`
	MaxTokens   int               `json:"maxTokens,omitempty"`
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type LanguageModelInfo struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName,omitempty"`
}

type ChatRequest struct {
	RequestID   string                 `json:"requestId"`
	OrgID       int32                  `json:"orgId"`
	Provider    string                 `json:"provider"`
	Model       LanguageModelInfo      `json:"model"`
	OpenAI      OpenAICompatibleConfig `json:"openaiCompatible"`
	Messages    []ChatMessage          `json:"messages"`
	Tools       []ToolInfo             `json:"tools,omitempty"`
	MaxAttempts int                    `json:"maxAttempts,omitempty"`
	Stream      bool                   `json:"stream,omitempty"`
	Budget      AnswerBudget           `json:"budget,omitempty"`
	Metadata    map[string]any         `json:"metadata,omitempty"`
}

type ChatResponse struct {
	RequestID  string            `json:"requestId"`
	Status     string            `json:"status"`
	Content    string            `json:"content,omitempty"`
	ToolCalls  []ToolCall        `json:"toolCalls,omitempty"`
	DurationMs int64             `json:"durationMs,omitempty"`
	Error      string            `json:"error,omitempty"`
	Model      LanguageModelInfo `json:"model"`
	Partial    bool              `json:"partial,omitempty"`
	UpdatedAt  string            `json:"updatedAt,omitempty"`
	Budget     AnswerBudget      `json:"budget,omitempty"`
	Metadata   map[string]any    `json:"metadata,omitempty"`
}

type AnswerBudget struct {
	Mode            string `json:"mode,omitempty"`
	MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
	MaxAnswerBytes  int    `json:"maxAnswerBytes,omitempty"`
}
