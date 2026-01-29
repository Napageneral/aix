package sync

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/Napageneral/aix/internal/models"
)

// DefaultNexusSessionsPath returns the path where sessions are exported for durability
func DefaultNexusSessionsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "nexus", "home", "sessions")
}

// CursorDBParser parses sessions directly from Cursor's SQLite database
type CursorDBParser struct {
	dbPath           string
	exportPath       string            // if set, exports sessions to this path for durability
	knownHashes      map[string]string // existing session ID -> hash for incremental sync (legacy)
	sinceRowID       int64             // if >0, only load composerData rows with rowid > sinceRowID
	sinceBubbleRowID int64             // if >0, also re-parse sessions with new bubbleId rows
	maxComposerID    int64             // observed max rowid for composerData (for sync_state)
	maxBubbleRowID   int64             // observed max rowid for bubbleId (for sync_state)
	bubbleExport     string            // "files" (default) or "jsonl" (packed per session)

	// Task dispatch tracking (populated during parsing)
	// Maps tool_call_id -> TaskDispatch for linking parent/child sessions
	taskDispatches   map[string]*TaskDispatch
	taskDispatchesMu sync.Mutex // Protects taskDispatches
}

// TaskDispatch represents a task_v2 tool call that spawned a subagent
type TaskDispatch struct {
	ToolCallID           string // The toolCallId (e.g. toolu_bdrk_01M8WJ...)
	ParentSessionID      string // The session that dispatched the task
	ParentMessageID      string // The bubble where the task was dispatched
	Description          string // Task description param
	Model                string // Model used for the task
	Status               string // pending, running, completed, failed
	Prompt               string // Full prompt sent to the subagent
	ParamsJSON           string // Raw params payload
	ResultJSON           string // Raw result payload
	DispatchCreatedAt    int64  // Timestamp of the dispatch bubble
	EmbeddedComposerData string // If present, the full composerData JSON embedded inline
}

// NewCursorDBParser creates a parser that reads directly from Cursor's DB
func NewCursorDBParser(dbPath string) *CursorDBParser {
	if dbPath == "" {
		dbPath = DefaultCursorDBPath()
	}
	return &CursorDBParser{
		dbPath:         dbPath,
		taskDispatches: make(map[string]*TaskDispatch),
	}
}

// WithExport configures the parser to export sessions to the given path
func (p *CursorDBParser) WithExport(exportPath string) *CursorDBParser {
	p.exportPath = exportPath
	return p
}

// WithBubbleExport configures how bubbles are exported when exporting is enabled.
// - "files": one JSON file per bubble under bubbles/<session>/<bubble>.json (legacy)
// - "jsonl": one file per session under bubbles/<session>/bubbles.jsonl (much faster for full exports)
func (p *CursorDBParser) WithBubbleExport(mode string) *CursorDBParser {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return p
	}
	p.bubbleExport = mode
	return p
}

func (p *CursorDBParser) bubbleExportMode() string {
	if p.bubbleExport == "" {
		return "files"
	}
	switch p.bubbleExport {
	case "files", "jsonl":
		return p.bubbleExport
	default:
		return "files"
	}
}

