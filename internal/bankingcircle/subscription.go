package bankingcircle

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Banking Circle's notification self-service API, as documented.
//
// The previous model here was a three-field {url, active, version} struct.
// That is not what a client integrates against: the real API has an
// optimistic-concurrency token on every mutation, a four-value status
// enum, a per-subscription encryption key that is never echoed back, a
// batch-size setting that actually changes the wire shape, and events with
// targets that decide which notifications a subscriber receives at all. A
// client written against the simplified version would fail on first
// contact with the real one, which defeats the point of the simulator.

// SubscriptionStatus is the documented status enum. The gap at 3 is real.
type SubscriptionStatus int

const (
	StatusNone     SubscriptionStatus = 0
	StatusInactive SubscriptionStatus = 1
	StatusActive   SubscriptionStatus = 2
	StatusRetired  SubscriptionStatus = 4
)

// TargetType is the documented target enum: which kind of entity a
// subscription event's targetIds refer to.
type TargetType int

const (
	TargetAccount      TargetType = 0
	TargetCompany      TargetType = 1
	TargetCompanyGroup TargetType = 2
)

// Batch-size bounds from the API contract. A value outside them is
// rejected rather than clamped: a client that asks for 2000 has
// misunderstood something, and silently giving it 1000 hides that until
// production.
const (
	MinNotificationsPerMessage = 5
	MaxNotificationsPerMessage = 1000
	// DefaultNotificationsPerMessage matches the documented common default.
	DefaultNotificationsPerMessage = 1000
)

// EncryptionKeyLength is the exact key length the API requires. It is not
// a recommendation: the webhook cipher uses the raw UTF-8 bytes of this
// string as an AES-256 key, so anything but 32 characters cannot work.
const EncryptionKeyLength = 32

// hiddenKey is what the API returns in place of a stored encryption key.
const hiddenKey = "*Hidden*"

var (
	// ErrSubscriptionNotFound is returned for an unknown subscription or
	// subscription-event id.
	ErrSubscriptionNotFound = errors.New("bankingcircle: subscription not found")
	// ErrRowVersionMismatch is returned when an If-Match header does not
	// match the record's current rowVersion -- the API's optimistic
	// concurrency check. It maps to 412.
	ErrRowVersionMismatch = errors.New("bankingcircle: rowVersion does not match")
	// ErrDuplicateEndpoint is returned when a subscription is created for
	// an endpoint another subscription of the same customer already uses.
	// A customer's subscriptions cannot share identical endpoint URLs.
	ErrDuplicateEndpoint = errors.New("bankingcircle: a subscription for that endpoint already exists")
	// ErrDuplicateEvent is returned when the same eventType is added to a
	// subscription twice.
	ErrDuplicateEvent = errors.New("bankingcircle: that eventType is already on this subscription")
	// ErrMixedTargets is returned when an event mixes Account and Company
	// targets, which the API forbids.
	ErrMixedTargets = errors.New("bankingcircle: an event cannot mix Account and Company targets")
)

// TargetDetail is one entity a subscription event is scoped to.
type TargetDetail struct {
	ID         string     `json:"id"`
	TargetID   string     `json:"targetId"`
	TargetType TargetType `json:"targetType"`
}

// SubscriptionEvent is one event type a subscription receives, optionally
// narrowed to specific targets.
type SubscriptionEvent struct {
	ID             string             `json:"id"`
	EventType      string             `json:"eventType"`
	IsActive       bool               `json:"isActive"`
	SubscriptionID string             `json:"subscriptionId"`
	Status         SubscriptionStatus `json:"status"`
	RowVersion     string             `json:"rowVersion"`
	Targets        []TargetDetail     `json:"subscriptionEventTargetDetails"`
}

// matches reports whether this event should fire for eventType against the
// given target ids (the payment's account and owning company).
//
// An event with no targets is subscription-wide. That is the case the
// previous implementation effectively hard-coded for everything, which
// meant every subscriber received every notification regardless of what it
// had actually subscribed to.
func (e *SubscriptionEvent) matches(eventType string, targetIDs ...string) bool {
	if !e.IsActive || e.Status != StatusActive {
		return false
	}
	if !strings.EqualFold(e.EventType, eventType) {
		return false
	}
	if len(e.Targets) == 0 {
		return true
	}
	for _, t := range e.Targets {
		for _, want := range targetIDs {
			if want != "" && t.TargetID == want {
				return true
			}
		}
	}
	return false
}

