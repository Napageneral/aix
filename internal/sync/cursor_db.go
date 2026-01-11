package sync

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/Napageneral/aix/internal/models"
)

// DefaultNexusSessionsPath returns the path where sessions are exported for durability
func DefaultNexusSessionsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "nexus", "home", "sessions")
}

// CursorDBParser parses sessions directly from Cursor's SQLite database
type CursorDBParser struct {
	dbPath     string
	exportPath string // if set, exports sessions to this path for durability
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

// ExportStats holds export statistics
type ExportStats struct {
	ComposerFiles int
	BubbleFiles   int
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

	// Get all composerData entries
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%' AND value IS NOT NULL`)
	if err != nil {
		return nil, []error{fmt.Errorf("failed to query sessions: %w", err)}
	}
	defer rows.Close()

	composerCount := 0
	bubbleCount := 0

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			errors = append(errors, err)
			continue
		}

		// Export composerData if exporting
		if composerDir != "" {
			composerID := strings.TrimPrefix(key, "composerData:")
			exportFile := filepath.Join(composerDir, composerID+".json")
			if err := os.WriteFile(exportFile, []byte(value), 0644); err != nil {
				errors = append(errors, fmt.Errorf("failed to export %s: %w", key, err))
			} else {
				composerCount++
			}
		}

		session, bubbles, err := p.parseSessionWithBubbles(db, key, value)
		if err != nil {
			errors = append(errors, fmt.Errorf("%s: %w", key, err))
			continue
		}
		if session != nil {
			sessions = append(sessions, session)

			// Export bubbles if exporting
			if bubblesDir != "" && len(bubbles) > 0 {
				sessionBubbleDir := filepath.Join(bubblesDir, session.Session.ID)
				if err := os.MkdirAll(sessionBubbleDir, 0755); err != nil {
					errors = append(errors, fmt.Errorf("failed to create bubble dir for %s: %w", session.Session.ID, err))
				} else {
					for bubbleID, bubbleJSON := range bubbles {
						bubbleFile := filepath.Join(sessionBubbleDir, bubbleID+".json")
						if err := os.WriteFile(bubbleFile, []byte(bubbleJSON), 0644); err != nil {
							errors = append(errors, fmt.Errorf("failed to export bubble %s: %w", bubbleID, err))
						} else {
							bubbleCount++
						}
					}
				}
			}
		}
	}

	if exportStats != nil {
		exportStats.ComposerFiles = composerCount
		exportStats.BubbleFiles = bubbleCount
	}

	return sessions, errors
}

// parseSession parses a single session, handling both old and new formats
func (p *CursorDBParser) parseSession(db *sql.DB, key, value string) (*ParsedSession, error) {
	session, _, err := p.parseSessionWithBubbles(db, key, value)
	return session, err
}

// parseSessionWithBubbles parses a session and returns bubble JSON for export
func (p *CursorDBParser) parseSessionWithBubbles(db *sql.DB, key, value string) (*ParsedSession, map[string]string, error) {
	bubbleJSONs := make(map[string]string) // bubbleID -> raw JSON
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}

	composerID, _ := raw["composerId"].(string)
	if composerID == "" {
		return nil, nil, nil // Skip invalid entries
	}

	createdAt := int64(0)
	if v, ok := raw["createdAt"].(float64); ok {
		createdAt = int64(v)
	}
	if createdAt == 0 {
		if ts, ok := uuidV7TimestampMillis(composerID); ok {
			createdAt = ts
		}
	}

	// Check which format this session uses
	conversation, hasOldFormat := raw["conversation"].([]interface{})
	headers, hasNewFormat := raw["fullConversationHeadersOnly"].([]interface{})

	// Extract project
	project := extractProjectFromRaw(raw)

	// Extract model from modelConfig
	model := ""
	if mc, ok := raw["modelConfig"].(map[string]interface{}); ok {
		if mn, ok := mc["modelName"].(string); ok {
			model = mn
		}
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

	if hasOldFormat && len(conversation) > 0 {
		// OLD FORMAT: messages are inline in conversation array
		convMaps := make([]map[string]interface{}, 0, len(conversation))
		for _, item := range conversation {
			if m, ok := item.(map[string]interface{}); ok {
				convMaps = append(convMaps, m)
			}
		}
		messages, meta, caps, lints, mfiles, codeblocks = parseMessages(composerID, createdAt, convMaps)

	} else if hasNewFormat && len(headers) > 0 {
		// NEW FORMAT: headers only in composerData, content in bubbleId:* keys
		for i, h := range headers {
			header, ok := h.(map[string]interface{})
			if !ok {
				continue
			}

			bubbleID, _ := header["bubbleId"].(string)
			if bubbleID == "" {
				continue
			}

			msgType := int(0)
			if v, ok := header["type"].(float64); ok {
				msgType = int(v)
			}

			// Fetch bubble content from separate key
			bubbleKey := fmt.Sprintf("bubbleId:%s:%s", composerID, bubbleID)
			var bubbleValue sql.NullString
			err := db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, bubbleKey).Scan(&bubbleValue)

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
			if err == nil && bubbleValue.Valid && bubbleValue.String != "" {
				// Store raw bubble JSON for export
				bubbleJSONs[bubbleID] = bubbleValue.String

				var bubble map[string]interface{}
				if json.Unmarshal([]byte(bubbleValue.String), &bubble) == nil {
					// Extract text
					if text, ok := bubble["text"].(string); ok && text != "" {
						msg.Content = text
					} else if rawText, ok := bubble["rawText"].(string); ok && rawText != "" {
						msg.Content = rawText
					}

					// Extract metadata, capabilities, lints, files from bubble
					bubbleCaps, bubbleLints, bubbleFiles, bubbleCBs := extractBubbleMetadata(composerID, bubbleID, bubble)
					caps = append(caps, bubbleCaps...)
					lints = append(lints, bubbleLints...)
					mfiles = append(mfiles, bubbleFiles...)
					codeblocks = append(codeblocks, bubbleCBs...)

					// Store raw metadata
					if b, err := json.Marshal(bubble); err == nil {
						meta[bubbleID] = string(b)
					}
				}
			}

			messages = append(messages, msg)
		}
	} else {
		// No conversation data - skip or create minimal session
		return nil, nil, nil
	}

	session.MessageCount = len(messages)

	// Collect files from session-level context
	files := extractFilesFromRaw(raw)

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
