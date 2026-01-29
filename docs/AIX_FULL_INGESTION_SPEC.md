# AIX Full Ingestion Specification

**Status:** DESIGN SPEC  
**Last Updated:** 2026-01-27  
**Purpose:** Complete ingestion of Cursor session data including subagents, and pipeline to Cortex  

---

## Executive Summary

AIX needs to:
1. **Parse ALL discoverable session data** from Cursor's local storage
2. **Link parent-child sessions** via toolCallId for subagents
3. **Extract rich metadata** (context, checkpoints, capabilities, files, etc.)
4. **Map to Nexus ontology** (Turn, Thread, Session, Message) for Cortex consumption
5. **Export in formats** consumable by Cortex's search and analysis

### Data Availability Summary

| ToolCallId Format | Count | Child Session Data | Recoverability |
|-------------------|-------|-------------------|----------------|
| `toolu_bdrk_*` (Anthropic) | 54 | ✅ Full | 100% recoverable |
| `call_*/fc_*` (OpenAI) | 94 | ✅ Full | 100% recoverable |
| UUID (Cursor internal) | 114 | ❌ None | **NOT recoverable locally** |

**148 of 262 subagents (56%) have full data locally. 114 (44%) have only the dispatch metadata.**

---

## Part 1: Cursor Storage Deep Dive

### 1.1 Storage Location

```
~/Library/Application Support/Cursor/User/globalStorage/state.vscdb
```

SQLite database with single table `cursorDiskKV` (key TEXT, value TEXT).

### 1.2 Key Patterns (Complete Reference)

| Key Pattern | Type | Description | AIX Status |
|-------------|------|-------------|------------|
| `composerData:<sessionId>` | Session header | Model config, context, conversation state | ✅ Parsed |
| `bubbleId:<sessionId>:<bubbleId>` | Message | Individual bubble content | ✅ Parsed |
| `composerData:task-<toolCallId>` | Subagent header | Child session metadata (rare) | ⚠️ Not relied on |
| `bubbleId:task-<toolCallId>:<bubbleId>` | Subagent message | Child session messages | 🔧 NEEDS IMPLEMENTATION |
| `agentKv:blob:<hash>` | Content blob | JSON or binary message data | ⏳ Future |
| `agentKv:bubbleCheckpoint:<sid>:<bid>` | Hash ref | Maps bubble → blob hash | ⏳ Future |
| `agentKv:checkpoint:<sessionId>` | Hash ref | Session checkpoint blob | ⏳ Future |
| `messageRequestContext:<sid>:<reqId>` | Request meta | Often minimal | ⏳ Low priority |

### 1.3 Session Data Structure

**Header (`composerData:*`):**
```json
{
  "composerId": "abc123-...",
  "createdAt": 1737500000000,
  "modelConfig": { "modelName": "claude-4.5-opus-high-thinking" },
  "fullConversationHeadersOnly": [
    { "bubbleId": "bubble-1", "type": 1 },
    { "bubbleId": "bubble-2", "type": 2 }
  ],
  "context": {
    "fileSelections": [...],
    "folderSelections": [...],
    "mentions": [...]
  },
  "contextTokenLimit": 176000,
  "contextTokensUsed": 109394,
  "isAgentic": true,
  "forceMode": "edit",
  "conversationState": "...",
  "codeBlockData": {...}
}
```

**Message (`bubbleId:*`):**
```json
{
  "bubbleId": "bubble-1",
  "type": 1,
  "createdAt": "2026-01-22T00:53:34.189Z",
  "text": "...",
  "rawText": "...",
  "checkpointId": "checkpoint-abc",
  "isAgentic": true,
  "isPlanExecution": false,
  "context": { "fileSelections": [...] },
  "cursorRules": [...],
  "relevantFiles": ["src/index.ts"],
  "recentLocationsHistory": [{ "relativeWorkspacePath": "...", "lineNumber": 42 }],
  "multiFileLinterErrors": [...],
  "capabilitiesRan": { "mutate-request": [1,2], "process-stream": [48] },
  "suggestedCodeBlocks": [...],
  "toolFormerData": {
    "name": "task_v2",
    "toolCallId": "toolu_bdrk_...",
    "params": { "description": "...", "prompt": "...", "model": "..." },
    "status": "completed",
    "additionalData": {
      "composerData": "{...embedded child session...}"
    }
  }
}
```