// Subscription is one registered webhook endpoint.
type Subscription struct {
	ID            string             `json:"id"`
	Endpoint      string             `json:"endpoint"`
	IsActive      bool               `json:"isActive"`
	MTLSEnabled   bool               `json:"mtlsEnabled"`
	Status        SubscriptionStatus `json:"status"`
	StatusMessage string             `json:"statusMessage"`
	RowVersion    string             `json:"rowVersion"`
	Version       int                `json:"version"`
	Email         string             `json:"email,omitempty"`

	MaxNotificationsPerMessage int                  `json:"maxNotificationsPerMessage"`
	Events                     []*SubscriptionEvent `json:"subscriptionEvents"`

	// EncryptionKey is stored but never rendered: Public() replaces it
	// with "*Hidden*", which is what the real API returns.
	EncryptionKey string `json:"-"`

	// Env is the environment whose subscription this is (internal/tenant):
	// "" for the default's. It only ever hears about its own environment's
	// payments. Never rendered: the wire shape is the vendor's.
	Env string `json:"-"`

	CreatedAt string `json:"-"`
	UpdatedAt string `json:"-"`
}

// PublicSubscription is the wire shape, with the encryption key masked.
type PublicSubscription struct {
	Subscription
	EncryptionKey string `json:"encryptionKey,omitempty"`
}

// Public renders s for a response body: a deep-enough copy that a caller
// mutating the result cannot reach back into the store, with the
// encryption key masked.
func (s *Subscription) Public() PublicSubscription {
	cp := *s
	cp.Events = make([]*SubscriptionEvent, len(s.Events))
	for i, e := range s.Events {
		ec := *e
		ec.Targets = append([]TargetDetail(nil), e.Targets...)
		cp.Events[i] = &ec
	}
	masked := ""
	if s.EncryptionKey != "" {
		masked = hiddenKey
	}
	return PublicSubscription{Subscription: cp, EncryptionKey: masked}
}

// matchingEvent returns the event that should fire for eventType against
// targetIDs, or nil. The subscription itself must be active: a deactivated
// subscription receives nothing, and events that occur while it is
// deactivated are not backfilled when it comes back.
func (s *Subscription) matchingEvent(eventType string, targetIDs ...string) *SubscriptionEvent {
	if !s.IsActive || s.Status != StatusActive {
		return nil
	}
	for _, e := range s.Events {
		if e.matches(eventType, targetIDs...) {
			return e
		}
	}
	return nil
}

// SubscriptionStore holds subscriptions and hands out rowVersions.
type SubscriptionStore struct {
	mu      sync.Mutex
	subs    map[string]*Subscription
	events  map[string]*SubscriptionEvent
	rowSeq  uint64
	idSeq   int
	newID   func(prefix string) string
	nowFunc func() string

	// OnChange is called after each change to a subscription or its
	// events. May be nil; it must not block or call back in.
	OnChange func()
}

// NewSubscriptionStore returns an empty store. newID may be nil, in which
// case ids are sequential and deterministic -- convenient in tests, and
// harmless in a simulator.
func NewSubscriptionStore(newID func(prefix string) string) *SubscriptionStore {
	return &SubscriptionStore{
		subs:   map[string]*Subscription{},
		events: map[string]*SubscriptionEvent{},
		newID:  newID,
		nowFunc: func() string {
			return time.Now().UTC().Format(time.RFC3339)
		},
	}
}

func (s *SubscriptionStore) id(prefix string) string {
	if s.newID != nil {
		return s.newID(prefix)
	}
	s.idSeq++
	return fmt.Sprintf("%s_%06d", prefix, s.idSeq)
}

