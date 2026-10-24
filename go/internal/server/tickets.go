package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// ticketTTL is fixed rather than configurable: a client that takes longer than
// this to open its socket is broken, and a shorter window keeps a leaked ticket
// useless. Clients request a fresh ticket for every reconnect.
const ticketTTL = 60 * time.Second

// TicketScope says which endpoint a ticket unlocks: one of session,
// dashboard, or a terminal (which sets both connector and terminal).
type TicketScope struct {
	SessionID string
	Dashboard bool
	// A terminal ticket names both the connector and the terminal, so it
	// cannot be replayed against another terminal on the same connector.
	ConnectorID string
	TerminalID  string
}

// Ticket is what a websocket knows about the person behind it.
type Ticket struct {
	UserID   string
	ClientID string
	Scope    TicketScope
}

type ticketEntry struct {
	ticket  Ticket
	expires time.Time
}

// TicketManager issues and redeems websocket tickets.
//
// Tickets are stored as hashes and removed on redemption, so a ticket works
// once. There is no distributed store: this deployment runs a single server
// process, and a restart invalidates outstanding tickets, which clients already
// handle by reconnecting with a new one.
type TicketManager struct {
	mu      sync.Mutex
	entries map[string]ticketEntry
	now     func() time.Time
}

// NewTicketManager returns an empty manager.
func NewTicketManager() *TicketManager {
	return &TicketManager{entries: map[string]ticketEntry{}, now: time.Now}
}

// Issue mints a ticket for the given scope and returns it with its expiry.
func (m *TicketManager) Issue(userID, clientID string, scope TicketScope) (string, time.Time) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is fatal for anything security-shaped; fall back
		// to an unusable ticket rather than a predictable one.
		return "", time.Time{}
	}
	token := "wst_" + base64.RawURLEncoding.EncodeToString(buf)
	expires := m.now().Add(ticketTTL)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	m.entries[hashTicket(token)] = ticketEntry{
		ticket:  Ticket{UserID: userID, ClientID: clientID, Scope: scope},
		expires: expires,
	}
	return token, expires
}

// Consume redeems a ticket for one scope. It returns the ticket only when the
// token exists, has not expired, and was issued for exactly that scope.
func (m *TicketManager) Consume(token string, scope TicketScope) (Ticket, bool) {
	key := hashTicket(token)

	m.mu.Lock()
	entry, ok := m.entries[key]
	if ok {
		delete(m.entries, key) // single use, whether or not it validates below
	}
	m.mu.Unlock()

	if !ok || m.now().After(entry.expires) {
		return Ticket{}, false
	}
	if !sameScope(entry.ticket.Scope, scope) {
		return Ticket{}, false
	}
	return entry.ticket, true
}

// sweepLocked drops expired entries so an unused ticket cannot accumulate.
func (m *TicketManager) sweepLocked() {
	now := m.now()
	for key, entry := range m.entries {
		if now.After(entry.expires) {
			delete(m.entries, key)
		}
	}
}

func sameScope(a, b TicketScope) bool {
	if a.Dashboard != b.Dashboard {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(a.SessionID), []byte(b.SessionID)) != 1 {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(a.ConnectorID), []byte(b.ConnectorID)) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a.TerminalID), []byte(b.TerminalID)) == 1
}

func hashTicket(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
