package agent_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func seqs(es []agent.Event) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}

	return out
}

func TestBufferCoalesceAckReplay(t *testing.T) {
	b := agent.NewBuffer(100, 1<<20)

	b.Push(agent.Event{Seq: 1, Type: rxv1.TypeNodeHeartbeat, Key: "heartbeat", Class: agent.ClassState, Size: 10})
	b.Push(agent.Event{Seq: 2, Type: rxv1.TypeConnectionOpened, Class: agent.ClassState, Size: 10})
	b.Push(agent.Event{Seq: 3, Type: rxv1.TypeNodeHeartbeat, Key: "heartbeat", Class: agent.ClassState, Size: 10})

	if got := seqs(b.After(0)); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("pending = %v, want [2 3] (heartbeat coalesced)", got)
	}

	if got := seqs(b.After(2)); len(got) != 1 || got[0] != 3 {
		t.Errorf("after 2 = %v", got)
	}

	b.Ack(2)

	if got := seqs(b.After(0)); len(got) != 1 || got[0] != 3 || b.Acked() != 2 {
		t.Errorf("after ack = %v", got)
	}

	b.Ack(1) // stale ack is ignored

	if b.Acked() != 2 {
		t.Errorf("acked = %d", b.Acked())
	}
}

func TestBufferDropsLowestClassFirst(t *testing.T) {
	b := agent.NewBuffer(3, 1<<20)

	b.Push(agent.Event{Seq: 1, Type: rxv1.TypeDecodeBatch, Class: agent.ClassDecode, Size: 1})
	b.Push(agent.Event{Seq: 2, Type: rxv1.TypeDiagTransition, Class: agent.ClassDiagnostics, Size: 1})
	b.Push(agent.Event{Seq: 3, Type: rxv1.TypeMapFeatureBatch, Class: agent.ClassMap, Size: 1})
	b.Push(agent.Event{Seq: 4, Type: rxv1.TypeDecodeBatch, Class: agent.ClassDecode, Size: 1})
	b.Push(agent.Event{Seq: 5, Type: rxv1.TypeDecodeBatch, Class: agent.ClassDecode, Size: 1})

	if got := seqs(b.After(0)); len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 5 {
		t.Fatalf("pending = %v, want [1 4 5]", got)
	}

	b.Push(agent.Event{Seq: 6, Type: rxv1.TypeDecodeBatch, Class: agent.ClassDecode, Size: 1})

	if got := seqs(b.After(0)); got[0] != 4 {
		t.Errorf("oldest decode must go first: %v", got)
	}

	count, kinds := b.TakeDropped()
	if count != 3 || kinds["diag.transition"] != 1 || kinds["map.feature.batch"] != 1 || kinds["decode.batch"] != 1 {
		t.Errorf("dropped = %d %v", count, kinds)
	}

	if count, _ := b.TakeDropped(); count != 0 {
		t.Error("drop counters not reset")
	}

	// The byte bound applies too.
	small := agent.NewBuffer(100, 25)
	for i := range int64(5) {
		small.Push(agent.Event{Seq: i + 1, Type: rxv1.TypeDecodeBatch, Class: agent.ClassDecode, Size: 10})
	}

	if small.Len() != 2 {
		t.Errorf("byte-bounded len = %d", small.Len())
	}
}
