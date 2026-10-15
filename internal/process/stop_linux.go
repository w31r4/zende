package process

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

type kernelProcess struct {
	stopWitness
	group, session int
	live           bool
}

func kernelBoot() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", protocol.E("process_unavailable", "kernel boot identity cannot be read")
	}
	return strings.TrimSpace(string(b)), nil
}

func kernelMember(pid int, boot string) (kernelProcess, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return kernelProcess{}, err
	}
	text := string(b)
	start, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	bad := protocol.E("process_unavailable", "kernel process identity cannot be verified")
	if start < 1 || end <= start {
		return kernelProcess{}, bad
	}
	actual, err := strconv.Atoi(strings.TrimSpace(text[:start]))
	if err != nil || actual != pid {
		return kernelProcess{}, bad
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return kernelProcess{}, bad
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return kernelProcess{}, bad
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return kernelProcess{}, bad
	}
	return kernelProcess{stopWitness: stopWitness{PID: pid, Identity: boot + ":" + fields[19]}, group: group, session: session, live: fields[0] != "Z" && fields[0] != "X"}, nil
}

func inStopScope(p StopProcess, member kernelProcess) bool {
	if p.PTY {
		return member.session == p.PID
	}
	return member.group == p.PID
}

func kernelScope(p StopProcess, boot string) ([]kernelProcess, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []kernelProcess
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid <= 0 {
			continue
		}
		member, err := kernelMember(pid, boot)
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue // Process exited between directory listing and stat.
		}
		if err != nil {
			return nil, err // An unreadable /proc is not proof of absence.
		}
		if inStopScope(p, member) {
			members = append(members, member)
			if len(members) > maxStopItems {
				return nil, protocol.E("limit_exceeded", "process scope exceeds 10000 kernel members")
			}
		}
	}
	return members, nil
}

// A PID/PGID alone is not ownership evidence. Verify the boot/start identity
// of the original leader or a descendant saved before termination. Even after
// leader exit, the matching witness pins the lifetime of its group/session.
func stopEvidence(p StopProcess) ([]stopWitness, bool, error) {
	bad := protocol.E("process_unavailable", "original process group cannot be verified; stop remains pending")
	if p.PID <= 1 {
		return nil, false, bad
	}
	boot, err := kernelBoot()
	if err != nil {
		return nil, false, err
	}
	if p.Identity != "" {
		originalBoot, _, ok := strings.Cut(p.Identity, ":")
		if !ok || originalBoot == "" {
			return nil, false, bad
		}
		if boot != originalBoot {
			return nil, true, nil // No original process survives a kernel reboot.
		}
	}
	members, err := kernelScope(p, boot)
	if err != nil {
		return nil, false, err
	}
	owned, live := false, false
	witnesses := make([]stopWitness, 0, len(members))
	for _, member := range members {
		live = live || member.live
		if member.PID == p.PID && member.Identity == p.Identity && member.group == p.PID {
			owned = true
		}
		if slices.Contains(p.Witnesses, member.stopWitness) {
			owned = true
		}
		witnesses = append(witnesses, member.stopWitness)
	}
	if !live {
		return witnesses, true, nil // Zombies cannot execute or create children.
	}
	if !owned {
		return nil, false, bad
	}
	return witnesses, false, nil
}

func killStopMembers(p StopProcess) error {
	boot, err := kernelBoot()
	if err != nil {
		return err
	}
	// Terminate the shell's group first, including children forked after the
	// scan. pidfds bind the original lifetime even if a PID/PGID is reused.
	// Do not SIGSTOP a pane: tmux automatically sends SIGCONT to stopped panes.
	witnesses := slices.Clone(p.Witnesses)
	slices.SortStableFunc(witnesses, func(a, b stopWitness) int {
		if a.PID == p.PID && b.PID != p.PID {
			return -1
		}
		if b.PID == p.PID && a.PID != p.PID {
			return 1
		}
		return 0
	})
	for _, witness := range witnesses {
		fd, err := unix.PidfdOpen(witness.PID, 0)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return protocol.E("process_unavailable", "kernel process handle could not be opened; stop remains pending")
		}
		member, err := kernelMember(witness.PID, boot)
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			unix.Close(fd)
			continue
		}
		if err != nil {
			unix.Close(fd)
			return err
		}
		if member.Identity != witness.Identity || !inStopScope(p, member) {
			unix.Close(fd)
			continue // Reused PID or a job that left the owned scope.
		}
		if member.group <= 1 || member.group == syscall.Getpgrp() {
			unix.Close(fd)
			return protocol.E("process_unavailable", "refusing an unowned or runner process group")
		}
		flags := 0
		if member.PID == member.group {
			flags = unix.PIDFD_SIGNAL_PROCESS_GROUP
		}
		err = killHandle(fd, flags, unix.PidfdSendSignal)
		unix.Close(fd)
		if err != nil && err != syscall.ESRCH {
			return protocol.E("process_unavailable", "owned process could not be terminated")
		}
	}
	return nil
}

func killHandle(fd, flags int, send func(int, unix.Signal, *unix.Siginfo, int) error) error {
	err := send(fd, unix.SIGKILL, nil, flags)
	if errors.Is(err, unix.EINVAL) && flags != 0 {
		// Older kernels support process handles but not group delivery.
		// Signal known members individually; any unverified descendant
		// keeps the stop pending, never fall back to a numeric group kill.
		err = send(fd, unix.SIGKILL, nil, 0)
	}
	return err
}
