package render

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/muesli/cancelreader"
)

// This directly exercises the dependency path used by Bubble Tea's forced
// shutdown, which need not wait for its input reader before calling Close.
func TestInputReaderConcurrentCloseJoinsRead(t *testing.T) {
	for range 100 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		reader, err := cancelreader.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { var b [1]byte; _, err := reader.Read(b[:]); done <- err }()
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reader.Cancel()
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, cancelreader.ErrCanceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Read outlived Close")
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInputCancelReleasesHandoffDescriptors(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	files, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	before := len(files)
	var retained []cancelreader.CancelReader
	for range 20 {
		reader, err := cancelreader.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, reader)
		if !reader.Cancel() {
			t.Fatal("cancel failed")
		}
	}
	files, err = os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != before {
		t.Fatalf("cancelled handoff readers retained descriptors: %d -> %d", before, len(files))
	}
	for _, reader := range retained {
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
