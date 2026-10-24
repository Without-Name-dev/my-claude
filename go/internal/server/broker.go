package server

import "sync"

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
// A full queue loses its oldest frame rather than closing the socket: a client
// that fell behind can recover from the HTTP event log, and dropping one frame
// is far less disruptive than a reconnect. This matches the Python behaviour.
func (b *Broker) Publish(sessionID string, frame []byte) {
	b.mu.RLock()
	set := b.subs[sessionID]
	targets := make([]*Subscriber, 0, len(set))
	for sub := range set {
		targets = append(targets, sub)
	}
	b.mu.RUnlock()

	for _, sub := range targets {
		select {
		case <-sub.done:
			continue
		default:
		}
		select {
		case sub.frames <- frame:
		default:
			// Drop the oldest, then enqueue. The drain is non-blocking because
			// only the owning socket goroutine removes frames.
			select {
			case <-sub.frames:
			default:
			}
			select {
			case sub.frames <- frame:
			case <-sub.done:
			default:
			}
		}
	}
}
