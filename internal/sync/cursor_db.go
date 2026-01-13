package sync

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

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
	dbPath        string
	exportPath    string            // if set, exports sessions to this path for durability
	knownHashes   map[string]string // existing session ID -> hash for incremental sync (legacy)
	sinceRowID    int64             // if >0, only load composerData rows with rowid > sinceRowID
	maxComposerID int64             // observed max rowid for composerData (for sync_state)
}

// NewCursorDBParser creates a parser that reads directly from Cursor's DB
func NewCursorDBParser(dbPath string) *CursorDBParser {
	if dbPath == "" {
		dbPath = DefaultCursorDBPath()
	}
	return &CursorDBParser{dbPath: dbPath}
}

// WithExport configures the parser to export sessions to the given path
func (p *CursorDBParser) WithExport(exportPath string) *CursorDBParser {
	p.exportPath = exportPath
	return p
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

// ComposerMaxRowID returns the max rowid observed for composerData entries during parsing.
func (p *CursorDBParser) ComposerMaxRowID() int64 {
	return p.maxComposerID
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

	// FAST PATH: rowid-based incremental sync (skip scanning all composerData values)
	// This avoids the expensive "read every value + hash" loop when nothing changed.
	if p.sinceRowID > 0 {
		rows, err := db.Query(
			`SELECT rowid, key, value FROM cursorDiskKV
			 WHERE key LIKE 'composerData:%' AND value IS NOT NULL AND rowid > ?`,
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

		if exportStats != nil {
			exportStats.ComposerFiles = composerCount
			exportStats.ComposerFilesSkipped = composerSkipped
			exportStats.BubbleFiles = bubbleCount
			exportStats.BubbleFilesSkipped = bubbleSkipped
		}

		return sessions, errors
	}

	// First pass: identify which sessions need processing (for incremental sync)
	type sessionEntry struct {
		key, value string
		skip       bool
	}
	var entriesToProcess []sessionEntry
	var changedSessionIDs = make(map[string]bool)

	rows1, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%' AND value IS NOT NULL`)
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

	return sessions, errors
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

	session := models.Session{
		ID:        composerID,
		Source:    "cursor",
		Project:   project,
		Model:     model,
		CreatedAt: createdAt,
	}

	var messages []models.Message
	meta := make(map[string]string)
	var caps []MessageCapability
	var lints []MessageLint
	var mfiles []MessageFileRef
	var codeblocks []MessageCodeblock

	if hasOldFormat && !hasNewFormat {
		conversation, _ := raw["conversation"].([]interface{})
		// OLD FORMAT: messages are inline in conversation array
		convMaps := make([]map[string]interface{}, 0, len(conversation))
		for _, item := range conversation {
			if m, ok := item.(map[string]interface{}); ok {
				convMaps = append(convMaps, m)
			}
		}
		messages, meta, caps, lints, mfiles, codeblocks = parseMessages(composerID, createdAt, convMaps)

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

			// Timestamp from UUIDv7
			if ts, ok := uuidV7TimestampMillis(bubbleID); ok {
				msg.Timestamp = ts
			} else if createdAt > 0 {
				msg.Timestamp = createdAt
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

				// Extract metadata, capabilities, lints, files from bubble (no json.Unmarshal)
				bubbleCaps, bubbleLints, bubbleFiles, bubbleCBs := extractBubbleMetadataJSON(composerID, bubbleID, bubbleValue)
				caps = append(caps, bubbleCaps...)
				lints = append(lints, bubbleLints...)
				mfiles = append(mfiles, bubbleFiles...)
				codeblocks = append(codeblocks, bubbleCBs...)

				// Store raw metadata (bubble JSON)
				meta[bubbleID] = bubbleValue
			}

			messages = append(messages, msg)
		}
	} else {
		// No conversation data - skip or create minimal session
		return nil, nil, nil
	}

	session.MessageCount = len(messages)

	return &ParsedSession{
		Session:         session,
		Messages:        messages,
		MessageMetadata: meta,
		Capabilities:    caps,
		Lints:           lints,
		MessageFiles:    mfiles,
		Codeblocks:      codeblocks,
		Files:           files,
		RawJSON:         value,
	}, bubbleJSONs, nil
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