func exportBubblesJSONL(sessionBubbleDir string, bubbles map[string]string) (int, error) {
	if len(bubbles) == 0 {
		return 0, nil
	}

	// Stable ordering for git diffs.
	ids := make([]string, 0, len(bubbles))
	for id := range bubbles {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	f, err := os.Create(filepath.Join(sessionBubbleDir, "bubbles.jsonl"))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Large buffer: amortize syscalls.
	w := bufio.NewWriterSize(f, 1<<20) // 1MB
	defer w.Flush()

	written := 0
	for _, bubbleID := range ids {
		redacted := RedactJSON(bubbles[bubbleID])
		bid, _ := json.Marshal(bubbleID) // always valid JSON string
		if _, err := w.WriteString(`{"bubble_id":`); err != nil {
			return written, err
		}
		if _, err := w.Write(bid); err != nil {
			return written, err
		}
		if _, err := w.WriteString(`,"data":`); err != nil {
			return written, err
		}
		if _, err := w.WriteString(redacted); err != nil {
			return written, err
		}
		if err := w.WriteByte('}'); err != nil {
			return written, err
		}
		if err := w.WriteByte('\n'); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// WithKnownHashes sets existing session hashes for incremental sync
// Sessions with matching hashes will be skipped during parsing
func (p *CursorDBParser) WithKnownHashes(hashes map[string]string) *CursorDBParser {
	p.knownHashes = hashes
	return p
}

// WithSinceRowID enables rowid-based incremental sync.
// If set, ParseAllWithStats only reads composerData rows with rowid > sinceRowID.
func (p *CursorDBParser) WithSinceRowID(sinceRowID int64) *CursorDBParser {
	p.sinceRowID = sinceRowID
	return p
}

// WithSinceBubbleRowID enables bubble-aware incremental sync.
// If set, ParseAllWithStats also re-parses sessions that have new bubbleId rows > sinceBubbleRowID.
func (p *CursorDBParser) WithSinceBubbleRowID(sinceBubbleRowID int64) *CursorDBParser {
	p.sinceBubbleRowID = sinceBubbleRowID
	return p
}

// ComposerMaxRowID returns the max rowid observed for composerData entries during parsing.
func (p *CursorDBParser) ComposerMaxRowID() int64 {
	return p.maxComposerID
}

// BubbleMaxRowID returns the max rowid observed for bubbleId entries during parsing.
func (p *CursorDBParser) BubbleMaxRowID() int64 {
	return p.maxBubbleRowID
}

// ExportStats holds export statistics
type ExportStats struct {
	ComposerFiles        int
	ComposerFilesSkipped int
	BubbleFiles          int
	BubbleFilesSkipped   int
}

// ParseAll parses all sessions from Cursor's database
func (p *CursorDBParser) ParseAll() ([]*ParsedSession, []error) {
	return p.ParseAllWithStats(nil)
}

// ParseAllWithStats parses all sessions and optionally returns export stats
func (p *CursorDBParser) ParseAllWithStats(exportStats *ExportStats) ([]*ParsedSession, []error) {
	// Check if DB exists
	if _, err := os.Stat(p.dbPath); os.IsNotExist(err) {
		return nil, []error{fmt.Errorf("cursor database not found: %s", p.dbPath)}
	}

	// Open read-only
	db, err := sql.Open("sqlite3", p.dbPath+"?mode=ro")
	if err != nil {
		return nil, []error{fmt.Errorf("failed to open cursor db: %w", err)}
	}
	defer db.Close()

	var sessions []*ParsedSession
	var errors []error

	// Setup export directories if exporting
	var composerDir, bubblesDir string
	if p.exportPath != "" {
		composerDir = filepath.Join(p.exportPath, "composer")
		bubblesDir = filepath.Join(p.exportPath, "bubbles")
		if err := os.MkdirAll(composerDir, 0755); err != nil {
			return nil, []error{fmt.Errorf("failed to create composer export dir: %w", err)}
		}
		if err := os.MkdirAll(bubblesDir, 0755); err != nil {
			return nil, []error{fmt.Errorf("failed to create bubbles export dir: %w", err)}
		}
	}

	// Capture current max rowid for composerData (used by sync_state)
	{
		var maxRow sql.NullInt64
		// Using IFNULL/MAX avoids NULL when no rows exist.
		if err := db.QueryRow(`SELECT IFNULL(MAX(rowid), 0) FROM cursorDiskKV WHERE key LIKE 'composerData:%' AND value IS NOT NULL`).Scan(&maxRow); err == nil {
			p.maxComposerID = maxRow.Int64
		}
	}

	// Capture current max rowid for bubbleId (used by sync_state for bubble-aware incremental)
	{
		var maxRow sql.NullInt64
		if err := db.QueryRow(`SELECT IFNULL(MAX(rowid), 0) FROM cursorDiskKV WHERE key LIKE 'bubbleId:%' AND value IS NOT NULL`).Scan(&maxRow); err == nil {
			p.maxBubbleRowID = maxRow.Int64
		}
	}

	// Find sessions with new bubbles (for bubble-aware incremental sync)
	sessionsWithNewBubbles := make(map[string]bool)
	if p.sinceBubbleRowID > 0 {
		rows, err := db.Query(
			`SELECT DISTINCT substr(key, 10, instr(substr(key, 10), ':') - 1) AS session_id
			 FROM cursorDiskKV
			 WHERE key LIKE 'bubbleId:%' AND rowid > ?`,
			p.sinceBubbleRowID,
		)
		if err == nil {
			for rows.Next() {
				var sessionID string
				if rows.Scan(&sessionID) == nil && sessionID != "" {
					sessionsWithNewBubbles[sessionID] = true
				}
			}
			rows.Close()
		}
	}

	// FAST PATH: rowid-based incremental sync (skip scanning all composerData values)
	// This avoids the expensive "read every value + hash" loop when nothing changed.
	if p.sinceRowID > 0 {
		// Exclude task sessions - they're handled separately in parseTaskSessions
		rows, err := db.Query(
			`SELECT rowid, key, value FROM cursorDiskKV
			 WHERE key LIKE 'composerData:%' AND key NOT LIKE 'composerData:task-%' AND value IS NOT NULL AND rowid > ?`,
			p.sinceRowID,
		)
		if err != nil {
			return nil, []error{fmt.Errorf("failed to query sessions: %w", err)}
		}
		defer rows.Close()

		composerCount := 0
		bubbleCount := 0
		composerSkipped := 0
		bubbleSkipped := 0

		for rows.Next() {
			var rowid int64
			var key, value string
			if err := rows.Scan(&rowid, &key, &value); err != nil {
				errors = append(errors, err)
				continue
			}

			composerID := strings.TrimPrefix(key, "composerData:")

			// Export composerData if exporting (redacted for git safety)
			if composerDir != "" {
				exportFile := filepath.Join(composerDir, composerID+".json")
				// This row is new/changed (rowid > sinceRowID), so always rewrite export output.
				redactedValue := RedactJSON(value)
				if err := os.WriteFile(exportFile, []byte(redactedValue), 0644); err != nil {
					errors = append(errors, fmt.Errorf("failed to export %s: %w", key, err))
				} else {
					composerCount++
				}
			}

			// For incremental parsing, fetch bubbles for *just this composerID* (small N => acceptable).
			bubbleCache := make(map[string]string)
			bubbleRows, err := db.Query(
				`SELECT key, value FROM cursorDiskKV WHERE key LIKE ? AND value IS NOT NULL`,
				fmt.Sprintf("bubbleId:%s:%%", composerID),
			)
			if err == nil {
				for bubbleRows.Next() {
					var bkey, bval string
					if err := bubbleRows.Scan(&bkey, &bval); err != nil {
						continue
					}
					bubbleCache[bkey] = bval
				}
				bubbleRows.Close()
			}

			session, bubbles, err := p.parseSessionWithBubblesFromCache(bubbleCache, key, value)
			if err != nil {
				errors = append(errors, fmt.Errorf("%s: %w", key, err))
				continue
			}
			if session == nil {
				continue
			}
			sessions = append(sessions, session)

			// Export bubbles if exporting and bubble payloads available
			if bubblesDir != "" && len(bubbles) > 0 {
				sessionBubbleDir := filepath.Join(bubblesDir, session.Session.ID)
				if err := os.MkdirAll(sessionBubbleDir, 0755); err != nil {
					errors = append(errors, fmt.Errorf("failed to create bubble dir for %s: %w", session.Session.ID, err))
				} else {
					if p.bubbleExportMode() == "jsonl" {
						n, err := exportBubblesJSONL(sessionBubbleDir, bubbles)
						if err != nil {
							errors = append(errors, fmt.Errorf("failed to export bubbles for %s: %w", session.Session.ID, err))
						} else {
							bubbleCount += n
						}
					} else {
						for bubbleID, bubbleJSON := range bubbles {
							bubbleFile := filepath.Join(sessionBubbleDir, bubbleID+".json")
							redactedBubble := RedactJSON(bubbleJSON)
							if err := os.WriteFile(bubbleFile, []byte(redactedBubble), 0644); err != nil {
								errors = append(errors, fmt.Errorf("failed to export bubble %s: %w", bubbleID, err))
							} else {
								bubbleCount++
							}
						}
					}
				}
			}
		}

		// Also process sessions with new bubbles (bubble-aware incremental)
		if len(sessionsWithNewBubbles) > 0 {
			// Remove sessions we already processed (by composerData rowid change)
			for _, s := range sessions {
				delete(sessionsWithNewBubbles, s.Session.ID)
			}

			// Process remaining sessions with new bubbles
			for sessionID := range sessionsWithNewBubbles {
				// Fetch composerData for this session
				var value string
				err := db.QueryRow(
					`SELECT value FROM cursorDiskKV WHERE key = ? AND value IS NOT NULL`,
					"composerData:"+sessionID,
				).Scan(&value)
				if err != nil {
					continue
				}

				// Fetch bubbles for this session
				bubbleCache := make(map[string]string)
				bubbleRows, err := db.Query(
					`SELECT key, value FROM cursorDiskKV WHERE key LIKE ? AND value IS NOT NULL`,
					fmt.Sprintf("bubbleId:%s:%%", sessionID),
				)
				if err == nil {
					for bubbleRows.Next() {
						var bkey, bval string
						if bubbleRows.Scan(&bkey, &bval) == nil {
							bubbleCache[bkey] = bval
						}
					}
					bubbleRows.Close()
				}

				session, bubbles, err := p.parseSessionWithBubblesFromCache(bubbleCache, "composerData:"+sessionID, value)
				if err != nil || session == nil {
					continue
				}
				sessions = append(sessions, session)

				// Export bubbles if exporting
				if bubblesDir != "" && len(bubbles) > 0 {
					sessionBubbleDir := filepath.Join(bubblesDir, session.Session.ID)
					if os.MkdirAll(sessionBubbleDir, 0755) == nil {
						if p.bubbleExportMode() == "jsonl" {
							n, _ := exportBubblesJSONL(sessionBubbleDir, bubbles)
							bubbleCount += n
						} else {
							for bubbleID, bubbleJSON := range bubbles {
								bubbleFile := filepath.Join(sessionBubbleDir, bubbleID+".json")
								redactedBubble := RedactJSON(bubbleJSON)
								if os.WriteFile(bubbleFile, []byte(redactedBubble), 0644) == nil {
									bubbleCount++
								}
							}
						}
					}
				}
			}
		}

		if exportStats != nil {
			exportStats.ComposerFiles = composerCount
			exportStats.ComposerFilesSkipped = composerSkipped
			exportStats.BubbleFiles = bubbleCount
			exportStats.BubbleFilesSkipped = bubbleSkipped
		}

		// Also parse task sessions in fast path
		taskSessions, taskErrors := p.parseTaskSessions(db, bubblesDir, p.bubbleExportMode())
		sessions = append(sessions, taskSessions...)
		errors = append(errors, taskErrors...)

		return sessions, errors
	}

	// First pass: identify which sessions need processing (for incremental sync)
	type sessionEntry struct {
		key, value string
		skip       bool
	}
	var entriesToProcess []sessionEntry
	var changedSessionIDs = make(map[string]bool)

	// Exclude task sessions (composerData:task-*) from regular processing - they're handled separately
	rows1, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%' AND key NOT LIKE 'composerData:task-%' AND value IS NOT NULL`)
	if err != nil {
		return nil, []error{fmt.Errorf("failed to query sessions: %w", err)}
	}
	for rows1.Next() {
		var key, value string
		if err := rows1.Scan(&key, &value); err != nil {
			continue
		}

		composerID := strings.TrimPrefix(key, "composerData:")
		skip := false

		// Legacy hash-based incremental sync (kept for compatibility)
		// NOTE: prefer rowid-based incremental via WithSinceRowID.
		if p.knownHashes != nil {
			// This path is intentionally a no-op now (hashing here was a major perf sink).
			// If caller wants incremental, they should use WithSinceRowID.
		}

		if !skip {
			changedSessionIDs[composerID] = true
		}
		entriesToProcess = append(entriesToProcess, sessionEntry{key: key, value: value, skip: skip})
	}
	rows1.Close()

	// Only fetch bubbles for changed sessions (major optimization for incremental sync)
	bubbleCache := make(map[string]string)
	if len(changedSessionIDs) > 0 {
		bubbleRows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'bubbleId:%' AND value IS NOT NULL`)
		if err != nil {
			return nil, []error{fmt.Errorf("failed to query bubbles: %w", err)}
		}
		for bubbleRows.Next() {
			var key, value string
			if err := bubbleRows.Scan(&key, &value); err != nil {
				continue
			}
			// Only cache bubbles for changed sessions
			// Key format: bubbleId:composerId:bubbleId
			parts := strings.Split(key, ":")
			if len(parts) >= 2 {
				composerID := parts[1]
				if changedSessionIDs[composerID] {
					bubbleCache[key] = value
				}
			}
		}
		bubbleRows.Close()
	}

	var composerCount64 int64
	var bubbleCount64 int64
	var composerSkipped64 int64
	var bubbleSkipped64 int64

	workerCount := runtime.NumCPU()
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > 8 {
		workerCount = 8
	}

	var mu sync.Mutex
	jobs := make(chan sessionEntry, workerCount*2)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for entry := range jobs {
			key := entry.key
			value := entry.value
			composerID := strings.TrimPrefix(key, "composerData:")

			if entry.skip {
				atomic.AddInt64(&composerSkipped64, 1)
				continue
			}

			// Fast incremental check: if composer file exists, skip exporting (still parse into aix.db)
			skipExport := false
			if composerDir != "" {
				exportFile := filepath.Join(composerDir, composerID+".json")
				if _, err := os.Stat(exportFile); err == nil {
					skipExport = true
					atomic.AddInt64(&composerSkipped64, 1)
				}
			}

			// Export composerData if enabled and not skipping
			if composerDir != "" && !skipExport {
				exportFile := filepath.Join(composerDir, composerID+".json")
				redactedValue := RedactJSON(value)
				if err := os.WriteFile(exportFile, []byte(redactedValue), 0644); err != nil {
					mu.Lock()
					errors = append(errors, fmt.Errorf("failed to export %s: %w", key, err))
					mu.Unlock()
				} else {
					atomic.AddInt64(&composerCount64, 1)
				}
			}

			session, bubbles, err := p.parseSessionWithBubblesFromCache(bubbleCache, key, value)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("%s: %w", key, err))
				mu.Unlock()
				continue
			}
			if session == nil {
				continue
			}

			mu.Lock()
			sessions = append(sessions, session)
			mu.Unlock()

			// Export bubbles if exporting and not skipping this session
			if bubblesDir != "" && len(bubbles) > 0 && !skipExport {
				sessionBubbleDir := filepath.Join(bubblesDir, session.Session.ID)
				if err := os.MkdirAll(sessionBubbleDir, 0755); err != nil {
					mu.Lock()
					errors = append(errors, fmt.Errorf("failed to create bubble dir for %s: %w", session.Session.ID, err))
					mu.Unlock()
				} else {
					if p.bubbleExportMode() == "jsonl" {
						n, err := exportBubblesJSONL(sessionBubbleDir, bubbles)
						if err != nil {
							mu.Lock()
							errors = append(errors, fmt.Errorf("failed to export bubbles for %s: %w", session.Session.ID, err))
							mu.Unlock()
						} else {
							atomic.AddInt64(&bubbleCount64, int64(n))
						}
					} else {
						for bubbleID, bubbleJSON := range bubbles {
							bubbleFile := filepath.Join(sessionBubbleDir, bubbleID+".json")
							redactedBubble := RedactJSON(bubbleJSON)
							if err := os.WriteFile(bubbleFile, []byte(redactedBubble), 0644); err != nil {
								mu.Lock()
								errors = append(errors, fmt.Errorf("failed to export bubble %s: %w", bubbleID, err))
								mu.Unlock()
							} else {
								atomic.AddInt64(&bubbleCount64, 1)
							}
						}
					}
				}
			} else if bubblesDir != "" && skipExport {
				atomic.AddInt64(&bubbleSkipped64, int64(len(bubbles)))
			}
		}
	}

	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go worker()
	}
	for _, entry := range entriesToProcess {
		jobs <- entry
	}
	close(jobs)
	wg.Wait()

	composerCount := int(atomic.LoadInt64(&composerCount64))
	bubbleCount := int(atomic.LoadInt64(&bubbleCount64))
	composerSkipped := int(atomic.LoadInt64(&composerSkipped64))
	bubbleSkipped := int(atomic.LoadInt64(&bubbleSkipped64))

	if exportStats != nil {
		exportStats.ComposerFiles = composerCount
		exportStats.ComposerFilesSkipped = composerSkipped
		exportStats.BubbleFiles = bubbleCount
		exportStats.BubbleFilesSkipped = bubbleSkipped
	}

	// SECOND PASS: Parse task sessions (subagents) and link to parents
	taskSessions, taskErrors := p.parseTaskSessions(db, bubblesDir, p.bubbleExportMode())
	sessions = append(sessions, taskSessions...)
	errors = append(errors, taskErrors...)

	return sessions, errors
}

// parseTaskSessions parses subagent sessions from embedded composerData,
// composerData:task-* keys, and bubbleId:task-* keys.
func (p *CursorDBParser) parseTaskSessions(db *sql.DB, bubblesDir string, bubbleExportMode string) ([]*ParsedSession, []error) {
	if bubbleExportMode == "" {
		bubbleExportMode = p.bubbleExportMode()
	}

	var errors []error
	subagentSessions := make(map[string]*ParsedSession)

	// Copy dispatches for thread-safety
	dispatches := make(map[string]*TaskDispatch)
	p.taskDispatchesMu.Lock()
	for k, v := range p.taskDispatches {
		dispatches[k] = v
	}
	p.taskDispatchesMu.Unlock()

	// Load all task bubbles once: bubbleId:task-<toolCallId>:<bubbleId>
	taskBubbles := make(map[string]map[string]string)
	if rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'bubbleId:task-%' AND value IS NOT NULL`); err == nil {
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				continue
			}
			parts := strings.SplitN(key, ":", 3)
			if len(parts) < 3 {
				continue
			}
			taskID := parts[1] // task-<toolCallId>
			toolCallID := strings.TrimPrefix(taskID, "task-")
			if toolCallID == "" {
				continue
			}
			group := taskBubbles[toolCallID]
			if group == nil {
				group = make(map[string]string)
				taskBubbles[toolCallID] = group
			}
			group[key] = value
		}
		rows.Close()
	} else {
		errors = append(errors, fmt.Errorf("failed to query task bubbles: %w", err))
	}

	mergeSession := func(ps *ParsedSession, toolCallID string, dispatch *TaskDispatch, bubbles map[string]string) {
		if ps == nil || toolCallID == "" {
			return
		}
		applyDispatchToParsedSession(ps, toolCallID, dispatch)
		sessionID := ps.Session.ID
		existing := subagentSessions[sessionID]
		if existing == nil {
			subagentSessions[sessionID] = ps
		} else {
			subagentSessions[sessionID] = chooseParsedSession(existing, ps)
		}
		if bubblesDir != "" && len(bubbles) > 0 {
			_ = exportBubblesForSession(bubblesDir, bubbleExportMode, sessionID, bubbles)
		}
	}

	// 1) composerData:task-* sessions (rare)
	if rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:task-%' AND value IS NOT NULL`); err == nil {
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				errors = append(errors, err)
				continue
			}
			taskID := strings.TrimPrefix(key, "composerData:")
			toolCallID := strings.TrimPrefix(taskID, "task-")
			bubbleCache := taskBubbles[toolCallID]
			session, bubbles, err := p.parseSessionWithBubblesFromCache(bubbleCache, key, value)
			if err != nil {
				errors = append(errors, fmt.Errorf("%s: %w", key, err))
				continue
			}
			mergeSession(session, toolCallID, dispatches[toolCallID], bubbles)
		}
		rows.Close()
	} else {
		errors = append(errors, fmt.Errorf("failed to query task sessions: %w", err))
	}

	// 2) embedded composerData from task dispatches
	for toolCallID, dispatch := range dispatches {
		if dispatch == nil || dispatch.EmbeddedComposerData == "" {
			continue
		}
		composerID := gjson.Get(dispatch.EmbeddedComposerData, "composerId").String()
		if composerID == "" {
			continue
		}
		bubbleCache := make(map[string]string)
		if group := taskBubbles[toolCallID]; group != nil {
			for k, v := range group {
				bubbleCache[k] = v
			}
			// If composerId differs, alias keys to match expected prefix
			if composerID != "task-"+toolCallID {
				for k, v := range group {
					parts := strings.SplitN(k, ":", 3)
					if len(parts) < 3 {
						continue
					}
					aliasKey := fmt.Sprintf("bubbleId:%s:%s", composerID, parts[2])
					bubbleCache[aliasKey] = v
				}
			}
		}
		session, bubbles, err := p.parseSessionWithBubblesFromCache(bubbleCache, "composerData:"+composerID, dispatch.EmbeddedComposerData)
		if err != nil {
			errors = append(errors, fmt.Errorf("embedded composerData (%s): %w", toolCallID, err))
			continue
		}
		mergeSession(session, toolCallID, dispatch, bubbles)
	}

	// 3) bubbleId:task-* sessions (when no composerData exists)
	for toolCallID, bubbleCache := range taskBubbles {
		if toolCallID == "" {
			continue
		}
		session, bubbles := parseTaskSessionFromBubbles(toolCallID, bubbleCache)
		mergeSession(session, toolCallID, dispatches[toolCallID], bubbles)
	}

	// 4) minimal subagent sessions for UUID-format dispatches
	for toolCallID, dispatch := range dispatches {
		if toolCallID == "" {
			continue
		}
		sessionID := "task-" + toolCallID
		if _, ok := subagentSessions[sessionID]; ok {
			continue
		}
		session := buildMinimalSubagentSession(toolCallID, dispatch)
		mergeSession(session, toolCallID, dispatch, nil)
	}

	// Flatten map to slice
	sessions := make([]*ParsedSession, 0, len(subagentSessions))
	for _, ps := range subagentSessions {
		sessions = append(sessions, ps)
	}
	return sessions, errors
}

