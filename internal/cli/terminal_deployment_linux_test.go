package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"
)

type binaryTUI struct {
	master, slave *os.File
	before        *unix.Termios
	mu            sync.Mutex
	output        bytes.Buffer
	done          chan error
	stopped       bool
}

func (ui *binaryTUI) Write(p []byte) (int, error) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.output.Len() < 8<<20 {
		_, _ = ui.output.Write(p)
	}
	return len(p), nil
}

func (ui *binaryTUI) waitText(t *testing.T, text string) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		ui.mu.Lock()
		found := strings.Contains(ansi.Strip(ui.output.String()), text)
		ui.mu.Unlock()
		if found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("binary TUI did not display %q", text)
}

func (ui *binaryTUI) detach(t *testing.T) {
	t.Helper()
	if _, err := ui.master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ui.done:
		ui.stopped = true
		if err != nil {
			t.Fatal("TUI detach", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not detach")
	}
	after, err := unix.IoctlGetTermios(int(ui.slave.Fd()), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(ui.before, after) {
		t.Fatal("TUI did not restore terminal modes", err)
	}
	ui.mu.Lock()
	colored := bytes.Contains(ui.output.Bytes(), []byte("\x1b[38;")) || bytes.Contains(ui.output.Bytes(), []byte("\x1b[48;"))
	ui.mu.Unlock()
	if colored {
		t.Fatal("NO_COLOR emitted RGB/indexed colors")
	}
}

// A real CLI child owns only this test PTY. Core/runner have separate sessions.
func startBinaryTUI(t *testing.T, binary, dir, agent string, env []string) *binaryTUI {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "cli-test-ptmx")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 80, Row: 24}); err != nil {
		t.Fatal(err)
	}
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	ui := &binaryTUI{master: master, slave: slave, before: before, done: make(chan error, 1)}
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(ui, master); close(readDone) }()
	t.Cleanup(func() {
		_ = master.Close()
		select {
		case <-readDone:
		case <-time.After(time.Second):
			t.Error("TUI output reader did not stop")
		}
	})
	cmd := exec.Command(binary, "--profile", "test", "tui", "--ascii", agent)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), env...), "TERM=xterm-256color", "NO_COLOR=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { ui.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !ui.stopped {
			_ = cmd.Process.Kill()
			<-ui.done
			ui.stopped = true
		}
	})
	// The tool heading proves this child replayed core history, not just that
	// Bubble Tea entered raw mode. Projection correctness has its own tests.
	ui.waitText(t, "exec_command")
	return ui
}
