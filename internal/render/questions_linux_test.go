package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	"zende.local/harness/internal/tool"
)

// The model and this executor are deterministic fixtures. Core, SQLite, UDS,
// image import, event replay and the terminal input/output are real.
type terminalQuestionTool struct{ core *runtime.Core }

func (terminalQuestionTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{Tool: model.Tool{Name: "fixture_question", Description: "Test question", InputSchema: json.RawMessage("{\"type\":\"object\"}")}, Version: "fixture-1"}
}
func (e terminalQuestionTool) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	req, err := protocol.NewRequest("question.ask", call.ID, protocol.AskQuestion{AgentID: call.AgentID, TaskID: call.TaskID, Prompt: "使用哪张图片？"})
	if err != nil {
		return tool.Result{}, err
	}
	result, err := e.core.Handle(ctx, req)
	if err != nil {
		return tool.Result{}, err
	}
	accepted := result.(protocol.Accepted)
	return tool.Result{Content: []model.Block{{Type: "text", Text: accepted.QuestionID}}}, nil
}

type questionProbe struct{ reply chan questionUIState }
type questionUIState struct {
	text, held, active, main string
	images                   []string
	skills                   []protocol.ResourceRef
	skillChoices             int
	questions                int
	pending, loading, ready  bool
}

type questionTerminal struct {
	program *tea.Program
	write   func(string)
	paste   func(string)
	wait    func(func(questionUIState) bool) questionUIState
	finish  func()
}

func startQuestionTerminal(t *testing.T, client Client, agent string) questionTerminal {
	t.Helper()
	master, slave, capture := testTerminal(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var program *tea.Program
	m := New(ctx, client, agent, func(msg tea.Msg) { program.Send(msg) })
	program = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(slave), tea.WithOutput(slave),
		tea.WithWindowSize(48, 16), tea.WithColorProfile(colorprofile.ASCII), tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
			if probe, ok := msg.(questionProbe); ok {
				current := model.(Shell)
				state := questionUIState{text: current.input.Value(), active: current.activeQuestion, images: current.editDraft().imageIDs(),
					questions: len(current.questions), pending: current.pending != nil, loading: len(current.imageLoads) > 0, ready: current.connected && current.haveSnapshot}
				state.skills = current.editDraft().skillRefs()
				if current.paletteActive() && current.palette.entry == "skill" {
					state.skillChoices = len(current.palette.items)
				}
				if current.commandDraft != nil {
					state.held = current.commandDraft.Value()
				}
				if current.mainDraft != nil {
					state.main = current.mainDraft.Value()
				}
				probe.reply <- state
				return nil
			}
			return msg
		}))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	t.Cleanup(func() { program.Kill(); program.Wait() })
	waitTerminal(t, capture, "\x1b[?1049h")
	write := func(text string) {
		t.Helper()
		if _, err := master.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(check func(questionUIState) bool) questionUIState {
		t.Helper()
		var state questionUIState
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			reply := make(chan questionUIState, 1)
			program.Send(questionProbe{reply})
			select {
			case state = <-reply:
				if check(state) {
					return state
				}
			case <-time.After(time.Second):
				t.Fatal("question UI probe timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("question UI state missing", state)
		return state
	}
	finish := func() {
		t.Helper()
		write("\x03")
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("question UI did not detach")
		}
		waitTerminal(t, capture, "\x1b[?1049l")
		waitTerminal(t, capture, "\x1b[?1002l")
		after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("question UI terminal modes not restored", err)
		}
	}
	return questionTerminal{program: program, write: write, paste: func(s string) { write("\x1b[200~" + s + "\x1b[201~") }, wait: wait, finish: finish}
}