func applyDispatchToParsedSession(ps *ParsedSession, toolCallID string, dispatch *TaskDispatch) {
	if ps == nil {
		return
	}
	if toolCallID != "" {
		targetID := "task-" + toolCallID
		if ps.Session.ID != targetID {
			rewriteParsedSessionID(ps, targetID)
		}
		ps.Session.ToolCallID = toolCallID
	}
	ps.Session.IsSubagent = true
	if dispatch == nil {
		return
	}
	if ps.Session.ParentSessionID == "" {
		ps.Session.ParentSessionID = dispatch.ParentSessionID
	}
	if ps.Session.ParentMessageID == "" {
		ps.Session.ParentMessageID = dispatch.ParentMessageID
	}
	if ps.Session.TaskDescription == "" {
		ps.Session.TaskDescription = dispatch.Description
	}
	if ps.Session.TaskStatus == "" {
		ps.Session.TaskStatus = dispatch.Status
	}
	if ps.Session.Model == "" {
		ps.Session.Model = dispatch.Model
	}
	if ps.Session.CreatedAt == 0 && dispatch.DispatchCreatedAt > 0 {
		ps.Session.CreatedAt = dispatch.DispatchCreatedAt
	}
	ps.Session.MessageCount = len(ps.Messages)
	ps.RawJSON = chooseRawJSON(ps.RawJSON, dispatch)

	if ps.Session.Model != "" {
		for i := range ps.Turns {
			if ps.Turns[i].Model == "" {
				ps.Turns[i].Model = ps.Session.Model
			}
		}
	}
}

