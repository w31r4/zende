package process

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

// Configuration is the execution-only projection of a Core publication. It
// carries no provider settings, credential references or environment values.
// A nil publication identifies standalone settings from before Core connected.
type Configuration struct {
	Publication  *protocol.ConfigPublication `json:"publication,omitempty"`
	Shell        string                      `json:"shell"`
	MaxProcesses int                         `json:"max_processes"`
	MaxOutput    int64                       `json:"max_output_bytes"`
}

func (c Configuration) Valid() bool {
	return filepath.IsAbs(c.Shell) && len(c.Shell) <= 4096 && utf8.ValidString(c.Shell) && !strings.ContainsRune(c.Shell, 0) && c.MaxProcesses > 0 && c.MaxOutput >= 1024 && (c.Publication == nil || c.Publication.Valid())
}

// Old process records did not capture configuration; never fill them using
// today's settings. New records require the snapshot saved before dispatch.
func (s State) ValidConfigurationMetadata() bool {
	return (s.SchemaVersion == 1 || s.SchemaVersion == 2) && (s.SchemaVersion != 2 || s.Configuration != nil) && (s.Configuration == nil || s.Configuration.Valid())
}

// CheckConfiguration checks local dependencies without launching the shell.
// Historical reads use Valid instead, so removing a shell never hides history.
func CheckConfiguration(c Configuration) error {
	if !c.Valid() {
		return protocol.E("invalid_config", "runner requires an absolute shell, positive process capacity and output limit of at least 1024 bytes")
	}
	st, err := os.Stat(c.Shell)
	if err != nil || !st.Mode().IsRegular() {
		return protocol.E("dependency_missing", "configured shell is not an accessible regular executable")
	}
	if _, err := exec.LookPath(c.Shell); err != nil {
		return protocol.E("dependency_missing", "configured shell is not executable")
	}
	return nil
}

func cloneConfiguration(c Configuration) Configuration {
	if c.Publication != nil {
		p := *c.Publication
		c.Publication = &p
	}
	return c
}

type configurationRecord struct {
	Version int `json:"version"`
	Configuration
}

type ConfigurationSource func(context.Context) (Configuration, error)

func runnerCoreOffline(err error) bool {
	var failure *protocol.Error
	return errors.As(err, &failure) && (failure.Data.Kind == "unavailable" || failure.Data.Kind == "transport_unknown") || errors.Is(err, context.DeadlineExceeded)
}

func (m *Manager) configurationLocked() Configuration {
	return cloneConfiguration(Configuration{Publication: m.configPublication, Shell: m.shell, MaxProcesses: m.maxProcesses, MaxOutput: m.maxOutput})
}

func (m *Manager) installConfigurationLocked(c Configuration) {
	c = cloneConfiguration(c)
	m.shell, m.maxProcesses, m.maxOutput = c.Shell, c.MaxProcesses, c.MaxOutput
	m.configPublication, m.configReady = c.Publication, true
}

