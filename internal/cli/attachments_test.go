package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestAttachmentCLIImportRetryReadAndImageOnlyInput(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-image-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	a := &app{profile: "images"}
	paths, err := a.paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	core := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	core.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Images: true, Adapter: fixture.New()}, nil
	})
	server, err := protocol.Listen(paths.Socket, core)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	run := func(args ...string) []byte {
		t.Helper()
		var out, diagnostics bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "images"}, args...), nil, &out, &diagnostics, BuildInfo{})
		if code != 0 || diagnostics.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, diagnostics.String())
		}
		return out.Bytes()
	}
	var created protocol.Accepted
	if err := json.Unmarshal(run("--request-id", "create", "agent", "create", "--cwd", dir), &created); err != nil {
		t.Fatal(err)
	}
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "中文 image.png")
	if err := os.WriteFile(path, imageData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--request-id", "import", "attachment", "import", created.AgentID, filepath.Base(path)}
	first := run(args...)
	var imported protocol.Accepted
	if err := json.Unmarshal(first, &imported); err != nil || imported.Attachment == nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, run(args...)) {
		t.Fatal("CLI retry changed saved image")
	}
	id := imported.Attachment.ID
	var info protocol.Attachment
	if err := json.Unmarshal(run("attachment", "inspect", created.AgentID, id), &info); err != nil || !reflect.DeepEqual(&info, imported.Attachment) {
		t.Fatal(info, err)
	}
	var data protocol.AttachmentData
	if err := json.Unmarshal(run("attachment", "read", created.AgentID, id), &data); err != nil || !bytes.Equal(data.Data, imageData.Bytes()) {
		t.Fatal("CLI byte read", err)
	}
	var page protocol.AttachmentPage
	if err := json.Unmarshal(run("attachment", "list", created.AgentID, "--limit", "1"), &page); err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	var task protocol.Accepted
	if err := json.Unmarshal(run("--request-id", "submit", "agent", "submit", created.AgentID, "--image", id), &task); err != nil || task.TaskID == "" {
		t.Fatal(task, err)
	}
	var snapshot protocol.Snapshot
	if err := json.Unmarshal(run("agent", "inspect", created.AgentID), &snapshot); err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Input != "" || !reflect.DeepEqual(snapshot.Tasks[0].Attachments, []string{id}) {
		t.Fatal(snapshot, err)
	}
	for _, args := range [][]string{{"attachment", "--help"}, {"attachment", "import", "--help"}, {"attachment", "read", "--help"}, {"agent", "submit", "--help"}} {
		if !strings.Contains(string(run(args...)), "Usage:") {
			t.Fatal("missing help", args)
		}
	}
}