func rewriteParsedSessionID(ps *ParsedSession, newID string) {
	if ps == nil || newID == "" {
		return
	}
	ps.Session.ID = newID
	for i := range ps.Messages {
		ps.Messages[i].SessionID = newID
	}
	for i := range ps.Capabilities {
		ps.Capabilities[i].SessionID = newID
	}
	for i := range ps.Lints {
		ps.Lints[i].SessionID = newID
	}
	for i := range ps.MessageFiles {
		ps.MessageFiles[i].SessionID = newID
	}
	for i := range ps.Codeblocks {
		ps.Codeblocks[i].SessionID = newID
	}
	for i := range ps.ToolCalls {
		ps.ToolCalls[i].SessionID = newID
	}
	for i := range ps.Turns {
		ps.Turns[i].SessionID = newID
	}
}

func chooseParsedSession(a, b *ParsedSession) *ParsedSession {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if len(b.Messages) > len(a.Messages) {
		return b
	}
	if len(b.Messages) < len(a.Messages) {
		return a
	}
	if len(b.MessageMetadata) > len(a.MessageMetadata) {
		return b
	}
	return a
}

func chooseRawJSON(existing string, dispatch *TaskDispatch) string {
	if existing != "" || dispatch == nil {
		return existing
	}
	raw := map[string]string{
		"tool_call_id":  dispatch.ToolCallID,
		"description":   dispatch.Description,
		"model":         dispatch.Model,
		"status":        dispatch.Status,
		"params_json":   dispatch.ParamsJSON,
		"result_json":   dispatch.ResultJSON,
		"prompt":        dispatch.Prompt,
		"parent_session": dispatch.ParentSessionID,
		"parent_message": dispatch.ParentMessageID,
	}
	if b, err := json.Marshal(raw); err == nil {
		return string(b)
	}
	return existing
}

func buildMinimalSubagentSession(toolCallID string, dispatch *TaskDispatch) *ParsedSession {
	if toolCallID == "" {
		return nil
	}
	sessionID := "task-" + toolCallID
	session := models.Session{
		ID:           sessionID,
		Source:       "cursor",
		CreatedAt:    0,
		MessageCount: 0,
		IsSubagent:   true,
		ToolCallID:   toolCallID,
	}
	if dispatch != nil {
		session.ParentSessionID = dispatch.ParentSessionID
		session.ParentMessageID = dispatch.ParentMessageID
		session.TaskDescription = dispatch.Description
		session.TaskStatus = dispatch.Status
		session.Model = dispatch.Model
		if dispatch.DispatchCreatedAt > 0 {
			session.CreatedAt = dispatch.DispatchCreatedAt
		}
	}
	return &ParsedSession{
		Session:         session,
		Messages:        nil,
		MessageMetadata: map[string]string{},
		Capabilities:    nil,
		Lints:           nil,
		MessageFiles:    nil,
		Codeblocks:      nil,
		ToolCalls:       nil,
		Turns:           nil,
		Files:           nil,
		RawJSON:         chooseRawJSON("", dispatch),
	}
}

