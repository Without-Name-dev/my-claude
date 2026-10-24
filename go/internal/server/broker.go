package server

import "sync"

// subscriberQueue is how many frames a slow client may fall behind before the
// broker starts dropping the oldest. The Python product used 256; keeping the
// number means a client that buffered just enough before still keeps up.
const subscriberQueue = 256

// Subscriber is one websocket's queue.
type Subscriber struct {
	frames chan []byte

	// closeOnce guards Close so a double close from two code paths is safe.
	closeOnce sync.Once
}

// Frames is the channel a websocket sends from.
func (s *Subscriber) Frames() <-chan []byte { return s.frames }

// Close ends the subscription; the reader sees a closed channel.
func (s *Subscriber) Close() { s.closeOnce.Do(func() { close(s.frames) }) }

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
	sub := &Subscriber{frames: make(chan []byte, subscriberQueue)}
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

// Unsubscribe removes a socket and closes its queue.
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
			default:
			}
		}
	}
}

// SubscriberCount reports how many sockets watch a session.
func (b *Broker) SubscriberCount(sessionID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[sessionID])
}