// nextRowVersion mints the next optimistic-concurrency token. Real Banking
// Circle renders these as base64 of an 8-byte counter ("AAAAAAAAAAA="), so
// a client that stores and echoes the string verbatim -- which is all it
// should ever do with it -- behaves identically here.
func (s *SubscriptionStore) nextRowVersion() string {
	s.rowSeq++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], s.rowSeq)
	return base64.StdEncoding.EncodeToString(b[:])
}

// CreateParams carries POST /subscription's body.
type CreateParams struct {
	// Env is the customer's environment, the subscription's owner.
	Env                        string
	Endpoint                   string
	EncryptionKey              string
	Status                     SubscriptionStatus
	Email                      string
	MaxNotificationsPerMessage int
	MTLSEnabled                bool
}

// Validate applies the documented field rules.
func (p CreateParams) Validate() error {
	if strings.TrimSpace(p.Endpoint) == "" {
		return errors.New("endpoint is required")
	}
	if len(p.EncryptionKey) != EncryptionKeyLength {
		return fmt.Errorf("encryptionKey must be exactly %d characters (got %d)", EncryptionKeyLength, len(p.EncryptionKey))
	}
	switch p.Status {
	case StatusActive, StatusInactive:
	default:
		return fmt.Errorf("status must be %d (active) or %d (inactive)", StatusActive, StatusInactive)
	}
	if p.MaxNotificationsPerMessage != 0 &&
		(p.MaxNotificationsPerMessage < MinNotificationsPerMessage || p.MaxNotificationsPerMessage > MaxNotificationsPerMessage) {
		return fmt.Errorf("maxNotificationsPerMessage must be between %d and %d",
			MinNotificationsPerMessage, MaxNotificationsPerMessage)
	}
	return nil
}

// Create registers a subscription.
func (s *SubscriptionStore) Create(p CreateParams) (*Subscription, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	owner := Owner(p.Env)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.subs {
		if existing.Env == owner && existing.Endpoint == p.Endpoint {
			return nil, ErrDuplicateEndpoint
		}
	}
	batch := p.MaxNotificationsPerMessage
	if batch == 0 {
		batch = DefaultNotificationsPerMessage
	}
	now := s.nowFunc()
	sub := &Subscription{
		ID:                         s.id("sub"),
		Endpoint:                   p.Endpoint,
		IsActive:                   p.Status == StatusActive,
		MTLSEnabled:                p.MTLSEnabled,
		Status:                     p.Status,
		StatusMessage:              "Subscription successfully created",
		RowVersion:                 s.nextRowVersion(),
		Version:                    1,
		Email:                      p.Email,
		MaxNotificationsPerMessage: batch,
		EncryptionKey:              p.EncryptionKey,
		Env:                        owner,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	s.subs[sub.ID] = sub
	changed(s.OnChange)
	return sub, nil
}

// UpdateParams carries PUT /subscription/{id}'s body. Every field is
// optional; a nil pointer means "leave alone", which is what distinguishes
// "clear the email" from "do not touch the email".
type UpdateParams struct {
	Endpoint                   *string
	EncryptionKey              *string
	Email                      *string
	MaxNotificationsPerMessage *int
	MTLSEnabled                *bool
}

// Update applies an If-Match-guarded update.
func (s *SubscriptionStore) Update(id, ifMatch string, p UpdateParams) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	if err := checkRowVersion(sub.RowVersion, ifMatch); err != nil {
		return nil, err
	}
	if p.Endpoint != nil {
		for otherID, other := range s.subs {
			if otherID != id && other.Env == sub.Env && other.Endpoint == *p.Endpoint {
				return nil, ErrDuplicateEndpoint
			}
		}
		sub.Endpoint = *p.Endpoint
	}
	if p.EncryptionKey != nil {
		if len(*p.EncryptionKey) != EncryptionKeyLength {
			return nil, fmt.Errorf("encryptionKey must be exactly %d characters", EncryptionKeyLength)
		}
		sub.EncryptionKey = *p.EncryptionKey
	}
	if p.Email != nil {
		sub.Email = *p.Email
	}
	if p.MaxNotificationsPerMessage != nil {
		v := *p.MaxNotificationsPerMessage
		if v < MinNotificationsPerMessage || v > MaxNotificationsPerMessage {
			return nil, fmt.Errorf("maxNotificationsPerMessage must be between %d and %d",
				MinNotificationsPerMessage, MaxNotificationsPerMessage)
		}
		sub.MaxNotificationsPerMessage = v
	}
	if p.MTLSEnabled != nil {
		sub.MTLSEnabled = *p.MTLSEnabled
	}
	sub.RowVersion = s.nextRowVersion()
	sub.UpdatedAt = s.nowFunc()
	changed(s.OnChange)
	return sub, nil
}

