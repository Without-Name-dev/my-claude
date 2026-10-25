package server

import (
	"encoding/json"
	"sync"
)

// subscriberQueue is how many frames a slow client may fall behind before the
// broker starts dropping the oldest. The Python product used 256; keeping the
// number means a client that buffered just enough before still keeps up.
const subscriberQueue = 256

// Subscriber is one websocket's queue.
type Subscriber struct {
	frames chan []byte
	// done is closed once when the subscription ends. The frames channel is
	// never closed: Publish may discover a subscriber just after it was
	// removed, and a send on a closed channel would panic.
	done      chan struct{}
	closeOnce sync.Once
}

// Frames is the channel a websocket sends from.
func (s *Subscriber) Frames() <-chan []byte { return s.frames }

// Done is closed when the subscription ends.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

// Close ends the subscription; the reader selects on Done.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
	})
}

// closed reports whether the subscription has already ended.
func (s *Subscriber) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Broker fans session events out to every socket watching that session.
type Broker struct {
	mu   sync.RWMutex
	subs map[string]map[*Subscriber]struct{}
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: map[string]map[*Subscriber]struct{}{}}
}

// Subscribe registers a socket for one session.
func (b *Broker) Subscribe(sessionID string) *Subscriber {
	sub := &Subscriber{
		frames: make(chan []byte, subscriberQueue),
		done:   make(chan struct{}),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.subs[sessionID]
	if !ok {
		set = map[*Subscriber]struct{}{}
		b.subs[sessionID] = set
	}
	set[sub] = struct{}{}
	return sub
}

// Unsubscribe removes a socket and signals its reader to stop.
func (b *Broker) Unsubscribe(sessionID string, sub *Subscriber) {
	b.mu.Lock()
	if set, ok := b.subs[sessionID]; ok {
		delete(set, sub)
		if len(set) == 0 {
			delete(b.subs, sessionID)
		}
	}
	b.mu.Unlock()
	sub.Close()
}

// Publish delivers one frame to every subscriber of a session.
//
// Enqueueing happens under the write lock so every subscriber of a session
// observes publishes in the same order. Without that, two concurrent publishes
// could reach two sockets in opposite orders.
//
// A full queue cannot receive the whole stream. Rather than drop its oldest
// frame and leave a hole the client cannot see, the queue is drained and a
// session.refetch_required frame takes its place: the client re-reads the
// durable event log from its own cursor. Reconnects are more disruptive than a
// refetch, so the socket is left open.
func (b *Broker) Publish(sessionID string, frame []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	set := b.subs[sessionID]
	if len(set) == 0 {
		return
	}
	var refetch []byte
	for sub := range set {
		if sub.closed() {
			continue
		}
		select {
		case sub.frames <- frame:
			continue
		default:
		}
		if refetch == nil {
			refetch = refetchFrame(sessionID, frame)
		}
		if refetch == nil {
			// Not an envelope that carries a sequence; fall back to losing the
			// oldest frame instead of the whole backlog.
			select {
			case <-sub.frames:
			default:
			}
			select {
			case sub.frames <- frame:
			case <-sub.done:
			default:
			}
			continue
		}
		// The queue is full. Drop the backlog: only the owning socket
		// goroutine removes frames, so a non-blocking drain cannot stall it.
	drain:
		for {
			select {
			case <-sub.frames:
			default:
				break drain
			}
		}
		select {
		case sub.frames <- refetch:
		default:
		}
		select {
		case sub.frames <- frame:
		case <-sub.done:
		default:
		}
	}
}

// refetchFrame builds the recovery marker a subscriber receives after its
// queue overflowed. The dropped frame's sequence tells the client where the
// loss started; clients keep their own cursor and ignore the payload value.
func refetchFrame(sessionID string, dropped []byte) []byte {
	var env struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(dropped, &env); err != nil {
		return nil
	}
	frame, err := newEnvelope(sessionID, env.Sequence, EventRefetch, map[string]any{
		"eventCursor": Cursor(env.Sequence),
	})
	if err != nil {
		return nil
	}
	return frame
}