func parseTaskSessionFromBubbles(toolCallID string, bubbleCache map[string]string) (*ParsedSession, map[string]string) {
	if toolCallID == "" || len(bubbleCache) == 0 {
		return nil, nil
	}
	sessionID := "task-" + toolCallID

	type bubbleEntry struct {
		id        string
		value     string
		timestamp int64
	}
	entries := make([]bubbleEntry, 0, len(bubbleCache))
	for key, value := range bubbleCache {
		parts := strings.SplitN(key, ":", 3)
		if len(parts) < 3 {
			continue
		}
		bubbleID := parts[2]
		ts := bubbleTimestampMillis(bubbleID, value)
		entries = append(entries, bubbleEntry{id: bubbleID, value: value, timestamp: ts})
	}
	if len(entries) == 0 {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].timestamp == entries[j].timestamp {
			return entries[i].id < entries[j].id
		}
		if entries[i].timestamp == 0 {
			return false
		}
		if entries[j].timestamp == 0 {
			return true
		}
		return entries[i].timestamp < entries[j].timestamp
	})

	bubbleJSONs := make(map[string]string, len(entries))
	meta := make(map[string]string, len(entries))
	var messages []models.Message
	var caps []MessageCapability
	var lints []MessageLint
	var mfiles []MessageFileRef
	var codeblocks []MessageCodeblock
	var toolCalls []models.ToolCall
	var createdAt int64

	for i, entry := range entries {
		bubbleJSONs[entry.id] = entry.value
		msg := models.Message{
			ID:        entry.id,
			SessionID: sessionID,
			Sequence:  i,
			Timestamp: entry.timestamp,
		}
		if msg.Timestamp == 0 && createdAt > 0 {
			msg.Timestamp = createdAt
		}
		msgType := int(gjson.Get(entry.value, "type").Int())
		switch msgType {
		case 1:
			msg.Role = "user"
		case 2:
			msg.Role = "assistant"
		default:
			msg.Role = "unknown"
		}
		text := gjson.Get(entry.value, "text").String()
		if text == "" {
			text = gjson.Get(entry.value, "rawText").String()
		}
		msg.Content = text
		msg.CheckpointID = gjson.Get(entry.value, "checkpointId").String()
		msg.IsAgentic = gjson.Get(entry.value, "isAgentic").Bool()
		msg.IsPlanExecution = gjson.Get(entry.value, "isPlanExecution").Bool()
		if ctx := gjson.Get(entry.value, "context"); ctx.Exists() {
			msg.ContextJSON = ctx.Raw
		}
		if rules := gjson.Get(entry.value, "cursorRules"); rules.Exists() && rules.IsArray() && len(rules.Array()) > 0 {
			msg.CursorRulesJSON = rules.Raw
		}
		bubbleCaps, bubbleLints, bubbleFiles, bubbleCBs := extractBubbleMetadataJSON(sessionID, entry.id, entry.value)
		caps = append(caps, bubbleCaps...)
		lints = append(lints, bubbleLints...)
		mfiles = append(mfiles, bubbleFiles...)
		codeblocks = append(codeblocks, bubbleCBs...)
		meta[entry.id] = entry.value
		toolCalls = append(toolCalls, extractToolCallsFromBubble(sessionID, entry.id, entry.value)...)
		messages = append(messages, msg)
		if createdAt == 0 && msg.Timestamp > 0 {
			createdAt = msg.Timestamp
		}
	}

	session := models.Session{
		ID:           sessionID,
		Source:       "cursor",
		CreatedAt:    createdAt,
		MessageCount: len(messages),
	}
	turns := buildTurnsForSession(&session, messages, toolCalls)

	return &ParsedSession{
		Session:         session,
		Messages:        messages,
		MessageMetadata: meta,
		Capabilities:    caps,
		Lints:           lints,
		MessageFiles:    mfiles,
		Codeblocks:      codeblocks,
		ToolCalls:       toolCalls,
		Turns:           turns,
		Files:           nil,
		RawJSON:         "",
	}, bubbleJSONs
}

func exportBubblesForSession(bubblesDir, bubbleExportMode, sessionID string, bubbles map[string]string) error {
	if bubblesDir == "" || len(bubbles) == 0 {
		return nil
	}
	sessionBubbleDir := filepath.Join(bubblesDir, sessionID)
	if err := os.MkdirAll(sessionBubbleDir, 0755); err != nil {
		return err
	}
	if bubbleExportMode == "jsonl" {
		_, err := exportBubblesJSONL(sessionBubbleDir, bubbles)
		return err
	}
	for bubbleID, bubbleJSON := range bubbles {
		bubbleFile := filepath.Join(sessionBubbleDir, bubbleID+".json")
		redactedBubble := RedactJSON(bubbleJSON)
		if err := os.WriteFile(bubbleFile, []byte(redactedBubble), 0644); err != nil {
			return err
		}
	}
	return nil
}

// parseSession parses a single session, handling both old and new formats
func (p *CursorDBParser) parseSession(db *sql.DB, key, value string) (*ParsedSession, error) {
	session, _, err := p.parseSessionWithBubbles(db, key, value)
	return session, err
}

// parseSessionWithBubbles parses a session and returns bubble JSON for export (legacy, uses DB queries)
func (p *CursorDBParser) parseSessionWithBubbles(db *sql.DB, key, value string) (*ParsedSession, map[string]string, error) {
	// Build a mini-cache for this session's bubbles by querying the DB
	// This is the slow path - prefer parseSessionWithBubblesFromCache
	bubbleCache := make(map[string]string)
	return p.parseSessionWithBubblesFromCache(bubbleCache, key, value)
}

