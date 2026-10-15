package render

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

type imageLoad struct {
	request protocol.Request
	draft   string
	attempt uint64
	waiting bool
}
type imageLoadedMsg struct {
	load     imageLoad
	accepted protocol.Accepted
	image    protocol.Attachment
	err      error
}
type imagesPanel struct {
	generation uint64
	page       protocol.AttachmentPage
	waiting    bool
	err        error
}
type imagesPageMsg struct {
	generation uint64
	page       protocol.AttachmentPage
	err        error
}

func (m *Shell) editDraft() *Composer {
	if m.commandDraft != nil {
		return m.commandDraft
	}
	return &m.input
}
func (m *Shell) findDraft(id string) *Composer {
	if m.input.id == id {
		return &m.input
	}
	if m.commandDraft != nil && m.commandDraft.id == id {
		return m.commandDraft
	}
	if m.mainDraft != nil && m.mainDraft.id == id {
		return m.mainDraft
	}
	for key, draft := range m.answerDrafts {
		if key != m.activeQuestion && draft.input.id == id {
			return &draft.input
		}
	}
	return nil
}
func (m *Shell) setImageLoad(id string, load *imageLoad) {
	next := make(map[string]imageLoad, len(m.imageLoads)+1)
	for key, value := range m.imageLoads {
		if key != id {
			next[key] = value
		}
	}
	if load != nil {
		next[id] = *load
	}
	m.imageLoads = next
}

func loadImageCmd(ctx context.Context, client Client, load imageLoad) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		msg := imageLoadedMsg{load: load}
		if load.request.Method == "attachment.import" {
			msg.err = client.Retry(ctx, load.request, &msg.accepted, 3, 100*time.Millisecond)
			if msg.accepted.Attachment != nil {
				msg.image = *msg.accepted.Attachment
			}
		} else {
			msg.err = client.Retry(ctx, load.request, &msg.image, 3, 100*time.Millisecond)
		}
		return msg
	}
}

func (m *Shell) imageCommand(name string, args []string) (tea.Cmd, error) {
	draft := m.editDraft()
	switch name {
	case "images":
		return m.openImages(""), nil
	case "image-remove":
		if len(args) != 1 {
			return nil, protocol.E("invalid_params", "use /image-remove IMAGE; add a space to choose")
		}
		return nil, draft.removeImage(args[0])
	case "image", "image-use":
		if old, exists := m.imageLoads[draft.id]; exists {
			if name != "image" || len(args) != 0 || old.waiting {
				return nil, protocol.E("request_pending", "image load is unresolved; /image retries its original request")
			}
			old.attempt++
			old.waiting = true
			m.setImageLoad(draft.id, &old)
			return loadImageCmd(m.ctx, m.client, old), nil
		}
		if len(args) != 1 || args[0] == "" {
			return nil, protocol.E("invalid_params", "use /image PATH or /images to choose a saved image")
		}
		if name == "image-use" {
			for _, image := range draft.images {
				if image.ID == args[0] {
					return nil, nil
				}
			}
		}
		if len(draft.images) >= protocol.MaxInputImages {
			return nil, protocol.E("attachment_limit", "draft supports up to 8 images; remove one with /image-remove")
		}
		method, op := "attachment.import", protocol.NewID("ui-image")
		var params any = protocol.ImportAttachment{AgentID: m.agent, Path: args[0]}
		if name == "image-use" {
			method, op = "attachment.inspect", ""
			params = protocol.AttachmentTarget{AgentID: m.agent, ID: args[0]}
		}
		req, err := protocol.NewRequest(method, op, params)
		if err != nil {
			return nil, err
		}
		load := imageLoad{request: req, draft: draft.id, attempt: 1, waiting: true}
		draft.revision++ // A delayed task acceptance must preserve this new action.
		m.setImageLoad(draft.id, &load)
		m.issues.resolve("image-load")
		return loadImageCmd(m.ctx, m.client, load), nil
	}
	return nil, protocol.E("invalid_params", "unknown image command")
}

func validImageInfo(image protocol.Attachment, agent string) bool {
	digest, err := hex.DecodeString(image.SHA256)
	return err == nil && len(digest) == 32 && image.AgentID == agent && image.ID != "" && len(image.ID) <= 128 && image.Name != "" && len(image.Name) <= 255 && image.Bytes > 0 && image.Bytes <= protocol.MaxImageBytes && image.Width > 0 && image.Height > 0 && image.Width <= protocol.MaxImagePixels && image.Height <= protocol.MaxImagePixels && int64(image.Width)*int64(image.Height) <= protocol.MaxImagePixels && (image.MediaType == "image/png" || image.MediaType == "image/jpeg")
}

