// Package domain holds the grid bounded context: nodes, enrollment, devices.
package domain

import (
	"regexp"
	"strconv"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrInvalidNodeID is returned when a node id is not a valid slug.
var ErrInvalidNodeID = shared.NewError(shared.KindInvalid, "invalid_node_id",
	"node id must match ^[a-z0-9][a-z0-9-]{1,62}$")

var nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// NodeID is the stable slug identifying a node (TECHNICAL_SPEC §4.1).
type NodeID struct {
	value string
}

// NewNodeID validates s and returns it as a NodeID.
func NewNodeID(s string) (NodeID, error) {
	if !nodeIDPattern.MatchString(s) {
		return NodeID{}, ErrInvalidNodeID.WithDetail("invalid node id " + strconv.Quote(s) + ": must match ^[a-z0-9][a-z0-9-]{1,62}$")
	}

	return NodeID{value: s}, nil
}

// MustNodeID is NewNodeID that panics on error. Tests and constants only.
func MustNodeID(s string) NodeID {
	id, err := NewNodeID(s)
	if err != nil {
		panic(err)
	}

	return id
}

// String returns the slug.
func (id NodeID) String() string { return id.value }