// parseSessionWithBubblesFromCache parses a session using a pre-fetched bubble cache (fast path)
func (p *CursorDBParser) parseSessionWithBubblesFromCache(bubbleCache map[string]string, key, value string) (*ParsedSession, map[string]string, error) {
	bubbleJSONs := make(map[string]string) // bubbleID -> raw JSON
	composerID := gjson.Get(value, "composerId").String()
	if composerID == "" {
		return nil, nil, nil // Skip invalid entries
	}

	createdAt := gjson.Get(value, "createdAt").Int()
	if createdAt == 0 {
		if ts, ok := uuidV7TimestampMillis(composerID); ok {
			createdAt = ts
		}
	}

	// Determine session format (fast checks)
	headersRes := gjson.Get(value, "fullConversationHeadersOnly")
	hasNewFormat := headersRes.Exists() && headersRes.IsArray() && len(headersRes.Array()) > 0
	convRes := gjson.Get(value, "conversation")
	hasOldFormat := convRes.Exists() && convRes.IsArray() && len(convRes.Array()) > 0

	// Extract model from modelConfig (fast)
	model := gjson.Get(value, "modelConfig.modelName").String()

	// Extract new session-level metadata
	contextTokenLimit := int(gjson.Get(value, "contextTokenLimit").Int())
	contextTokensUsed := int(gjson.Get(value, "contextTokensUsed").Int())
	isAgentic := gjson.Get(value, "isAgentic").Bool()
	forceMode := gjson.Get(value, "forceMode").String()
	conversationState := gjson.Get(value, "conversationState").String()

	// Extract workspace path from context.fileSelections or context.folderSelections
	workspacePath := ""
	if fs := gjson.Get(value, "context.fileSelections.0.uri.fsPath").String(); fs != "" {
		// Extract workspace root from file path (find common prefix)
		workspacePath = extractWorkspaceFromPath(fs)
	} else if fs := gjson.Get(value, "context.folderSelections.0.absolutePath").String(); fs != "" {
		workspacePath = fs
	}

	// Extract session-level context as JSON (fileSelections, folderSelections, etc.)
	contextJSON := ""
	if ctx := gjson.Get(value, "context"); ctx.Exists() {
		contextJSON = ctx.Raw
	}

	// Project + session-level files:
	// - For new format: extract without unmarshalling
	// - For old format: fall back to existing map-based helpers
	project := ""
	files := []string(nil)
	var raw map[string]interface{}
	if hasNewFormat {
		project = extractProjectFromComposerJSON(value)
		files = extractFilesFromComposerJSON(value)
	} else {
		// Old/unknown format: keep compatibility via json.Unmarshal
		if err := json.Unmarshal([]byte(value), &raw); err != nil {
			return nil, nil, fmt.Errorf("invalid JSON: %w", err)
		}
		project = extractProjectFromRaw(raw)
		files = extractFilesFromRaw(raw)
	}

	// Use extracted workspace path if project inference failed
	if project == "" && workspacePath != "" {
		project = workspacePath
	}

	session := models.Session{
		ID:                composerID,
		Source:            "cursor",
		Project:           project,
		Model:             model,
		CreatedAt:         createdAt,
		ContextTokenLimit: contextTokenLimit,
		ContextTokensUsed: contextTokensUsed,
		IsAgentic:         isAgentic,
		ForceMode:         forceMode,
		WorkspacePath:     workspacePath,
		ContextJSON:       contextJSON,
		ConversationState: conversationState,
	}

	var messages []models.Message
	meta := make(map[string]string)
	var caps []MessageCapability
	var lints []MessageLint
	var mfiles []MessageFileRef
	var codeblocks []MessageCodeblock
	var toolCalls []models.ToolCall
	var turns []models.Turn

	if hasOldFormat && !hasNewFormat {
		conversation, _ := raw["conversation"].([]interface{})
		// OLD FORMAT: messages are inline in conversation array
		convMaps := make([]map[string]interface{}, 0, len(conversation))
		for _, item := range conversation {
			if m, ok := item.(map[string]interface{}); ok {
				convMaps = append(convMaps, m)
			}
		}
		var oldToolCalls []models.ToolCall
		messages, meta, caps, lints, mfiles, codeblocks, oldToolCalls = parseMessages(composerID, createdAt, convMaps)
		toolCalls = append(toolCalls, oldToolCalls...)

	} else if hasNewFormat {
		// NEW FORMAT: headers only in composerData, content in bubbleId:* keys
		for i, h := range headersRes.Array() {
			bubbleID := h.Get("bubbleId").String()
			if bubbleID == "" {
				continue
			}

			msgType := int(h.Get("type").Int())

			// Look up bubble content from pre-fetched cache (fast!) instead of N+1 DB queries
			bubbleKey := fmt.Sprintf("bubbleId:%s:%s", composerID, bubbleID)
			bubbleValue := bubbleCache[bubbleKey]

			msg := models.Message{
				ID:        bubbleID,
				SessionID: composerID,
				Sequence:  i,
			}

			// Set role based on type
			switch msgType {
			case 1:
				msg.Role = "user"
			case 2:
				msg.Role = "assistant"
			default:
				msg.Role = "unknown"
			}

			// Timestamp: prefer bubble's createdAt, then UUIDv7, then session createdAt
			if bubbleValue != "" {
				// Try ISO timestamp from bubble (e.g., "2026-01-22T00:53:34.189Z")
				if bubbleCreatedAt := gjson.Get(bubbleValue, "createdAt").String(); bubbleCreatedAt != "" {
					if t, err := time.Parse(time.RFC3339Nano, bubbleCreatedAt); err == nil {
						msg.Timestamp = t.UnixMilli()
					} else if t, err := time.Parse("2006-01-02T15:04:05.999Z", bubbleCreatedAt); err == nil {
						msg.Timestamp = t.UnixMilli()
					}
				}
			}
			if msg.Timestamp == 0 {
				if ts, ok := uuidV7TimestampMillis(bubbleID); ok {
					msg.Timestamp = ts
				} else if createdAt > 0 {
					msg.Timestamp = createdAt
				}
			}

			// Parse bubble content if available
			if bubbleValue != "" {
				// Store raw bubble JSON for export
				bubbleJSONs[bubbleID] = bubbleValue
				// Extract text (no json.Unmarshal)
				text := gjson.Get(bubbleValue, "text").String()
				if text == "" {
					text = gjson.Get(bubbleValue, "rawText").String()
				}
				msg.Content = text

				// Extract new per-message metadata fields
				msg.CheckpointID = gjson.Get(bubbleValue, "checkpointId").String()
				msg.IsAgentic = gjson.Get(bubbleValue, "isAgentic").Bool()
				msg.IsPlanExecution = gjson.Get(bubbleValue, "isPlanExecution").Bool()

				// Per-message context (fileSelections, folderSelections, mentions, etc.)
				if ctx := gjson.Get(bubbleValue, "context"); ctx.Exists() {
					msg.ContextJSON = ctx.Raw
				}

				// Cursor rules in effect for this message
				if rules := gjson.Get(bubbleValue, "cursorRules"); rules.Exists() && rules.IsArray() && len(rules.Array()) > 0 {
					msg.CursorRulesJSON = rules.Raw
				}

				// Extract metadata, capabilities, lints, files from bubble (no json.Unmarshal)
				bubbleCaps, bubbleLints, bubbleFiles, bubbleCBs := extractBubbleMetadataJSON(composerID, bubbleID, bubbleValue)
				caps = append(caps, bubbleCaps...)
				lints = append(lints, bubbleLints...)
				mfiles = append(mfiles, bubbleFiles...)
				codeblocks = append(codeblocks, bubbleCBs...)

				// Store raw metadata (bubble JSON)
				meta[bubbleID] = bubbleValue

				// Tool calls (toolFormerData)
				toolCalls = append(toolCalls, extractToolCallsFromBubble(composerID, bubbleID, bubbleValue)...)
			}

			// Check for task_v2 tool calls that spawn subagents
			if dispatch := extractTaskDispatchFromBubble(composerID, bubbleID, bubbleValue); dispatch != nil {
				p.taskDispatchesMu.Lock()
				if p.taskDispatches != nil {
					p.taskDispatches[dispatch.ToolCallID] = dispatch
				}
				p.taskDispatchesMu.Unlock()
			}

			messages = append(messages, msg)
		}
		// Capture orphaned task dispatches not present in headers
		for bubbleKey, bubbleValue := range bubbleCache {
			if !strings.HasPrefix(bubbleKey, "bubbleId:"+composerID+":") {
				continue
			}
			parts := strings.SplitN(bubbleKey, ":", 3)
			if len(parts) < 3 {
				continue
			}
			bubbleID := parts[2]
			if dispatch := extractTaskDispatchFromBubble(composerID, bubbleID, bubbleValue); dispatch != nil {
				p.taskDispatchesMu.Lock()
				if p.taskDispatches != nil {
					p.taskDispatches[dispatch.ToolCallID] = dispatch
				}
				p.taskDispatchesMu.Unlock()
			}
		}
	} else {
		// No conversation data - skip or create minimal session
		return nil, nil, nil
	}

	session.MessageCount = len(messages)
	turns = buildTurnsForSession(&session, messages, toolCalls)

	return &ParsedSession{
		Session:         session,
		Messages:        messages,
		MessageMetadata: meta,
		Capabilities:    caps,
		Lints:           lints,
		MessageFiles:    mfiles,
		Codeblocks:      codeblocks,
		ToolCalls:       toolCalls,
		Turns:           turns,
		Files:           files,
		RawJSON:         value,
	}, bubbleJSONs, nil
}

