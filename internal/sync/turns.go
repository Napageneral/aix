package sync

import "github.com/Napageneral/aix/internal/models"

func buildTurnsForSession(session *models.Session, messages []models.Message, toolCalls []models.ToolCall) []models.Turn {
	if session == nil || len(messages) == 0 {
		return nil
	}

	toolCallsByMessage := make(map[string]int)
	for _, tc := range toolCalls {
		if tc.MessageID != "" {
			toolCallsByMessage[tc.MessageID]++
		}
	}

	var turns []models.Turn
	var queryIDs []string
	var responseIDs []string
	parentTurnID := ""
	lastResponseTimestamp := int64(0)

	finalize := func() {
		if len(responseIDs) == 0 {
			return
		}
		responseID := responseIDs[len(responseIDs)-1]
		toolCallCount := 0
		for _, rid := range responseIDs {
			toolCallCount += toolCallsByMessage[rid]
		}
		turn := models.Turn{
			ID:                responseID,
			SessionID:         session.ID,
			ParentTurnID:      parentTurnID,
			QueryMessageIDs:   append([]string(nil), queryIDs...),
			ResponseMessageID: responseID,
			Model:             session.Model,
			TokenCount:        0,
			Timestamp:         lastResponseTimestamp,
			HasChildren:       false,
			ToolCallCount:     toolCallCount,
		}
		turns = append(turns, turn)
		parentTurnID = turn.ID
		queryIDs = nil
		responseIDs = nil
		lastResponseTimestamp = 0
	}

	for _, msg := range messages {
		switch msg.Role {
		case "assistant":
			if msg.ID != "" {
				responseIDs = append(responseIDs, msg.ID)
			}
			if msg.Timestamp > 0 {
				lastResponseTimestamp = msg.Timestamp
			}
		case "user":
			// Finalize any prior response sequence (one turn per user request).
			if len(responseIDs) > 0 {
				finalize()
			}
			if msg.ID != "" {
				queryIDs = append(queryIDs, msg.ID)
			}
		default:
			// Treat non-assistant messages as part of the query input.
			if msg.ID != "" {
				queryIDs = append(queryIDs, msg.ID)
			}
		}
	}

	// Finalize any trailing assistant sequence (handles assistant-only sessions).
	if len(responseIDs) > 0 {
		finalize()
	}

	return turns
}
