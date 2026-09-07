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
	// NonTranscriptFiles counts `.jsonl` files under the root holding no
	// transcript event at all — today, Workflow-tool journals. They are kept
	// out of Files, Lines, UnknownTypes and the event stream, and reported in
	// their own row rather than dropped in silence.
	NonTranscriptFiles int
	// NonTranscriptRecords counts the records inside those files.
	NonTranscriptRecords int
}

// Scan walks dir for `.jsonl` transcripts, streams each line, and calls visit
// once per decoded event. Events are not retained: the caller aggregates.
//
// Reading uses bufio.Reader.ReadString, not bufio.Scanner. The longest line
// measured on this corpus is 514,199 bytes; Scanner needs an explicit maximum
// token size and fails the whole file past it, so it would ship a knob to
// re-tune as the corpus grows. ReadString grows to whatever the line needs.
//
// The extension is not enough to call a file a transcript: Claude Code writes
// Workflow-tool journals under the same tree, so a file is classified by what
// its records are rather than by where it sits. scanFile does that, which is
// why the per-file counters are incremented there and not here.
func Scan(dir string, visit func(*Event)) (ScanStats, error) {
	stats := ScanStats{UnknownTypes: map[string]int{}}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
			return nil
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

// scanFile streams one file and classifies it as it goes.
//
// Bytes are counted for every line, transcript or not: the row they feed is
// labelled bytes on disk, and the whole root is on the disk. Lines, unknown
// types and the visit callback see transcript events only, so a journal record
// cannot inflate the event inventory or reach a command that would read it as
// a conversation.
//
// The file itself is counted after the loop, because what it is can only be
// known once its records have been read.
func scanFile(path string, stats *ScanStats, visit func(*Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sidechainFile := isSubagentPath(path)
	var transcriptLines, journalRecords int
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		stats.Bytes += int64(len(line))
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			ev := Event{File: path, InSubagentDir: sidechainFile, LineBytes: int64(len(trimmed))}
			switch {
			case json.Unmarshal([]byte(trimmed), &ev) != nil:
				// A line json refused is a defect in a transcript, so it stays
				// on the transcript side of the count rather than being
				// excused as a journal record it cannot be shown to be.
				transcriptLines++
				stats.Lines++
				stats.ParseErrors++
			case ev.IsWorkflowJournal():
				journalRecords++
			default:
				transcriptLines++
				stats.Lines++
				if !knownTypes[ev.Type] {
					stats.UnknownTypes[ev.Type]++
				}
				visit(&ev)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			break
		}
	}

	// Every excluded record is reported, whichever file it came from. A record
	// dropped from the event count and absent from this one would be a silent
	// loss, which is the failure mode this whole change exists to remove.
	stats.NonTranscriptRecords += journalRecords

	// The *file* is reclassified only when it holds journal records and no
	// transcript line at all. An empty file, or one whose every line failed to
	// decode, stays a transcript: it is a transcript that is empty or broken,
	// and moving it out of the count would hide that finding.
	if journalRecords > 0 && transcriptLines == 0 {
		stats.NonTranscriptFiles++
		return nil
	}
	stats.Files++
	if sidechainFile {
		stats.SubagentFiles++
	}
	return nil
}
