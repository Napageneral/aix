# Cursor Storage Format

**Purpose:** Document how Cursor stores session data so AIX can properly ingest it.

---

## Storage Location

Cursor stores all session data in a SQLite database at:
```
~/Library/Application Support/Cursor/User/globalStorage/state.vscdb
```

The database has a single key-value table called `cursorDiskKV` with columns: `key` and `value` (both TEXT).

---

## Key Patterns

| Key Pattern | Contents | Description |
|------------|----------|-------------|
| `composerData:<sessionId>` | Session metadata | Model config, conversation headers, context |
| `bubbleId:<sessionId>:<bubbleId>` | Message content | Individual message/bubble data |
| `composerData:task-<toolCallId>` | Subagent session (rare) | Child session header, sometimes missing |
| `bubbleId:task-<toolCallId>:<bubbleId>` | Subagent message | Messages in the child session |
| `agentKv:bubbleCheckpoint:<sessionId>:<bubbleId>` | Content hash | Maps bubble to `agentKv:blob:<hash>` |
| `agentKv:checkpoint:<sessionId>` | Content hash | Maps checkpoint to `agentKv:blob:<hash>` |
| `agentKv:blob:<hash>` | Message/checkpoint blob | JSON or binary-encoded payload |
| `messageRequestContext:<sessionId>:<requestId>` | Request context | Often minimal (e.g., `{"terminalFiles":[]}`) |

---

## Session Format (`composerData:*`)

Two formats exist based on Cursor version:

### Old Format (Inline Conversation)
```json
{
  "composerId": "abc123-...",
  "createdAt": 1737500000000,
  "text": "Initial user text",
  "richText": "{\"root\":{...}}",  // Lexical editor JSON
  "modelConfig": {
    "modelName": "claude-4.5-opus-high-thinking"
  },
  "conversation": [
    {
      "bubbleId": "bubble-1",
      "type": 1,  // 1 = user, 2 = assistant
      "text": "...",
      "relevantFiles": [...],
      "capabilitiesRan": {...}
    },
    ...
  ],
  "context": {
    "fileSelections": [
      { "uri": { "fsPath": "/path/to/file.ts" } }
    ]
  }
}
```

### New Format (Headers Only)
```json
{
  "composerId": "abc123-...",
  "createdAt": 1737500000000,
  "modelConfig": {
    "modelName": "claude-4.5-opus-high-thinking"
  },
  "fullConversationHeadersOnly": [
    { "bubbleId": "bubble-1", "type": 1 },
    { "bubbleId": "bubble-2", "type": 2 },
    ...
  ],
  "context": {...},
  "codeBlockData": {...},
  "originalFileStates": {...}
}
```

In the new format, full bubble content is stored separately in `bubbleId:*` keys.

---

## Bubble Format (`bubbleId:*`)

Individual message data:

```json
{
  "bubbleId": "bubble-1",
  "type": 1,
  "createdAt": "2026-01-22T00:53:34.189Z",
  "text": "The user's message or assistant response",
  "rawText": "...",
  
  // Tool/capability usage
  "capabilitiesRan": {
    "mutate-request": [1, 2, 3],
    "process-stream": [48, 49]
  },
  
  // For tool calls (subagent dispatch)
  "toolFormerData": {
    "tool": 48,
    "name": "task_v2",
    "toolCallId": "toolu_bdrk_01M8WJ...",
    "status": "completed",
    "params": {
      "description": "Task description",
      "prompt": "Full prompt sent to subagent",
      "model": "claude-4.5-opus-high-thinking",
      "name": "general-purpose"
    },
    "additionalData": {
      "status": "completed",
      "composerData": "{...embedded child session JSON...}"
    }
  },
  
  // Context at time of message
  "relevantFiles": ["src/index.ts", "src/utils.ts"],
  "recentLocationsHistory": [
    { "relativeWorkspacePath": "src/file.ts", "lineNumber": 42 }
  ],
  
  // Linter errors captured
  "multiFileLinterErrors": [
    {
      "relativeWorkspacePath": "src/file.ts",
      "errors": [
        {
          "message": "Type error...",
          "source": "typescript",
          "range": {...}
        }
      ]
    }
  ],
  
  // Code blocks (for edits)
  "suggestedCodeBlocks": [...],
  "codeBlocks": [...]
}
```

---

## AgentKV Store (Content-Addressed)

Cursor also keeps a parallel content-addressed store under `agentKv:*`:

- `agentKv:bubbleCheckpoint:<sessionId>:<bubbleId>` → `<hash>` (maps each bubble to a blob)
- `agentKv:checkpoint:<sessionId>` → `<hash>` (session-level checkpoint snapshot)
- `agentKv:blob:<hash>` → payload (some JSON messages, some binary-encoded data)

