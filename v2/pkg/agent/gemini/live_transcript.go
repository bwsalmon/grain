package gemini

import (
	"os"
	"path/filepath"
	"strings"
)

// LiveTranscriptDir implements pkg/ui's LiveTranscript interface --
// structurally, not by importing it, the same way claude.LiveTranscriptDir
// does for the claude framework -- by reading back whatever Framework.Run
// has mirrored so far into Dir/<runID> (RunConfig.TranscriptPath's own doc
// comment). Unlike claude's own LiveTranscriptDir, there is no stream-json
// to parse here: gemini.Framework.Run tees the same already-human-readable
// narrative it builds Result.Transcript from, so Tail only needs to read
// the file back and trim it, not decode it. A deployment wires one of
// these into ui.Config.LiveTranscripts at the same Dir it hands
// orchestrator.Config as TranscriptDir, so the run ID a task's own Attempt
// names is the same file both sides agree on (bwsalmon/agents#467,
// bwsalmon/agents#513).
type LiveTranscriptDir struct {
	Dir string
}

// Tail reads Dir/<runID>'s current contents. ok is false when that file
// does not exist yet -- a run whose Framework has not opened
// RunConfig.TranscriptPath yet, or was never given one at all -- which a
// caller should read the same way a LiveTranscript-less deployment would:
// fall back to whatever the store has recorded once the run finishes.
func (d LiveTranscriptDir) Tail(runID string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(d.Dir, runID))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	text := strings.TrimSpace(string(data))
	return text, text != "", nil
}
