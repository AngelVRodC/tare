package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScanStats is what a walk of the corpus saw. Counts only; no line content is
// retained.
type ScanStats struct {
	Files         int
	SubagentFiles int
	Bytes         int64
	Lines         int
	// UnknownTypes counts events whose top-level `type` is outside knownTypes.
	// An unknown type is a counter, never fatal.
	UnknownTypes map[string]int
	// ParseErrors counts lines encoding/json refused. Measured baseline: 0.
	ParseErrors int
}

// Scan walks dir for `.jsonl` transcripts, streams each line, and calls visit
// once per decoded event. Events are not retained: the caller aggregates.
//
// Reading uses bufio.Reader.ReadString, not bufio.Scanner. The longest line
// measured on this corpus is 514,199 bytes; Scanner needs an explicit maximum
// token size and fails the whole file past it, so it would ship a knob to
// re-tune as the corpus grows. ReadString grows to whatever the line needs.
func Scan(dir string, visit func(*Event)) (ScanStats, error) {
	stats := ScanStats{UnknownTypes: map[string]int{}}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
			return nil
		}
		stats.Files++
		if isSubagentPath(path) {
			stats.SubagentFiles++
		}
		return scanFile(path, &stats, visit)
	})
	return stats, err
}

// isSubagentPath reports whether path sits under a `subagents/` directory.
func isSubagentPath(path string) bool {
	sep := string(filepath.Separator)
	return strings.Contains(path, sep+"subagents"+sep)
}

func scanFile(path string, stats *ScanStats, visit func(*Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sidechainFile := isSubagentPath(path)
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		stats.Bytes += int64(len(line))
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			stats.Lines++
			ev := Event{File: path, InSubagentDir: sidechainFile, LineBytes: int64(len(trimmed))}
			if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
				stats.ParseErrors++
			} else {
				if !knownTypes[ev.Type] {
					stats.UnknownTypes[ev.Type]++
				}
				visit(&ev)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}
