package protocol

// Service instances, unlike Tasks, end with their hosting process. Stop is
// fenced to this instance and never retried against a replacement owner.
type ServiceStop struct {
	InstanceID string `json:"instance_id"`
}

type ServiceStopInfo struct {
	Service    string `json:"service"`
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	State      string `json:"state"`
}

// ResponseFinalizer completes local transport actions only after writing their
// response. Failure can release a temporary admission fence without stopping.
type ResponseFinalizer interface {
	ResponseSent(Request, any, error)
}

// ServiceDrainer keeps the listener's ownership lock until its runtime workers
// and state writers have stopped, not merely until Accept has returned.
type ServiceDrainer interface {
	Drain()
}
