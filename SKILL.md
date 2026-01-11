---
name: aix
description: AI session intelligence - search and analyze your AI conversation history from Cursor, with semantic search via embeddings
homepage: https://github.com/Napageneral/aix
metadata: {"nexus":{"emoji":"🧠","os":["darwin","linux"],"requires":{"bins":["aix"]},"install":[{"id":"brew","kind":"brew","formula":"Napageneral/tap/aix","bins":["aix"],"label":"Install via Homebrew"},{"id":"go","kind":"shell","script":"go install github.com/Napageneral/aix/cmd/aix@latest","bins":["aix"],"label":"Install via Go"}]}}
---

# aix 🧠

AI session intelligence - search and analyze your AI conversation history. Reads directly from Cursor's internal database, exports raw sessions to `~/nexus/home/sessions/` for durability, and stores conversations in a queryable SQLite database with support for semantic search via embeddings.

## Quick Start

```bash
aix init                              # Initialize config and database
aix sync --source cursor              # Sync from Cursor (reads DB, exports to nexus, imports to aix.db)
aix sessions                          # List all sessions
aix show <session-id>                 # View session details
aix stats                             # Show database statistics
```

## Commands

### Sync & Import

| Command | Description |
|---------|-------------|
| `aix sync --source cursor` | Read from Cursor DB, export to nexus, import to aix.db |
| `aix sync --source cursor --no-export` | Skip export (not recommended) |
| `aix sync --export-path <path>` | Custom export location |

The sync command:
1. Reads directly from Cursor's SQLite database
2. Exports raw session JSON to `~/nexus/home/sessions/composer/` and bubbles to `bubbles/`
3. Imports into aix.db for analysis

### Browse & Query

| Command | Description |
|---------|-------------|
| `aix sessions [--project <p>] [--today] [--week]` | List sessions with filters |
| `aix show <session-id>` | Show full session with messages (partial ID ok) |
| `aix db query <sql>` | Run raw SQL queries (SELECT/WITH only) |
| `aix stats` | Show database statistics |

### Semantic Search (requires GEMINI_API_KEY)

| Command | Description |
|---------|-------------|
| `aix embed [--model <m>] [--limit <n>]` | Generate embeddings for messages |
| `aix compute embed` | High-throughput embedding via taskengine |
| `aix compute status` | Show compute queue status |
| `aix search <query> [--project <p>]` | Semantic search across messages |

Default embedding model: `gemini-embedding-1`

## Examples

```bash
# Initialize and sync
aix init
aix sync --source cursor

# Browse sessions
aix sessions                          # All sessions (default limit 50)
aix sessions --today                  # Today's sessions only
aix sessions --week                   # Last 7 days
aix sessions --project nexus -n 100   # Filter by project, limit 100

# View a session (supports partial ID)
aix show a46d032c                     # Partial ID works
aix show a46d032c-bedf-4ef5-...       # Full ID

# Query database directly
aix db query "SELECT COUNT(*) as count FROM sessions"
aix db query "SELECT project, model, COUNT(*) FROM sessions GROUP BY project, model ORDER BY COUNT(*) DESC"
aix db query "SELECT * FROM messages WHERE content LIKE '%error%' LIMIT 5"

# Semantic search (after generating embeddings)
export GEMINI_API_KEY=your-key
aix embed --limit 10000               # Embed first 10k messages
aix search "how to fix TypeScript errors"
aix search "database migrations" --project HTAA
```

## Output Formats

All commands support `--json` / `-j` for JSON output:

```bash
aix sessions --json                   # JSON array of sessions
aix show abc123 --json                # Session with messages as JSON
aix sync --json                       # {"synced": 1934, "new": 1202, "exported": {...}}
aix stats --json                      # {"sessions": 1958, "messages": 220039, ...}
```

## Database Schema

```sql
-- Core tables
sessions(id, source, project, model, created_at, message_count, summary, raw_json)
messages(id, session_id, role, content, sequence, timestamp)
files_referenced(id, session_id, file_path)

-- Rich metadata (extracted from Cursor)
message_metadata(message_id, session_id, metadata_json)
message_capabilities(id, message_id, session_id, phase, capability)
message_lints(id, message_id, session_id, file_path, message, source, ...)
message_files(id, message_id, session_id, kind, file_path, line_number)
message_codeblocks(id, message_id, session_id, idx, raw_json)

-- Embeddings for semantic search
embeddings(id, entity_type, entity_id, model, embedding_blob, dimension, created_at)
```

## Useful Queries

```sql
-- Model usage by session count
SELECT model, COUNT(*) as sessions FROM sessions 
WHERE model IS NOT NULL GROUP BY model ORDER BY sessions DESC;

-- Sessions by project
SELECT project, COUNT(*) as sessions, SUM(message_count) as msgs 
FROM sessions GROUP BY project ORDER BY sessions DESC;

-- Recent activity by day
SELECT date(created_at/1000, 'unixepoch') as day, COUNT(*) as sessions 
FROM sessions WHERE created_at > 0 GROUP BY day ORDER BY day DESC LIMIT 14;

-- Most referenced files
SELECT file_path, COUNT(*) as refs FROM message_files 
GROUP BY file_path ORDER BY refs DESC LIMIT 20;

-- Common lint errors
SELECT SUBSTR(message, 1, 80) as lint, COUNT(*) as cnt 
FROM message_lints GROUP BY SUBSTR(message, 1, 80) ORDER BY cnt DESC LIMIT 10;

-- Longest sessions
SELECT id, project, model, message_count, datetime(created_at/1000, 'unixepoch') as created 
FROM sessions ORDER BY message_count DESC LIMIT 10;
```

## Data Storage

| Path | Purpose |
|------|---------|
| `~/.config/aix/config.json` | Configuration |
| `~/Library/Application Support/aix/aix.db` | Analysis database (macOS) |
| `~/.local/share/aix/aix.db` | Analysis database (Linux) |
| `~/nexus/home/sessions/composer/` | Exported session JSON (git-tracked) |
| `~/nexus/home/sessions/bubbles/` | Exported message bubbles (git-tracked) |

The `aix.db` can always be rebuilt from the exported sessions in `~/nexus/home/sessions/`.

## Data Sources

### Cursor (macOS)

Source: `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`

Handles both Cursor storage formats:
- **Old format** (pre-March 2025): Messages inline in `composerData:uuid`
- **New format** (post-March 2025): Headers in `composerData:uuid`, content in `bubbleId:composerId:bubbleId`

Extracts rich metadata including:
- Model used (`modelConfig.modelName`)
- Capabilities run per message
- Linter errors captured during editing
- File references and code blocks

## Bootstrap (for AI agents)

```bash
# Check if installed
which aix && aix version --json

# Build from source
cd ~/nexus/home/projects/aix
go build -o aix ./cmd/aix/
./aix init
./aix sync --source cursor

# Verify
./aix stats
./aix db query "SELECT COUNT(*) as sessions FROM sessions"
./aix db query "SELECT model, COUNT(*) FROM sessions WHERE model != '' GROUP BY model"
```

## Dependencies

- Go 1.22+
- SQLite3 (via go-sqlite3)
- For embeddings: `GEMINI_API_KEY` environment variable

## Related

- [taskengine](https://github.com/Napageneral/taskengine) - Durable job queue used for high-throughput embeddings
- [.intent/ROADMAP.md](.intent/ROADMAP.md) - Future plans including conversation chunking
