package transcript

import (
	"encoding/json"
	"os"
)

// Persisted is an externalised tool result. Bash output past roughly 30 KB is
// written to a side file and only a short placeholder enters context, so the
// bytes the tool produced and the bytes it cost diverge — measured on the
// corpus at 2,107,309 produced against 85,786 in context across 39 records,
// a 24.6x reduction the harness performs for free.
type Persisted struct {
	// Path is the side file named by toolUseResult.persistedOutputPath.
	Path string
	// Size is toolUseResult.persistedOutputSize: the produced byte count.
	Size int64
}

// Persisted reads the externalisation record off ev.ToolUseResult, or returns
// nil when the event has none.
//
// Detection is by a non-empty persistedOutputPath — deliberately NOT by a null
// `content`. For these records `content` is never null; it is a string
// beginning "<persisted-output>\nOutput too large …", so keying on null finds
// zero of the 39 records on the corpus.
func (ev *Event) Persisted() *Persisted {
	if len(ev.ToolUseResult) == 0 {
		return nil
	}
	// toolUseResult is a map on most tools but a string or an array on some,
	// and those simply have no externalisation record.
	var r struct {
		Path string `json:"persistedOutputPath"`
		Size int64  `json:"persistedOutputSize"`
	}
	if json.Unmarshal(ev.ToolUseResult, &r) != nil || r.Path == "" {
		return nil
	}
	return &Persisted{Path: r.Path, Size: r.Size}
}

// OnDisk returns the side file's actual size.
//
// The measured baseline is 39 of 39 byte-exact, so a size that disagrees with
// Size is a real defect and the caller must report it. A file that is gone is
// only a warning: a transcript outlives its side file.
func (p Persisted) OnDisk() (int64, error) {
	fi, err := os.Stat(p.Path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