// checkRowVersion implements the If-Match rule. An empty header is a
// rejection, not a bypass: the whole point of the check is that a client
// which has not read the current state cannot write over it.
func checkRowVersion(current, ifMatch string) error {
	if strings.TrimSpace(ifMatch) == "" {
		return ErrRowVersionMismatch
	}
	if strings.Trim(ifMatch, `"`) != current {
		return ErrRowVersionMismatch
	}
	return nil
}

// Get returns one subscription, whichever environment's it is.
func (s *SubscriptionStore) Get(id string) (*Subscription, error) { return s.GetIn(tenant.All, id) }

// GetIn returns one of scope's subscriptions: another environment's is
// ErrSubscriptionNotFound, as an unknown one is.
func (s *SubscriptionStore) GetIn(scope, id string) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok || !tenant.InScope(scope, sub.Env) {
		return nil, ErrSubscriptionNotFound
	}
	return sub, nil
}

// List returns one page of subscriptions, every environment's, ordered by
// id so paging is stable. pageNumber is 1-based.
func (s *SubscriptionStore) List(pageNumber, pageSize int) ([]*Subscription, int) {
	return s.ListIn(tenant.All, pageNumber, pageSize)
}

// ListIn returns one page of scope's subscriptions, and how many scope has.
func (s *SubscriptionStore) ListIn(scope string, pageNumber, pageSize int) ([]*Subscription, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.subs))
	for id, sub := range s.subs {
		if tenant.InScope(scope, sub.Env) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	total := len(ids)
	if pageNumber < 1 {
		pageNumber = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	start := (pageNumber - 1) * pageSize
	if start >= total {
		return nil, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	out := make([]*Subscription, 0, end-start)
	for _, id := range ids[start:end] {
		out = append(out, s.subs[id])
	}
	return out, total
}

// SetActive activates or deactivates a subscription under If-Match.
func (s *SubscriptionStore) SetActive(id, ifMatch string, active bool) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	if err := checkRowVersion(sub.RowVersion, ifMatch); err != nil {
		return nil, err
	}
	s.setActiveLocked(sub, active, "")
	return sub, nil
}

// OverrideActive is SetActive without the If-Match check, for the lab's
// control port: the lab switching a subscription on or off is not a client
// that could be writing over another's change. The rowVersions move as
// they do under SetActive, so a client holding the old one still gets its
// mismatch.
func (s *SubscriptionStore) OverrideActive(id string, active bool) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	s.setActiveLocked(sub, active, "")
	return sub, nil
}

// Deactivate turns a subscription off without an If-Match check, for the
// one case the API itself does it: retry exhaustion. reason becomes the
// statusMessage a client sees on its next read, which is how it finds out
// why its subscription stopped.
func (s *SubscriptionStore) Deactivate(id, reason string) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	s.setActiveLocked(sub, false, reason)
	return sub, nil
}

func (s *SubscriptionStore) setActiveLocked(sub *Subscription, active bool, reason string) {
	sub.IsActive = active
	if active {
		sub.Status = StatusActive
		sub.StatusMessage = "Subscription is active"
	} else {
		sub.Status = StatusInactive
		sub.StatusMessage = "Subscription is inactive"
	}
	if reason != "" {
		sub.StatusMessage = reason
	}
	for _, e := range sub.Events {
		e.IsActive = active
		if active {
			e.Status = StatusActive
		} else {
			e.Status = StatusInactive
		}
		e.RowVersion = s.nextRowVersion()
	}
	sub.RowVersion = s.nextRowVersion()
	sub.UpdatedAt = s.nowFunc()
	changed(s.OnChange)
}