Notes:
- Many `agentKv:blob` values are JSON (role/tool messages), while others are binary-encoded.
- No `agentKv:bubbleCheckpoint:task-*` entries observed locally; task sessions are not indexed here.
- A sample `agentKv:checkpoint:*` blob appears protobuf-like (binary, no readable strings).

---

## Subagent/Task Sessions

When the assistant uses the `task_v2` tool to spawn a subagent:

1. **Parent bubble** contains `toolFormerData` with:
   - `toolCallId`: Identifier(s) for the child session(s)
   - `params.description`: Task description
   - `params.prompt`: Full prompt sent to child
   - `additionalData.composerData`: Embedded JSON of child session (sometimes present)

2. **Child session** stored separately as:
   - Messages: `bubbleId:task-<toolCallId>:<bubbleId>`
   - Session header: `composerData:task-<toolCallId>` (rare; not guaranteed)

### Observed `toolCallId` Formats

- `toolu_bdrk_*` (Anthropic tool calls)
- `call_*` and `fc_*` (OpenAI tool calls; often two IDs separated by a newline)
- UUID (Cursor-internal IDs)

### Newline-Separated `toolCallId` (OpenAI)

Some OpenAI tool call IDs show up as **two IDs separated by a newline** in `toolFormerData.toolCallId`:

```
call_VFmgSpDQl9hyIlW0VrzwEOKg
fc_0541b53cd69fa3be006978f7fa8bcc819290693e82055fcfa3
```

Cursor uses the **entire string (including the newline)** in the task bubble keys, e.g.:

```
bubbleId:task-call_VFmgSpDQl9hyIlW0VrzwEOKg\nfc_0541b53cd69fa3be006978f7fa8bcc819290693e82055fcfa3:<bubbleId>
```

### Observed Storage Behavior

- `toolu_bdrk_*`, `call_*`, and `fc_*` IDs have child bubbles under `bubbleId:task-*`.
- `composerData:task-*` exists for only a small subset; do not rely on it.
- UUID-format task IDs have **no** `bubbleId:task-*` or `composerData:task-*` entries locally.
  Only the parent dispatch bubble is present in `bubbleId:<sessionId>:<bubbleId>`.
  This suggests the logs are stored elsewhere or not persisted locally (open question).
- `toolFormerData.result.agentId` does **not** map to any known key prefix in `cursorDiskKV`.
  It appears only in the parent dispatch bubble (no separate agentId-indexed blobs found).

### Linking Parent → Child

```
Parent Session: composerData:abc123
  └── Bubble: bubbleId:abc123:bubble-5
        └── toolFormerData.toolCallId = "toolu_bdrk_01M8WJ"
              └── Child: bubbleId:task-toolu_bdrk_01M8WJ:*
```

---

## Session Forking (Duplicate Chat)

Cursor's "Duplicate Chat" feature creates a fork:
- Creates a **new session** with a **new composerId**
- Copies all conversation history up to the fork point
- Both sessions have identical message history, then diverge

**How to detect forks:**
- Messages will have the same content and timestamps
- Different composerId but shared message content
- Need to deduplicate by comparing message hashes

**AIX/Cortex handling:**
- Store raw messages with their original IDs
- Detect forks by comparing message sequences
- Build thread tree structure at query time or in Cortex

---

## System Prompts and Injected Context

**Important:** Cursor does NOT store the system prompt in the session data. It's injected at runtime.

### What IS stored:
- `modelConfig.modelName` - Which model was used
- `context.fileSelections` - Files user explicitly attached
- Bubble-level `relevantFiles` - Files referenced in that turn
- Bubble-level `recentLocationsHistory` - Recent cursor locations
- `codeBlockData` - Code being edited

### What is NOT stored:
- The actual system prompt text
- AGENTS.md content
- .cursorrules content
- Dynamically injected context

**Implication:** We cannot reconstruct the exact system prompt that was used. We can only know:
- Model name
- Files that were attached/relevant
- User's workspace (from file paths)

---

## Fields We Extract

AIX currently extracts:

