// Package attachment validates immutable image bytes before persistence.
package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

func Inspect(name string, data []byte) (protocol.Attachment, error) {
	if len(data) == 0 || len(data) > protocol.MaxImageBytes {
		return protocol.Attachment{}, protocol.E("attachment_limit", "image must contain 1 byte to 4 MiB")
	}
	if name == "" || len(name) > 255 || !utf8.ValidString(name) {
		return protocol.Attachment{}, protocol.E("invalid_params", "image name must be valid UTF-8 up to 255 bytes")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" && format != "jpeg" {
		return protocol.Attachment{}, protocol.E("unsupported_image", "expected a PNG or JPEG image")
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > protocol.MaxImagePixels {
		return protocol.Attachment{}, protocol.E("attachment_limit", "decoded image exceeds 16,777,216 pixels")
	}
	// Verify the complete image after checking allocation bounds. The original
	// encoded bytes are retained exactly; decoding is validation, not conversion.
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return protocol.Attachment{}, protocol.E("invalid_image", "image is incomplete or corrupt")
	}
	digest := sha256.Sum256(data)
	return protocol.Attachment{Name: name, MediaType: "image/" + format, SHA256: hex.EncodeToString(digest[:]), Bytes: len(data), Width: config.Width, Height: config.Height}, nil
}
