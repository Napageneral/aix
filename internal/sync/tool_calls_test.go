package sync

import "testing"

func TestExtractToolCallsFromBubble(t *testing.T) {
	bubbleJSON := `{
		"toolFormerData": {
			"toolCallId": "toolu_123",
			"name": "task_v2",
			"tool": 48,
			"params": "{\"description\":\"Do it\",\"prompt\":\"Hello\",\"model\":\"m\"}",
			"result": {"ok": true},
			"status": "completed"
		}
	}`
	calls := extractToolCallsFromBubble("s1", "m1", bubbleJSON)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "toolu_123" || calls[0].ToolName != "task_v2" {
		t.Fatalf("unexpected tool call: %+v", calls[0])
	}
	if calls[0].ChildSessionID != "task-toolu_123" {
		t.Fatalf("expected child_session_id task-toolu_123, got %s", calls[0].ChildSessionID)
	}
	if calls[0].ParamsJSON == "" || calls[0].ResultJSON == "" {
		t.Fatalf("expected params/result json, got params=%q result=%q", calls[0].ParamsJSON, calls[0].ResultJSON)
	}

	arrayJSON := `{
		"toolFormerData": [
			{"toolCallId": "call_1", "name": "Shell", "tool": 12, "params": {"cmd": "ls"}}
		]
	}`
	calls = extractToolCallsFromBubble("s2", "m2", arrayJSON)
	if len(calls) != 1 || calls[0].ID != "call_1" {
		t.Fatalf("unexpected array tool calls: %+v", calls)
	}
}
