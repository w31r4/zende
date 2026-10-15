// Package resource owns configuration and versioned, hot-loadable resources.
package resource

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Paths struct {
	Profile      string `json:"profile"`
	Config       string `json:"config"`
	State        string `json:"state"`
	Runtime      string `json:"runtime"`
	Socket       string `json:"socket"`
	RunnerSocket string `json:"runner_socket"`
	Database     string `json:"database"`
}

// ResolvePaths is pure: inspection does not create directories or configuration.
func ResolvePaths(profile, home string, env func(string) string) (Paths, error) {
	if !namePattern.MatchString(profile) {
		return Paths{}, fmt.Errorf("invalid profile name")
	}
	if !filepath.IsAbs(home) {
		return Paths{}, fmt.Errorf("home must be absolute")
	}
	xdg := func(key, fallback string) string {
		if value := env(key); filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return fallback
	}
	config := filepath.Join(xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "zende", profile)
	state := filepath.Join(xdg("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "zende", profile)
	// Include the state root so isolated XDG profiles cannot connect to a user's core.
	hash := sha256.Sum256([]byte(state))
	run := filepath.Join(xdg("XDG_RUNTIME_DIR", filepath.Join(os.TempDir(), fmt.Sprintf("zende-%d", os.Getuid()))), "zende", fmt.Sprintf("%x", hash[:12]))
	if len(filepath.Join(run, "runner.sock")) > 103 {
		return Paths{}, fmt.Errorf("XDG runtime path exceeds Unix socket length limit")
	}
	return Paths{Profile: profile, Config: config, State: state, Runtime: run,
		Socket: filepath.Join(run, "core.sock"), RunnerSocket: filepath.Join(run, "runner.sock"), Database: filepath.Join(state, "state.db")}, nil
}

func (p Paths) EnsureState() error {
	for _, dir := range []string{p.State, p.Runtime} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}
