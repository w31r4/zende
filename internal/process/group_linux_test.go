package process

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestExternalGroupSharesVerifiedKernelStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	group, err := CaptureGroup(cmd.Process.Pid)
	if err != nil || group.Identity == "" {
		t.Fatal(group, err)
	}
	wrong := group
	boot, _ := kernelBoot()
	wrong.Identity = boot + ":0"
	if _, ended, err := ObserveGroup(wrong); err == nil || ended {
		t.Fatal("unowned lifetime accepted", ended, err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("observation signaled unrelated lifetime", err)
	}
	group, ended, err := ObserveGroup(group)
	if err != nil || ended || len(group.Witnesses) != 1 {
		t.Fatal(group, ended, err)
	}
	if err := KillGroup(group); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, ended, err := ObserveGroup(group); err != nil || !ended {
		t.Fatal("exited group not recognized", ended, err)
	}
	if g, err := CaptureGroup(group.PID); err == nil || g.PID != group.PID || g.Identity != "" {
		t.Fatal("missing leader identity fabricated", g, err)
	}
}
