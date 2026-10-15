package render

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

type skillCatalogMsg struct {
	request    uint64
	generation int64
	items      []protocol.ResourceInfo
	err        error
}

func validSkillInfo(info protocol.ResourceInfo) bool {
	digest, err := hex.DecodeString(info.Version)
	return err == nil && len(digest) == 32 && info.ID != "" && len(info.ID) <= 128 && info.Kind == "skill" && len(info.Name) > 0 && len(info.Name) <= 64 && utf8.ValidString(info.Name) && len(info.Description) > 0 && utf8.RuneCountInString(info.Description) <= 1024 && utf8.ValidString(info.Description) && filepath.IsAbs(info.Source) && len(info.Source) <= 4096 && utf8.ValidString(info.Source) && info.Files > 0 && info.Files <= protocol.MaxResourceFiles && info.Bytes > 0 && info.Bytes <= protocol.MaxResourceBytes
}

// Load metadata only. Each page must belong to one generation; concurrent
// publication restarts the bounded read instead of mixing two catalogs.
func skillCatalogCmd(ctx context.Context, client Client, agent string, request uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		msg := skillCatalogMsg{request: request}
		for range 3 {
			msg.items = nil
			after := ""
			generation := int64(-1)
			for {
				req, _ := protocol.NewRequest("resource.list", "", protocol.ResourceList{AgentID: agent, Kind: "skill", After: after, Limit: 100})
				var page protocol.ResourcePage
				if err := client.Retry(ctx, req, &page, 3, 100*time.Millisecond); err != nil {
					msg.err = err
					return msg
				}
				if generation >= 0 && page.Generation != generation {
					break
				}
				generation = page.Generation
				if generation < 0 || len(page.Items) > 100 || len(msg.items)+len(page.Items) > 512 {
					msg.err = protocol.E("invalid_state", "skill catalog exceeds its limits")
					return msg
				}
				previous := after
				for _, item := range page.Items {
					if !validSkillInfo(item) || item.ID <= previous {
						msg.err = protocol.E("invalid_state", "skill catalog contains invalid metadata or ordering")
						return msg
					}
					previous = item.ID
				}
				if page.Next != "" && (len(page.Items) == 0 || page.Next != previous) {
					msg.err = protocol.E("invalid_state", "skill catalog has an invalid cursor")
					return msg
				}
				msg.items = append(msg.items, page.Items...)
				if page.Next == "" {
					msg.generation = generation
					return msg
				}
				after = page.Next
			}
		}
		msg.err = protocol.E("resource_changed", "skill catalog changed repeatedly; open /skill to retry")
		return msg
	}
}

func (m *Shell) loadSkills(force bool) tea.Cmd {
	if m.skillsWaiting || m.skillsReady && !force && m.skillsGeneration >= m.skillsFloor {
		return nil
	}
	m.skillsWaiting = true
	m.skillsRequest++
	return skillCatalogCmd(m.ctx, m.client, m.agent, m.skillsRequest)
}
func (m *Shell) skillCommand(name string, args []string) (tea.Cmd, error) {
	if len(args) == 0 {
		m.skillPicker = name
		if name == "skill" {
			m.skillsStale = 0
			return m.loadSkills(true), nil
		}
		return nil, nil
	}
	items := m.skills
	if name == "skill-remove" {
		items = m.editDraft().skills
	}
	var chosen *protocol.ResourceInfo
	for _, item := range items {
		if item.ID != args[0] && item.Name != args[0] {
			continue
		}
		if len(args) == 2 && item.Version != args[1] {
			continue
		}
		if chosen != nil {
			return nil, protocol.E("invalid_params", "skill name is ambiguous; choose its source in /skill")
		}
		copy := item
		chosen = &copy
	}
	if chosen == nil {
		return nil, protocol.E("not_found", "skill/version is not in the current list; open /skill to refresh")
	}
	if name == "skill-remove" {
		return nil, m.editDraft().removeSkill(chosen.ID)
	}
	return nil, m.editDraft().addSkill(*chosen)
}

// Applied after typed command consumption, so opening a picker never erases
// the draft it belongs to. This also serves direct mouse/registry invocation.
func (m *Shell) applySkillPicker() {
	if m.skillPicker == "" {
		return
	}
	name := m.skillPicker
	m.skillPicker = ""
	if m.commandDraft == nil {
		saved := m.input
		m.commandDraft = &saved
		m.input = NewComposer()
		m.input.SetWidth(m.width)
		m.input.setTheme(m.theme)
	}
	m.input.SetValue("/" + name + " ")
	m.palette = commandPalette{}
	m.syncPaletteKey()
}
func (m *Shell) refreshSkillPalette() {
	if m.palette.entry != "skill" && m.palette.entry != "skill-remove" {
		return
	}
	// Keep the selected identity, not its former row number, on a refresh.
	var selected []string
	if m.palette.index >= 0 && m.palette.index < len(m.palette.items) {
		selected = m.palette.items[m.palette.index].args
	}
	m.palette = commandPalette{}
	m.syncPaletteKey()
	for i, item := range m.palette.items {
		if len(item.args) > 0 && len(selected) > 0 && item.args[0] == selected[0] {
			m.palette.index = i
			break
		}
	}
}
func (m Shell) skillStrip() string {
	var names []string
	for _, skill := range m.editDraft().skills {
		names = append(names, "$"+imageName(skill.Name))
	}
	return ansi.Truncate(strings.Join(names, " "), m.width, "...")
}