// Only the latest projection is cached here. Immutable publication history
// belongs to Core; each process also retains its own captured configuration.
func (m *Manager) loadConfiguration() {
	path := filepath.Join(m.root, ".configuration.json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	invalid := func() {
		m.configFault, m.configReady = true, false
		m.configError = protocol.E("invalid_state", "saved runner configuration is unreadable or unsupported; process history is retained and new starts are disabled")
	}
	if err != nil {
		invalid()
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 32768 {
		invalid()
		return
	}
	raw, err := io.ReadAll(io.LimitReader(f, 32769))
	var record configurationRecord
	if err != nil || len(raw) > 32768 || protocol.StrictDecode(raw, &record) != nil || record.Version != 1 || !record.Configuration.Valid() {
		invalid()
		return
	}
	m.installConfigurationLocked(record.Configuration)
	m.configLoaded = true
}

// InitializeConfiguration is called once before Serve. Core owns subsequent
// changes; the local loader only bootstraps a runner that has never connected.
func (m *Manager) InitializeConfiguration(ctx context.Context, source ConfigurationSource, local func() (Configuration, error)) {
	m.configSource = source
	if m.configFault {
		return // Preserve damaged/future metadata; still allow process queries.
	}
	m.configReady = m.configLoaded
	if err := m.syncConfiguration(ctx); err == nil || m.configLoaded || ctx.Err() != nil || !runnerCoreOffline(err) {
		return
	}
	if local == nil {
		return
	}
	candidate, err := local()
	if err == nil {
		err = CheckConfiguration(candidate)
	}
	if err != nil {
		m.configError = protocol.E("invalid_config", "initial runner settings are unavailable; repair configuration and publish it through Core")
		return
	}
	if err := saveJSON(filepath.Join(m.root, ".configuration.json"), configurationRecord{Version: 1, Configuration: candidate}); err != nil {
		m.configReady = false
		m.configError = protocol.E("persistence_failed", "runner settings could not be saved; new starts are disabled")
		return
	}
	m.installConfigurationLocked(candidate)
	m.configLoaded = true
}

// Sync never calls Core while holding m.mu: process observation and control
// remain available if Core is offline. Serialization prevents late snapshots
// from replacing newer settings or errors from overwriting a later success.
func (m *Manager) syncConfiguration(ctx context.Context) error {
	if m.configSource == nil {
		return nil
	}
	m.configRefreshMu.Lock()
	defer m.configRefreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.configFault {
		err := m.configError
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	query, cancel := context.WithTimeout(ctx, 2*time.Second)
	candidate, err := m.configSource(query)
	cancel()
	candidate = cloneConfiguration(candidate)
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if runnerCoreOffline(err) {
			m.configError = protocol.E("unavailable", "runner cannot read Core's published settings; existing processes keep their configuration")
		} else {
			m.configError = protocol.E("invalid_state", "Core's runner configuration response is unavailable or incompatible; retained settings are unchanged")
		}
		return m.configError
	}
	if candidate.Publication == nil || !candidate.Valid() {
		m.configError = protocol.E("invalid_state", "Core returned invalid runner configuration metadata")
		return m.configError
	}
	current := m.configurationLocked()
	if current.Publication != nil {
		if candidate.Publication.Generation < current.Publication.Generation || candidate.Publication.Generation == current.Publication.Generation && (*candidate.Publication != *current.Publication || candidate.Shell != current.Shell || candidate.MaxProcesses != current.MaxProcesses || candidate.MaxOutput != current.MaxOutput) {
			m.configError = protocol.E("config_changed", "Core publication regressed or changed identity; retained runner settings are unchanged")
			return m.configError
		}
	}
	if err := CheckConfiguration(candidate); err != nil {
		m.configError = protocol.E("dependency_missing", "published runner settings are not ready; inspect the configured shell")
		return m.configError
	}
	if current.Publication != nil && *candidate.Publication == *current.Publication && m.configReady {
		m.configError = nil
		return nil
	}
	if err := saveJSON(filepath.Join(m.root, ".configuration.json"), configurationRecord{Version: 1, Configuration: candidate}); err != nil {
		// Rename may have happened even if directory sync failed. Disable new
		// starts until a subsequent synchronization durably installs a version.
		m.configReady = false
		m.configError = protocol.E("persistence_failed", "runner configuration persistence is uncertain; new starts wait for synchronization")
		return m.configError
	}
	m.installConfigurationLocked(candidate)
	m.configLoaded, m.configError = true, nil
	return nil
}

// FollowConfiguration observes publications while idle. Start also synchronizes
// before accepting a new identity, so the polling interval is not a launch race.
func (m *Manager) FollowConfiguration(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.syncConfiguration(ctx)
		}
	}
}

type RunnerStatus struct {
	Instance string `json:"instance_id"`
	PID      int    `json:"pid"`
	Configuration
	ConfigurationState string          `json:"configuration_state"`
	UpdateError        *protocol.Error `json:"update_error,omitempty"`
}

func (m *Manager) Status() RunnerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := RunnerStatus{Instance: m.instance, PID: os.Getpid(), Configuration: m.configurationLocked(), ConfigurationState: "current"}
	if m.configPublication == nil {
		s.ConfigurationState = "local"
	} else if m.configError != nil {
		s.ConfigurationState = "stale"
	}
	if !m.configReady {
		s.ConfigurationState = "unavailable"
	}
	if m.configFault {
		s.ConfigurationState = "invalid"
	}
	if m.configError != nil {
		failure := *m.configError
		s.UpdateError = &failure
	}
	return s
}