| Field | Where Stored | AIX Table |
|-------|--------------|-----------|
| Session ID | composerData key | `sessions.id` |
| Model | `modelConfig.modelName` | `sessions.model` |
| Created At | `createdAt` or UUIDv7 | `sessions.created_at` |
| Project | Inferred from file paths | `sessions.project` |
| Messages | `conversation` or bubbles | `messages` |
| Message Type | `type` (1=user, 2=assistant) | `messages.role` |
| Message Content | `text` or `rawText` | `messages.content` |
| Capabilities | `capabilitiesRan` | `message_capabilities` |
| Linter Errors | `multiFileLinterErrors` | `message_lints` |
| File Refs | `relevantFiles`, `recentLocationsHistory` | `message_files` |
| Code Blocks | `suggestedCodeBlocks`, `codeBlocks` | `message_codeblocks` |
| Raw Metadata | Full bubble JSON | `message_metadata` |
| **Parent Session** (NEW) | Linked via `toolCallId` | `sessions.parent_session_id` |
| **Tool Call ID** (NEW) | `toolFormerData.toolCallId` | `sessions.tool_call_id` |
| **Context Token Limit** | `contextTokenLimit` | `sessions.context_token_limit` |
| **Context Tokens Used** | `contextTokensUsed` | `sessions.context_tokens_used` |
| **Is Agentic (Session)** | `isAgentic` | `sessions.is_agentic` |
| **Force Mode** | `forceMode` | `sessions.force_mode` |
| **Workspace Path** | inferred from context paths | `sessions.workspace_path` |
| **Session Context JSON** | `context` | `sessions.context_json` |
| **Conversation State** | `conversationState` | `sessions.conversation_state` |
| **Checkpoint ID** | `checkpointId` | `messages.checkpoint_id` |
| **Is Agentic (Message)** | `isAgentic` | `messages.is_agentic` |
| **Is Plan Execution** | `isPlanExecution` | `messages.is_plan_execution` |
| **Message Context JSON** | `context` | `messages.context_json` |
| **Cursor Rules JSON** | `cursorRules` | `messages.cursor_rules_json` |

---

## Important Notes

### Orphaned Bubbles

Some bubbles exist in `bubbleId:*` keys but are NOT listed in `fullConversationHeadersOnly`.
This happens with tool call bubbles that spawn subagents - they're tracked separately.

**AIX handles this:** We scan ALL bubbles (not just those in headers) to extract `toolFormerData`
for task dispatch linking. This ensures subagent sessions get linked to their parents even
when the parent bubble is "orphaned".

---

## Data NOT Stored by Cursor (Important!)

These are injected at runtime and NOT persisted:
- **System prompt text** - The actual instructions sent to the model
- **AGENTS.md content** - Rules are injected dynamically
- **.cursorrules content** - Workspace rules
- **Per-message token counts** - `tokenCount` is often {inputTokens: 0, outputTokens: 0} and not reliable
- **Full thinking trace** - `allThinkingBlocks` is empty; some bubbles include a `thinking` field, but it is not consistently present

**Session-level tokens ARE stored:**
- `contextTokenLimit` - e.g., 176000
- `contextTokensUsed` - e.g., 109394

---

## Fields Present But Not Fully Interpreted

**Session-level:**
- `conversationState` - Serialized conversation state (format still unclear)
- `context` - Contains `fileSelections`, `folderSelections`, mentions, etc.
- `workspaceUris` - Workspace references (rare)

**Bubble-level:**
- `cursorRules` - Sometimes contains rules that were in effect
- `messageRequestContext:*` entries - Often minimal; unclear how/when they are used

**AgentKV:**
- `agentKv:checkpoint:*` blobs are binary-encoded; format not yet decoded

---

## Fork Detection Strategy

Cursor's "Duplicate Chat" creates a new session with copied message history.

**How to detect in AIX/Cortex:**
1. Hash the first N messages of each session
2. Group sessions by matching message prefixes
3. Build fork tree based on divergence points

This is better done in Cortex's thread analysis than in AIX import.

---

## Open Questions

1. **System prompt reconstruction:** Can we infer it from workspace files (find AGENTS.md at same path)?

2. **checkpointId usage:** Is this related to forking? Need more investigation.

3. **conversationState format:** What's serialized here? Could be useful.

4. **Turn abstraction:** Should AIX build turns (query+response pairs) or leave that to Cortex?

5. **UUID-format task logs:** Where are UUID toolCallId subagent logs stored locally (if at all)?

6. **AgentKV encoding:** What is the binary format for `agentKv:checkpoint:*` blobs?

7. **Other local stores:** We searched logs and browser storage without finding task logs:
   - `Cursor/logs/*` contain `ToolCallEventService` start/end for tool calls, but no task payloads.
   - `Local Storage/leveldb`, `Service Worker/`, and `WebStorage/` contain no `toolCallId` matches.
   - `workspaceStorage/*/state.vscdb` contains no task entries.

---

## References

- AIX parser: `internal/sync/cursor_db.go`
- AIX models: `internal/models/session.go`
- AIX schema: `internal/db/schema.sql`
