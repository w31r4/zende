package cli

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
)

// Shutdown is an instance-scoped control action, not a new Agent task or a
// durable mutation to replay after replacing a service process.
type serviceControl struct {
	handler           protocol.Handler
	service, instance string
	cancel            context.CancelFunc
	drain             func()
	prepare           func() error
	reopen            func()
	mu                sync.Mutex
	pending           int
	stopping          bool
}

type serviceStopReply struct {
	protocol.ServiceStopInfo
	owner *serviceControl
}

func (s *serviceControl) Handle(ctx context.Context, req protocol.Request) (any, error) {
	if req.Method != s.service+".stop" {
		return s.handler.Handle(ctx, req)
	}
	var target protocol.ServiceStop
	if err := protocol.StrictDecode(req.Params, &target); err != nil {
		return nil, err
	}
	if req.OperationID != "" {
		return nil, protocol.E("invalid_params", "service control uses instance_id, not a durable task operation_id")
	}
	if target.InstanceID != s.instance {
		return nil, protocol.E("instance_changed", "service owner changed; inspect it before deciding to stop the replacement")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, protocol.E("unavailable", "this service instance is already stopping")
	}
	if s.pending == 0 && s.prepare != nil {
		if err := s.prepare(); err != nil {
			return nil, err
		}
	}
	s.pending++
	return &serviceStopReply{ServiceStopInfo: protocol.ServiceStopInfo{Service: s.service, InstanceID: s.instance, PID: os.Getpid(), State: "stopping"}, owner: s}, nil
}

func (s *serviceControl) Subscribe(ctx context.Context, req protocol.Request, consume func(protocol.Event) error) error {
	return s.handler.Subscribe(ctx, req, consume)
}

func (s *serviceControl) ResponseSent(_ protocol.Request, result any, err error) {
	reply, ok := result.(*serviceStopReply)
	if !ok || reply.owner != s {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	if err == nil {
		s.stopping = true
		s.cancel()
	} else if s.pending == 0 && !s.stopping && s.reopen != nil {
		s.reopen()
	}
}

func (s *serviceControl) Drain() {
	s.cancel()
	if s.drain != nil {
		s.drain()
	}
}

func (a *app) stopCommand(service string) *cobra.Command {
	var instance string
	var waitMS int
	command := &cobra.Command{Use: "stop", Short: "Stop this service instance without deleting profile data", Args: cobra.NoArgs,
		Long: "Stops the currently observed profile owner, or only --instance when provided. Core stop preserves the independent runner and its processes. Runner stop rejects active or unverified processes. Returns stopping on a bounded wait timeout; it never escalates to a kill. Reuse --instance after an uncertain response; a replacement owner is not stopped. This instance control does not use --request-id.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.stopService(cmd, service, instance, waitMS)
		}}
	command.Flags().StringVar(&instance, "instance", "", "Expected service owner; preserve this identity on retry")
	command.Flags().IntVar(&waitMS, "wait-ms", 5000, "Wait for this owner's runtime to drain (0–30000)")
	return command
}

func (a *app) stopService(cmd *cobra.Command, service, expected string, waitMS int) error {
	if a.operationID != "" || waitMS < 0 || waitMS > 30000 || len(expected) > 128 || !utf8.ValidString(expected) {
		return protocol.E("invalid_params", "service stop accepts --instance and --wait-ms 0–30000, without --request-id")
	}
	paths, err := a.paths()
	if err != nil {
		return err
	}
	socket := paths.Socket
	if service == "runner" {
		socket = paths.RunnerSocket
	}
	// Hold an existing lock-file reference across status and shutdown. Never
	// create/unlink it or signal a PID obtained from an old status response.
	lock, err := os.OpenFile(socket+".lock", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return protocol.E("unavailable", "service ownership lock is unavailable; no stop was sent")
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return protocol.E("invalid_state", "service ownership lock is not a regular file")
	}
	client := protocol.Client{Socket: socket}
	identity, err := inspectService(cmd.Context(), client, service)
	if err != nil {
		return err
	}
	if expected != "" && expected != identity.InstanceID {
		return protocol.E("instance_changed", "service owner changed; no stop was sent to its replacement")
	}
	request, err := protocol.NewRequest(service+".stop", "", protocol.ServiceStop{InstanceID: identity.InstanceID})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	var result protocol.ServiceStopInfo
	err = client.Call(ctx, request, &result) // Never automatically repeat shutdown.
	cancel()
	if err != nil {
		var known *protocol.Error
		if !errors.As(err, &known) || known.Data.Kind != "transport_unknown" {
			return err
		}
		result = protocol.ServiceStopInfo{Service: service, InstanceID: identity.InstanceID, PID: identity.PID, State: "unknown"}
	} else if result.Service != service || result.InstanceID != identity.InstanceID || result.PID != identity.PID || result.State != "stopping" {
		return protocol.E("service_stop_unknown", "stop receipt did not match its original service owner; inspect --instance "+identity.InstanceID)
	}
	if waitMS > 0 {
		stopped, waitErr := waitServiceStopped(cmd.Context(), client, lock, service, identity.InstanceID, time.Duration(waitMS)*time.Millisecond)
		if waitErr != nil {
			return waitErr
		}
		if stopped {
			result.State = "stopped"
		}
	}
	if result.State == "unknown" {
		return protocol.E("service_stop_unknown", "stop response was lost; inspect the original --instance "+identity.InstanceID+" before retrying")
	}
	return encode(cmd, result)
}

type serviceIdentity struct {
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
}

func inspectService(parent context.Context, client protocol.Client, service string) (serviceIdentity, error) {
	ctx, cancel := context.WithTimeout(parent, 750*time.Millisecond)
	defer cancel()
	req, err := protocol.NewRequest(service+".status", "", struct{}{})
	var identity serviceIdentity
	if err == nil {
		err = client.Call(ctx, req, &identity)
	}
	if err == nil && (identity.InstanceID == "" || len(identity.InstanceID) > 128 || !utf8.ValidString(identity.InstanceID) || identity.PID <= 1) {
		err = protocol.E("invalid_state", "service status has no valid instance identity")
	}
	return identity, err
}

func waitServiceStopped(parent context.Context, client protocol.Client, lock *os.File, service, instance string, duration time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
				return false, err
			}
			return true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return false, protocol.E("unavailable", "original stop was sent but its ownership lock could not be observed")
		}
		// A new owner can acquire the lock before our observation. Its different
		// instance proves the old runtime drained; never send it another stop.
		identity, err := inspectService(ctx, client, service)
		if err == nil && identity.InstanceID != instance {
			return true, nil
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return false, parent.Err()
			}
			return false, nil
		case <-tick.C:
		}
	}
}
