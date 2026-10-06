package domain

import (
	"slices"
	"strconv"
	"strings"
)

// ControlProtocol is the control channel protocol both sides must speak.
const ControlProtocol = "rx-ctl.v1"

// SemVer is a product version. Development builds ("dev", "(devel)",
// pseudo-versions, pre-releases of 0.0.0) are flagged as such.
type SemVer struct {
	major, minor, patch int
	dev                 bool
	raw                 string
}

// ParseSemVer reads MAJOR.MINOR.PATCH with an optional "v" prefix and
// -prerelease / +build suffixes.
func ParseSemVer(s string) (SemVer, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if raw == "" || raw == "dev" || raw == "(devel)" {
		return SemVer{dev: true, raw: "dev"}, nil
	}

	core, _, _ := strings.Cut(raw, "+")
	core, pre, hasPre := strings.Cut(core, "-")

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return SemVer{}, ErrInvalidVersion.WithDetail("invalid version " + strconv.Quote(s))
	}

	var n [3]int

	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || (len(p) > 1 && p[0] == '0') {
			return SemVer{}, ErrInvalidVersion.WithDetail("invalid version " + strconv.Quote(s))
		}

		n[i] = v
	}

	// Go pseudo-versions (v0.0.0-20261006…-abcdef) are development builds.
	dev := hasPre && (n == [3]int{} || strings.Count(pre, "-") >= 1 && len(pre) > 14)

	return SemVer{major: n[0], minor: n[1], patch: n[2], dev: dev, raw: raw}, nil
}

// String returns the version.
func (v SemVer) String() string { return v.raw }

// Dev reports a development build.
func (v SemVer) Dev() bool { return v.dev }

// line returns the (major, minor) compatibility line: for 0.y.z, y plays
// the role of the major and z the minor (ADR 0008 Q22).
func (v SemVer) line() (int, int) {
	if v.major == 0 {
		return v.minor, v.patch
	}

	return v.major, v.minor
}

// CompatLevel is the outcome of a version check.
type CompatLevel string

// Compatibility levels.
const (
	CompatOK           CompatLevel = "ok"
	CompatOlder        CompatLevel = "older"
	CompatIncompatible CompatLevel = "incompatible"
)

// Compatibility hints (stable codes, shown in the admin view).
const (
	HintUpgradeRecommended = "upgrade_recommended"
	HintNodeTooOld         = "node_too_old"
	HintNodeIncompatible   = "node_incompatible"
	HintProtocol           = "protocol_unsupported"
	HintDevBuild           = "dev_build"
)

// Compatibility is the result of CheckCompatibility.
type Compatibility struct {
	Level CompatLevel
	Hint  string
}

// CheckCompatibility applies §4.8 with the hub at hubVersion: same major,
// node minor N or newer → ok; N-1 → older (degraded, upgrade
// recommended); older → incompatible (node_too_old); other major →
// incompatible. The node must speak rx-ctl.v1. Development builds are
// compatible (dev_build hint).
func CheckCompatibility(hubVersion, nodeVersion string, protocols []string) Compatibility {
	if !slices.Contains(protocols, ControlProtocol) {
		return Compatibility{Level: CompatIncompatible, Hint: HintProtocol}
	}

	hub, herr := ParseSemVer(hubVersion)
	node, nerr := ParseSemVer(nodeVersion)

	switch {
	case nerr != nil:
		return Compatibility{Level: CompatIncompatible, Hint: HintNodeIncompatible}
	case herr != nil || hub.Dev() || node.Dev():
		return Compatibility{Level: CompatOK, Hint: HintDevBuild}
	}

	hMajor, hMinor := hub.line()
	nMajor, nMinor := node.line()

	switch {
	case hMajor != nMajor:
		return Compatibility{Level: CompatIncompatible, Hint: HintNodeIncompatible}
	case nMinor >= hMinor:
		return Compatibility{Level: CompatOK}
	case nMinor == hMinor-1:
		return Compatibility{Level: CompatOlder, Hint: HintUpgradeRecommended}
	default:
		return Compatibility{Level: CompatIncompatible, Hint: HintNodeTooOld}
	}
}