### 1.4 Subagent Data Patterns

**Pattern A: Embedded composerData (most reliable)**
```
bubbleId:<parentSessionId>:<bubbleId>
  └── toolFormerData.additionalData.composerData = "{child session JSON}"
```
- Full child session embedded as JSON string
- Must parse and extract child messages
- Available for toolu_bdrk_* and call_*/fc_* formats

**Pattern B: Separate task keys (legacy/alternative)**
```
composerData:task-<toolCallId>           (header, often missing)
bubbleId:task-<toolCallId>:<bubbleId>    (messages)
```
- Messages stored separately from parent
- Must scan for `bubbleId:task-*` keys
- Handles newline-containing toolCallIds (OpenAI)

**Pattern C: UUID dispatch only (unrecoverable)**
```
bubbleId:<parentSessionId>:<bubbleId>
  └── toolFormerData.toolCallId = "uuid-format"
  └── toolFormerData.additionalData = { status, agentId } (minimal)
```
- No embedded composerData
- No bubbleId:task-* keys
- Only dispatch metadata available (description, prompt from params)

---

## Part 2: AIX Schema Alignment

### 2.1 Current Schema (Already Implemented)

**sessions table:**
```sql
id TEXT PRIMARY KEY,              -- composerId or task-<toolCallId>
source TEXT NOT NULL,             -- 'cursor'
project TEXT,                     -- inferred from paths
model TEXT,
created_at INTEGER,
message_count INTEGER,
summary TEXT,
raw_json TEXT,

-- Subagent fields
parent_session_id TEXT,           -- links to parent
parent_message_id TEXT,           -- bubble where dispatched
tool_call_id TEXT,                -- the toolCallId
task_description TEXT,            -- from params.description
task_status TEXT,                 -- pending/running/completed/failed
is_subagent INTEGER DEFAULT 0,

-- Session metadata
context_token_limit INTEGER,
context_tokens_used INTEGER,
is_agentic INTEGER DEFAULT 0,
force_mode TEXT,
workspace_path TEXT,
context_json TEXT,
conversation_state TEXT
```

**messages table:**
```sql
id TEXT PRIMARY KEY,              -- bubbleId
session_id TEXT,
role TEXT,
content TEXT,
sequence INTEGER,
timestamp INTEGER,

-- Message metadata
checkpoint_id TEXT,
is_agentic INTEGER DEFAULT 0,
is_plan_execution INTEGER DEFAULT 0,
context_json TEXT,
cursor_rules_json TEXT
```

### 2.2 Schema Extensions Needed

**NEW: tool_calls table (for rich tool usage tracking)**
```sql
CREATE TABLE tool_calls (
    id TEXT PRIMARY KEY,                    -- toolCallId
    message_id TEXT REFERENCES messages(id),
    session_id TEXT REFERENCES sessions(id),
    
    -- Tool metadata
    tool_name TEXT,                         -- 'task_v2', 'Shell', 'Read', etc.
    tool_number INTEGER,                    -- Cursor's numeric tool ID
    params_json TEXT,                       -- Full params object
    result_json TEXT,                       -- Full result object
    status TEXT,                            -- pending/completed/failed
    
    -- Subagent linking
    child_session_id TEXT,                  -- links to spawned subagent session
    
    -- Timing
    started_at INTEGER,
    completed_at INTEGER
);

CREATE INDEX idx_tool_calls_session ON tool_calls(session_id);
CREATE INDEX idx_tool_calls_message ON tool_calls(message_id);
CREATE INDEX idx_tool_calls_child ON tool_calls(child_session_id);
```

**NEW: turns table (Nexus ontology alignment)**
```sql
CREATE TABLE turns (
    id TEXT PRIMARY KEY,                    -- Same as final assistant message ID
    session_id TEXT REFERENCES sessions(id),
    parent_turn_id TEXT REFERENCES turns(id),
    
    -- Turn boundaries
    query_message_ids TEXT,                 -- JSON array of input message IDs
    response_message_id TEXT,               -- Final assistant message ID
    
    -- Turn metadata
    model TEXT,
    token_count INTEGER,
    timestamp INTEGER,
    has_children INTEGER DEFAULT 0,
    
    -- Tool usage in this turn
    tool_call_count INTEGER DEFAULT 0
);

CREATE INDEX idx_turns_session ON turns(session_id);
CREATE INDEX idx_turns_parent ON turns(parent_turn_id);
```