// Delete removes a subscription under If-Match. Irreversible, as documented.
func (s *SubscriptionStore) Delete(id, ifMatch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return ErrSubscriptionNotFound
	}
	if err := checkRowVersion(sub.RowVersion, ifMatch); err != nil {
		return err
	}
	for _, e := range sub.Events {
		delete(s.events, e.ID)
	}
	delete(s.subs, id)
	changed(s.OnChange)
	return nil
}

// AddEvent attaches an event type to a subscription.
func (s *SubscriptionStore) AddEvent(subscriptionID, eventType string, targetType TargetType, targetIDs []string) (*SubscriptionEvent, error) {
	if strings.TrimSpace(eventType) == "" {
		return nil, errors.New("eventType is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[subscriptionID]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	for _, e := range sub.Events {
		if strings.EqualFold(e.EventType, eventType) {
			return nil, ErrDuplicateEvent
		}
	}
	ev := &SubscriptionEvent{
		ID:             s.id("subev"),
		EventType:      eventType,
		IsActive:       sub.IsActive,
		SubscriptionID: sub.ID,
		Status:         sub.Status,
		RowVersion:     s.nextRowVersion(),
	}
	for _, tid := range targetIDs {
		ev.Targets = append(ev.Targets, TargetDetail{
			ID:         s.id("tgt"),
			TargetID:   tid,
			TargetType: targetType,
		})
	}
	sub.Events = append(sub.Events, ev)
	s.events[ev.ID] = ev
	sub.RowVersion = s.nextRowVersion()
	sub.UpdatedAt = s.nowFunc()
	changed(s.OnChange)
	return ev, nil
}

// GetEvent returns one subscription event, whichever environment's it is.
func (s *SubscriptionStore) GetEvent(id string) (*SubscriptionEvent, error) {
	return s.GetEventIn(tenant.All, id)
}

// GetEventIn returns an event on one of scope's subscriptions: an event on
// another environment's is ErrSubscriptionNotFound, as an unknown one is.
func (s *SubscriptionStore) GetEventIn(scope, id string) (*SubscriptionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.events[id]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	if sub, ok := s.subs[ev.SubscriptionID]; ok && !tenant.InScope(scope, sub.Env) {
		return nil, ErrSubscriptionNotFound
	}
	return ev, nil
}

// ReplaceTargets replaces every target on an event under If-Match.
func (s *SubscriptionStore) ReplaceTargets(eventID, ifMatch string, targetType TargetType, targetIDs []string) (*SubscriptionEvent, error) {
	if len(targetIDs) == 0 {
		return nil, errors.New("targetIds must contain at least one id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.events[eventID]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	if err := checkRowVersion(ev.RowVersion, ifMatch); err != nil {
		return nil, err
	}
	ev.Targets = nil
	for _, tid := range targetIDs {
		ev.Targets = append(ev.Targets, TargetDetail{
			ID:         s.id("tgt"),
			TargetID:   tid,
			TargetType: targetType,
		})
	}
	ev.RowVersion = s.nextRowVersion()
	changed(s.OnChange)
	return ev, nil
}

// DeleteEvent removes an event from its subscription under If-Match.
func (s *SubscriptionStore) DeleteEvent(eventID, ifMatch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.events[eventID]
	if !ok {
		return ErrSubscriptionNotFound
	}
	if err := checkRowVersion(ev.RowVersion, ifMatch); err != nil {
		return err
	}
	sub, ok := s.subs[ev.SubscriptionID]
	if ok {
		kept := sub.Events[:0]
		for _, e := range sub.Events {
			if e.ID != eventID {
				kept = append(kept, e)
			}
		}
		sub.Events = kept
		sub.RowVersion = s.nextRowVersion()
		sub.UpdatedAt = s.nowFunc()
	}
	delete(s.events, eventID)
	changed(s.OnChange)
	return nil
}

// Recipients is RecipientsIn for the default environment.
func (s *SubscriptionStore) Recipients(eventType string, targetIDs ...string) []Recipient {
	return s.RecipientsIn(tenant.Default, eventType, targetIDs...)
}

// RecipientsIn returns, for one notification about env's payment, every
// subscription of env's that should receive it paired with the event that
// matched -- so the notification can carry the subscriptionEventId a real
// one does. Another customer's subscriptions never hear of it, whatever
// their targets.
func (s *SubscriptionStore) RecipientsIn(env, eventType string, targetIDs ...string) []Recipient {
	owner := Owner(env)
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.subs))
	for id, sub := range s.subs {
		if sub.Env == owner {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids) // deterministic fan-out order
	var out []Recipient
	for _, id := range ids {
		sub := s.subs[id]
		if ev := sub.matchingEvent(eventType, targetIDs...); ev != nil {
			out = append(out, Recipient{Subscription: sub, Event: ev})
		}
	}
	return out
}

// Recipient pairs a subscription with the event on it that matched.
type Recipient struct {
	Subscription *Subscription
	Event        *SubscriptionEvent
}

// subscriptionState is a store's contents, deep-copied: the start-up seed a
// reset of scope all puts back (Bank).
type subscriptionState struct {
	subs   map[string]*Subscription
	rowSeq uint64
	idSeq  int
}

// snapshot copies the store as it stands.
func (s *SubscriptionStore) snapshot() subscriptionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, _ := copySubscriptions(s.subs)
	return subscriptionState{subs: subs, rowSeq: s.rowSeq, idSeq: s.idSeq}
}

