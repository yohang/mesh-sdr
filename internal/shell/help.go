package shell

import (
	"net/url"
	"strconv"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrInvalidHelpLink is returned for a help link that is not an absolute
// http or https URL.
var ErrInvalidHelpLink = shared.NewError(shared.KindInvalid, "invalid_help_link",
	"the help link must be an http or https URL")

// HelpLink is the admin-set help and documentation URL (UI-002,
// receiver.help_url). The zero value is no help link: the help entries are
// hidden.
type HelpLink struct {
	url string
}

// NewHelpLink validates s: empty means no help link, otherwise an absolute
// http or https URL with a host.
func NewHelpLink(s string) (HelpLink, error) {
	if s == "" {
		return HelpLink{}, nil
	}

	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return HelpLink{}, ErrInvalidHelpLink.WithDetail("invalid help link " + strconv.Quote(s))
	}

	return HelpLink{url: u.String()}, nil
}

// IsZero reports whether no help link is set.
func (h HelpLink) IsZero() bool { return h.url == "" }

// String returns the URL, or "".
func (h HelpLink) String() string { return h.url }

// ErrInvalidShortcutSet is returned for a shortcut set other than default
// or off.
var ErrInvalidShortcutSet = shared.NewError(shared.KindInvalid, "invalid_shortcut_set",
	"shortcut set must be default or off")

// ShortcutSet is the admin-chosen keyboard shortcut set (ui.shortcut_set):
// the default set, or off to disable single-key shortcuts. The zero value
// is the default set.
type ShortcutSet struct {
	off bool
}

// NewShortcutSet validates s.
func NewShortcutSet(s string) (ShortcutSet, error) {
	switch s {
	case "default":
		return ShortcutSet{}, nil
	case "off":
		return ShortcutSet{off: true}, nil
	default:
		return ShortcutSet{}, ErrInvalidShortcutSet.WithDetail("invalid shortcut set " + strconv.Quote(s))
	}
}

// Enabled reports whether single-key shortcuts are on.
func (s ShortcutSet) Enabled() bool { return !s.off }

// String returns default or off.
func (s ShortcutSet) String() string {
	if s.off {
		return "off"
	}

	return "default"
}