---

## Part 3: Parser Implementation

### 3.1 Current Parser Gaps

The Cursor parser (`cursor_db.go`) currently:
- ✅ Parses `composerData:*` session headers
- ✅ Parses `bubbleId:<sessionId>:*` messages
- ✅ Extracts session-level metadata (tokens, context, etc.)
- ⚠️ Detects task dispatches (toolFormerData) but doesn't fully process
- ❌ Does NOT parse embedded composerData from toolFormerData.additionalData
- ❌ Does NOT scan `bubbleId:task-*` keys for separate child sessions
- ❌ Does NOT create child Session records linked to parents

### 3.2 Implementation Tasks

#### Task 1: Parse Embedded composerData

```go
// In cursor_db.go, when processing a bubble:
func extractTaskDispatchFromBubble(bubbleValue string, parentSessionId string, bubbleId string) (*models.Session, []models.Message, error) {
    toolFormerData := gjson.Get(bubbleValue, "toolFormerData")
    if !toolFormerData.Exists() || toolFormerData.Get("name").String() != "task_v2" {
        return nil, nil, nil
    }
    
    toolCallId := toolFormerData.Get("toolCallId").String()
    embeddedData := toolFormerData.Get("additionalData.composerData").String()
    
    if embeddedData == "" || len(embeddedData) < 10 {
        // UUID-format dispatch with no embedded data
        return createMinimalSubagentSession(toolFormerData, parentSessionId, bubbleId)
    }
    
    // Parse embedded composerData JSON
    var composerData map[string]interface{}
    if err := json.Unmarshal([]byte(embeddedData), &composerData); err != nil {
        return nil, nil, fmt.Errorf("failed to parse embedded composerData: %w", err)
    }
    
    // Build child session
    childSession := &models.Session{
        ID:              fmt.Sprintf("task-%s", toolCallId),
        Source:          "cursor",
        ParentSessionID: parentSessionId,
        ParentMessageID: bubbleId,
        ToolCallID:      toolCallId,
        TaskDescription: toolFormerData.Get("params.description").String(),
        TaskStatus:      toolFormerData.Get("status").String(),
        IsSubagent:      true,
        Model:           toolFormerData.Get("params.model").String(),
        // ... extract more fields from composerData
    }
    
    // Extract messages from embedded conversation
    messages := extractMessagesFromComposerData(composerData, childSession.ID)
    
    return childSession, messages, nil
}
```

#### Task 2: Scan bubbleId:task-* Keys

```go
func parseTaskSessionsFromDB(db *sql.DB) (map[string]*models.Session, map[string][]models.Message, error) {
    // Single-pass scan for all task-related keys
    rows, err := db.Query(`
        SELECT key, value FROM cursorDiskKV 
        WHERE key LIKE 'bubbleId:task-%'
        ORDER BY key
    `)
    
    taskSessions := make(map[string]*models.Session)
    taskMessages := make(map[string][]models.Message)
    
    for rows.Next() {
        var key, value string
        rows.Scan(&key, &value)
        
        // Parse key: bubbleId:task-<toolCallId>:<bubbleId>
        // Note: toolCallId may contain newlines (OpenAI format)
        parts := strings.SplitN(key, ":", 3)
        taskKey := parts[1] // "task-<toolCallId>"
        toolCallId := strings.TrimPrefix(taskKey, "task-")
        bubbleId := parts[2]
        
        // Parse message
        msg := parseMessageFromBubble(value, taskKey, bubbleId)
        taskMessages[toolCallId] = append(taskMessages[toolCallId], msg)
    }
    
    return taskSessions, taskMessages, nil
}
```

#### Task 3: Build Turn Boundaries

```go
func buildTurnsForSession(session *models.Session, messages []models.Message) []Turn {
    var turns []Turn
    var currentQueryMsgs []string
    var parentTurnId string
    
    for _, msg := range messages {
        if msg.Role == "user" || msg.Role == "system" {
            currentQueryMsgs = append(currentQueryMsgs, msg.ID)
        } else if msg.Role == "assistant" {
            // Turn completes when assistant responds
            turn := Turn{
                ID:                msg.ID,  // Turn ID = final assistant message ID
                SessionID:         session.ID,
                ParentTurnID:      parentTurnId,
                QueryMessageIDs:   currentQueryMsgs,
                ResponseMessageID: msg.ID,
                Model:             session.Model,
                Timestamp:         msg.Timestamp,
            }
            turns = append(turns, turn)
            
            // Next turn's parent is this turn
            parentTurnId = turn.ID
            currentQueryMsgs = nil
        }
    }
    
    return turns
}
```

