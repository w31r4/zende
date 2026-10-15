package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

type imageTerminalAdapter struct {
	data     []byte
	received chan model.Request
}

func (a imageTerminalAdapter) Stream(_ context.Context, req model.Request, emit func(model.Delta) error) error {
	if len(req.Images) != 1 {
		return fmt.Errorf("expected one resolved image")
	}
	for _, img := range req.Images {
		if img.MediaType != "image/png" || !bytes.Equal(img.Data, a.data) {
			return fmt.Errorf("image source was reread or changed")
		}
	}
	a.received <- req
	for _, delta := range fixture.Answer("final", "image fixture complete") {
		if err := emit(delta); err != nil {
			return err
		}
	}
	return nil
}

type imageProbe struct{ reply chan imageUIState }
type imageUIState struct {
	text, held, errorKind              string
	images                             []string
	loading, pending, palette, catalog bool
}

func TestRealTerminalImagesThroughCorePreserveRejectedDraft(t *testing.T) {
	// Keep UDS paths below Linux's sockaddr_un limit, including long test names.
	dir, err := os.MkdirTemp("/tmp", "zende-image-ui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "中文 image.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	var enabled atomic.Bool
	adapter := imageTerminalAdapter{data: encoded.Bytes(), received: make(chan model.Request, 1)}
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", Images: enabled.Load(), NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	server, err := protocol.Listen(filepath.Join(dir, "core.sock"), c)
	if err != nil {
		t.Fatal(err)
	}
	coreCtx, stopCore := context.WithCancel(context.Background())
	serverDone, coreDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- server.Serve(coreCtx) }()
	go func() { coreDone <- c.Run(coreCtx) }()
	defer func() {
		stopCore()
		for _, done := range []chan error{serverDone, coreDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("core cleanup timed out")
			}
		}
	}()
	client := protocol.Client{Socket: filepath.Join(dir, "core.sock")}
	req, _ := protocol.NewRequest("agent.create", "initial", protocol.CreateAgent{Cwd: dir})
	var created protocol.Accepted
	if err := client.Call(coreCtx, req, &created); err != nil {
		t.Fatal(err)
	}
	master, slave, capture := testTerminal(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	uiCtx, stopUI := context.WithCancel(context.Background())
	defer stopUI()
	var program *tea.Program
	m := New(uiCtx, client, created.AgentID, func(msg tea.Msg) { program.Send(msg) })
	program = tea.NewProgram(m, tea.WithContext(uiCtx), tea.WithInput(slave), tea.WithOutput(slave), tea.WithWindowSize(48, 16), tea.WithColorProfile(colorprofile.ASCII), tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
		if probe, ok := msg.(imageProbe); ok {
			current := model.(Shell)
			state := imageUIState{text: current.input.Value(), images: current.editDraft().imageIDs(), loading: len(current.imageLoads) > 0, pending: current.pending != nil, palette: current.paletteActive(), catalog: current.imagesPanel != nil && !current.imagesPanel.waiting}
			if current.commandDraft != nil {
				state.held = current.commandDraft.Value()
			}
			if err, ok := current.err.(*protocol.Error); ok {
				state.errorKind = err.Data.Kind
			}
			probe.reply <- state
			return nil
		}
		return msg
	}))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	defer func() { program.Kill(); program.Wait() }()
	waitTerminal(t, capture, "\x1b[?1049h")
	waitState := func(check func(imageUIState) bool) imageUIState {
		t.Helper()
		var state imageUIState
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			reply := make(chan imageUIState, 1)
			program.Send(imageProbe{reply})
			select {
			case state = <-reply:
				if check(state) {
					return state
				}
			case <-time.After(time.Second):
				t.Fatal("UI probe timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("UI state missing", state)
		return state
	}
	write := func(text string) {
		t.Helper()
		if _, err := master.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	paste := func(text string) { write("\x1b[200~" + text + "\x1b[201~") }
	const draft = "中文 é 🙂\n看一下图片 "
	paste(draft)
	write("/image")
	waitState(func(s imageUIState) bool { return s.palette && s.text == draft+"/image" })
	write("\t")
	waitState(func(s imageUIState) bool { return s.held == draft && s.text == "/image " })
	paste("'" + path + "'")
	write("\r")
	state := waitState(func(s imageUIState) bool { return s.text == draft && !s.loading && len(s.images) == 1 })
	imageID := state.images[0]
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write("\r")
	waitState(func(s imageUIState) bool {
		return !s.pending && s.errorKind == "unsupported_capability" && s.text == draft && reflect.DeepEqual(s.images, []string{imageID})
	})
	snapshot, err := s.Snapshot(coreCtx, created.AgentID)
	if err != nil || len(snapshot.Tasks) != 0 {
		t.Fatal("rejected image created task", err, snapshot)
	}
	enabled.Store(true)
	write("\r")
	waitState(func(s imageUIState) bool {
		return !s.pending && s.text == "" && len(s.images) == 0 && s.errorKind == ""
	})
	select {
	case request := <-adapter.received:
		if len(request.Messages) == 0 {
			t.Fatal("missing input")
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "user" || len(last.Content) != 2 || last.Content[0].Text != draft || last.Content[1].Attachment != imageID {
			t.Fatal("lost submitted text or reference", last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model did not receive retained bytes")
	}
	waitTerminal(t, capture, "image fixture complete")
	// Re-select the stored image with only keyboard commands after its source vanished.
	paste("next draft ")
	write("/images\r")
	waitState(func(s imageUIState) bool { return s.catalog && s.text == "next draft " })
	write("/image-use\t")
	waitState(func(s imageUIState) bool { return s.palette && s.held == "next draft " && s.text == "/image-use " })
	write("\r")
	waitState(func(s imageUIState) bool {
		return s.text == "next draft " && !s.loading && reflect.DeepEqual(s.images, []string{imageID})
	})
	write("/image-remove\t")
	waitState(func(s imageUIState) bool { return s.palette && strings.HasPrefix(s.text, "/image-remove ") })
	write("\r")
	waitState(func(s imageUIState) bool { return s.text == "next draft " && len(s.images) == 0 })
	write("\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not detach")
	}
	waitTerminal(t, capture, "\x1b[?1049l")
	waitTerminal(t, capture, "\x1b[?1002l")
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("terminal modes not restored", err)
	}
	retained, err := s.ReadImage(coreCtx, protocol.AttachmentTarget{AgentID: created.AgentID, ID: imageID})
	if err != nil || !bytes.Equal(retained.Data, encoded.Bytes()) {
		t.Fatal("draft remove deleted saved image", err)
	}
	snapshot, err = s.Snapshot(coreCtx, created.AgentID)
	if err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].State != "completed" || !reflect.DeepEqual(snapshot.Tasks[0].Attachments, []string{imageID}) {
		t.Fatal("wrong durable task", snapshot, err)
	}
}
