package protocol

// ConfigPublication identifies a committed profile configuration. It contains
// neither endpoint metadata nor resolved credentials.
type ConfigPublication struct {
	Generation  int64  `json:"generation"`
	Version     string `json:"version"`
	OperationID string `json:"operation_id"`
}

type ConfigHistory struct {
	Items  []ConfigPublication `json:"items"`
	Next   int64               `json:"next"`
	Latest int64               `json:"latest"`
}
