package render

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"zende.local/harness/internal/protocol"
)

// Composer owns editing only. Submission, resources and task lifetime remain
// outside, so another prompt can reuse it with an independent draft.
type Composer struct {
	textarea.Model
	id       string
	revision uint64
	images   []protocol.Attachment
}

type draftStamp struct {
	id       string
	revision uint64
}

func NewComposer() Composer {
	c := Composer{Model: textarea.New(), id: protocol.NewID("draft")}
	c.Prompt, c.Placeholder = "> ", ""
	c.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "> "
		}
		return "  "
	})
	c.ShowLineNumbers, c.EndOfBufferCharacter = false, ' '
	c.CharLimit = 2 << 20
	c.SetHeight(2)
	c.SetWidth(80)
	c.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	c.SetVirtualCursor(false)
	c.setTheme(theme{})
	c.Focus()
	return c
}
func (c Composer) Update(msg tea.Msg) (Composer, tea.Cmd) {
	var cmd tea.Cmd
	before := c.Value()
	c.Model, cmd = c.Model.Update(msg)
	if before != c.Value() {
		c.revision++
	}
	return c, cmd
}

func (c *Composer) SetValue(text string) { c.Model.SetValue(text); c.revision++ }
func (c *Composer) ResetText()           { c.Model.Reset(); c.revision++ }
func (c *Composer) Reset() {
	c.ResetText()
	c.images = nil
	c.id = protocol.NewID("draft")
}
func (c Composer) stamp() draftStamp { return draftStamp{c.id, c.revision} }
func (c Composer) imageIDs() []string {
	var ids []string
	for _, image := range c.images {
		ids = append(ids, image.ID)
	}
	return ids
}
func (c *Composer) addImage(image protocol.Attachment) error {
	total := image.Bytes
	for _, old := range c.images {
		if old.ID == image.ID {
			if old != image {
				return protocol.E("invalid_state", "saved image identity changed")
			}
			return nil
		}
		total += old.Bytes
	}
	if len(c.images) >= protocol.MaxInputImages || total > protocol.MaxRequestImageBytes {
		return protocol.E("attachment_limit", "draft supports up to 8 images and 12 MiB; saved image remains available through /images")
	}
	c.images = append(append([]protocol.Attachment(nil), c.images...), image)
	c.revision++
	return nil
}
func (c *Composer) removeImage(id string) error {
	images := make([]protocol.Attachment, 0, len(c.images))
	found := false
	for _, image := range c.images {
		if image.ID == id {
			found = true
		} else {
			images = append(images, image)
		}
	}
	if !found {
		return protocol.E("not_found", "image is not selected in this draft")
	}
	c.images = images
	c.revision++
	return nil
}
func (c *Composer) setTheme(t theme) {
	base := t.input()
	selection := lipgloss.NewStyle().Reverse(true)
	cursor := textarea.CursorStyle{Shape: tea.CursorBar}
	if t.profile >= colorprofile.ANSI256 {
		selection = lipgloss.NewStyle().Foreground(lipgloss.Color(background)).Background(lipgloss.Color(cursorColor))
		cursor.Color = lipgloss.Color(cursorColor)
	}
	state := textarea.StyleState{Base: base, Text: base, CursorLine: base, Prompt: base, Placeholder: base, EndOfBuffer: base, Selection: selection}
	c.SetStyles(textarea.Styles{Focused: state, Blurred: state, Cursor: cursor})
}
