# AIX → Mnemonic Pipeline Specification

**Status:** DESIGN SPEC  
**Last Updated:** 2026-01-29  
**Purpose:** Define AIX's role as capture layer and how it feeds into Mnemonic  

---

## Executive Summary

**AIX is the capture layer.** It syncs AI sessions from multiple harnesses (Cursor, Codex, Nexus, Clawdbot) into a local SQLite database with full fidelity.

**Mnemonic is the memory layer.** It consumes AIX data via two adapters:
1. **aix-events** — Trimmed turns for the Events ledger (memory extraction)
2. **aix-agents** — Full fidelity for the Agents ledger (smart forking)

AIX does NOT do analysis, embeddings, or smart forking. Those live in Mnemonic.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         Data Sources                            │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐            │
│  │ Cursor  │  │  Codex  │  │  Nexus  │  │Clawdbot │            │
│  │state.vsc│  │  JSONL  │  │  JSONL  │  │  JSONL  │            │
│  └────┬────┘  └────┬────┘  └────┬────┘  └────┬────┘            │
│       │            │            │            │                  │
│       └────────────┼────────────┼────────────┘                  │
│                    ▼                                            │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                         AIX                              │   │
│  │                   (Capture Layer)                        │   │
│  │                                                          │   │
│  │  • Parses source-specific formats                        │   │
│  │  • Normalizes into unified schema                        │   │
│  │  • Stores full fidelity locally                          │   │
│  │  • NO analysis, NO embeddings, NO forking                │   │
│  │                                                          │   │
│  │  aix.db:                                                 │   │
│  │    sessions, messages, turns, tool_calls                 │   │
│  └────────────────────────┬────────────────────────────────┘   │
│                           │                                     │
│              ┌────────────┴────────────┐                       │
│              ▼                         ▼                        │
│  ┌───────────────────┐     ┌───────────────────┐               │
│  │   aix-events      │     │   aix-agents      │               │
│  │   Adapter         │     │   Adapter         │               │
│  │                   │     │                   │               │
│  │ Extracts trimmed  │     │ Copies full       │               │
│  │ turns: 1 user     │     │ fidelity:         │               │
│  │ event + 1 asst    │     │ sessions, msgs,   │               │
│  │ event per turn    │     │ turns, tool_calls │               │
│  └─────────┬─────────┘     └─────────┬─────────┘               │
│            │                         │                          │
│            ▼                         ▼                          │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                       Mnemonic                           │   │
│  │                    (Memory Layer)                        │   │
│  │                                                          │   │
│  │  Events Ledger          Agents Ledger                    │   │
│  │  ┌────────────┐         ┌────────────────────┐          │   │
│  │  │ events     │         │ agent_sessions     │          │   │
│  │  │ threads    │         │ agent_messages     │          │   │
│  │  │            │         │ agent_turns        │          │   │
│  │  │ (trimmed   │         │ agent_tool_calls   │          │   │
│  │  │  AI turns) │         │                    │          │   │
│  │  └────────────┘         │ (full fidelity)    │          │   │
│  │                         └────────────────────┘          │   │
│  │                                                          │   │
│  │  Core Ledger (shared):                                   │   │
│  │  episodes, analysis_runs, facets, embeddings             │   │
│  │                                                          │   │
│  │  • Chunking (episodes)                                   │   │
│  │  • Analysis (LLM extraction)                             │   │
│  │  • Embeddings (semantic search)                          │   │
│  │  • Smart forking                                         │   │
│  └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

---

## AIX Responsibilities

### What AIX Does

1. **Source Parsing**
   - Cursor: Parse `state.vscdb` (SQLite key-value store)
   - Codex/Claude Code: Parse JSONL transcripts
   - Nexus/Clawdbot: Parse pi-coding-agent JSONL

2. **Schema Normalization**
   - Map source-specific formats to unified schema
   - Extract session metadata (model, context, workspace)
   - Extract message metadata (checkpoints, capabilities, rules)
   - Build turn boundaries
   - Extract tool calls

3. **Subagent Linking**
   - Parse embedded composerData from parent dispatches
   - Scan `bubbleId:task-*` keys for separate child sessions
   - Link parent→child via `tool_call_id`
   - Create minimal sessions for UUID-format dispatches (prompt preserved, no messages)

4. **Local Storage**
   - Store in `aix.db` SQLite database
   - Maintain full fidelity (raw_json preserved)
   - Track sync state for incremental updates

### What AIX Does NOT Do

