package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

// Real CLI subprocesses, shell processes and Unix socket; core/runner run in
// this test process and the model is deterministic. No external credentials.
func TestModelInstallsRefreshesAndUsesSkillThroughBinaryCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-resources-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runDir)
	paths, err := (&app{profile: "resources"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manager, err := process.NewManager(filepath.Join(paths.State, "processes"), "/bin/sh", 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	c.Runner = manager
	c.Tools, err = tool.NewRegistry(tool.ProcessExecutors(manager)...)
	if err != nil {
		t.Fatal(err)
	}
	c.Commands = resourceHints(binary, "resources")
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	doneServer := make(chan error, 1)
	go func() { doneServer <- server.Serve(ctx) }()
	defer func() { cancel(); <-doneServer }()
	p := protocol.CreateAgent{Cwd: dir, Input: "Install and use a skill without ending this task"}
	req, _ := protocol.NewRequest("agent.create", "resource-task", p)
	a, err := s.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	cli := quoteCommandPart(binary) + " --profile resources "
	skillFile := "skills/review/SKILL.md"
	v1 := "---\nname: review\ndescription: Local review\n---\nOriginal skill instructions. Read scripts/run.sh at this version before running it.\n"
	v2 := strings.Replace(v1, "Original", "Updated", 1)
	prompt := "---\nname: check\ndescription: Check output\n---\nPrompt instructions remain deferred.\n"
	asset := "printf x >> skill-effect\nprintf skill-script-ran\n"
	commandEvents := func(id, command string) []model.Delta {
		args, _ := json.Marshal(map[string]any{"command": command, "wait_ms": 3000})
		return fixture.Call(id, "exec_command", string(args))
	}
	output := func(r model.Request) (string, error) {
		for i := len(r.Messages) - 1; i >= 0; i-- {
			if r.Messages[i].Role != "tool" {
				continue
			}
			var result tool.ProcessResult
			if len(r.Messages[i].Content) != 1 || json.Unmarshal([]byte(r.Messages[i].Content[0].Text), &result) != nil || result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
				return "", fmt.Errorf("CLI process did not exit successfully: %+v", r.Messages[i])
			}
			for _, page := range result.Output {
				if page.Stream == "stdout" {
					return page.Text, nil
				}
			}
		}
		return "", fmt.Errorf("missing CLI stdout")
	}
	var original, updated, promptInfo protocol.ResourceInfo
	var scriptBody string
	step := 0
	adapter := resourceModelFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		step++
		var events []model.Delta
		body, err := output(r)
		if step > 1 && err != nil {
			return err
		}
		notices := 0
		for _, message := range r.Messages {
			for _, block := range message.Content {
				if strings.HasPrefix(block.Text, "<resource_changes ") {
					notices++
				}
			}
		}
		wantNotices := 0
		if step >= 2 {
			wantNotices++
		}
		if step >= 4 {
			wantNotices++
		}
		if notices != wantNotices {
			return fmt.Errorf("resource updates missing or duplicated at request %d: %d", step, notices)
		}
		read := func(version string, path string) string {
			command := cli + "resource read " + quoteCommandPart(a.AgentID) + " " + quoteCommandPart(original.ID) + " --version " + quoteCommandPart(version)
			if path != "" {
				command += " --path " + quoteCommandPart(path)
			}
			return command
		}
		switch step {
		case 1:
			events = commandEvents("install", "mkdir -p skills/review/scripts && printf %s "+quoteCommandPart(v1)+" > "+skillFile+" && printf %s "+quoteCommandPart(asset)+" > skills/review/scripts/run.sh && "+cli+"--request-id register-skill resource register "+quoteCommandPart(a.AgentID)+" skills/review")
		case 2:
			var receipt protocol.Accepted
			if json.Unmarshal([]byte(body), &receipt) != nil || receipt.Resource == nil {
				return fmt.Errorf("missing installation receipt: %s", body)
			}
			original = *receipt.Resource
			events = commandEvents("read-original", read(original.Version, ""))
		case 3:
			var content protocol.ResourceContent
			if json.Unmarshal([]byte(body), &content) != nil || content.Version != original.Version || content.Text != v1 {
				return fmt.Errorf("original instructions unreadable: %s", body)
			}
			events = commandEvents("refresh", "printf %s "+quoteCommandPart(v2)+" > "+skillFile+" && printf %s "+quoteCommandPart(prompt)+" > prompt.md && "+cli+"--request-id register-prompt resource register "+quoteCommandPart(a.AgentID)+" prompt.md --kind prompt && "+cli+"--request-id refresh-skill resource refresh "+quoteCommandPart(a.AgentID)+" "+quoteCommandPart(original.ID))
		case 4:
			var receipt protocol.Accepted
			decoder := json.NewDecoder(strings.NewReader(body))
			if decoder.Decode(&receipt) != nil || receipt.Resource == nil || receipt.Resource.Kind != "prompt" {
				return fmt.Errorf("missing prompt version: %s", body)
			}
			promptInfo = *receipt.Resource
			if decoder.Decode(&receipt) != nil || receipt.Resource == nil || receipt.Resource.Version == original.Version {
				return fmt.Errorf("missing updated version: %s", body)
			}
			updated = *receipt.Resource
			events = commandEvents("read-versions", read(original.Version, "")+" && "+read(updated.Version, "")+" && "+cli+"resource read "+quoteCommandPart(a.AgentID)+" "+quoteCommandPart(promptInfo.ID)+" --version "+quoteCommandPart(promptInfo.Version)+" && "+read(updated.Version, "scripts/run.sh"))
		case 5:
			decoder := json.NewDecoder(strings.NewReader(body))
			for _, want := range []struct{ version, text string }{{original.Version, v1}, {updated.Version, v2}, {promptInfo.Version, prompt}, {updated.Version, asset}} {
				var content protocol.ResourceContent
				if err := decoder.Decode(&content); err != nil || content.Version != want.version || content.Text != want.text {
					return fmt.Errorf("versioned read failed: %+v %v", content, err)
				}
				scriptBody = content.Text
			}
			if _, err := os.Stat(filepath.Join(dir, "skill-effect")); !os.IsNotExist(err) {
				return fmt.Errorf("resource parsing executed a script: %v", err)
			}
			events = commandEvents("run-script", "sh -c "+quoteCommandPart(scriptBody))
		case 6:
			if body != "skill-script-ran" {
				return fmt.Errorf("script output lost: %q", body)
			}
			events = fixture.Answer("final", "installed, refreshed and used")
		default:
			return fmt.Errorf("unexpected model request")
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", NativeCategories: true}, nil
	})
	doneCore := make(chan error, 1)
	go func() { doneCore <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-doneCore; err != nil {
			t.Error(err)
		}
	}()
	var snap protocol.Snapshot
	for {
		snap, err = s.Snapshot(ctx, a.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Tasks) == 1 && snap.Tasks[0].State == "completed" {
			break
		}
		if snap.Tasks[0].State == "failed" || snap.Tasks[0].State == "unknown" || ctx.Err() != nil {
			t.Fatal(snap, ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.Agent.Cwd != dir || snap.Agent.Model != "" || snap.Tasks[0].ID != a.TaskID {
		t.Fatal("resource changed task or runtime selection", snap)
	}
	data, err := os.ReadFile(filepath.Join(dir, "skill-effect"))
	if err != nil || string(data) != "x" {
		t.Fatal("script did not run exactly once", string(data), err)
	}
	history, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range history.Events {
		counts[e.Kind]++
		if e.Kind == "model.started" {
			var record struct {
				Catalog protocol.ResourceCatalog `json:"resource_catalog"`
			}
			if json.Unmarshal(e.Payload, &record) != nil || !record.Catalog.Valid() {
				t.Fatal("missing request resource provenance", string(e.Payload))
			}
		}
	}
	if counts["resource.published"] != 3 || counts["model.started"] != 6 || counts["tool.dispatching"] != 5 || counts["task.accepted"] != 1 {
		t.Fatal(counts)
	}
}

type resourceModelFunc func(context.Context, model.Request, func(model.Delta) error) error

func (f resourceModelFunc) Stream(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
	return f(ctx, r, emit)
}
