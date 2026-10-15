package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestZombieWaitStatusRequiresOriginalIdentity(t *testing.T) {
	fields := strings.Fields(strings.Repeat("0 ", 50))
	fields[0], fields[1], fields[19] = "Z", "20", "1234"
	for _, raw := range []int{0, 7 << 8, int(syscall.SIGKILL), int(syscall.SIGSEGV) | 128} {
		fields[49] = strconv.Itoa(raw)
		stat := []byte("30 (name with ) and spaces) " + strings.Join(fields, " "))
		code, signal, ok := zombieExit(stat, 30, 20, "boot:1234", "boot")
		if !ok {
			t.Fatal(raw, "valid wait status rejected")
		}
		if raw&127 == 0 {
			if code == nil || *code != raw>>8 || signal != "" {
				t.Fatal(code, signal)
			}
		} else if code != nil || signal != syscall.Signal(raw&127).String() {
			t.Fatal(code, signal)
		}
	}
	for _, scenario := range []string{"pid", "parent", "start", "boot", "identity", "running", "short", "status", "stopped", "continued"} {
		t.Run(scenario, func(t *testing.T) {
			values := append([]string(nil), fields...)
			values[49] = "9"
			pid, parent, expected, boot := 30, 20, "boot:1234", "boot"
			switch scenario {
			case "pid":
				pid++
			case "parent":
				parent++
			case "start":
				expected = "boot:1235"
			case "boot":
				boot = "new-boot"
			case "identity":
				expected = ""
			case "running":
				values[0] = "R"
			case "short":
				values = values[:49]
			case "status":
				values[49] = "invalid"
			case "stopped":
				values[49] = "127"
			case "continued":
				values[49] = "65535"
			}
			if _, _, ok := zombieExit([]byte("30 (fixture) "+strings.Join(values, " ")), pid, parent, expected, boot); ok {
				t.Fatal("unverified exit accepted")
			}
		})
	}
}

func TestRecoverPaneExitFromOwnedZombie(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 7")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Wait() // Keep the kernel exit record until after verification.
	pid := command.Process.Pid
	dir := t.TempDir()
	l := launch{Spec: Spec{ID: "zombie", Command: "exit 7", Cwd: dir}, Terminal: Terminal{Pane: "%fixture", Session: "fixture", Socket: "fixture", Token: "original", ServerPID: os.Getpid(), ServerIdentity: identity(os.Getpid())}}
	saved := State{Spec: l.Spec, SchemaVersion: 1, PID: pid, Identity: identity(pid), Terminal: &l.Terminal}
	if err := saveJSON(filepath.Join(dir, "state.json"), saved); err != nil {
		t.Fatal(err)
	}
	pane := paneState{PID: pid, Dead: true}
	var recovered paneState
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		recovered = recoverPaneExit(dir, l, pane)
		if recovered.ExitCode != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if recovered.ExitCode == nil || *recovered.ExitCode != 7 || recovered.ExitSignal != "" {
		t.Fatal(recovered)
	}
	for _, scenario := range []string{"identity", "parent", "pane", "token", "spec"} {
		t.Run(scenario, func(t *testing.T) {
			copy := snapshot(saved)
			switch scenario {
			case "identity":
				copy.Identity = "different"
			case "parent":
				copy.Terminal.ServerPID++
			case "pane":
				copy.PID++
			case "token":
				copy.Terminal.Token = "different"
			case "spec":
				copy.Command = "different"
			}
			if err := saveJSON(filepath.Join(dir, "state.json"), copy); err != nil {
				t.Fatal(err)
			}
			if got := recoverPaneExit(dir, l, pane); got.ExitCode != nil || got.ExitSignal != "" {
				t.Fatal(got)
			}
		})
	}
}