- **Analysis** — No LLM extraction, no facet generation
- **Embeddings** — No vector generation
- **Smart forking** — No fork context building
- **Search** — No semantic search
- **Episodes** — No chunking (turns are computed but not "episodes")

These all live in Mnemonic.

---

## AIX Schema (Current)

```sql
-- sessions: AI conversation sessions
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,                -- 'cursor', 'codex', 'nexus', 'clawdbot'
    project TEXT,
    model TEXT,
    created_at INTEGER,
    message_count INTEGER,
    summary TEXT,
    raw_json TEXT,
    
    -- Subagent fields
    parent_session_id TEXT,
    parent_message_id TEXT,
    tool_call_id TEXT,
    task_description TEXT,
    task_status TEXT,
    is_subagent INTEGER DEFAULT 0,
    
    -- Session context
    context_token_limit INTEGER,
    context_tokens_used INTEGER,
    is_agentic INTEGER DEFAULT 0,
    force_mode TEXT,
    workspace_path TEXT,
    context_json TEXT,
    conversation_state TEXT
);

-- messages: Individual messages within sessions
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    session_id TEXT REFERENCES sessions(id),
    role TEXT NOT NULL,
    content TEXT,
    sequence INTEGER,
    timestamp INTEGER,
    
    -- Message metadata
    checkpoint_id TEXT,
    is_agentic INTEGER DEFAULT 0,
    is_plan_execution INTEGER DEFAULT 0,
    context_json TEXT,
    cursor_rules_json TEXT
);

-- turns: Query+response exchanges (Nexus ontology alignment)
CREATE TABLE turns (
    id TEXT PRIMARY KEY,                 -- Same as final response message ID
    session_id TEXT NOT NULL REFERENCES sessions(id),
    parent_turn_id TEXT REFERENCES turns(id),
    
    query_message_ids TEXT,              -- JSON array
    response_message_id TEXT REFERENCES messages(id),
    
    model TEXT,
    token_count INTEGER,
    timestamp INTEGER,
    has_children INTEGER DEFAULT 0,
    tool_call_count INTEGER DEFAULT 0
);

-- tool_calls: Tool invocations within messages
CREATE TABLE tool_calls (
    id TEXT PRIMARY KEY,
    message_id TEXT REFERENCES messages(id),
    session_id TEXT NOT NULL REFERENCES sessions(id),
    
    tool_name TEXT,
    tool_number INTEGER,
    params_json TEXT,
    result_json TEXT,
    status TEXT,
    
    child_session_id TEXT,               -- Links to spawned subagent
    started_at INTEGER,
    completed_at INTEGER
);
```

---

## Mnemonic Adapters

### aix-events Adapter

**Purpose:** Export trimmed turns to Events ledger for memory extraction.

**Logic:**
```go
// For each AIX turn:
// 1. Consolidate all query messages into ONE user content string
// 2. Extract ONLY the final assistant text (drop tool calls, thinking)
// 3. Create 2 events: user + assistant

func (a *AIXEventsAdapter) exportTrimmedTurn(turn *Turn) (Event, Event) {
    // User event: consolidate query messages
    userContent := ""
    for _, msgID := range turn.QueryMessageIDs {
        msg := a.getMessage(msgID)
        if msg.Role == "user" {
            userContent += msg.Content + "\n"
        }
    }
    
    userEvent := Event{
        ID:            fmt.Sprintf("aix:turn:%s:user", turn.ID),
        Timestamp:     turn.Timestamp,
        Channel:       sessionSource,  // "cursor", "codex", etc.
        Content:       strings.TrimSpace(userContent),
        Direction:     "sent",
        ThreadID:      fmt.Sprintf("aix:session:%s", turn.SessionID),
        SourceAdapter: "aix-events",
        SourceID:      turn.ID + ":user",
    }
    
    // Assistant event: final response only (no tool calls)
    responseMsg := a.getMessage(turn.ResponseMessageID)
    assistantContent := extractTextOnly(responseMsg.Content)  // Strip tool call blocks
    
    assistantEvent := Event{
        ID:            fmt.Sprintf("aix:turn:%s:assistant", turn.ID),
        Timestamp:     turn.Timestamp,
        Channel:       sessionSource,
        Content:       assistantContent,
        Direction:     "received",
        ThreadID:      fmt.Sprintf("aix:session:%s", turn.SessionID),
        SourceAdapter: "aix-events",
        SourceID:      turn.ID + ":assistant",
        MetadataJSON:  `{"model": "` + turn.Model + `"}`,
    }
    
    return userEvent, assistantEvent
}
```

