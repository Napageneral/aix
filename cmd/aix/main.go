package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Napageneral/aix/internal/db"
	"github.com/Napageneral/aix/internal/embeddings"
	"github.com/Napageneral/aix/internal/gemini"
	"github.com/Napageneral/aix/internal/sync"

	teengine "github.com/Napageneral/taskengine/engine"
	tequeue "github.com/Napageneral/taskengine/queue"
)

var (
	version    = "dev"
	commit     = "none"
	buildDate  = "unknown"
	jsonOutput bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "aix",
		Short: "AI session intelligence - search and analyze your AI conversation history",
	}

	rootCmd.PersistentFlags().BoolVarP(&jsonOutput, "json", "j", false, "Output as JSON")

	// version
	rootCmd.AddCommand(versionCmd())

	// init
	rootCmd.AddCommand(initCmd())

	// sync
	rootCmd.AddCommand(syncCmd())

	// db
	rootCmd.AddCommand(dbCmd())

	// sessions
	rootCmd.AddCommand(sessionsCmd())

	// show
	rootCmd.AddCommand(showCmd())

	// embed
	rootCmd.AddCommand(embedCmd())

	// compute (taskengine-powered)
	rootCmd.AddCommand(computeCmd())

	// search (placeholder)
	rootCmd.AddCommand(searchCmd())

	// stats
	rootCmd.AddCommand(statsCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version info",
		Run: func(cmd *cobra.Command, args []string) {
			if jsonOutput {
				printJSON(map[string]string{
					"version": version,
					"commit":  commit,
					"date":    buildDate,
				})
			} else {
				fmt.Printf("%s %s (%s, %s)\n", "aix", version, commit, buildDate)
			}
		},
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize config and database",
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := getConfigDir()
			dataDir := getDataDir()

			if err := os.MkdirAll(configDir, 0755); err != nil {
				return fmt.Errorf("failed to create config dir: %w", err)
			}
			if err := os.MkdirAll(dataDir, 0755); err != nil {
				return fmt.Errorf("failed to create data dir: %w", err)
			}

			// Create default config
			configPath := filepath.Join(configDir, "config.json")
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				config := map[string]interface{}{
					"data_dir":             dataDir,
					"default_session_path": sync.DefaultCursorPath(),
				}
				data, _ := json.MarshalIndent(config, "", "  ")
				os.WriteFile(configPath, data, 0644)
			}

			// Initialize database
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to initialize database: %w", err)
			}
			database.Close()

			if jsonOutput {
				printJSON(map[string]string{
					"config_dir": configDir,
					"data_dir":   dataDir,
					"database":   db.DefaultDBPath(),
					"status":     "initialized",
				})
			} else {
				fmt.Printf("✓ Config: %s\n", configDir)
				fmt.Printf("✓ Data: %s\n", dataDir)
				fmt.Printf("✓ Database: %s\n", db.DefaultDBPath())
				fmt.Println("\nRun 'aix sync --source cursor' to import sessions")
			}
			return nil
		},
	}
}

