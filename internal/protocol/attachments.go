package protocol

const (
	MaxImageBytes        = 4 << 20
	MaxImagePixels       = 16 << 20
	MaxInputImages       = 8
	MaxRequestImageBytes = 12 << 20
)

// Attachment describes an immutable local image. Byte data is fetched only by
// an explicit read; ordinary history and acceptance contain references/metadata.
type Attachment struct {
	ID        string `json:"attachment_id"`
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Bytes     int    `json:"bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type ImportAttachment struct {
	AgentID string `json:"agent_id"`
	Path    string `json:"path"`
}

type AttachmentTarget struct {
	AgentID string `json:"agent_id"`
	ID      string `json:"attachment_id"`
}

type AttachmentData struct {
	Attachment
	Data []byte `json:"data"`
}

type AttachmentList struct {
	AgentID string `json:"agent_id"`
	After   string `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type AttachmentPage struct {
	Items []Attachment `json:"items"`
	Next  string       `json:"next,omitempty"`
}