func TestRealTerminalQuestionReconnectImageAndPinnedSkills(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-question-ui-")
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
	t.Cleanup(func() { _ = s.Close() })
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "中文 image.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	const answer = "这张图片\n中文 é 🙂 "
	received := make(chan model.Request, 1)
	script := fixture.New(fixture.Step{Events: fixture.Call("ask", "fixture_question", "{}"), Check: func(req model.Request) error {
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" || len(req.Messages[0].Content) != 1 || !strings.Contains(req.Messages[0].Content[0].Text, "ORIGINAL instructions") || strings.Contains(req.Messages[0].Content[0].Text, "UPDATED instructions") {
			return fmt.Errorf("main skill selection did not pin its original user-role body")
		}
		return nil
	}},
		fixture.Step{Events: fixture.Answer("final", "question fixture complete"), Check: func(req model.Request) error {
			if len(req.Messages) != 4 || req.Messages[2].Role != "tool" || req.Messages[3].Role != "user" {
				return fmt.Errorf("answer did not follow completed tool result")
			}
			content := req.Messages[3].Content
			if len(content) != 3 || !strings.Contains(content[0].Text, answer) || content[1].Type != "image" || !bytes.Equal(req.Images[content[1].Attachment].Data, encoded.Bytes()) || !strings.Contains(content[2].Text, "UPDATED instructions") {
				return fmt.Errorf("answer, retained image bytes or selected skill changed")
			}
			received <- req
			return nil
		}})
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: script, Model: "fixture", Protocol: "fixture", Images: true, NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	c.Tools, err = tool.NewRegistry(terminalQuestionTool{core: c})
	if err != nil {
		t.Fatal(err)
	}
	server, err := protocol.Listen(filepath.Join(dir, "core.sock"), c)
	if err != nil {
		t.Fatal(err)
	}
	coreCtx, stopCore := context.WithCancel(context.Background())
	serverDone, coreDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- server.Serve(coreCtx) }()
	go func() { coreDone <- c.Run(coreCtx) }()
	t.Cleanup(func() {
		stopCore()
		for _, done := range []chan error{serverDone, coreDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("question core cleanup timed out")
			}
		}
	})
	client := protocol.Client{Socket: filepath.Join(dir, "core.sock")}
	req, _ := protocol.NewRequest("agent.create", "initial", protocol.CreateAgent{Cwd: dir})
	var created protocol.Accepted
	if err := client.Call(coreCtx, req, &created); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, "skill")
	if err := os.Mkdir(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	publish := func(body string) protocol.ResourceInfo {
		t.Helper()
		if err := os.WriteFile(skillPath, []byte("---\nname: local-review\ndescription: Terminal test\n---\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
		r, _ := protocol.NewRequest("resource.register", protocol.NewID("publish"), protocol.RegisterResource{AgentID: created.AgentID, Kind: "skill", Path: skillPath})
		var a protocol.Accepted
		if err := client.Call(coreCtx, r, &a); err != nil || a.Resource == nil {
			t.Fatal(a, err)
		}
		return *a.Resource
	}
	original := publish("ORIGINAL instructions")
	first := startQuestionTerminal(t, client, created.AgentID)
	first.wait(func(s questionUIState) bool { return s.ready })
	first.write("$local-review")
	first.wait(func(s questionUIState) bool { return s.skillChoices == 1 })
	first.write("\r")
	first.wait(func(s questionUIState) bool {
		return s.text == "" && len(s.skills) == 1 && s.skills[0].Version == original.Version
	})
	updated := publish("UPDATED instructions")
	first.write("\r")
	first.wait(func(s questionUIState) bool { return s.ready && s.questions == 1 })
	first.finish()
	snapshot, err := s.Snapshot(coreCtx, created.AgentID)
	if err != nil || len(snapshot.Questions) != 1 || snapshot.Tasks[0].State != "running" || len(script.Requests()) != 1 {
		t.Fatal("detaching UI stopped or advanced waiting task", snapshot, err)
	}
	questionID := snapshot.Questions[0].ID
	second := startQuestionTerminal(t, client, created.AgentID)
	second.wait(func(s questionUIState) bool { return s.ready && s.questions == 1 })
	const mainDraft = "继续后的主草稿 "
	second.paste(mainDraft)
	second.write("/answer\r")
	second.wait(func(s questionUIState) bool { return s.active == questionID && s.main == mainDraft && s.text == "" })
	second.paste(answer)
	second.write("$local-review")
	second.wait(func(s questionUIState) bool { return s.skillChoices == 1 })
	second.write("\r")
	second.wait(func(s questionUIState) bool {
		return s.text == answer && len(s.skills) == 1 && s.skills[0].Version == updated.Version
	})
	second.write("/image\t")
	second.wait(func(s questionUIState) bool { return s.text == "/image " && s.held == answer })
	second.paste("'" + path + "'")
	second.write("\r")
	state := second.wait(func(s questionUIState) bool { return s.text == answer && !s.loading && len(s.images) == 1 })
	imageID := state.images[0]
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}
	second.write("\r")
	second.wait(func(s questionUIState) bool {
		return !s.pending && s.active == "" && s.text == mainDraft && len(s.images) == 0 && s.questions == 0
	})
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not continue with answer and image")
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		snapshot, err = s.Snapshot(coreCtx, created.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Tasks[0].State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("answered task did not finish", snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
	second.finish()
	q, err := s.Question(coreCtx, protocol.QuestionTarget{AgentID: created.AgentID, ID: questionID})
	if err != nil || q.Answer != answer || q.DeliveredSeq == 0 || !reflect.DeepEqual(q.Attachments, []string{imageID}) || len(snapshot.Tasks) != 1 || len(script.Requests()) != 2 {
		t.Fatal("answer created another task, lost data or repeated model work", q, err)
	}
	if len(q.Skills) != 1 || q.Skills[0].Version != updated.Version || len(snapshot.Tasks[0].Skills) != 1 || snapshot.Tasks[0].Skills[0].Version != original.Version {
		t.Fatal("durable skill versions lost")
	}
	events, err := s.Events(coreCtx, protocol.Cursor{AgentID: created.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, event := range events.Events {
		counts[event.Kind]++
	}
	for _, kind := range []string{"question.asked", "question.answered", "question.delivered", "tool.dispatching"} {
		if counts[kind] != 1 {
			t.Fatal("question replayed after reconnect", kind, counts[kind])
		}
	}
}