func syncCmd() *cobra.Command {
	var source string
	var noExport bool
	var exportPath string

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync sessions from source",
		Long: `Import AI sessions from various sources (cursor, claude, chatgpt).

For Cursor, reads directly from Cursor's database and exports raw JSON to
~/nexus/home/sessions/ for durability before importing into aix.db.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()

			// Default to cursor if not specified
			if source == "" {
				source = "cursor"
			}

			// Open database
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			var synced, newCount, updated, errors int
			var exportStats *sync.ExportStats

			switch source {
			case "cursor":
				var sessions []*sync.ParsedSession
				var errs []error

				// Always read directly from Cursor's database
				dbPath := sync.DefaultCursorDBPath()
				parser := sync.NewCursorDBParser(dbPath)

				// Export to nexus for durability (unless disabled)
				if !noExport {
					expPath := exportPath
					if expPath == "" {
						expPath = sync.DefaultNexusSessionsPath()
					}
					parser.WithExport(expPath)
					exportStats = &sync.ExportStats{}
				}

				sessions, errs = parser.ParseAllWithStats(exportStats)

				for _, ps := range sessions {
					isNew, err := database.UpsertSession(&ps.Session, ps.RawJSON)
					if err != nil {
						errors++
						if !jsonOutput {
							fmt.Fprintf(os.Stderr, "Error saving session %s: %v\n", ps.Session.ID, err)
						}
						continue
					}

					// Clear old messages/files and insert new ones
					if err := database.DeleteSessionMessages(ps.Session.ID); err != nil {
						errors++
						if !jsonOutput {
							fmt.Fprintf(os.Stderr, "Error deleting messages for session %s: %v\n", ps.Session.ID, err)
						}
						continue
					}
					if err := database.DeleteSessionFiles(ps.Session.ID); err != nil {
						errors++
						if !jsonOutput {
							fmt.Fprintf(os.Stderr, "Error deleting files for session %s: %v\n", ps.Session.ID, err)
						}
						continue
					}

					for _, msg := range ps.Messages {
						if err := database.InsertMessage(&msg); err != nil {
							errors++
							if !jsonOutput {
								fmt.Fprintf(os.Stderr, "Error inserting message %s: %v\n", msg.ID, err)
							}
						}
						if meta, ok := ps.MessageMetadata[msg.ID]; ok && meta != "" {
							if err := database.UpsertMessageMetadata(msg.ID, ps.Session.ID, meta); err != nil {
								errors++
								if !jsonOutput {
									fmt.Fprintf(os.Stderr, "Error inserting message metadata %s: %v\n", msg.ID, err)
								}
							}
						}
					}

					// Insert flattened capabilities and lints
					for _, c := range ps.Capabilities {
						if err := database.InsertMessageCapability(c.MessageID, c.SessionID, c.Phase, c.Capability); err != nil {
							errors++
							if !jsonOutput {
								fmt.Fprintf(os.Stderr, "Error inserting message capability %s: %v\n", c.MessageID, err)
							}
						}
					}
					for _, l := range ps.Lints {
						if err := database.InsertMessageLint(l.MessageID, l.SessionID, l.FilePath, l.Message, l.Source, l.StartLine, l.StartCol, l.EndLine, l.EndCol); err != nil {
							errors++
							if !jsonOutput {
								fmt.Fprintf(os.Stderr, "Error inserting message lint %s: %v\n", l.MessageID, err)
							}
						}
					}
					for _, f := range ps.MessageFiles {
						if err := database.InsertMessageFile(f.MessageID, f.SessionID, f.Kind, f.FilePath, f.LineNumber); err != nil {
							errors++
							if !jsonOutput {
								fmt.Fprintf(os.Stderr, "Error inserting message file %s: %v\n", f.MessageID, err)
							}
						}
					}
					for _, cb := range ps.Codeblocks {
						if err := database.InsertMessageCodeblock(cb.MessageID, cb.SessionID, cb.Idx, cb.RawJSON); err != nil {
							errors++
							if !jsonOutput {
								fmt.Fprintf(os.Stderr, "Error inserting message codeblock %s: %v\n", cb.MessageID, err)
							}
						}
					}

					for _, file := range ps.Files {
						if err := database.InsertFileReference(ps.Session.ID, file); err != nil {
							errors++
							if !jsonOutput {
								fmt.Fprintf(os.Stderr, "Error inserting file reference for session %s: %v\n", ps.Session.ID, err)
							}
						}
					}

					synced++
					if isNew {
						newCount++
					} else {
						updated++
					}
				}

				errors += len(errs)

			default:
				return fmt.Errorf("unsupported source: %s (supported: cursor)", source)
			}

			duration := time.Since(start)

			if jsonOutput {
				result := map[string]interface{}{
					"source":      source,
					"synced":      synced,
					"new":         newCount,
					"updated":     updated,
					"errors":      errors,
					"duration_ms": duration.Milliseconds(),
				}
				if exportStats != nil {
					result["exported"] = map[string]int{
						"composer_files": exportStats.ComposerFiles,
						"bubble_files":   exportStats.BubbleFiles,
					}
				}
				printJSON(result)
			} else {
				fmt.Printf("✓ Synced %d sessions from %s\n", synced, source)
				fmt.Printf("  New: %d, Updated: %d, Errors: %d\n", newCount, updated, errors)
				if exportStats != nil {
					expPath := exportPath
					if expPath == "" {
						expPath = sync.DefaultNexusSessionsPath()
					}
					fmt.Printf("  Exported: %d composer files, %d bubble files\n", exportStats.ComposerFiles, exportStats.BubbleFiles)
					fmt.Printf("  Export path: %s\n", expPath)
				}
				fmt.Printf("  Duration: %v\n", duration.Round(time.Millisecond))
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&source, "source", "s", "cursor", "Data source (cursor, claude, chatgpt)")
	cmd.Flags().BoolVar(&noExport, "no-export", false, "Skip exporting raw sessions to nexus (not recommended)")
	cmd.Flags().StringVar(&exportPath, "export-path", "", "Custom export path (default: ~/nexus/home/sessions)")

	return cmd
}

func dbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Database operations",
	}

	queryCmd := &cobra.Command{
		Use:   "query <sql>",
		Short: "Run raw SQL query (SELECT/WITH only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			result := database.Query(args[0])

			if jsonOutput {
				printJSON(result)
			} else {
				if !result.OK {
					return fmt.Errorf("%s", result.Error)
				}

				if len(result.Rows) == 0 {
					fmt.Println("No results")
					return nil
				}

				// Print as simple table
				printTable(result.Rows)
			}

			return nil
		},
	}

	cmd.AddCommand(queryCmd)
	return cmd
}

func sessionsCmd() *cobra.Command {
	var project string
	var today bool
	var week bool
	var limit int

	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			var since, until int64

			if today {
				now := time.Now()
				since = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixMilli()
			} else if week {
				since = time.Now().AddDate(0, 0, -7).UnixMilli()
			}

			sessions, err := database.ListSessions(project, since, until, limit)
			if err != nil {
				return fmt.Errorf("failed to list sessions: %w", err)
			}

			if jsonOutput {
				printJSON(sessions)
			} else {
				if len(sessions) == 0 {
					fmt.Println("No sessions found")
					return nil
				}

				for _, s := range sessions {
					ts := "unknown"
					if s.CreatedAt > 0 {
						ts = time.UnixMilli(s.CreatedAt).Format("2006-01-02 15:04")
					}
					proj := s.Project
					if proj == "" {
						proj = "-"
					}
					fmt.Printf("%s  %-20s  %3d msgs  %s\n",
						s.ID[:8], truncate(proj, 20), s.MessageCount, ts)
				}
				fmt.Printf("\n%d sessions\n", len(sessions))
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&project, "project", "p", "", "Filter by project")
	cmd.Flags().BoolVar(&today, "today", false, "Show only today's sessions")
	cmd.Flags().BoolVar(&week, "week", false, "Show sessions from last 7 days")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "Maximum sessions to show")

	return cmd
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <session-id>",
		Short: "Show session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			sessionID := args[0]

			// Try to match partial ID
			if len(sessionID) < 36 {
				resolved, err := database.ResolveSessionIDPrefix(sessionID)
				if err != nil {
					return fmt.Errorf("failed to resolve session id: %w", err)
				}
				if resolved != "" {
					sessionID = resolved
				}
			}

			session, err := database.GetSession(sessionID)
			if err != nil {
				return fmt.Errorf("failed to get session: %w", err)
			}
			if session == nil {
				return fmt.Errorf("session not found: %s", sessionID)
			}

			messages, err := database.GetSessionMessages(sessionID)
			if err != nil {
				return fmt.Errorf("failed to get messages: %w", err)
			}

			files, err := database.GetSessionFiles(sessionID)
			if err != nil {
				return fmt.Errorf("failed to get files: %w", err)
			}

			if jsonOutput {
				printJSON(map[string]interface{}{
					"session":  session,
					"messages": messages,
					"files":    files,
				})
			} else {
				// Header
				fmt.Printf("Session: %s\n", session.ID)
				fmt.Printf("Source: %s\n", session.Source)
				if session.Project != "" {
					fmt.Printf("Project: %s\n", session.Project)
				}
				if session.CreatedAt > 0 {
					fmt.Printf("Created: %s\n", time.UnixMilli(session.CreatedAt).Format(time.RFC3339))
				}
				fmt.Printf("Messages: %d\n", len(messages))

				// Files
				if len(files) > 0 {
					fmt.Printf("\nFiles referenced (%d):\n", len(files))
					for _, f := range files {
						fmt.Printf("  %s\n", f)
					}
				}

				// Messages
				if len(messages) > 0 {
					fmt.Printf("\n--- Conversation ---\n\n")
					for _, m := range messages {
						roleLabel := strings.ToUpper(m.Role)
						content := m.Content
						if len(content) > 500 {
							content = content[:500] + "..."
						}
						fmt.Printf("[%s]\n%s\n\n", roleLabel, content)
					}
				}
			}

			return nil
		},
	}
}

func searchCmd() *cobra.Command {
	var project string
	var limit int
	var model string

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Semantic search across messages using embeddings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("GEMINI_API_KEY is required for semantic search")
			}
			if model == "" {
				model = os.Getenv("AIX_EMBED_MODEL")
			}
			if model == "" {
				model = "gemini-embedding-1"
			}
			if limit <= 0 {
				limit = 10
			}

			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			// Embed the query
			client := gemini.NewClient(apiKey)
			resp, err := client.EmbedContent(cmd.Context(), &gemini.EmbedContentRequest{
				Model: model,
				Content: gemini.Content{
					Parts: []gemini.Part{{Text: query}},
				},
			})
			if err != nil {
				return fmt.Errorf("failed to generate query embedding: %w", err)
			}
			if resp == nil || resp.Embedding == nil || len(resp.Embedding.Values) == 0 {
				return fmt.Errorf("empty embedding response")
			}
			qvec := resp.Embedding.Values

			// Load embedded messages
			corpus, err := database.GetEmbeddedMessages(model, project)
			if err != nil {
				return fmt.Errorf("failed to load message embeddings: %w", err)
			}
			if len(corpus) == 0 {
				if jsonOutput {
					printJSON(map[string]any{
						"ok":      true,
						"query":   query,
						"count":   0,
						"results": []any{},
						"hint":    "no embeddings found; run `aix embed` first",
					})
					return nil
				}
				fmt.Println("No embeddings found. Run `aix embed` first.")
				return nil
			}

			type result struct {
				Score     float64 `json:"score"`
				SessionID string  `json:"session_id"`
				MessageID string  `json:"message_id"`
				Role      string  `json:"role"`
				Project   string  `json:"project,omitempty"`
				CreatedAt int64   `json:"created_at,omitempty"`
				Sequence  int     `json:"sequence"`
				Snippet   string  `json:"snippet"`
			}

			results := make([]result, 0, len(corpus))
			for _, m := range corpus {
				score := cosineSimilarity(qvec, m.Embedding)
				snippet := m.Content
				if len(snippet) > 240 {
					snippet = snippet[:240] + "..."
				}
				results = append(results, result{
					Score:     score,
					SessionID: m.SessionID,
					MessageID: m.MessageID,
					Role:      m.Role,
					Project:   m.Project,
					CreatedAt: m.CreatedAt,
					Sequence:  m.Sequence,
					Snippet:   snippet,
				})
			}

			// Sort by score desc (small n; simple sort)
			for i := 0; i < len(results)-1; i++ {
				for j := i + 1; j < len(results); j++ {
					if results[j].Score > results[i].Score {
						results[i], results[j] = results[j], results[i]
					}
				}
			}
			if len(results) > limit {
				results = results[:limit]
			}

			if jsonOutput {
				printJSON(map[string]any{
					"ok":      true,
					"query":   query,
					"model":   model,
					"count":   len(results),
					"results": results,
				})
				return nil
			}

			for _, r := range results {
				ts := ""
				if r.CreatedAt > 0 {
					ts = time.UnixMilli(r.CreatedAt).Format("2006-01-02 15:04")
				}
				proj := r.Project
				if proj == "" {
					proj = "-"
				}
				fmt.Printf("%.4f  %s  %s  %s  %s\n%s\n\n", r.Score, r.SessionID[:8], truncate(proj, 20), ts, strings.ToUpper(r.Role), r.Snippet)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&project, "project", "p", "", "Filter results to project")
	cmd.Flags().IntVarP(&limit, "limit", "n", 10, "Maximum number of results")
	cmd.Flags().StringVar(&model, "model", "", "Embedding model (default: AIX_EMBED_MODEL or gemini-embedding-1)")

	return cmd
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < len(a); i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func float64SliceToBlob(values []float64) ([]byte, error) {
	blob := make([]byte, len(values)*8)
	for i, v := range values {
		bits := math.Float64bits(v)
		binary.LittleEndian.PutUint64(blob[i*8:(i+1)*8], bits)
	}
	return blob, nil
}

func embedCmd() *cobra.Command {
	var model string
	var limit int

	cmd := &cobra.Command{
		Use:   "embed",
		Short: "Generate embeddings for messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("GEMINI_API_KEY is required for embedding")
			}
			if model == "" {
				model = os.Getenv("AIX_EMBED_MODEL")
			}
			if model == "" {
				model = "gemini-embedding-1"
			}

			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			// Ensure schema is up to date (embeddings table exists)
			// schema is embedded and applied on Open()

			msgs, err := database.GetMessagesMissingEmbeddings(model, limit)
			if err != nil {
				return fmt.Errorf("failed to load messages to embed: %w", err)
			}
			if len(msgs) == 0 {
				if jsonOutput {
					printJSON(map[string]any{
						"ok":     true,
						"status": "no_work",
						"model":  model,
						"count":  0,
					})
					return nil
				}
				fmt.Println("No messages missing embeddings.")
				return nil
			}

			client := gemini.NewClient(apiKey)
			batcher := embeddings.NewBatcher(client, model)
			defer batcher.Close()

			// Feed tasks
			for _, m := range msgs {
				batcher.Add(embeddings.Task{
					EntityType: "message",
					EntityID:   m.ID,
					Text:       m.Content,
				})
			}
			// Force a flush so we don't wait for timer.
			batcher.Flush()

			processed := 0
			succeeded := 0
			failed := 0

			timeout := time.NewTimer(10 * time.Minute)
			defer timeout.Stop()

			for processed < len(msgs) {
				select {
				case <-timeout.C:
					return fmt.Errorf("embedding timed out after processing %d/%d", processed, len(msgs))
				case res, ok := <-batcher.Results():
					if !ok {
						return fmt.Errorf("batcher stopped unexpectedly")
					}
					processed++
					if res.Error != nil {
						failed++
						if !jsonOutput && failed <= 3 {
							fmt.Fprintf(os.Stderr, "Embedding error: %v\n", res.Error)
						}
						continue
					}
					if err := database.UpsertEmbedding(res.Task.EntityType, res.Task.EntityID, model, res.Embedding); err != nil {
						failed++
						continue
					}
					succeeded++
				}
			}

			if jsonOutput {
				printJSON(map[string]any{
					"ok":        true,
					"model":     model,
					"requested": len(msgs),
					"succeeded": succeeded,
					"failed":    failed,
				})
				return nil
			}

			fmt.Printf("Embedded %d messages (failed: %d) using %s\n", succeeded, failed, model)
			return nil
		},
	}

	cmd.Flags().StringVar(&model, "model", "", "Embedding model (default: AIX_EMBED_MODEL or gemini-embedding-1)")
	cmd.Flags().IntVar(&limit, "limit", 5000, "Maximum number of messages to embed")

	return cmd
}

func computeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compute",
		Short: "Durable compute engine (taskengine)",
	}

	cmd.AddCommand(computeEmbedCmd())
	cmd.AddCommand(computeStatusCmd())

	return cmd
}

func computeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show compute queue status",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			if err := tequeue.Init(database.Conn()); err != nil {
				return fmt.Errorf("failed to init taskengine schema: %w", err)
			}

			q := tequeue.New(database.Conn())
			stats, err := q.GetStats()
			if err != nil {
				return fmt.Errorf("failed to get queue stats: %w", err)
			}

			if jsonOutput {
				printJSON(stats)
				return nil
			}
			fmt.Printf("pending=%d leased=%d succeeded=%d failed=%d dead=%d total=%d\n",
				stats.Pending, stats.Leased, stats.Succeeded, stats.Failed, stats.Dead, stats.Total)
			return nil
		},
	}
}

func computeEmbedCmd() *cobra.Command {
	var model string
	var limit int
	var batchSize int
	var workers int

	cmd := &cobra.Command{
		Use:   "embed",
		Short: "Embed messages using taskengine queue + workers",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				return fmt.Errorf("GEMINI_API_KEY is required for embedding")
			}
			if model == "" {
				model = os.Getenv("AIX_EMBED_MODEL")
			}
			if model == "" {
				model = "gemini-embedding-1"
			}
			if batchSize <= 0 {
				batchSize = 50
			}
			if workers <= 0 {
				workers = 20
			}
			if limit <= 0 {
				limit = 5000
			}

			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			// Ensure taskengine schema exists in the aix.db file.
			if err := tequeue.Init(database.Conn()); err != nil {
				return fmt.Errorf("failed to init taskengine schema: %w", err)
			}

			// Find messages missing embeddings and enqueue in batches.
			msgs, err := database.GetMessagesMissingEmbeddings(model, limit)
			if err != nil {
				return fmt.Errorf("failed to load messages to embed: %w", err)
			}
			if len(msgs) == 0 {
				if jsonOutput {
					printJSON(map[string]any{"ok": true, "status": "no_work", "model": model, "count": 0})
					return nil
				}
				fmt.Println("No messages missing embeddings.")
				return nil
			}

			q := tequeue.New(database.Conn())

			// Chunk message ids
			var batches [][]string
			for i := 0; i < len(msgs); i += batchSize {
				end := i + batchSize
				if end > len(msgs) {
					end = len(msgs)
				}
				ids := make([]string, 0, end-i)
				for _, m := range msgs[i:end] {
					ids = append(ids, m.ID)
				}
				batches = append(batches, ids)
			}

			type payload struct {
				Model      string   `json:"model"`
				MessageIDs []string `json:"message_ids"`
			}

			jobType := "aix.embed_messages_v1"
			enqueued := 0
			for _, ids := range batches {
				h := sha1.Sum([]byte(model + ":" + strings.Join(ids, ",")))
				key := "embed:" + hex.EncodeToString(h[:])[:12]
				if err := q.Enqueue(tequeue.EnqueueOptions{
					Type:     jobType,
					Key:      key,
					Payload:  payload{Model: model, MessageIDs: ids},
					Priority: 0,
				}); err != nil {
					return fmt.Errorf("failed to enqueue job: %w", err)
				}
				enqueued++
			}

			// Shared embedding client and DB writer.
			embedClient := gemini.NewClient(apiKey)
			writer := teengine.NewTxBatchWriter(database.Conn(), teengine.TxBatchWriterConfig{
				BatchSize:     50,
				FlushInterval: 100 * time.Millisecond,
			})
			writer.Start()
			defer writer.Close()

			engCfg := teengine.DefaultConfig()
			engCfg.WorkerCount = workers
			engCfg.LeaseOwner = "aix"
			engCfg.LeaseTTL = 10 * time.Minute
			eng := teengine.New(q, engCfg)

			eng.RegisterHandler(jobType, func(ctx context.Context, job *tequeue.Job) error {
				var p payload
				if err := json.Unmarshal([]byte(job.PayloadJSON), &p); err != nil {
					return fmt.Errorf("invalid payload: %w", err)
				}
				if p.Model == "" {
					p.Model = model
				}
				if len(p.MessageIDs) == 0 {
					return fmt.Errorf("empty message_ids")
				}

				// Load message texts, preserving input order.
				byID := make(map[string]string, len(p.MessageIDs))
				{
					// Build IN clause
					var b strings.Builder
					args := make([]interface{}, 0, len(p.MessageIDs))
					b.WriteString("SELECT id, content FROM messages WHERE id IN (")
					for i, id := range p.MessageIDs {
						if i > 0 {
							b.WriteString(",")
						}
						b.WriteString("?")
						args = append(args, id)
					}
					b.WriteString(")")

					rows, err := database.Conn().QueryContext(ctx, b.String(), args...)
					if err != nil {
						return fmt.Errorf("failed to load messages: %w", err)
					}
					for rows.Next() {
						var id string
						var content sql.NullString
						if err := rows.Scan(&id, &content); err != nil {
							rows.Close()
							return err
						}
						byID[id] = content.String
					}
					rows.Close()
				}

				reqs := make([]gemini.EmbedContentRequest, 0, len(p.MessageIDs))
				orderedIDs := make([]string, 0, len(p.MessageIDs))
				for _, id := range p.MessageIDs {
					txt := strings.TrimSpace(byID[id])
					if txt == "" {
						continue
					}
					orderedIDs = append(orderedIDs, id)
					reqs = append(reqs, gemini.EmbedContentRequest{
						Model: p.Model,
						Content: gemini.Content{
							Parts: []gemini.Part{{Text: txt}},
						},
					})
				}
				if len(reqs) == 0 {
					return nil
				}

				resp, err := embedClient.BatchEmbedContents(ctx, p.Model, reqs)
				if err != nil {
					return fmt.Errorf("batch embed failed: %w", err)
				}
				if resp == nil || len(resp.Embeddings) != len(reqs) {
					return fmt.Errorf("unexpected embeddings response size")
				}

				// Persist embeddings in one microbatched tx.
				return writer.Submit(ctx, func(tx *sql.Tx) error {
					for i, id := range orderedIDs {
						emb := resp.Embeddings[i].Values
						if len(emb) == 0 {
							continue
						}
						blob, err := func(values []float64) ([]byte, error) {
							// reuse db helper via UpsertEmbedding on db, but we need tx.
							// Inline the same encoding for now.
							return float64SliceToBlob(values)
						}(emb)
						if err != nil {
							return err
						}
						_, err = tx.Exec(`
							INSERT INTO embeddings (entity_type, entity_id, model, embedding_blob, dimension, created_at)
							VALUES ('message', ?, ?, ?, ?, ?)
							ON CONFLICT(entity_type, entity_id, model) DO UPDATE SET
								embedding_blob = excluded.embedding_blob,
								dimension = excluded.dimension,
								created_at = excluded.created_at
						`, id, p.Model, blob, len(emb), time.Now().UnixMilli())
						if err != nil {
							return err
						}
					}
					return nil
				})
			})

			stats, err := eng.Run(cmd.Context())
			if err != nil {
				return err
			}

			if jsonOutput {
				printJSON(map[string]any{
					"ok":       true,
					"model":    model,
					"enqueued": enqueued,
					"stats":    stats,
				})
				return nil
			}
			fmt.Printf("enqueued=%d succeeded=%d failed=%d skipped=%d\n", enqueued, stats.Succeeded, stats.Failed, stats.Skipped)
			return nil
		},
	}

	cmd.Flags().StringVar(&model, "model", "", "Embedding model (default: AIX_EMBED_MODEL or gemini-embedding-1)")
	cmd.Flags().IntVar(&limit, "limit", 5000, "Max messages to enqueue")
	cmd.Flags().IntVar(&batchSize, "batch-size", 50, "Messages per embedding job")
	cmd.Flags().IntVar(&workers, "workers", 20, "Concurrent workers")

	return cmd
}

func statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show database statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := db.Open()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer database.Close()

			stats, err := database.Stats()
			if err != nil {
				return fmt.Errorf("failed to get stats: %w", err)
			}

			if jsonOutput {
				printJSON(stats)
			} else {
				fmt.Printf("Sessions: %v\n", stats["sessions"])
				fmt.Printf("Messages: %v\n", stats["messages"])
				fmt.Printf("Files referenced: %v\n", stats["files_referenced"])
				fmt.Printf("Projects: %v\n", stats["projects"])
				fmt.Printf("Database: %v\n", stats["db_path"])
			}

			return nil
		},
	}
}

func getConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "aix")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "aix")
}

func getDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "aix")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "aix")
	}
	return filepath.Join(home, ".local", "share", "aix")
}

func printJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func printTable(rows []map[string]interface{}) {
	if len(rows) == 0 {
		return
	}

	// Get columns from first row
	var columns []string
	for k := range rows[0] {
		columns = append(columns, k)
	}

	// Print header
	for i, col := range columns {
		if i > 0 {
			fmt.Print("\t")
		}
		fmt.Print(col)
	}
	fmt.Println()

	// Print rows
	for _, row := range rows {
		for i, col := range columns {
			if i > 0 {
				fmt.Print("\t")
			}
			v := row[col]
			if s, ok := v.(string); ok && len(s) > 50 {
				v = s[:50] + "..."
			}
			fmt.Print(v)
		}
		fmt.Println()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
