package process

import "zende.local/harness/internal/protocol"

// Stop only an idle supervisor. Its capture goroutines own pipe output and
// cannot disappear while work is running or its exit result remains unknown.
func (m *Manager) PrepareShutdown() error {
	m.admission.Lock()
	defer m.admission.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.entries {
		entry.mu.Lock()
		terminal := !entry.quarantined && (entry.state.Status == "exited" || entry.state.Status == "failed")
		finished := false
		select {
		case <-entry.done:
			finished = true
		default:
		}
		entry.mu.Unlock()
		if !terminal || !finished {
			return protocol.E("runner_busy", "runner has active or unverified processes; inspect and explicitly finish them before stopping the supervisor")
		}
	}
	m.shutdown = true
	return nil
}

// A failed response did not commit shutdown. Reopen only this instance's local
// admission; durable Agent-tree stop fences remain independently effective.
func (m *Manager) CancelShutdown() {
	m.admission.Lock()
	defer m.admission.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdown = false
}
