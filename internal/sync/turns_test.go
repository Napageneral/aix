package sync

import (
	"testing"

	"github.com/Napageneral/aix/internal/models"
)

func TestBuildTurnsForSession(t *testing.T) {
	session := models.Session{ID: "s1", Model: "m1"}
	messages := []models.Message{
		{ID: "u1", SessionID: "s1", Role: "user", Sequence: 0},
		{ID: "u2", SessionID: "s1", Role: "user", Sequence: 1},
		{ID: "a1", SessionID: "s1", Role: "assistant", Sequence: 2, Timestamp: 100},
		{ID: "a1b", SessionID: "s1", Role: "assistant", Sequence: 3, Timestamp: 110},
		{ID: "u3", SessionID: "s1", Role: "user", Sequence: 4},
		{ID: "a2", SessionID: "s1", Role: "assistant", Sequence: 5, Timestamp: 200},
		{ID: "a2b", SessionID: "s1", Role: "assistant", Sequence: 6, Timestamp: 210},
	}
	toolCalls := []models.ToolCall{
		{ID: "tc1", MessageID: "a1", SessionID: "s1"},
		{ID: "tc2", MessageID: "a1b", SessionID: "s1"},
	}

	turns := buildTurnsForSession(&session, messages, toolCalls)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}
	if turns[0].ID != "a1b" || turns[0].ParentTurnID != "" {
		t.Fatalf("unexpected first turn ids: %+v", turns[0])
	}
	if len(turns[0].QueryMessageIDs) != 2 || turns[0].QueryMessageIDs[0] != "u1" || turns[0].QueryMessageIDs[1] != "u2" {
		t.Fatalf("unexpected query ids for first turn: %+v", turns[0].QueryMessageIDs)
	}
	if turns[0].ToolCallCount != 2 {
		t.Fatalf("expected tool call count 2, got %d", turns[0].ToolCallCount)
	}
	if turns[1].ParentTurnID != "a1b" || turns[1].ID != "a2b" {
		t.Fatalf("unexpected second turn ids: %+v", turns[1])
	}
	if len(turns[1].QueryMessageIDs) != 1 || turns[1].QueryMessageIDs[0] != "u3" {
		t.Fatalf("unexpected query ids for second turn: %+v", turns[1].QueryMessageIDs)
	}
}

func TestBuildTurnsForSession_AssistantOnly(t *testing.T) {
	session := models.Session{ID: "s2", Model: "m1"}
	messages := []models.Message{
		{ID: "a1", SessionID: "s2", Role: "assistant", Sequence: 0, Timestamp: 100},
		{ID: "a2", SessionID: "s2", Role: "assistant", Sequence: 1, Timestamp: 110},
	}
	turns := buildTurnsForSession(&session, messages, nil)
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if turns[0].ID != "a2" || turns[0].ResponseMessageID != "a2" {
		t.Fatalf("unexpected turn ids: %+v", turns[0])
	}
	if len(turns[0].QueryMessageIDs) != 0 {
		t.Fatalf("expected empty query ids, got %+v", turns[0].QueryMessageIDs)
	}
}
