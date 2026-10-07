// Package app is the in-process hub events bus (TECHNICAL_SPEC §6.6, ADR
// 0016 decision 13): modules publish events after commit, the connections
// of /api/ws subscribe to topics and receive the events of those topics
// their viewer may see. It also admits connections (socket caps, ADR 0018)
// and ends the connections of revoked sessions.
package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/yohang/mesh-sdr/internal/events/domain"
)

// Event is one hub event. Type is the rx.v1 message type ("node.status").
// Payload is a JSON-encodable view DTO: plain text only, never HTML, never
// a domain type or a secret (§6.1). Audience, when set, restricts the event
// to the viewers it accepts.
type Event struct {
	Topic    domain.Topic
	Type     string
	Payload  any
	Audience domain.Audience
}

// Publisher publishes hub events; producers depend on it (or on their own
// narrower port adapted in the composition root).
type Publisher interface {
	Publish(ctx context.Context, ev Event)
}

// Authorizer decides whether the subscriber of ctx may receive a topic. It
// returns domain.ErrTopicForbidden or domain.ErrUnauthenticated.
type Authorizer interface {
	AuthorizeTopic(ctx context.Context, t domain.Topic) error
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

// Subscribers returns the number of attached subscribers.
func (b *Broker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.subs)
}

// Attach registers a subscriber with no topic yet.
func (b *Broker) Attach(v domain.Viewer, authz Authorizer, sink Sink) *Subscription {
	s := &Subscription{
		b: b, viewer: v, authz: authz, sink: sink, topics: map[domain.Topic]struct{}{},
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
	viewer domain.Viewer
	authz  Authorizer
	sink   Sink

	mu     sync.RWMutex
	topics map[domain.Topic]struct{}

	endOnce sync.Once
	ended   chan struct{}
	recheck chan struct{}
}

func (s *Subscription) has(t domain.Topic) bool {
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
func (s *Subscription) Viewer() domain.Viewer { return s.viewer }

// Subscribe parses and authorises every topic, then adds them all, or none
// when one fails (ADR 0016 decision 6): the error comes with the offending
// topic.
func (s *Subscription) Subscribe(ctx context.Context, raw []string) ([]domain.Topic, string, error) {
	topics := make([]domain.Topic, 0, len(raw))

	for _, r := range raw {
		t, err := domain.ParseTopic(r)
		if err != nil {
			return nil, r, err
		}

		if err := s.authz.AuthorizeTopic(ctx, t); err != nil {
			return nil, r, err
		}

		topics = append(topics, t)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	added := map[domain.Topic]struct{}{}

	for _, t := range topics {
		if _, ok := s.topics[t]; !ok {
			added[t] = struct{}{}
		}
	}

	if len(s.topics)+len(added) > domain.MaxTopics {
		return nil, "", domain.ErrTooManyTopics
	}

	for _, t := range topics {
		s.topics[t] = struct{}{}
	}

	return topics, "", nil
}

// Unsubscribe removes topics; unknown or malformed ones are ignored.
func (s *Subscription) Unsubscribe(raw []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range raw {
		if t, err := domain.ParseTopic(r); err == nil {
			delete(s.topics, t)
		}
	}
}

// Topics returns the current topics, sorted.
func (s *Subscription) Topics() []domain.Topic {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.Topic, 0, len(s.topics))
	for t := range s.topics {
		out = append(out, t)
	}

	slices.SortFunc(out, func(a, b domain.Topic) int { return strings.Compare(a.String(), b.String()) })

	return out
}

// Recheck authorises every current topic again and drops the ones now
// denied (ADR 0016 decision 5). It returns the dropped topics, or
// domain.ErrUnauthenticated when the subscriber's session ended; another
// error leaves the topics untouched.
func (s *Subscription) Recheck(ctx context.Context) ([]domain.Topic, error) {
	var dropped []domain.Topic

	for _, t := range s.Topics() {
		err := s.authz.AuthorizeTopic(ctx, t)
		if err == nil {
			continue
		}

		if !errors.Is(err, domain.ErrTopicForbidden) {
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