### 3.3 Newline ToolCallId Handling

OpenAI format toolCallIds contain literal newlines:
```
call_VFmgSpDQl9hyIlW0VrzwEOKg
fc_0541b53cd69fa3be006978f7fa8bcc819290693e82055fcfa3
```

**Key insight:** Cursor uses the ENTIRE string (including newline) as the key suffix.

```go
// When looking up child bubbles, preserve the raw toolCallId
childKeyPrefix := fmt.Sprintf("bubbleId:task-%s:", toolCallId) // toolCallId may contain \n
```

---

## Part 4: Nexus Ontology Mapping

### 4.1 Terminology Alignment

| Cursor Concept | AIX Storage | Nexus Ontology | Notes |
|----------------|-------------|----------------|-------|
| composerData | `sessions` row | **Thread head** | Current conversation endpoint |
| bubbleId | `messages` row | **Message** | Atomic content unit |
| User + Assistant bubble pair | `turns` row | **Turn** | Query + response exchange |
| Session with messages | `sessions` + `messages` | **Thread** | Cumulative context to a point |
| toolFormerData (task_v2) | `tool_calls` row | **Tool invocation** | Spawns child thread |
| task-<toolCallId> session | `sessions` row (is_subagent=1) | **Worker Thread** | Child of parent turn |

### 4.2 Key Mappings

**Message → Nexus Message:**
```go
type NexusMessage struct {
    ID        string                 // bubbleId
    ParentID  string                 // Previous message (implicit via sequence)
    Role      string                 // 'user', 'assistant', 'system', 'tool'
    Source    string                 // 'human', 'trigger', 'agent' (inferred)
    Content   string                 // text
    Timestamp int64
    Model     string                 // For assistant messages
    ToolCalls []ToolCall             // If this message dispatches tools
    ToolResult *ToolResult           // If this is a tool result
}
```

**Turn construction:**
```go
type NexusTurn struct {
    ID              string            // Final assistant message ID
    ParentTurnID    string            // Previous turn
    QueryMessages   []NexusMessage    // Input messages (user, system, tool results)
    ResponseMessage NexusMessage      // Assistant response
    Model           string
    TokenCount      int
    Timestamp       int64
    ToolCalls       []ToolCall        // Tools invoked in response
    HasChildren     bool              // Has this turn been forked from?
}
```

**Thread reconstruction:**
```go
type NexusThread struct {
    TurnID      string                // Which turn this thread points to
    Ancestry    []string              // Turn IDs from root to this turn
    TotalTokens int                   // Accumulated
    Depth       int                   // How many turns deep
    
    // For Cursor sessions:
    WorkspacePath string
    PersonaID     string              // Could be "cursor-default"
    ThreadKey     string              // "cursor:<sessionId>"
}
```

### 4.3 Subagent as Worker Thread

When a Cursor session dispatches a task_v2:
1. **Parent thread** has a turn with tool_calls containing task dispatch
2. **Worker thread** is created with:
   - `parent_turn_id` pointing to the dispatching turn
   - Inherited persona (same model/context)
   - Own message sequence
3. **On completion**, tool_result is injected back into parent thread

```
Parent Thread (Turn 5)
  └── QueryMessages: [user request]
  └── ResponseMessage: "I'll spawn a subagent..."
  └── ToolCalls: [task_v2: "explore codebase"]
        │
        └── Worker Thread (Turn W1)
              └── QueryMessages: [task prompt]
              └── ResponseMessage: [exploration result]
              └── (may have its own tool calls)
        │
        └── ToolResult injected as Turn 6 query
```

---

## Part 5: Cortex Integration

### 5.1 Export Format for Cortex

AIX should export sessions in a format Cortex can index and search.