**Result:**
- ~18k turns → ~18k user events + ~18k assistant events
- NOT 294k assistant bubbles
- Suitable for memory extraction ("what did the user ask?", "what did the AI respond?")

### aix-agents Adapter

**Purpose:** Export full fidelity to Agents ledger for smart forking.

**Logic:**
```go
// Copy everything: sessions, messages, turns, tool_calls
func (a *AIXAgentsAdapter) Sync(ctx context.Context) error {
    sessions := a.aix.GetAllSessions()
    
    for _, session := range sessions {
        // Copy session
        a.mnemonic.UpsertAgentSession(AgentSession{
            ID:                session.ID,
            Source:            session.Source,
            Model:             session.Model,
            Project:           session.Project,
            ParentSessionID:   session.ParentSessionID,
            ParentMessageID:   session.ParentMessageID,
            ToolCallID:        session.ToolCallID,
            TaskDescription:   session.TaskDescription,
            TaskStatus:        session.TaskStatus,
            IsSubagent:        session.IsSubagent,
            ContextTokenLimit: session.ContextTokenLimit,
            ContextTokensUsed: session.ContextTokensUsed,
            IsAgentic:         session.IsAgentic,
            ForceMode:         session.ForceMode,
            WorkspacePath:     session.WorkspacePath,
            ContextJSON:       session.ContextJSON,
            ConversationState: session.ConversationState,
            RawJSON:           session.RawJSON,
            CreatedAt:         session.CreatedAt,
        })
        
        // Copy all messages
        messages := a.aix.GetMessagesForSession(session.ID)
        for _, msg := range messages {
            a.mnemonic.UpsertAgentMessage(AgentMessage{
                ID:              msg.ID,
                SessionID:       session.ID,
                Role:            msg.Role,
                Content:         msg.Content,
                Sequence:        msg.Sequence,
                Timestamp:       msg.Timestamp,
                CheckpointID:    msg.CheckpointID,
                IsAgentic:       msg.IsAgentic,
                IsPlanExecution: msg.IsPlanExecution,
                ContextJSON:     msg.ContextJSON,
                CursorRulesJSON: msg.CursorRulesJSON,
                MetadataJSON:    msg.MetadataJSON,
            })
        }
        
        // Copy turns
        turns := a.aix.GetTurnsForSession(session.ID)
        for _, turn := range turns {
            a.mnemonic.UpsertAgentTurn(AgentTurn{
                ID:                turn.ID,
                SessionID:         turn.SessionID,
                ParentTurnID:      turn.ParentTurnID,
                QueryMessageIDs:   turn.QueryMessageIDs,
                ResponseMessageID: turn.ResponseMessageID,
                Model:             turn.Model,
                TokenCount:        turn.TokenCount,
                Timestamp:         turn.Timestamp,
                HasChildren:       turn.HasChildren,
                ToolCallCount:     turn.ToolCallCount,
            })
        }
        
        // Copy tool calls
        toolCalls := a.aix.GetToolCallsForSession(session.ID)
        for _, tc := range toolCalls {
            a.mnemonic.UpsertAgentToolCall(AgentToolCall{
                ID:             tc.ID,
                MessageID:      tc.MessageID,
                SessionID:      tc.SessionID,
                ToolName:       tc.ToolName,
                ToolNumber:     tc.ToolNumber,
                ParamsJSON:     tc.ParamsJSON,
                ResultJSON:     tc.ResultJSON,
                Status:         tc.Status,
                ChildSessionID: tc.ChildSessionID,
                StartedAt:      tc.StartedAt,
                CompletedAt:    tc.CompletedAt,
            })
        }
    }
    
    return nil
}
```

**Result:**
- Full copy of AIX data into Mnemonic's Agents ledger
- Enables smart forking, full session replay, tool analysis
- Subagent relationships preserved

---

## Smart Forking (Lives in Mnemonic)

Smart forking requires finding relevant past context to seed a new conversation. This needs:

1. **Embedding search** — Find semantically similar past turns
2. **Episode analysis** — Understand what happened (extracted facets)
3. **Context assembly** — Build optimal context window

All of this lives in Mnemonic, not AIX.

### Fork Context Builder (Mnemonic)

