package layout

import (
	"net/url"
	"strconv"
)

// DateTimeLocal is the value layout of a datetime-local input: the time
// filters of the lists, in UTC, to the minute.
const DateTimeLocal = "2006-01-02T15:04"

// FormatHz formats a frequency in Hz for people: in MHz, kHz or Hz.
func FormatHz(hz int64) string {
	switch {
	case hz >= 1_000_000:
		return strconv.FormatFloat(float64(hz)/1e6, 'f', -1, 64) + " MHz"
	case hz >= 1_000:
		return strconv.FormatFloat(float64(hz)/1e3, 'f', -1, 64) + " kHz"
	default:
		return strconv.FormatInt(hz, 10) + " Hz"
	}
}

// HumanBytes formats a size in bytes for people (KiB, MiB, GiB…).
func HumanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}

	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

// Count is a number of things, singular for one: "1 bookmark", "3
// bookmarks".
func Count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}

	return strconv.Itoa(n) + " " + many
}

// Problem is a problem of a refused form (ErrorSummary): its text, and the
// id of the field it is about ("" for the whole form).
type Problem struct{ ID, Text string }

// Paging is the position of a page in a list (Pager): page Page (from 1)
// of Path, with the filters of Query; More tells that a next page exists.
// Prev and Next are the link texts ("Newer files", "Older files"), Label
// names the navigation.
type Paging struct {
	Label, Prev, Next string
	Page              int
	More              bool
	Path              string
	Query             url.Values
}

// URL is the address of page n with the filters.
func (p Paging) URL(n int) string {
	q := url.Values{}
	for k, v := range p.Query {
		if k != "page" {
			q[k] = v
		}
	}

	if n > 1 {
		q.Set("page", strconv.Itoa(n))
	}

	if len(q) == 0 {
		return p.Path
	}

	return p.Path + "?" + q.Encode()
}
