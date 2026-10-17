package process

import (
	"io"
	"os"
	"path/filepath"

	"zende.local/harness/internal/protocol"
)

// A partially created or damaged record reserves its ID without preventing
// recovery of other processes. Preserve its files for inspection; never repair
// it by replacing the command, inventing an exit result, or dispatching again.
func recoverEntry(dir, id string) *entry {
	e := &entry{dir: dir, done: make(chan struct{})}
	quarantine := func(reason string) *entry {
		e.quarantined = true
		e.state = State{Spec: Spec{ID: id}, SchemaVersion: 1, Status: "unknown", Failure: reason + "; original files preserved, process_id reserved"}
		e.finish()
		return e
	}
	path := filepath.Join(dir, "state.json")
	st, err := os.Lstat(path)
	if err != nil {
		return quarantine("process metadata is missing")
	}
	if !st.Mode().IsRegular() || st.Size() > protocol.MaxFrameBytes {
		return quarantine("process metadata is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return quarantine("process metadata cannot be opened")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, protocol.MaxFrameBytes+1))
	if err != nil || len(data) > protocol.MaxFrameBytes || protocol.StrictDecode(data, &e.state) != nil {
		return quarantine("process metadata is unreadable")
	}
	s := e.state
	if s.SchemaVersion != 1 && s.SchemaVersion != 2 {
		return quarantine("process metadata version is unsupported")
	}
	if !s.ValidConfigurationMetadata() {
		return quarantine("process configuration metadata is invalid")
	}
	if s.ID != id || !validStreams(s) || !filepath.IsAbs(s.Cwd) || s.Command == "" {
		return quarantine("process identity or output metadata is invalid")
	}
	switch s.Status {
	case "starting", "running", "unknown", "lost", "exited", "failed":
	default:
		return quarantine("process status is invalid")
	}
	if s.PTY {
		if t := s.Terminal; t != nil && (t.Socket != terminalSocket(filepath.Dir(dir)) || t.Session != "z-"+id || t.Token == "") {
			return quarantine("terminal ownership metadata is invalid")
		}
		return e // Independent collector is refreshed when queried.
	}
	if s.Status == "starting" || s.Status == "running" || s.Status == "unknown" && s.ExitCode == nil {
		e.state.StdinOpen = false
		e.state.Status, e.state.Failure = "lost", "runner restarted; original pipe process and descriptors cannot be recovered"
		if s.Identity == "" || s.PID <= 0 || identity(s.PID) == s.Identity {
			e.state.Status, e.state.Failure = "unknown", "pipe dispatch or exit is uncertain; inspect effects before continuing"
		}
		for _, stream := range streamNames(s) {
			output := s.Output[stream]
			st, err := os.Lstat(filepath.Join(dir, stream))
			if os.IsNotExist(err) && output.Stored == 0 {
				continue // Intent may precede stream-file creation.
			}
			if err != nil || !st.Mode().IsRegular() || st.Size() < output.Stored {
				e.state.Failure += "; saved output is missing or incomplete"
				continue // Keep known byte ranges so Read reports the damage.
			}
			output.Stored = st.Size()
			output.Total = max(output.Total, output.Stored)
			e.state.Output[stream] = output
		}
		if err := e.save(); err != nil {
			e.state.Status = "unknown"
			e.state.Failure += "; recovery state could not be saved"
		}
	}
	e.finish()
	return e
}