```go
type ForkContextBuilder struct {
    agents *AgentsLedger
    search *SearchEngine
}

func (b *ForkContextBuilder) Build(query string, opts ForkOptions) (*ForkContext, error) {
    // 1. Semantic search over agent turns
    results := b.search.SearchAgentTurns(query, SearchOptions{
        MaxResults: opts.MaxTurns,
        Source:     opts.Source,  // optional: filter by source
    })
    
    // 2. Build context from relevant turns
    var contexts []TurnContext
    for _, result := range results {
        turn := b.agents.GetTurn(result.TurnID)
        session := b.agents.GetSession(turn.SessionID)
        messages := b.agents.GetMessagesForTurn(turn)
        
        contexts = append(contexts, TurnContext{
            Turn:        turn,
            Session:     session,
            Messages:    messages,
            Score:       result.Score,
            WorkspacePath: session.WorkspacePath,
        })
    }
    
    // 3. Token-aware selection
    selectedContexts := selectWithinTokenBudget(contexts, opts.MaxTokens)
    
    return &ForkContext{
        Query:         query,
        RelevantTurns: selectedContexts,
        TotalTokens:   calculateTokens(selectedContexts),
    }, nil
}
```

### Why Not in AIX?

AIX would need to duplicate:
- Embedding generation infrastructure
- Vector storage and search
- Episode/facet extraction
- LLM analysis pipelines

All of which Mnemonic already has. AIX's job is capture, Mnemonic's job is memory.

---

## CLI Commands

### AIX Commands

```bash
# Sync from all sources
aix sync

# Sync from specific source
aix sync --source cursor
aix sync --source codex
aix sync --source nexus
aix sync --source clawdbot

# Force full resync
aix sync --source cursor --full

# Export stats
aix stats
# Sessions: 2,274 (cursor: 2100, codex: 50, nexus: 100, clawdbot: 24)
# Messages: 309,046
# Turns: 18,734
# Tool calls: 136,218
# Subagents: 262 (148 with messages, 114 minimal)
```

### Mnemonic Commands

```bash
# Sync all adapters
mnemonic sync

# Sync AIX adapters specifically
mnemonic sync --adapter aix-events    # Trimmed turns → Events
mnemonic sync --adapter aix-agents    # Full fidelity → Agents

# Run analysis
mnemonic analyze --type memory_extraction
mnemonic analyze --type entity_extraction

# Search
mnemonic search "authentication flow"
mnemonic search --ledger agents "subagent dispatch"

# Smart fork (future)
mnemonic fork --query "user auth implementation" --max-turns 10
```

---

## Data Counts (Expected)

After full sync:

| Metric | AIX | Mnemonic Events | Mnemonic Agents |
|--------|-----|-----------------|-----------------|
| Sessions | 2,274 | - | 2,274 (agent_sessions) |
| Messages | 309,046 | - | 309,046 (agent_messages) |
| Turns | 18,734 | ~37k events (user+assistant per turn) | 18,734 (agent_turns) |
| Tool calls | 136,218 | - | 136,218 (agent_tool_calls) |
| Threads | - | 2,274 (from sessions) | - |

---

## Implementation Checklist

### AIX (Current State)

- [x] Cursor parser with full metadata extraction
- [x] Subagent parsing (embedded + bubbleId:task-*)
- [x] Turns table with proper grouping
- [x] Tool calls extraction
- [x] Sessions table with subagent linking
- [x] Codex/Claude Code parser
- [x] Nexus/Clawdbot pi-agent parser
- [ ] Export command (for manual inspection)

### Mnemonic (TODO)

- [ ] Rename cortex → mnemonic
- [ ] Add Agents ledger tables (agent_sessions, agent_messages, agent_turns, agent_tool_calls)
- [ ] Implement aix-events adapter (trimmed turns)
- [ ] Implement aix-agents adapter (full fidelity)
- [ ] Add `aix_turn` episode definition
- [ ] Add `agent_turn` episode definition
- [ ] Generate embeddings for agent_turns
- [ ] Implement ForkContextBuilder
- [ ] Update CLI with new commands

---

## References

- `aix/docs/AIX_FULL_INGESTION_SPEC.md` — AIX schema and parsing details
- `aix/docs/CURSOR_STORAGE_FORMAT.md` — Cursor storage deep dive
- `cortex/docs/MNEMONIC_ARCHITECTURE.md` — Mnemonic architecture
- `nexus-specs/specs/agent-system/ONTOLOGY.md` — Nexus ontology
- `nexus-specs/specs/agent-system/SESSION_FORMAT.md` — Session format

---

*AIX captures. Mnemonic remembers.*