// extractTaskDispatchFromBubble extracts task_v2 tool calls from a bubble's toolFormerData
// Returns nil if no task dispatch is found
func extractTaskDispatchFromBubble(sessionID, messageID, bubbleJSON string) *TaskDispatch {
	// Look for toolFormerData with name "task_v2"
	tfd := gjson.Get(bubbleJSON, "toolFormerData")
	if !tfd.Exists() {
		return nil
	}

	name := tfd.Get("name").String()
	if name != "task_v2" && name != "task" {
		return nil
	}

	toolCallID := tfd.Get("toolCallId").String()
	if toolCallID == "" {
		return nil
	}

	paramsJSON := normalizeToolFormerPayload(tfd.Get("params"))
	description := gjson.Get(paramsJSON, "description").String()
	model := gjson.Get(paramsJSON, "model").String()
	prompt := gjson.Get(paramsJSON, "prompt").String()
	resultJSON := normalizeToolFormerPayload(tfd.Get("result"))
	status := tfd.Get("status").String()
	if status == "" {
		status = tfd.Get("additionalData.status").String()
	}

	// Check for embedded composerData
	embeddedComposerData := tfd.Get("additionalData.composerData").String()

	return &TaskDispatch{
		ToolCallID:           toolCallID,
		ParentSessionID:      sessionID,
		ParentMessageID:      messageID,
		Description:          description,
		Model:                model,
		Status:               status,
		Prompt:               prompt,
		ParamsJSON:           paramsJSON,
		ResultJSON:           resultJSON,
		DispatchCreatedAt:    bubbleTimestampMillis(messageID, bubbleJSON),
		EmbeddedComposerData: embeddedComposerData,
	}
}

func extractToolCallsFromBubble(sessionID, messageID, bubbleJSON string) []models.ToolCall {
	tfd := gjson.Get(bubbleJSON, "toolFormerData")
	if !tfd.Exists() {
		return nil
	}
	var out []models.ToolCall
	if tfd.IsArray() {
		for _, item := range tfd.Array() {
			if tc := buildToolCallFromToolFormerData(sessionID, messageID, item); tc != nil {
				out = append(out, *tc)
			}
		}
		return out
	}
	if tc := buildToolCallFromToolFormerData(sessionID, messageID, tfd); tc != nil {
		out = append(out, *tc)
	}
	return out
}

func buildToolCallFromToolFormerData(sessionID, messageID string, tfd gjson.Result) *models.ToolCall {
	toolCallID := tfd.Get("toolCallId").String()
	if toolCallID == "" {
		return nil
	}
	toolName := tfd.Get("name").String()
	status := tfd.Get("status").String()
	if status == "" {
		status = tfd.Get("additionalData.status").String()
	}
	childSessionID := ""
	if toolName == "task_v2" || toolName == "task" {
		childSessionID = "task-" + toolCallID
	}
	return &models.ToolCall{
		ID:             toolCallID,
		MessageID:      messageID,
		SessionID:      sessionID,
		ToolName:       toolName,
		ToolNumber:     int(tfd.Get("tool").Int()),
		ParamsJSON:     normalizeToolFormerPayload(tfd.Get("params")),
		ResultJSON:     normalizeToolFormerPayload(tfd.Get("result")),
		Status:         status,
		ChildSessionID: childSessionID,
	}
}

func normalizeToolFormerPayload(val gjson.Result) string {
	if !val.Exists() {
		return ""
	}
	if val.Type == gjson.String {
		return strings.TrimSpace(val.String())
	}
	return strings.TrimSpace(val.Raw)
}

func bubbleTimestampMillis(bubbleID, bubbleJSON string) int64 {
	if bubbleJSON != "" {
		if bubbleCreatedAt := gjson.Get(bubbleJSON, "createdAt").String(); bubbleCreatedAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, bubbleCreatedAt); err == nil {
				return t.UnixMilli()
			}
			if t, err := time.Parse("2006-01-02T15:04:05.999Z", bubbleCreatedAt); err == nil {
				return t.UnixMilli()
			}
		}
	}
	if bubbleID != "" {
		if ts, ok := uuidV7TimestampMillis(bubbleID); ok {
			return ts
		}
	}
	return 0
}

// extractBubbleMetadataJSON extracts capabilities, lints, files from raw bubble JSON without unmarshalling.
func extractBubbleMetadataJSON(sessionID, messageID string, bubbleJSON string) ([]MessageCapability, []MessageLint, []MessageFileRef, []MessageCodeblock) {
	var caps []MessageCapability
	var lints []MessageLint
	var files []MessageFileRef
	var codeblocks []MessageCodeblock

	// Capabilities from capabilitiesRan
	cr := gjson.Get(bubbleJSON, "capabilitiesRan")
	if cr.Exists() && cr.IsObject() {
		cr.ForEach(func(k, v gjson.Result) bool {
			phase := k.String()
			if v.IsArray() {
				for _, item := range v.Array() {
					if item.Type == gjson.Number {
						caps = append(caps, MessageCapability{
							MessageID: messageID, SessionID: sessionID, Phase: phase, Capability: int(item.Int()),
						})
					}
				}
			}
			return true
		})
	}

	// Lints from multiFileLinterErrors
	mfe := gjson.Get(bubbleJSON, "multiFileLinterErrors")
	if mfe.Exists() && mfe.IsArray() {
		for _, fileEntry := range mfe.Array() {
			filePath := fileEntry.Get("relativeWorkspacePath").String()
			errs := fileEntry.Get("errors")
			if !errs.Exists() || !errs.IsArray() {
				continue
			}
			for _, e := range errs.Array() {
				msgText := e.Get("message").String()
				src := e.Get("source").String()
				sl := int(e.Get("range.startPosition.line").Int())
				sc := int(e.Get("range.startPosition.column").Int())
				el := int(e.Get("range.endPosition.line").Int())
				ec := int(e.Get("range.endPosition.column").Int())
				lints = append(lints, MessageLint{
					MessageID: messageID, SessionID: sessionID,
					FilePath: filePath, Message: msgText, Source: src,
					StartLine: sl, StartCol: sc, EndLine: el, EndCol: ec,
				})
			}
		}
	}

	// Files from relevantFiles
	rf := gjson.Get(bubbleJSON, "relevantFiles")
	if rf.Exists() && rf.IsArray() {
		for _, v := range rf.Array() {
			p := v.String()
			if p != "" {
				files = append(files, MessageFileRef{
					MessageID: messageID, SessionID: sessionID, Kind: "relevant", FilePath: p,
				})
			}
		}
	}

	// Files from recentLocationsHistory
	rh := gjson.Get(bubbleJSON, "recentLocationsHistory")
	if rh.Exists() && rh.IsArray() {
		for _, v := range rh.Array() {
			p := v.Get("relativeWorkspacePath").String()
			if p != "" {
				ln := int(v.Get("lineNumber").Int())
				files = append(files, MessageFileRef{
					MessageID: messageID, SessionID: sessionID, Kind: "recent_location", FilePath: p, LineNumber: ln,
				})
			}
		}
	}

	// Suggested code blocks
	scb := gjson.Get(bubbleJSON, "suggestedCodeBlocks")
	if scb.Exists() && scb.IsArray() {
		for idx, v := range scb.Array() {
			codeblocks = append(codeblocks, MessageCodeblock{
				MessageID: messageID, SessionID: sessionID, Idx: idx, RawJSON: v.Raw,
			})
		}
	}

	return caps, lints, files, codeblocks
}

func extractProjectFromComposerJSON(raw string) string {
	// context.fileSelections[].uri.fsPath
	fs := gjson.Get(raw, "context.fileSelections")
	if fs.Exists() && fs.IsArray() {
		for _, item := range fs.Array() {
			p := item.Get("uri.fsPath").String()
			if p != "" {
				if proj := inferProjectFromPath(p); proj != "" {
					return proj
				}
			}
		}
	}

	// codeBlockData keys (file:// URIs)
	cbd := gjson.Get(raw, "codeBlockData")
	if cbd.Exists() && cbd.IsObject() {
		found := ""
		cbd.ForEach(func(k, v gjson.Result) bool {
			p := k.String()
			if p != "" && found == "" {
				if proj := inferProjectFromPath(p); proj != "" {
					found = proj
					return false
				}
			}
			return true
		})
		if found != "" {
			return found
		}
	}

	// originalFileStates keys
	ofs := gjson.Get(raw, "originalFileStates")
	if ofs.Exists() && ofs.IsObject() {
		found := ""
		ofs.ForEach(func(k, v gjson.Result) bool {
			p := k.String()
			if p != "" && found == "" {
				if proj := inferProjectFromPath(p); proj != "" {
					found = proj
					return false
				}
			}
			return true
		})
		if found != "" {
			return found
		}
	}

	return ""
}

