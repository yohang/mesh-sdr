package domain

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// CapabilityStatus is the probe outcome of one capability (§7.1).
type CapabilityStatus string

// Capability statuses.
const (
	CapabilityOK       CapabilityStatus = "ok"
	CapabilityMissing  CapabilityStatus = "missing"
	CapabilityTooOld   CapabilityStatus = "too_old"
	CapabilityUntested CapabilityStatus = "untested"
	CapabilityError    CapabilityStatus = "error"
)

func (s CapabilityStatus) valid() bool {
	return slices.Contains([]CapabilityStatus{CapabilityOK, CapabilityMissing, CapabilityTooOld, CapabilityUntested, CapabilityError}, s)
}

var capabilityKeyPattern = regexp.MustCompile(`^(tool|driver|mode|feature|codec|cap):[A-Za-z0-9._:+-]{1,56}$`)

// Capability is one namespaced capability of a node: tool:<name>,
// driver:<type>, mode:<id>, feature:<flag>, codec:<name>.
type Capability struct {
	key       string
	available bool
	status    CapabilityStatus
	version   string
	detail    json.RawMessage
	err       string
}

// NewCapability validates a capability row.
func NewCapability(key string, available bool, status CapabilityStatus, version string, detail json.RawMessage, errMsg string) (Capability, error) {
	if !capabilityKeyPattern.MatchString(key) {
		return Capability{}, ErrInvalidCapabilityReport.WithDetail("invalid capability key " + strconv.Quote(key))
	}

	if !status.valid() || len(version) > 64 || len(errMsg) > 512 {
		return Capability{}, ErrInvalidCapabilityReport.WithDetail("invalid capability " + key)
	}

	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}

	if !json.Valid(detail) {
		return Capability{}, ErrInvalidCapabilityReport.WithDetail("invalid detail of " + key)
	}

	return Capability{key: key, available: available, status: status, version: version, detail: detail, err: errMsg}, nil
}

// Key returns the namespaced key.
func (c Capability) Key() string { return c.key }

// Available reports whether the capability can be used.
func (c Capability) Available() bool { return c.available }

// Status returns the probe status.
func (c Capability) Status() CapabilityStatus { return c.status }

// Version returns the detected version.
func (c Capability) Version() string { return c.version }

// Detail returns the probe evidence (JSON object).
func (c Capability) Detail() json.RawMessage { return c.detail }

// Error returns the probe error.
func (c Capability) Error() string { return c.err }

// CapabilityReport is the last full capability report of a node; it
// replaces the previous one as a whole.
type CapabilityReport struct {
	node           NodeID
	hash           string
	productVersion string
	protocols      []string
	platform       json.RawMessage
	document       json.RawMessage
	reportedAt     time.Time
	caps           []Capability
}

// NewCapabilityReport validates a report. Capability keys must be unique.
func NewCapabilityReport(node NodeID, hash, productVersion string, protocols []string, platform, document json.RawMessage,
	reportedAt time.Time, caps []Capability,
) (CapabilityReport, error) {
	if hash == "" || len(hash) > 64 || len(productVersion) > 32 || !json.Valid(platform) || !json.Valid(document) {
		return CapabilityReport{}, ErrInvalidCapabilityReport
	}

	seen := map[string]bool{}

	for _, c := range caps {
		if seen[c.key] {
			return CapabilityReport{}, ErrInvalidCapabilityReport.WithDetail("duplicate capability " + c.key)
		}

		seen[c.key] = true
	}

	if protocols == nil {
		protocols = []string{}
	}

	return CapabilityReport{
		node: node, hash: hash, productVersion: productVersion, protocols: slices.Clone(protocols),
		platform: platform, document: document, reportedAt: ms(reportedAt), caps: slices.Clone(caps),
	}, nil
}

// Node returns the reporting node.
func (r CapabilityReport) Node() NodeID { return r.node }

// Hash identifies the report content.
func (r CapabilityReport) Hash() string { return r.hash }

// ProductVersion returns the node product version.
func (r CapabilityReport) ProductVersion() string { return r.productVersion }

// Protocols returns the protocols the node speaks.
func (r CapabilityReport) Protocols() []string { return slices.Clone(r.protocols) }

// Platform returns the platform document.
func (r CapabilityReport) Platform() json.RawMessage { return r.platform }

// Document returns the full report as received.
func (r CapabilityReport) Document() json.RawMessage { return r.document }

// ReportedAt returns the hub receive time.
func (r CapabilityReport) ReportedAt() time.Time { return r.reportedAt }

// Capabilities returns the capability rows.
func (r CapabilityReport) Capabilities() []Capability { return slices.Clone(r.caps) }

// CapabilityRepository persists capability reports.
type CapabilityRepository interface {
	// Replace stores r and replaces every capability row of its node.
	Replace(ctx context.Context, r CapabilityReport) error
	// Get returns the last report; ErrCapabilitiesNotReported when none.
	Get(ctx context.Context, id NodeID) (CapabilityReport, error)
}
