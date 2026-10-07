package media

import (
	"testing"
	"time"
)

func TestMsgLimiter(t *testing.T) {
	l := newMsgLimiter(20)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

	for i := range msgBurst {
		if ok, _, _ := l.allow(now); !ok {
			t.Fatalf("burst message %d refused", i)
		}
	}

	ok, retry, escalate := l.allow(now)
	if ok || escalate || retry <= 0 || retry > 50*time.Millisecond {
		t.Fatalf("over burst: ok %v retry %v escalate %v", ok, retry, escalate)
	}

	// 20 msg/s sustained is fine.
	for range 100 {
		now = now.Add(50 * time.Millisecond)

		if ok, _, _ := l.allow(now); !ok {
			t.Fatal("sustained rate refused")
		}
	}

	// A flood: strikes escalate at the tenth refusal within a minute.
	var esc bool
	for i := 0; i < 20 && !esc; i++ {
		_, _, esc = l.allow(now)
	}

	if !esc {
		t.Fatal("flood never escalated")
	}
}
