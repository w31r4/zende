package render

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

func imageInfo(id string) protocol.Attachment {
	return protocol.Attachment{ID: id, AgentID: "a", Name: "中文 image.png", MediaType: "image/png", SHA256: strings.Repeat("a", 64), Bytes: 128, Width: 2, Height: 3}
}

func addDraftImage(t *testing.T, c *Composer, image protocol.Attachment) {
	t.Helper()
	if err := c.addImage(image); err != nil {
		t.Fatal(err)
	}
}

type imageTestClient struct {
	requests []protocol.Request
	handle   func(protocol.Request, any) error
}

func (c *imageTestClient) Retry(_ context.Context, req protocol.Request, target any, _ int, _ time.Duration) error {
	c.requests = append(c.requests, req)
	return c.handle(req, target)
}
func (*imageTestClient) Watch(context.Context, protocol.Cursor, func(protocol.Event) error) error {
	panic("unexpected watch")
}

func TestComposerImagesAreIndependentAndCommandsOnlyConsumeText(t *testing.T) {
	c := NewComposer()
	c.SetValue("中文\ntext")
	addDraftImage(t, &c, imageInfo("one"))
	other := c
	addDraftImage(t, &other, imageInfo("two"))
	if err := other.removeImage("one"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.imageIDs(), []string{"one"}) || !reflect.DeepEqual(other.imageIDs(), []string{"two"}) {
		t.Fatal("shared image slice")
	}
	stamp := c.stamp()
	c.ResetText()
	if c.Value() != "" || c.stamp() == stamp || c.id != stamp.id || len(c.images) != 1 {
		t.Fatal("text reset lost image or identity")
	}
	c.Reset()
	if c.id == stamp.id || len(c.images) != 0 {
		t.Fatal("new draft kept old identity or image")
	}
	m := commandShell(t)
	addDraftImage(t, &m.input, imageInfo("one"))
	m = typeText(t, m, "中文 draft /help")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.help || m.input.Value() != "中文 draft " || !reflect.DeepEqual(m.input.imageIDs(), []string{"one"}) {
		t.Fatal("inline command consumed image draft")
	}
	m = typeText(t, m, "/image-remove")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !m.paletteActive() || len(m.palette.items) != 1 || m.commandDraft == nil {
		t.Fatal("missing held-draft image candidates")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != "中文 draft " || len(m.input.images) != 0 || m.pending != nil {
		t.Fatal("remove lost text or mutated core")
	}
	large := imageInfo("large")
	large.Bytes = protocol.MaxImageBytes
	for i := range 3 {
		large.ID = fmt.Sprint(i)
		addDraftImage(t, &c, large)
	}
	large.ID = "overflow"
	if c.addImage(large) == nil || len(c.images) != 3 {
		t.Fatal("unbounded draft bytes")
	}
}

func TestImageOnlySubmitAndLateAcceptanceKeepNewDraft(t *testing.T) {
	for _, mode := range []string{"same", "held", "edited", "aba", "new-image", "importing", "reset"} {
		t.Run(mode, func(t *testing.T) {
			m := commandShell(t)
			addDraftImage(t, &m.input, imageInfo("one"))
			m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd == nil || m.pending == nil {
				t.Fatal("image-only submission ignored")
			}
			req := *m.pending
			var p protocol.Submit
			if protocol.StrictDecode(req.Params, &p) != nil || p.Input != "" || !reflect.DeepEqual(p.Attachments, []string{"one"}) {
				t.Fatal("image-only request", p)
			}
			original := m.input.id
			switch mode {
			case "held":
				draft := m.input
				m.commandDraft = &draft
				m.input = NewComposer()
				m.input.SetValue("/images")
			case "edited":
				m.input.SetValue("next draft")
			case "aba":
				m.input.SetValue("temporary")
				m.input.SetValue("")
			case "new-image":
				addDraftImage(t, &m.input, imageInfo("two"))
			case "importing":
				if _, err := m.imageCommand("image", []string{"next.png"}); err != nil {
					t.Fatal(err)
				}
			case "reset":
				m.input.Reset()
				addDraftImage(t, &m.input, imageInfo("two"))
			}
			m, _ = update(t, m, acceptedMsg{Request: req, Accepted: protocol.Accepted{OperationID: req.OperationID, AgentID: m.agent}})
			if m.pending != nil {
				t.Fatal("pending after acceptance")
			}
			if mode == "same" || mode == "held" {
				draft := m.editDraft()
				if draft.id == original || len(draft.images) != 0 {
					t.Fatal("accepted unchanged draft not reset")
				}
				if mode == "held" && m.input.Value() != "/images" {
					t.Fatal("acceptance consumed command editor")
				}
			} else if len(m.input.images) == 0 {
				t.Fatal("acceptance erased edited draft")
			}
		})
	}
}

func TestImageImportRetryUsesOriginalOperationAndDraft(t *testing.T) {
	m := commandShell(t)
	client := &imageTestClient{}
	m.client = client
	m.input.SetValue("中文 draft")
	owner := m.input.id
	cmd, err := m.imageCommand("image", []string{"中文 image.png"})
	if err != nil || cmd == nil || len(client.requests) != 0 {
		t.Fatal("import ran I/O in Update", err)
	}
	load := m.imageLoads[owner]
	client.handle = func(req protocol.Request, _ any) error {
		if req.Method != "attachment.import" || req.OperationID == "" {
			t.Fatal("missing import identity")
		}
		return protocol.E("transport_unknown", "receipt lost")
	}
	m, _ = update(t, m, cmd())
	if m.imageLoads[owner].waiting || len(m.input.images) != 0 {
		t.Fatal("unknown result attached or still waiting")
	}
	if cmd, err = m.imageCommand("image", []string{"different.png"}); cmd != nil || err == nil {
		t.Fatal("unknown import replaced")
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || m.pending != nil || m.input.Value() != "中文 draft" {
		t.Fatal("submitted before image receipt")
	}
	cmd, err = m.imageCommand("image", nil)
	if err != nil || cmd == nil {
		t.Fatal("retry unavailable", err)
	}
	if !reflect.DeepEqual(load.request, m.imageLoads[owner].request) {
		t.Fatal("retry changed import request")
	}
	// A previous attempt's reply must not complete the newer attempt.
	m, _ = update(t, m, imageLoadedMsg{load: load, err: protocol.E("invalid_params", "old failure")})
	if !m.imageLoads[owner].waiting {
		t.Fatal("stale attempt replaced current load")
	}
	client.handle = func(req protocol.Request, target any) error {
		info := imageInfo("saved")
		*target.(*protocol.Accepted) = protocol.Accepted{OperationID: req.OperationID, AgentID: m.agent, State: "accepted", Attachment: &info}
		return nil
	}
	// The main draft can be held while its temporary command editor is active.
	draft := m.input
	m.commandDraft = &draft
	m.input = NewComposer()
	m.input.SetValue("/help")
	m, _ = update(t, m, cmd())
	if len(m.imageLoads) != 0 || len(m.input.images) != 0 || !reflect.DeepEqual(m.commandDraft.imageIDs(), []string{"saved"}) || m.commandDraft.Value() != "中文 draft" {
		t.Fatal("result attached to wrong editor")
	}
	if len(client.requests) != 2 || !reflect.DeepEqual(client.requests[0], client.requests[1]) {
		t.Fatal("retry changed transport params")
	}
	m.restoreCommandDraft()
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	req := *m.pending
	m, _ = update(t, m, acceptedMsg{Request: req, Err: protocol.E("unsupported_capability", "model does not accept images")})
	if m.pending != nil || m.input.Value() != "中文 draft" || len(m.input.images) != 1 || m.err == nil {
		t.Fatal("unsupported model lost draft")
	}
}

func TestImageResultValidationAndOrphanedDraft(t *testing.T) {
	for _, kind := range []string{"owner", "digest", "bytes", "size", "operation", "state", "orphan", "reject"} {
		t.Run(kind, func(t *testing.T) {
			m := commandShell(t)
			if _, err := m.imageCommand("image", []string{"image.png"}); err != nil {
				t.Fatal(err)
			}
			load := m.imageLoads[m.input.id]
			info := imageInfo("one")
			msg := imageLoadedMsg{load: load, image: info, accepted: protocol.Accepted{AgentID: m.agent, State: "accepted", OperationID: load.request.OperationID}}
			switch kind {
			case "owner":
				msg.image.AgentID = "other"
			case "digest":
				msg.image.SHA256 = "bad"
			case "bytes":
				msg.image.Bytes = protocol.MaxImageBytes + 1
			case "size":
				msg.image.Width = protocol.MaxImagePixels
			case "operation":
				msg.accepted.OperationID = "other"
			case "state":
				msg.accepted.State = "unknown"
			case "orphan":
				m.input.Reset()
			case "reject":
				msg.err = protocol.E("invalid_params", "not an image")
			}
			m, _ = update(t, m, msg)
			if len(m.input.images) != 0 {
				t.Fatal("wrong result attached")
			}
			if kind == "orphan" || kind == "reject" {
				if len(m.imageLoads) != 0 {
					t.Fatal("definite result not retired")
				}
			} else if len(m.imageLoads) != 1 || m.imageLoads[load.draft].waiting {
				t.Fatal("invalid receipt not kept for verification")
			}
		})
	}
}

func TestImageCatalogGenerationCandidatesAndReadOnlySelection(t *testing.T) {
	m := commandShell(t)
	client := &imageTestClient{}
	m.client = client
	cmd := m.openImages("")
	generation := m.imagesPanel.generation
	client.handle = func(req protocol.Request, target any) error {
		if req.Method != "attachment.list" || req.OperationID != "" {
			t.Fatal(req.Method)
		}
		var p protocol.AttachmentList
		if protocol.StrictDecode(req.Params, &p) != nil || p.Limit != 20 || p.After != "" {
			t.Fatal(p)
		}
		*target.(*protocol.AttachmentPage) = protocol.AttachmentPage{Items: []protocol.Attachment{imageInfo("one"), imageInfo("two")}, Next: "two"}
		return nil
	}
	m = typeText(t, m, "/image-use ")
	m, _ = update(t, m, cmd())
	if !m.paletteActive() || len(m.palette.items) != 2 {
		t.Fatal("async page did not refresh candidate list")
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.input.Value() != "" {
		t.Fatal("selection not dispatched")
	}
	client.handle = func(req protocol.Request, target any) error {
		var p protocol.AttachmentTarget
		if req.Method != "attachment.inspect" || req.OperationID != "" || protocol.StrictDecode(req.Params, &p) != nil || p.ID != "two" {
			t.Fatal("selection mutated core", req.Method, p)
		}
		*target.(*protocol.Attachment) = imageInfo("two")
		return nil
	}
	m, _ = update(t, m, cmd())
	if !reflect.DeepEqual(m.input.imageIDs(), []string{"two"}) {
		t.Fatal("missing selected image")
	}
	cmd, err := m.invoke(invocation{name: "next"})
	if err != nil || cmd == nil || m.imagesPanel.generation == generation {
		t.Fatal("paging reused generation", err)
	}
	client.handle = func(req protocol.Request, target any) error {
		var p protocol.AttachmentList
		if protocol.StrictDecode(req.Params, &p) != nil || p.After != "two" {
			t.Fatal("wrong next cursor", p)
		}
		*target.(*protocol.AttachmentPage) = protocol.AttachmentPage{}
		return nil
	}
	m, _ = update(t, m, cmd())
	m, _ = update(t, m, imagesPageMsg{generation: generation, page: protocol.AttachmentPage{Items: []protocol.Attachment{imageInfo("stale")}}})
	if len(m.imagesPanel.page.Items) != 0 {
		t.Fatal("stale page replaced current one")
	}
	for _, target := range m.layout().targets {
		if target.call.name == "images" && target.y != len(m.layout().body) {
			t.Fatal("strip click misplaced")
		}
		if _, ok := lookupCommand(target.call.name); !ok {
			t.Fatal("click has no command", target)
		}
	}
	m.openProcesses("")
	if m.imagesPanel != nil || m.panel == nil {
		t.Fatal("process panel obscured by image catalog")
	}
}

func TestImageCatalogRejectsInvalidPagesAndStaleClosedPanel(t *testing.T) {
	for _, mode := range []string{"oversize", "duplicate", "owner", "cursor", "empty-cursor"} {
		t.Run(mode, func(t *testing.T) {
			m := commandShell(t)
			m.openImages("")
			page := protocol.AttachmentPage{Items: []protocol.Attachment{imageInfo("one")}}
			switch mode {
			case "oversize":
				for i := range 20 {
					page.Items = append(page.Items, imageInfo(fmt.Sprint(i)))
				}
			case "duplicate":
				page.Items = append(page.Items, imageInfo("one"))
			case "owner":
				page.Items[0].AgentID = "other"
			case "cursor":
				page.Next = "missing"
			case "empty-cursor":
				page.Items = nil
				page.Next = "missing"
			}
			msg := imagesPageMsg{generation: m.imagesPanel.generation, page: page}
			m, _ = update(t, m, msg)
			if m.imagesPanel.err == nil || m.imagesPanel.waiting || len(m.imagesPanel.page.Items) != 0 {
				t.Fatal("bad page accepted")
			}
			m.closePanel()
			m, _ = update(t, m, msg)
			if m.imagesPanel != nil {
				t.Fatal("closed panel reopened by stale page")
			}
		})
	}
}

func TestImageClickAndCommandHaveSameEffectWithoutConsumingDraft(t *testing.T) {
	makeModel := func() Shell {
		m := commandShell(t)
		m.input.SetValue("中文 draft")
		addDraftImage(t, &m.input, imageInfo("selected"))
		m.openImages("")
		m.imagesPanel.waiting = false
		m.imagesPanel.page = protocol.AttachmentPage{Items: []protocol.Attachment{imageInfo("selected"), imageInfo("other")}, Next: "other"}
		return m
	}
	base := makeModel()
	seen := make(map[string]bool)
	for _, target := range base.layout().targets {
		mouse, _ := update(t, makeModel(), tea.MouseClickMsg{X: target.x, Y: target.y, Button: tea.MouseLeft})
		typed := makeModel()
		draft := typed.input
		typed.commandDraft = &draft
		typed.input = NewComposer()
		text := "/" + target.call.name
		for _, arg := range target.call.args {
			text += " '" + arg + "'"
		}
		typed.input.SetValue(text)
		typed, _ = update(t, typed, tea.KeyPressMsg{Code: tea.KeyEnter})
		if !reflect.DeepEqual(mouse.input.imageIDs(), typed.input.imageIDs()) || mouse.input.Value() != "中文 draft" || typed.input.Value() != "中文 draft" || mouse.imagesPanel.waiting != typed.imagesPanel.waiting {
			t.Fatal("click/command changed draft differently", target.call)
		}
		left, right := mouse.imageLoads[mouse.input.id], typed.imageLoads[typed.input.id]
		if left.request.Method != right.request.Method || string(left.request.Params) != string(right.request.Params) || mouse.pending != nil || typed.pending != nil {
			t.Fatal("click/command transport differs", target.call)
		}
		seen[target.call.name] = true
	}
	for _, name := range []string{"images", "image-use", "image-remove", "next"} {
		if !seen[name] {
			t.Fatal("missing covered target", name)
		}
	}
}

func TestImagesLayoutIsConditionalBoundedAndKeyboardReachable(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.ASCII, colorprofile.ANSI, colorprofile.TrueColor} {
		for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
			m := commandShell(t)
			m, _ = update(t, m, tea.ColorProfileMsg{Profile: profile})
			m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			if m.layout().images != "" || strings.Contains(ansi.Strip(m.View().Content), "img 0") {
				t.Fatal("idle attachment chrome")
			}
			info := imageInfo("one")
			info.Name = "中文\n é🙂\t\x1b]52;c;bad\a\x1b[2J.png"
			addDraftImage(t, &m.input, info)
			m.openImages("")
			m.imagesPanel.waiting = false
			m.imagesPanel.page = protocol.AttachmentPage{Items: []protocol.Attachment{info, imageInfo("two")}, Next: "two"}
			m = typeText(t, m, "/image-remove ")
			m.report(protocol.E("unsupported_capability", "model does not accept images"))
			f, v := m.layout(), m.View()
			lines := strings.Split(ansi.Strip(v.Content), "\n")
			if len(lines) != size[1] || v.Cursor == nil || v.Cursor.Y != len(f.body)+1 || len(f.candidates) != 1 {
				t.Fatal("image strip moved input/candidates", size, len(lines), v.Cursor)
			}
			if strings.Contains(v.Content, "\x1b]52;") || strings.Contains(v.Content, "\x1b[2J") {
				t.Fatal("filename escape executed")
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatal("image overflow", size, line)
				}
			}
			if strings.TrimRight(lines[len(f.body)], " ") != f.images || strings.TrimRight(lines[len(f.body)+3], " ") != f.candidates[0] || strings.TrimRight(lines[size[1]-1], " ") != f.status {
				t.Fatal("attachment/palette/status ordering")
			}
			for _, target := range f.targets {
				if _, ok := lookupCommand(target.call.name); !ok {
					t.Fatal("unreachable click", target)
				}
			}
			m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if len(m.input.images) != 0 || m.pending != nil || m.layout().images != "" {
				t.Fatal("keyboard remove failed")
			}
		}
	}
}

