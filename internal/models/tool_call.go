package models

// ToolCall represents a tool invocation captured from a message.
type ToolCall struct {
	ID             string `json:"id"`                 // toolCallId
	MessageID      string `json:"message_id"`         // bubbleId
	SessionID      string `json:"session_id"`         // parent session id
	ToolName       string `json:"tool_name"`          // tool name (e.g. task_v2)
	ToolNumber     int    `json:"tool_number"`        // numeric tool id (Cursor)
	ParamsJSON     string `json:"params_json"`        // raw params (JSON string or object)
	ResultJSON     string `json:"result_json"`        // raw result (JSON string or object)
	Status         string `json:"status"`             // pending, running, completed, failed
	ChildSessionID string `json:"child_session_id"`   // task-<toolCallId> if a subagent was spawned
	StartedAt      int64  `json:"started_at"`         // optional timestamp (ms)
	CompletedAt    int64  `json:"completed_at"`       // optional timestamp (ms)
}
