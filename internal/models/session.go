package models

// Session represents a parsed AI conversation session
type Session struct {
	ID           string `json:"id"`            // composerId UUID or task-<toolCallId> for subagents
	Source       string `json:"source"`        // 'cursor', 'claude', etc.
	Project      string `json:"project"`       // inferred from file paths
	Model        string `json:"model"`         // AI model used (e.g. 'claude-4.5-opus-high-thinking')
	CreatedAt    int64  `json:"created_at"`    // unix timestamp (ms)
	MessageCount int    `json:"message_count"` // number of messages
	Summary      string `json:"summary"`       // optional generated summary

	// Subagent/task tracking (Cursor task_v2 support)
	ParentSessionID string `json:"parent_session_id,omitempty"` // parent session that spawned this task
	ParentMessageID string `json:"parent_message_id,omitempty"` // bubble in parent where task was dispatched
	ToolCallID      string `json:"tool_call_id,omitempty"`      // toolCallId linking parent → child
	TaskDescription string `json:"task_description,omitempty"`  // description param from task dispatch
	TaskStatus      string `json:"task_status,omitempty"`       // pending, running, completed, failed
	IsSubagent      bool   `json:"is_subagent,omitempty"`       // true if this is a subagent/task session

	// Session-level metadata (Cursor-specific)
	ContextTokenLimit  int    `json:"context_token_limit,omitempty"`  // e.g. 176000
	ContextTokensUsed  int    `json:"context_tokens_used,omitempty"`  // tokens used in context window
	IsAgentic          bool   `json:"is_agentic,omitempty"`           // true if agentic mode
	ForceMode          string `json:"force_mode,omitempty"`           // 'edit', 'chat', etc.
	WorkspacePath      string `json:"workspace_path,omitempty"`       // workspace root path
	ContextJSON        string `json:"context_json,omitempty"`         // session-level context (fileSelections, etc.)
	ConversationState  string `json:"conversation_state,omitempty"`   // serialized conversation state
}

// Message represents a single message in a conversation
type Message struct {
	ID        string `json:"id"`         // bubbleId
	SessionID string `json:"session_id"` // references Session.ID
	Role      string `json:"role"`       // 'user', 'assistant', 'tool'
	Content   string `json:"content"`    // text content
	Sequence  int    `json:"sequence"`   // order in conversation
	Timestamp int64  `json:"timestamp"`  // optional timestamp

	// Message-level metadata (Cursor-specific)
	CheckpointID     string `json:"checkpoint_id,omitempty"`      // for forking/checkpoints
	IsAgentic        bool   `json:"is_agentic,omitempty"`         // true if agentic mode
	IsPlanExecution  bool   `json:"is_plan_execution,omitempty"`  // true if plan execution mode
	ContextJSON      string `json:"context_json,omitempty"`       // per-message context
	CursorRulesJSON  string `json:"cursor_rules_json,omitempty"`  // cursor rules in effect
}

// FileReference represents a file referenced in a session
type FileReference struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
}

// SyncResult contains stats from a sync operation
type SyncResult struct {
	Source     string `json:"source"`
	Synced     int    `json:"synced"`
	New        int    `json:"new"`
	Updated    int    `json:"updated"`
	Errors     int    `json:"errors"`
	DurationMs int64  `json:"duration_ms"`
}

// SessionWithMessages is a session with its messages for display
type SessionWithMessages struct {
	Session  Session   `json:"session"`
	Messages []Message `json:"messages"`
	Files    []string  `json:"files"`
}