// restoreLocked puts back scope's part of st -- a copy of it, so it can be
// put back again -- and says how many subscriptions that changed: each of
// scope's that st does not have, and each of st's in scope that had been
// deleted or written to since. Another environment's subscriptions are left
// as they are. The counters go back to st's only when nothing but st is
// left, as on first start: while another customer's subscriptions remain,
// they keep counting, so no id or rowVersion is handed out twice. Caller
// holds s.mu.
func (s *SubscriptionStore) restoreLocked(scope string, st subscriptionState) int {
	seed := map[string]*Subscription{}
	for id, sub := range st.subs {
		if tenant.InScope(scope, sub.Env) {
			seed[id] = sub
		}
	}
	changed := 0
	for id := range seed {
		if _, ok := s.subs[id]; !ok {
			changed++ // deleted since
		}
	}
	for id, sub := range s.subs {
		if !tenant.InScope(scope, sub.Env) {
			continue
		}
		if was, ok := seed[id]; !ok || !sameVersion(was, sub) {
			changed++ // not the seed's, or written to since
		}
		for _, e := range sub.Events {
			delete(s.events, e.ID)
		}
		delete(s.subs, id)
	}
	others := len(s.subs)
	subs, events := copySubscriptions(seed)
	for id, sub := range subs {
		s.subs[id] = sub
	}
	for id, ev := range events {
		s.events[id] = ev
	}
	if others == 0 {
		s.rowSeq, s.idSeq = st.rowSeq, st.idSeq
	}
	return changed
}

// sameVersion reports whether two copies of a subscription have the same
// rowVersions throughout. Every write moves one: the subscription's own, or
// an event's for a change of targets.
func sameVersion(a, b *Subscription) bool {
	if a.RowVersion != b.RowVersion || len(a.Events) != len(b.Events) {
		return false
	}
	for i := range a.Events {
		if a.Events[i].RowVersion != b.Events[i].RowVersion {
			return false
		}
	}
	return true
}

// copySubscriptions deep-copies subscriptions, and indexes the copies'
// events by id the way the store does.
func copySubscriptions(in map[string]*Subscription) (map[string]*Subscription, map[string]*SubscriptionEvent) {
	subs := make(map[string]*Subscription, len(in))
	events := map[string]*SubscriptionEvent{}
	for id, sub := range in {
		cp := *sub
		cp.Events = make([]*SubscriptionEvent, len(sub.Events))
		for i, e := range sub.Events {
			ec := *e
			ec.Targets = append([]TargetDetail(nil), e.Targets...)
			cp.Events[i] = &ec
			events[ec.ID] = &ec
		}
		subs[id] = &cp
	}
	return subs, events
}