func TestTranscriptKeepsBoundedImageReferencesWithoutImageBytes(t *testing.T) {
	p := Transcript{Agent: protocol.Agent{ID: "a"}}
	event := func(seq int64, refs []string) protocol.Event {
		payload, _ := json.Marshal(protocol.Task{ID: "task", AgentID: "a", Attachments: refs})
		return protocol.Event{Version: protocol.Version, AgentID: "a", TaskID: "task", Seq: seq, Kind: "task.accepted", Payload: payload}
	}
	for _, refs := range [][]string{{""}, {"one", "one"}, {strings.Repeat("x", 129)}, make([]string, 9)} {
		if p.Apply(event(1, refs)) == nil || p.Cursor != 0 || len(p.Entries) != 0 || p.bytes != 0 {
			t.Fatal("invalid refs changed projection")
		}
	}
	if err := p.Apply(event(1, []string{"one", "two"})); err != nil {
		t.Fatal(err)
	}
	if p.bytes != 6 || !strings.Contains(strings.Join(p.Lines(80, 20, 0, false), "\n"), "> [img 2]") {
		t.Fatal("image-only history blank or unaccounted")
	}
	if err := p.Apply(event(2, []string{"one"})); err != nil || p.bytes != 3 {
		t.Fatal("image replacement double counted", err, p.bytes)
	}
	for i := range transcriptEntries {
		p.entry(protocol.Event{Seq: int64(i + 3)}, fmt.Sprint(i), "notice")
	}
	p.trim()
	if p.bytes != 0 || p.DroppedThrough != 2 {
		t.Fatal("eviction retained image budget", p.bytes, p.DroppedThrough)
	}
}
