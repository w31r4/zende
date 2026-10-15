package render

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The user's Tango copy maps ANSI green to pink. Low-color output must use
// terminal defaults, not approximate the foreground with ANSI green/white.
const foreground, background, cursorColor = "#3BE13B", "#1A1A1A", "#FFFFFF"

// Same sRGB mixes as .design/tui-preview/styles.css: background/border 74/26,
// foreground/background 72/28. These keep secondary text in the green hue.
const inputSurface, secondaryForeground = "#292A29", "#32A932"

type theme struct {
	profile colorprofile.Profile
	ascii   bool
}

func (t theme) base() lipgloss.Style {
	s := lipgloss.NewStyle()
	if t.profile >= colorprofile.ANSI256 {
		s = s.Foreground(lipgloss.Color(foreground)).Background(lipgloss.Color(background))
	}
	return s
}
func (t theme) muted() lipgloss.Style {
	s := t.base()
	if t.profile >= colorprofile.ANSI256 {
		s = s.Foreground(lipgloss.Color(secondaryForeground))
	} else if t.profile > colorprofile.ASCII {
		s = s.Faint(true)
	}
	return s
}
func (t theme) input() lipgloss.Style {
	s := t.base()
	if t.profile >= colorprofile.ANSI256 {
		s = s.Background(lipgloss.Color(inputSurface))
	}
	return s
}
func (t theme) separator() string {
	if t.ascii {
		return " | "
	}
	return " · "
}
func (t theme) modelMark() string {
	if t.ascii {
		return "@"
	}
	return "◇"
}
func (t theme) workMark() string {
	if t.ascii {
		return "bg"
	}
	return "≡"
}
