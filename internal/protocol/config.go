package protocol

import "encoding/hex"

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

type ConfigRead struct {
	Version string `json:"version"`
}
type ConfigQuery struct {
	After int64 `json:"after,omitempty"`
	Limit int   `json:"limit,omitempty"`
}

func (p ConfigPublication) Valid() bool {
	digest, err := hex.DecodeString(p.Version)
	return p.Generation > 0 && p.OperationID != "" && len(p.OperationID) <= 128 && err == nil && len(digest) == 32 && p.Version == hex.EncodeToString(digest)
}
