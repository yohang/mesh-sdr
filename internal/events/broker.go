// Package events is the hub events module (TECHNICAL_SPEC §6.1, §6.2, §6.6,
// ADR 0016, ADR 0018): the topics a client subscribes to and who receives
// an event; the in-process bus (modules publish events after commit, the
// connections of /api/ws subscribe to topics and receive the events of
// those topics their viewer may see); the admission of connections (socket
// caps) and the end of the connections of revoked sessions; and the
// WebSocket GET /api/ws itself (rx.v1 envelopes, session cookie or
// anonymous, Origin check, a presence row per socket).
package events

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
)

// Event is one hub event. Type is the rx.v1 message type ("node.status").
// Payload is a JSON-encodable view DTO: plain text only, never HTML, never
// a domain type or a secret (§6.1). Audience, when set, restricts the event
// to the viewers it accepts.
type Event struct {
	Topic    Topic
	Type     string
	Payload  any
	Audience Audience
}

// Publisher publishes hub events; producers depend on it (or on their own
// narrower port adapted in the composition root).
type Publisher interface {
	Publish(ctx context.Context, ev Event)
}

// Authorizer decides whether the subscriber of ctx may receive a topic. It
// returns ErrTopicForbidden or ErrUnauthenticated.
type Authorizer interface {
	AuthorizeTopic(ctx context.Context, t Topic) error
}

// Sink receives the events of one subscriber. It MUST NOT block: it
// enqueues, and the transport applies its own back-pressure.
type Sink func(Event)

// Broker fans events out to subscribers. Safe for concurrent use.
type Broker struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBroker returns an empty broker.
func NewBroker() *Broker { return &Broker{subs: map[*Subscription]struct{}{}} }

var _ Publisher = (*Broker)(nil)

// Publish delivers ev to every subscriber of its topic its audience
// accepts. It never blocks on a subscriber. Producers call it after their
// transaction committed, and after the derived state is updated.
func (b *Broker) Publish(_ context.Context, ev Event) {
	for _, s := range b.snapshot() {
		if s.has(ev.Topic) && (ev.Audience == nil || ev.Audience(s.viewer)) {
			s.sink(ev)
		}
	}
}

func (b *Broker) snapshot() []*Subscription {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]*Subscription, 0, len(b.subs))
	for s := range b.subs {
		out = append(out, s)
	}

	return out
}

// Attach registers a subscriber with no topic yet.
func (b *Broker) Attach(v Viewer, authz Authorizer, sink Sink) *Subscription {
	s := &Subscription{
		b: b, viewer: v, authz: authz, sink: sink, topics: map[Topic]struct{}{},
		ended: make(chan struct{}), recheck: make(chan struct{}, 1),
	}

	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	return s
}

// EndSessions ends the subscribers of the given sessions and users (a
// logout, a revocation, a role change, a disabled or deleted user: ADR 0016
// decision 4). Their connections close at once.
func (b *Broker) EndSessions(sessionRefs, userIDs []string) {
	for _, s := range b.snapshot() {
		v := s.viewer
		if v.Anonymous() {
			continue
		}

		if slices.Contains(userIDs, v.UserID) || (v.SessionRef != "" && slices.Contains(sessionRefs, v.SessionRef)) {
			s.end()
		}
	}
}

// RecheckAll asks every subscriber to authorise its topics again (a
// listen policy changed). Each connection runs Recheck in its own context.
func (b *Broker) RecheckAll() {
	for _, s := range b.snapshot() {
		select {
		case s.recheck <- struct{}{}:
		default:
		}
	}
}

// Subscription is the topic set of one subscriber (one /api/ws connection).
type Subscription struct {
	b      *Broker
	viewer Viewer
	authz  Authorizer
	sink   Sink

	mu     sync.RWMutex
	topics map[Topic]struct{}

	endOnce sync.Once
	ended   chan struct{}
	recheck chan struct{}
}

func (s *Subscription) has(t Topic) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.topics[t]

	return ok
}

func (s *Subscription) end() { s.endOnce.Do(func() { close(s.ended) }) }

// Ended is closed when the subscriber's session ended (EndSessions).
func (s *Subscription) Ended() <-chan struct{} { return s.ended }

// Rechecks receives a value when the topics must be authorised again.
func (s *Subscription) Rechecks() <-chan struct{} { return s.recheck }

// Viewer returns the subscriber.
func (s *Subscription) Viewer() Viewer { return s.viewer }

// Subscribe parses every topic, drops duplicates, refuses a request that
// would exceed MaxTopics before any authorisation, then authorises each
// topic and adds them all, or none when one fails (ADR 0016 decision 6):
// the error comes with the offending topic.
func (s *Subscription) Subscribe(ctx context.Context, raw []string) ([]Topic, string, error) {
	topics := make([]Topic, 0, min(len(raw), MaxTopics+1))
	seen := map[Topic]struct{}{}

	for _, r := range raw {
		t, err := ParseTopic(r)
		if err != nil {
			return nil, r, err
		}

		if _, dup := seen[t]; dup {
			continue
		}

		seen[t] = struct{}{}
		topics = append(topics, t)

		if len(topics) > MaxTopics {
			return nil, "", ErrTooManyTopics
		}
	}

	if s.added(topics) > MaxTopics {
		return nil, "", ErrTooManyTopics
	}

	for _, t := range topics {
		if err := s.authz.AuthorizeTopic(ctx, t); err != nil {
			return nil, t.String(), err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.addedLocked(topics) > MaxTopics {
		return nil, "", ErrTooManyTopics
	}

	for _, t := range topics {
		s.topics[t] = struct{}{}
	}

	return topics, "", nil
}

// added returns the number of topics the subscription would hold with
// topics.
func (s *Subscription) added(topics []Topic) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.addedLocked(topics)
}

func (s *Subscription) addedLocked(topics []Topic) int {
	n := len(s.topics)

	for _, t := range topics {
		if _, ok := s.topics[t]; !ok {
			n++
		}
	}

	return n
}

// Unsubscribe removes topics; unknown or malformed ones are ignored.
func (s *Subscription) Unsubscribe(raw []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range raw {
		if t, err := ParseTopic(r); err == nil {
			delete(s.topics, t)
		}
	}
}

// Topics returns the current topics, sorted.
func (s *Subscription) Topics() []Topic {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Topic, 0, len(s.topics))
	for t := range s.topics {
		out = append(out, t)
	}

	slices.SortFunc(out, func(a, b Topic) int { return strings.Compare(a.String(), b.String()) })

	return out
}

// Recheck authorises every current topic again and drops the ones now
// denied (ADR 0016 decision 5). It returns the dropped topics, or
// ErrUnauthenticated when the subscriber's session ended; another
// error leaves the topics untouched.
func (s *Subscription) Recheck(ctx context.Context) ([]Topic, error) {
	var dropped []Topic

	for _, t := range s.Topics() {
		err := s.authz.AuthorizeTopic(ctx, t)
		if err == nil {
			continue
		}

		if !errors.Is(err, ErrTopicForbidden) {
			// The session ended, or the check failed: nothing is dropped.
			return nil, err
		}

		dropped = append(dropped, t)
	}

	s.mu.Lock()
	for _, t := range dropped {
		delete(s.topics, t)
	}
	s.mu.Unlock()

	return dropped, nil
}

// Detach removes the subscriber from the broker.
func (s *Subscription) Detach() {
	s.b.mu.Lock()
	delete(s.b.subs, s)
	s.b.mu.Unlock()
}