func (m *Shell) updateImages(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case imageLoadedMsg:
		load, ok := m.imageLoads[msg.load.draft]
		if !ok || load.attempt != msg.load.attempt || load.request.ID != msg.load.request.ID {
			return true, nil
		}
		if msg.err == nil {
			valid := validImageInfo(msg.image, m.agent)
			if load.request.Method == "attachment.import" {
				valid = valid && msg.accepted.OperationID == load.request.OperationID && msg.accepted.AgentID == m.agent && msg.accepted.State == "accepted"
			} else {
				var p protocol.AttachmentTarget
				valid = valid && protocol.StrictDecode(load.request.Params, &p) == nil && p.ID == msg.image.ID
			}
			if !valid {
				msg.err = protocol.E("transport_unknown", "image result identity or metadata changed; /image retries the original request")
			}
		}
		if msg.err != nil {
			load.waiting = false
			var known *protocol.Error
			if errors.As(msg.err, &known) && known.Data.Kind != "transport_unknown" && known.Data.Kind != "operation_unknown" && known.Data.Kind != "unavailable" {
				m.setImageLoad(load.draft, nil)
			} else {
				m.setImageLoad(load.draft, &load)
			}
			m.issues.raise("image-load", msg.err)
			return true, nil
		}
		m.setImageLoad(load.draft, nil)
		m.issues.resolve("image-load")
		if draft := m.findDraft(load.draft); draft != nil {
			m.issues.raise("image-load", draft.addImage(msg.image))
		}
		return true, nil
	case imagesPageMsg:
		if m.imagesPanel == nil || m.imagesPanel.generation != msg.generation {
			return true, nil
		}
		if msg.err == nil {
			seen := make(map[string]bool)
			if len(msg.page.Items) > 20 {
				msg.err = protocol.E("invalid_state", "image page exceeds its requested limit")
			}
			for _, image := range msg.page.Items {
				if !validImageInfo(image, m.agent) || seen[image.ID] {
					msg.err = protocol.E("invalid_state", "image page contains invalid identities or metadata")
				}
				seen[image.ID] = true
			}
			if msg.page.Next != "" && (len(msg.page.Items) == 0 || msg.page.Items[len(msg.page.Items)-1].ID != msg.page.Next) {
				msg.err = protocol.E("invalid_state", "image page has an invalid cursor")
			}
		}
		m.imagesPanel.waiting, m.imagesPanel.err = false, msg.err
		m.issues.raise("image-list", msg.err)
		if msg.err == nil {
			m.imagesPanel.page = msg.page
		}
		return true, nil
	}
	return false, nil
}

func (m *Shell) openImages(after string) tea.Cmd {
	m.closePanel()
	m.issues.resolve("image-list")
	generation := m.imagesGeneration
	m.imagesPanel = &imagesPanel{generation: generation, waiting: true}
	ctx, client, agent := m.ctx, m.client, m.agent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, _ := protocol.NewRequest("attachment.list", "", protocol.AttachmentList{AgentID: agent, After: after, Limit: 20})
		var page protocol.AttachmentPage
		err := client.Retry(ctx, req, &page, 3, 100*time.Millisecond)
		return imagesPageMsg{generation, page, err}
	}
}

func imageTitle(image protocol.Attachment, separator string) string {
	return imageName(image.Name) + separator + fmt.Sprintf("%dx%d", image.Width, image.Height)
}

func imageName(name string) string {
	// File names may contain newlines or tabs; each item still owns one row.
	return singleLine(name)
}

func (m *Shell) refreshImagePalette() {
	if m.palette.entry != "image-use" && m.palette.entry != "image-remove" {
		return
	}
	index := m.palette.index
	m.palette = commandPalette{}
	m.syncPalette(tea.KeyPressMsg{})
	m.palette.index = min(index, max(0, len(m.palette.items)-1))
}

func (m Shell) imageRows(f *frame, rows int) {
	p := m.imagesPanel
	f.body = append(f.body, "Images")
	if p.waiting {
		f.body = append(f.body, "...")
		return
	}
	if p.err != nil {
		f.body = append(f.body, "unavailable")
		return
	}
	if len(p.page.Items) == 0 {
		f.body = append(f.body, "empty")
		return
	}
	selected := make(map[string]bool)
	for _, image := range m.editDraft().images {
		selected[image.ID] = true
	}
	limit := rows
	if p.page.Next != "" {
		limit--
	}
	start := min(m.scroll, max(0, len(p.page.Items)-(limit-1)))
	for _, image := range p.page.Items[start:] {
		if len(f.body) >= limit {
			break
		}
		mark, name := "  ", "image-use"
		if selected[image.ID] {
			mark, name = "* ", "image-remove"
		}
		line := ansi.Truncate(mark+imageTitle(image, m.theme.separator()), m.width, "...")
		f.targets = append(f.targets, hitTarget{x: 0, y: len(f.body), width: ansi.StringWidth(line), call: invocation{name: name, args: []string{image.ID}}, candidate: -1})
		f.body = append(f.body, line)
	}
	if p.page.Next != "" {
		f.targets = append(f.targets, hitTarget{x: 0, y: len(f.body), width: 5, call: invocation{name: "next"}, candidate: -1})
		f.body = append(f.body, "/next")
	}
}

func (m Shell) imageStrip() string {
	draft := m.editDraft()
	load, pending := m.imageLoads[draft.id]
	if len(draft.images) == 0 && !pending {
		return ""
	}
	text := fmt.Sprintf("img %d", len(draft.images))
	if pending {
		if load.waiting {
			text += " ..."
		} else {
			text += " ?"
		}
	}
	if len(draft.images) > 0 {
		var names []string
		for _, image := range draft.images {
			names = append(names, imageName(image.Name))
		}
		text += m.theme.separator() + strings.Join(names, ", ")
	}
	return ansi.Truncate(text, m.width, "...")
}
