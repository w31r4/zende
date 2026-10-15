package attachment

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"testing"

	"zende.local/harness/internal/protocol"
)

func TestInspectVerifiesBytesAndBoundsBeforeDecoding(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		var b bytes.Buffer
		img := image.NewNRGBA(image.Rect(0, 0, 2, 3))
		if format == "png" {
			if err := png.Encode(&b, img); err != nil {
				t.Fatal(err)
			}
		} else if err := jpeg.Encode(&b, img, nil); err != nil {
			t.Fatal(err)
		}
		data := b.Bytes()
		info, err := Inspect("wrong-extension.txt", data)
		if err != nil || info.MediaType != "image/"+format || info.Width != 2 || info.Height != 3 || info.Bytes != len(data) || len(info.SHA256) != 64 {
			t.Fatal(info, err)
		}
		if _, err := Inspect("broken", data[:len(data)/2]); err == nil {
			t.Fatal("accepted truncated image")
		}
		if format == "png" {
			oversized := append([]byte(nil), data...)
			binary.BigEndian.PutUint32(oversized[16:20], protocol.MaxImagePixels)
			binary.BigEndian.PutUint32(oversized[20:24], 2)
			binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
			if _, err := Inspect("huge.png", oversized); err == nil || err.(*protocol.Error).Data.Kind != "attachment_limit" {
				t.Fatal("pixel allocation was not rejected before decode", err)
			}
		}
	}
	for _, data := range [][]byte{nil, []byte("text"), bytes.Repeat([]byte{'x'}, protocol.MaxImageBytes+1)} {
		if _, err := Inspect("image.png", data); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
}