**Option A: JSONL (Recommended)**
```jsonl
{"type":"session","id":"abc123","source":"cursor","model":"claude-4.5-opus","created_at":1737500000000,"workspace":"/path/to/project","is_subagent":false}
{"type":"turn","id":"turn-1","session_id":"abc123","parent_turn_id":null,"model":"claude-4.5-opus","timestamp":1737500001000}
{"type":"message","id":"bubble-1","session_id":"abc123","turn_id":"turn-1","role":"user","content":"..."}
{"type":"message","id":"bubble-2","session_id":"abc123","turn_id":"turn-1","role":"assistant","content":"..."}
{"type":"tool_call","id":"toolu_bdrk_...","message_id":"bubble-2","tool":"task_v2","child_session_id":"task-toolu_bdrk_..."}
{"type":"session","id":"task-toolu_bdrk_...","source":"cursor","is_subagent":true,"parent_session_id":"abc123","parent_message_id":"bubble-2"}
...
```

**Option B: SQLite dump**
- Export aix.db directly
- Cortex imports and indexes
- Simpler but less flexible

### 5.2 Cortex Indexing Requirements

Cortex needs to:
1. **Full-text search** across message content
2. **Semantic search** via embeddings (AIX can pre-compute or Cortex generates)
3. **Thread navigation** — find all ancestors/descendants of a turn
4. **Project scoping** — filter by workspace/project
5. **Time range queries** — sessions within date range
6. **Subagent traversal** — given a parent session, find all worker threads

### 5.3 Proposed Pipeline

```
Cursor state.vscdb
       │
       ▼
   AIX Parser
       │
       ├─► aix.db (local SQLite)
       │      - sessions, messages, turns, tool_calls
       │      - embeddings (optional)
       │
       └─► Export (JSONL or SQLite dump)
              │
              ▼
          Cortex Import
              │
              ├─► Full-text index (Bleve/Tantivy)
              ├─► Vector index (embeddings)
              └─► Graph index (thread relationships)
```

---

## Part 6: Implementation Plan

### Phase 1: Complete Subagent Parsing (Priority: HIGH)

**Goal:** Parse 100% of recoverable subagent data (148 of 262).

1. [ ] Implement `extractTaskDispatchFromBubble()` for embedded composerData
2. [ ] Implement `parseTaskSessionsFromDB()` for bubbleId:task-* scanning
3. [ ] Handle newline-containing toolCallIds correctly
4. [ ] Create child Session records with proper linking
5. [ ] Extract messages from both embedded and separate storage
6. [ ] Unit tests for all parsing paths

**Acceptance criteria:**
- `aix sync --source cursor` creates 148 subagent sessions
- Each subagent has `parent_session_id`, `tool_call_id` populated
- Each subagent has messages extracted

### Phase 2: Turn Construction (Priority: MEDIUM)

**Goal:** Build turn boundaries for Nexus ontology alignment.

1. [ ] Add `turns` table to schema
2. [ ] Implement `buildTurnsForSession()` 
3. [ ] Populate turn records during sync
4. [ ] Mark turns that have been forked (`has_children`)
5. [ ] Unit tests for turn boundary logic

### Phase 3: Tool Call Tracking (Priority: MEDIUM)

**Goal:** Rich tool usage data for analysis.

1. [ ] Add `tool_calls` table to schema
2. [ ] Extract all toolFormerData (not just task_v2)
3. [ ] Link tool_calls to child sessions where applicable
4. [ ] Track timing (started_at, completed_at)

### Phase 4: Cortex Export (Priority: HIGH after Phase 1)

**Goal:** Get data flowing to Cortex.

1. [ ] Implement JSONL export: `aix export --format jsonl`
2. [ ] Include sessions, turns, messages, tool_calls
3. [ ] Proper parent-child relationships in export
4. [ ] Cortex import command
5. [ ] Full-text indexing of messages
6. [ ] Thread graph construction

### Phase 5: Fork Detection (Priority: LOW)

**Goal:** Detect Cursor "Duplicate Chat" forks.

1. [ ] Hash first N messages of each session
2. [ ] Group sessions by matching prefixes
3. [ ] Build fork tree in Cortex (not AIX)
4. [ ] Update `has_children` on forked turns

### Phase 6: AgentKV Exploration (Priority: LOW)

**Goal:** Decode additional content-addressed data.

1. [ ] Investigate `agentKv:checkpoint:*` binary format
2. [ ] Map `agentKv:bubbleCheckpoint:*` to messages
3. [ ] Determine if UUID-format task logs are hidden here
4. [ ] Document findings

---

## Part 7: Data Not Recoverable

### 7.1 UUID-Format Subagents (114 dispatches)

