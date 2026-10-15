package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/tool"
)

func inputImageFile(t *testing.T, c *Core) (string, []byte) {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.Config.Config.Cwd, "中文 image.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, data.Bytes()
}

func importTestImage(t *testing.T, c *Core, agent, path string) (protocol.Request, protocol.Accepted) {
	t.Helper()
	r, _ := protocol.NewRequest("attachment.import", protocol.NewID("import"), protocol.ImportAttachment{AgentID: agent, Path: path})
	result, err := c.Handle(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	a := result.(protocol.Accepted)
	if a.Attachment == nil || a.Attachment.ID == "" {
		t.Fatal("missing import result")
	}
	return r, a
}

func TestImageImportAndCapabilityErrorsKeepOriginalBytesAndInput(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	a := createWork(t, c, "")
	path, data := inputImageFile(t, c)
	r, imported := importTestImage(t, c, a.AgentID, filepath.Base(path))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	again, err := c.Handle(ctx, r)
	if err != nil || !reflect.DeepEqual(again, imported) {
		t.Fatal("retry reread missing source", again, err)
	}
	p := protocol.Submit{AgentID: a.AgentID, Input: "中文 draft", Attachments: []string{imported.Attachment.ID}}
	submit, _ := protocol.NewRequest("agent.submit", "image-submit", p)
	if _, err := c.Handle(ctx, submit); err == nil || err.(*protocol.Error).Data.Kind != "unsupported_capability" {
		t.Fatal(err)
	}
	snap, err := c.Store.Snapshot(ctx, a.AgentID)
	if err != nil || len(snap.Tasks) != 0 {
		t.Fatal("rejected input created a task", snap, err)
	}
	read, err := c.Store.ReadImage(ctx, protocol.AttachmentTarget{AgentID: a.AgentID, ID: imported.Attachment.ID})
	if err != nil || !bytes.Equal(read.Data, data) {
		t.Fatal("unsupported model lost image", err)
	}
	// A declared capability can accept the same retained input and refs later.
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Images: true, Adapter: adapterFunc(func(context.Context, model.Request, func(model.Delta) error) error { return nil })}, nil
	})
	accepted, err := c.Handle(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	c.Models = nil
	duplicate, err := c.Handle(ctx, submit)
	if err != nil || !reflect.DeepEqual(accepted, duplicate) {
		t.Fatal("retry rechecked changed capabilities", err)
	}
	for _, target := range []string{c.Config.Config.Cwd, filepath.Join(c.Config.Config.Cwd, "fifo")} {
		if target != c.Config.Config.Cwd {
			if err := unix.Mkfifo(target, 0600); err != nil {
				t.Fatal(err)
			}
		}
		req, _ := protocol.NewRequest("attachment.import", protocol.NewID("invalid"), protocol.ImportAttachment{AgentID: a.AgentID, Path: target})
		start := time.Now()
		if _, err := c.Handle(ctx, req); err == nil || time.Since(start) > time.Second {
			t.Fatal("non-regular source blocked or was accepted", err)
		}
	}
}

func TestResponsesImageBytesSurviveToolRoundAndSourceReplacement(t *testing.T) {
	var requests atomic.Int32
	var expected string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(body.Input) == 0 {
			t.Error("missing user input")
			return
		}
		content, ok := body.Input[0]["content"].([]any)
		if !ok || len(content) != 2 {
			t.Error("missing image part")
			return
		}
		imagePart, ok := content[1].(map[string]any)
		if !ok || imagePart["type"] != "input_image" || imagePart["image_url"] != expected || imagePart["detail"] != "auto" {
			t.Error("image wire bytes changed")
		}
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, responsesRound(map[string]any{"id": "fc", "call_id": "call", "type": "function_call", "name": "act", "arguments": "{}", "status": "completed"}, false))
		} else {
			if n != 2 {
				t.Error("unexpected image request", n)
			}
			io.WriteString(w, responsesRound(map[string]any{"id": "answer", "type": "message", "role": "assistant", "phase": "final_answer", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "image checked", "annotations": []any{}}}}, false))
		}
	}))
	defer server.Close()
	c := loopCore(t, nil, testTool{"act", func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{Content: []model.Block{{Type: "text", Text: "ok"}}}, nil
	}})
	configureResponses(t, c, server.URL, true)
	conn := c.Config.Config.Connections["test"]
	conn.Models[0].Images = true
	c.Config.Config.Connections["test"] = conn
	var err error
	c.Models, err = provider.New(c.Config.Config, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := createWork(t, c, "")
	path, data := inputImageFile(t, c)
	expected = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	_, imported := importTestImage(t, c, a.AgentID, path)
	if err := os.WriteFile(path, []byte("source replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	p := protocol.Submit{AgentID: a.AgentID, Input: "看一下这张图", Attachments: []string{imported.Attachment.ID}}
	req, _ := protocol.NewRequest("agent.submit", "image-task", p)
	result, err := c.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	accepted := result.(protocol.Accepted)
	runCore(t, c)
	snap := waitTask(t, c, accepted, "completed")
	if requests.Load() != 2 || snap.Tasks[0].Result != "image checked" || !reflect.DeepEqual(snap.Tasks[0].Attachments, p.Attachments) {
		t.Fatal(requests.Load(), snap)
	}
	page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "data:image") || strings.Contains(string(encoded), "source replaced") {
		t.Fatal("image bytes/source content entered history")
	}
	msgs, _, err := c.Store.ModelContext(context.Background(), a.AgentID)
	if err != nil || msgs[0].Content[1].Attachment != imported.Attachment.ID {
		t.Fatal("image reference lost", err)
	}
}
