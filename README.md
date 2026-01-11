# aix 🧠

AI session intelligence - search and analyze your AI conversation history.

`aix` reads directly from Cursor's internal database, exports raw sessions for durability, and provides queryable storage with semantic search capabilities.

## Features

- **Direct Cursor sync** - Reads from Cursor's SQLite DB (handles new sharded format)
- **Durable export** - Exports raw sessions to `~/nexus/home/sessions/` for git-tracking
- **Model tracking** - Captures which AI model was used for each session
- **Rich metadata extraction** - Capabilities, lints, file references, code blocks
- **Semantic search** - Find conversations by meaning using embeddings
- **Raw SQL queries** - Full access to your conversation data

## Installation

```bash
# Via Homebrew
brew install Napageneral/tap/aix

# Via Go
go install github.com/Napageneral/aix/cmd/aix@latest

# From source
git clone https://github.com/Napageneral/aix
cd aix
make install
```

## Quick Start

```bash
# Initialize and sync from Cursor
aix init
aix sync --source cursor

# Browse your sessions
aix sessions --today
aix show <session-id>
aix stats

# Query directly
aix db query "SELECT model, COUNT(*) as sessions FROM sessions GROUP BY model ORDER BY sessions DESC"

# Semantic search (requires GEMINI_API_KEY)
export GEMINI_API_KEY=your-key
aix embed --limit 10000
aix search "how to fix TypeScript errors"
```

## Commands

| Command | Description |
|---------|-------------|
| `aix init` | Initialize config and database |
| `aix sync --source cursor` | Sync from Cursor (exports + imports) |
| `aix sessions` | List sessions (filters: --project, --today, --week) |
| `aix show <id>` | View session details (partial ID ok) |
| `aix db query <sql>` | Run SQL queries (SELECT only) |
| `aix stats` | Show database statistics |
| `aix embed` | Generate embeddings for search |
| `aix search <query>` | Semantic search across messages |

All commands support `--json` for machine-readable output.

## Data Flow

```
Cursor DB (9.9GB)
    │
    ▼
~/nexus/home/sessions/     ◄── Git-tracked, portable
    │   composer/*.json       (session metadata)
    │   bubbles/*/*.json      (message content)
    │
    ▼
aix.db (3.3GB)             ◄── Analysis database (rehydratable)
```

### Storage Locations

| Path | Purpose |
|------|---------|
| `~/nexus/home/sessions/` | Exported raw sessions (durable, git-tracked) |
| `~/Library/Application Support/aix/aix.db` | Analysis database (macOS) |
| `~/.local/share/aix/aix.db` | Analysis database (Linux) |

The `aix.db` can always be rebuilt from the exported sessions.

### Sample Stats

```
Sessions:        1,958
Messages:        220,039
File references: 15,342
Projects:        41
Models:          18 (claude-4.5-opus-high-thinking, gpt-5-high, etc.)
Date range:      Aug 2024 - Jan 2026
```

## Requirements

- macOS or Linux
- Go 1.22+ (for building from source)
- Cursor (for session data)
- `GEMINI_API_KEY` (optional, for semantic search with `gemini-embedding-1`)

## Related

- [taskengine](https://github.com/Napageneral/taskengine) - Durable job queue for high-throughput embeddings

## License

MIT