These have **dispatch metadata only**:
- `params.description` - What the task was supposed to do
- `params.prompt` - The full prompt sent (valuable!)
- `params.model` - Which model was used
- `status` - completed/failed
- `result.agentId` - Cursor's internal ID (not useful for lookup)

**What we CAN store:**
```sql
-- Even for UUID-format, we can create a "minimal" subagent record
INSERT INTO sessions (
    id,                -- 'task-<uuid>'
    parent_session_id, -- parent session
    parent_message_id, -- dispatch bubble
    tool_call_id,      -- the UUID
    task_description,  -- from params.description
    task_status,       -- from status
    is_subagent,       -- 1
    model,             -- from params.model
    raw_json           -- the full params.prompt (valuable!)
) VALUES (...);

-- No messages, but we have the prompt and description
```

**This preserves:**
- That a subagent was dispatched
- What it was asked to do (description)
- The full prompt it received (often detailed)
- Which model was used
- Final status

### 7.2 System Prompts

Cursor does NOT store system prompts. We can:
- Infer workspace from file paths
- Look for AGENTS.md in that workspace (at query time)
- Store `workspace_path` for later resolution

### 7.3 Thinking Traces

`allThinkingBlocks` is usually empty. Some bubbles have `thinking` field but inconsistently.

---

## Part 8: Alignment with Nexus Specs

### 8.1 ONTOLOGY.md Concepts

| Nexus Concept | AIX Implementation | Notes |
|---------------|-------------------|-------|
| **Message** | `messages` table | Maps 1:1 with Cursor bubbles |
| **Turn** | `turns` table | Built from message sequences |
| **Thread** | Query over `turns` ancestry | Computed at query time |
| **Session** | Active thread tip | `sessions` with `has_children=0` |
| **Persona** | Not in Cursor | Default to "cursor-default" |

### 8.2 SESSION_FORMAT.md Integration

Nexus uses pi-coding-agent JSONL format. For Cursor data:
- Convert sessions to JSONL during export
- Map bubble types (1=user, 2=assistant) to roles
- Preserve parent-child links as `parentTurnId`
- Include origin metadata: `{ provider: "cursor", surface: "ide", workspace: "..." }`

### 8.3 Future: Unified Session Store

When Nexus has its own sessions:
```
~/nexus/state/sessions/
  ├── {nexusSessionId}.jsonl    # Native nexus sessions
  └── imported/
      ├── cursor/               # Cursor-sourced
      │   └── {sessionId}.jsonl
      └── clawdbot/             # Clawdbot-sourced
          └── {sessionId}.jsonl
```

AIX export → Cortex import → Nexus unified view.

---

## Appendix A: Cursor Capability IDs (Partial)

| ID | Name | Description |
|----|------|-------------|
| 1 | mutate-request | Request mutation |
| 2 | ? | Unknown |
| 48 | task_v2 | Subagent dispatch |
| 49 | ? | Unknown |

(Need to reverse-engineer the full list from Cursor source)

---

## Appendix B: Sample Queries

**Find all subagents for a parent session:**
```sql
SELECT s.*, m.content as dispatch_context
FROM sessions s
JOIN messages m ON s.parent_message_id = m.id
WHERE s.parent_session_id = 'abc123'
  AND s.is_subagent = 1;
```

**Build turn sequence:**
```sql
WITH RECURSIVE turn_tree AS (
  SELECT id, parent_turn_id, 0 as depth
  FROM turns WHERE parent_turn_id IS NULL AND session_id = 'abc123'
  
  UNION ALL
  
  SELECT t.id, t.parent_turn_id, tt.depth + 1
  FROM turns t
  JOIN turn_tree tt ON t.parent_turn_id = tt.id
)
SELECT * FROM turn_tree ORDER BY depth;
```

**Export for Cortex:**
```sql
SELECT 
  'message' as type,
  m.id,
  m.session_id,
  m.role,
  m.content,
  m.timestamp,
  s.workspace_path,
  s.model
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE s.source = 'cursor'
ORDER BY s.created_at, m.sequence;
```

---

## References

- `aix/docs/CURSOR_STORAGE_FORMAT.md` — Cursor storage deep dive
- `nexus-specs/specs/agent-system/ONTOLOGY.md` — Nexus ontology
- `nexus-specs/specs/agent-system/SESSION_FORMAT.md` — Session format spec
- `aix/internal/sync/cursor_db.go` — Current parser implementation
- `aix/internal/db/schema.sql` — Current schema
