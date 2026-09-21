// Package codex streams local Codex rollouts. It never executes transcript
// contents or reads configuration, credentials, or an external service.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Event is one rollout record. Payload is valid only for the duration of visit.
// Session follows session_meta.id, including a switch from parent to child
// metadata inside a file. session_id can identify a wider agent group instead.
type Event struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
	Session   string          `json:"-"`
	File      string          `json:"-"`
}

type Stats struct {
	Files, ParseErrors, InvalidTimes, UnscopedRecords, ExcludedFiles int
	Bytes                                                            int64
	UnknownTypes                                                     map[string]int
}

// Timestamp validates and normalizes RFC3339 to fixed-precision UTC. Unlike
// RFC3339Nano's variable fractions, this representation sorts chronologically.
func Timestamp(s string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", err
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
}

// Scan uses a Reader without a line-size cap. Memory is proportional to the
// largest record, not corpus size. Callers may retain identifiers and counts.
func Scan(dir string, visit func(Event) error) (Stats, error) {
	s := Stats{UnknownTypes: map[string]int{}}
	info, err := os.Stat(dir)
	if err != nil {
		return s, fmt.Errorf("codex corpus: %w", err)
	}
	if !info.IsDir() {
		return s, fmt.Errorf("codex --dir must be a rollout directory: %s", dir)
	}
	var candidates int
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
			return nil
		}
		candidates++
		return scanFile(path, &s, visit)
	})
	if err != nil {
		return s, err
	}
	if candidates == 0 {
		return s, fmt.Errorf("no JSONL rollouts found in codex corpus %s", dir)
	}
	if s.Files == 0 {
		return s, fmt.Errorf("no Codex session_meta with an id found in %s (%d JSONL files, %d malformed records); check --harness and --dir", dir, candidates, s.ParseErrors)
	}
	return s, nil
}

func scanFile(path string, stats *Stats, visit func(Event) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	session := ""
	found := false
	for {
		line, readErr := r.ReadBytes('\n')
		stats.Bytes += int64(len(line))
		if len(bytes.TrimSpace(line)) != 0 {
			var ev Event
			if json.Unmarshal(line, &ev) != nil || ev.Type == "" || len(ev.Payload) == 0 || ev.Payload[0] != '{' {
				stats.ParseErrors++
			} else {
				if ev.Type == "session_meta" {
					var meta struct {
						ID string `json:"id"`
					}
					if json.Unmarshal(ev.Payload, &meta) != nil || meta.ID == "" {
						stats.ParseErrors++
						// Never attribute a malformed session switch to the old session.
						session = ""
					} else {
						session, found = meta.ID, true
					}
				}
				if session == "" {
					stats.UnscopedRecords++
				} else {
					switch ev.Type {
					case "session_meta", "response_item", "event_msg", "turn_context", "compacted", "world_state", "inter_agent_communication_metadata", "token_usage_record":
					default:
						stats.UnknownTypes[ev.Type]++
					}
					if ev.Timestamp != "" {
						ev.Timestamp, err = Timestamp(ev.Timestamp)
						if err != nil {
							stats.InvalidTimes++
						}
					}
					ev.Session, ev.File = session, path
					if err := visit(ev); err != nil {
						return err
					}
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			break
		}
	}
	if found {
		stats.Files++
	} else {
		stats.ExcludedFiles++
	}
	return nil
}
