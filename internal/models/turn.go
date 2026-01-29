package models

// Turn represents a single query/response exchange within a session.
type Turn struct {
	ID                string   `json:"id"`                  // final assistant message id
	SessionID         string   `json:"session_id"`          // parent session id
	ParentTurnID      string   `json:"parent_turn_id"`       // previous turn id
	QueryMessageIDs   []string `json:"query_message_ids"`    // input message ids
	ResponseMessageID string   `json:"response_message_id"`  // assistant message id
	Model             string   `json:"model"`                // model used
	TokenCount        int      `json:"token_count"`          // optional
	Timestamp         int64    `json:"timestamp"`            // ms
	HasChildren       bool     `json:"has_children"`         // forked from
	ToolCallCount     int      `json:"tool_call_count"`       // number of tool calls in this turn
}
