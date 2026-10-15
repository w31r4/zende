package process

import "zende.local/harness/internal/protocol"

// Group is kernel ownership evidence for a non-runner subprocess (currently
// Git checkout). Persist the original identity after Start and new witnesses
// before KillGroup. It does not supply an exit status or output receipt.
type Group struct {
	PID       int           `json:"pid"`
	Identity  string        `json:"identity,omitempty"`
	Witnesses []GroupMember `json:"witnesses,omitempty"`
}

type GroupMember struct {
	PID      int    `json:"pid"`
	Identity string `json:"identity"`
}

// CaptureGroup always returns the PID, including when the process already
// exited. Missing identity permits absence checks, never blind PID signaling.
func CaptureGroup(pid int) (Group, error) {
	g := Group{PID: pid}
	boot, err := kernelBoot()
	if err != nil {
		return g, err
	}
	p, err := kernelMember(pid, boot)
	if err != nil {
		return g, err
	}
	if pid <= 1 || p.group != pid {
		return g, protocol.E("process_unavailable", "subprocess is not an independent process group")
	}
	g.Identity = p.Identity
	return g, nil
}

// ObserveGroup performs no writes or signals. Save the returned witnesses
// before termination so another core can finish after the leader exits.
func ObserveGroup(g Group) (Group, bool, error) {
	witnesses, stopped, err := stopEvidence(StopProcess{PID: g.PID, Identity: g.Identity, Witnesses: g.Witnesses})
	if err == nil {
		g.Witnesses = witnesses
	}
	return g, stopped, err
}

// KillGroup rechecks each saved lifetime through a pidfd. It never falls back
// to signaling a bare numeric PID/PGID. Call ObserveGroup again for completion.
func KillGroup(g Group) error {
	return killStopMembers(StopProcess{PID: g.PID, Identity: g.Identity, Witnesses: g.Witnesses})
}