func extractFilesFromComposerJSON(raw string) []string {
	seen := make(map[string]bool)
	var out []string

	fs := gjson.Get(raw, "context.fileSelections")
	if fs.Exists() && fs.IsArray() {
		for _, item := range fs.Array() {
			p := item.Get("uri.fsPath").String()
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}

	cbd := gjson.Get(raw, "codeBlockData")
	if cbd.Exists() && cbd.IsObject() {
		cbd.ForEach(func(k, v gjson.Result) bool {
			p := k.String()
			if strings.HasPrefix(p, "file://") {
				p = strings.TrimPrefix(p, "file://")
			}
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			return true
		})
	}

	return out
}

// extractWorkspaceFromPath tries to find the workspace root from a file path
// by looking for common project markers (go.mod, package.json, .git, etc.)
func extractWorkspaceFromPath(filePath string) string {
	if filePath == "" {
		return ""
	}
	// Walk up the path looking for project markers
	parts := strings.Split(filePath, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.Join(parts[:i+1], "/")
		// Check common patterns
		for _, marker := range []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py"} {
			if _, err := os.Stat(candidate + "/" + marker); err == nil {
				return candidate
			}
		}
		// Common workspace directory names
		if parts[i] == "nexus" || parts[i] == "projects" || parts[i] == "src" || parts[i] == "home" {
			return candidate
		}
	}
	// Fall back to first few path components
	if len(parts) > 4 {
		return strings.Join(parts[:4], "/")
	}
	return filePath
}

// extractBubbleMetadata extracts capabilities, lints, files from a bubble
func extractBubbleMetadata(sessionID, messageID string, bubble map[string]interface{}) ([]MessageCapability, []MessageLint, []MessageFileRef, []MessageCodeblock) {
	var caps []MessageCapability
	var lints []MessageLint
	var files []MessageFileRef
	var codeblocks []MessageCodeblock

	// Capabilities from capabilitiesRan or capabilityStatuses
	if cr, ok := bubble["capabilitiesRan"].(map[string]interface{}); ok {
		for phase, raw := range cr {
			if arr, ok := raw.([]interface{}); ok {
				for _, v := range arr {
					if n, ok := v.(float64); ok {
						caps = append(caps, MessageCapability{
							MessageID: messageID, SessionID: sessionID, Phase: phase, Capability: int(n),
						})
					}
				}
			}
		}
	}

	// Lints from multiFileLinterErrors
	if mfe, ok := bubble["multiFileLinterErrors"].([]interface{}); ok {
		for _, fileEntry := range mfe {
			if fm, ok := fileEntry.(map[string]interface{}); ok {
				filePath, _ := fm["relativeWorkspacePath"].(string)
				if errs, ok := fm["errors"].([]interface{}); ok {
					for _, e := range errs {
						if em, ok := e.(map[string]interface{}); ok {
							msgText, _ := em["message"].(string)
							src, _ := em["source"].(string)
							var sl, sc, el, ec int
							if r, ok := em["range"].(map[string]interface{}); ok {
								if sp, ok := r["startPosition"].(map[string]interface{}); ok {
									sl = int(getFloat(sp["line"]))
									sc = int(getFloat(sp["column"]))
								}
								if ep, ok := r["endPosition"].(map[string]interface{}); ok {
									el = int(getFloat(ep["line"]))
									ec = int(getFloat(ep["column"]))
								}
							}
							lints = append(lints, MessageLint{
								MessageID: messageID, SessionID: sessionID,
								FilePath: filePath, Message: msgText, Source: src,
								StartLine: sl, StartCol: sc, EndLine: el, EndCol: ec,
							})
						}
					}
				}
			}
		}
	}

	// Files from relevantFiles
	if rf, ok := bubble["relevantFiles"].([]interface{}); ok {
		for _, v := range rf {
			if p, ok := v.(string); ok && p != "" {
				files = append(files, MessageFileRef{
					MessageID: messageID, SessionID: sessionID, Kind: "relevant", FilePath: p,
				})
			}
		}
	}

	// Files from recentLocationsHistory
	if rh, ok := bubble["recentLocationsHistory"].([]interface{}); ok {
		for _, v := range rh {
			if mm, ok := v.(map[string]interface{}); ok {
				if p, _ := mm["relativeWorkspacePath"].(string); p != "" {
					ln := int(getFloat(mm["lineNumber"]))
					files = append(files, MessageFileRef{
						MessageID: messageID, SessionID: sessionID, Kind: "recent_location", FilePath: p, LineNumber: ln,
					})
				}
			}
		}
	}

	// Suggested code blocks
	if scb, ok := bubble["suggestedCodeBlocks"].([]interface{}); ok {
		for idx, v := range scb {
			if b, err := json.Marshal(v); err == nil {
				codeblocks = append(codeblocks, MessageCodeblock{
					MessageID: messageID, SessionID: sessionID, Idx: idx, RawJSON: string(b),
				})
			}
		}
	}

	return caps, lints, files, codeblocks
}

// extractProjectFromRaw extracts project name from raw session data
func extractProjectFromRaw(raw map[string]interface{}) string {
	// Try context.fileSelections
	if ctx, ok := raw["context"].(map[string]interface{}); ok {
		if fs, ok := ctx["fileSelections"].([]interface{}); ok {
			for _, item := range fs {
				if fm, ok := item.(map[string]interface{}); ok {
					if uri, ok := fm["uri"].(map[string]interface{}); ok {
						if fsPath, ok := uri["fsPath"].(string); ok && fsPath != "" {
							if proj := inferProjectFromPath(fsPath); proj != "" {
								return proj
							}
						}
					}
				}
			}
		}
	}

	// Try codeBlockData keys (which are file:// URIs)
	if cbd, ok := raw["codeBlockData"].(map[string]interface{}); ok {
		for path := range cbd {
			if proj := inferProjectFromPath(path); proj != "" {
				return proj
			}
		}
	}

	// Try originalFileStates
	if ofs, ok := raw["originalFileStates"].(map[string]interface{}); ok {
		for path := range ofs {
			if proj := inferProjectFromPath(path); proj != "" {
				return proj
			}
		}
	}

	return ""
}

// extractFilesFromRaw extracts file references from raw session data
func extractFilesFromRaw(raw map[string]interface{}) []string {
	seen := make(map[string]bool)
	var files []string

	// From context.fileSelections
	if ctx, ok := raw["context"].(map[string]interface{}); ok {
		if fs, ok := ctx["fileSelections"].([]interface{}); ok {
			for _, item := range fs {
				if fm, ok := item.(map[string]interface{}); ok {
					if uri, ok := fm["uri"].(map[string]interface{}); ok {
						if fsPath, ok := uri["fsPath"].(string); ok && fsPath != "" && !seen[fsPath] {
							seen[fsPath] = true
							files = append(files, fsPath)
						}
					}
				}
			}
		}
	}

	// From codeBlockData keys
	if cbd, ok := raw["codeBlockData"].(map[string]interface{}); ok {
		for path := range cbd {
			// Remove file:// prefix if present
			cleanPath := path
			if len(path) > 7 && path[:7] == "file://" {
				cleanPath = path[7:]
			}
			if cleanPath != "" && !seen[cleanPath] {
				seen[cleanPath] = true
				files = append(files, cleanPath)
			}
		}
	}

	return files
}
