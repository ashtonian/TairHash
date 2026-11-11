// Package presence tracks which of a user's devices are online.
//
// Each user is one hash and each connected device one field with its own TTL. A
// device stays online by heartbeating; one that stops (crash, network partition,
// dead gateway) drops out when its TTL lapses, with no cleanup job and without
// affecting the user's other devices. Per-field expiry is exactly what TairHash
// provides, so this is the reference workload for comparing fieldhash backends.
package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ashtonian/TairHash/go/fieldhash"
	"github.com/ashtonian/TairHash/go/tairhash"
)

// Defaults for Registry options.
const (
	DefaultTTL       = 30 * time.Second
	DefaultKeyPrefix = "presence:"
)

// ErrInvalidSession means a heartbeat carried an empty identifier.
var ErrInvalidSession = errors.New("presence: invalid session")

// Session is one connected device.
type Session struct {
	Device string `json:"-"`
	// Node is the gateway holding the device's connection.
	Node string `json:"node"`
	// Status is an application-defined state such as "active" or "idle".
	Status string `json:"status,omitempty"`
}

// Registry records device heartbeats in a fieldhash.Store. It is safe for
// concurrent use.
type Registry struct {
	store  fieldhash.Store
	ttl    time.Duration
	prefix string
}

// Option configures a Registry.
type Option func(*Registry)

// WithTTL sets how long a device stays online after its last heartbeat.
func WithTTL(d time.Duration) Option { return func(r *Registry) { r.ttl = d } }

// WithKeyPrefix sets the prefix of each user's hash key.
func WithKeyPrefix(p string) Option { return func(r *Registry) { r.prefix = p } }

// New returns a Registry over store.
func New(store fieldhash.Store, opts ...Option) (*Registry, error) {
	r := &Registry{store: store, ttl: DefaultTTL, prefix: DefaultKeyPrefix}
	for _, o := range opts {
		o(r)
	}
	if r.ttl < time.Millisecond {
		return nil, fmt.Errorf("presence: TTL %s must be at least 1ms", r.ttl)
	}
	return r, nil
}

// TTL is how long a device stays online after a heartbeat.
func (r *Registry) TTL() time.Duration { return r.ttl }

func (r *Registry) key(user string) string { return r.prefix + user }

// Heartbeat marks s online for user and extends its lease by the registry TTL.
func (r *Registry) Heartbeat(ctx context.Context, user string, s Session) error {
	if user == "" || s.Device == "" {
		return fmt.Errorf("%w: user %q device %q", ErrInvalidSession, user, s.Device)
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("presence: encode session for device %q: %w", s.Device, err)
	}
	if _, err := r.store.Set(ctx, r.key(user), s.Device, string(payload), tairhash.TTL(r.ttl), tairhash.Always); err != nil {
		return fmt.Errorf("presence: heartbeat user %q device %q: %w", user, s.Device, err)
	}
	return nil
}

// Online returns user's live sessions ordered by device.
func (r *Registry) Online(ctx context.Context, user string) ([]Session, error) {
	fields, err := r.store.GetAll(ctx, r.key(user))
	if err != nil {
		return nil, fmt.Errorf("presence: list user %q: %w", user, err)
	}
	sessions := make([]Session, 0, len(fields))
	for device, payload := range fields {
		var s Session
		if err := json.Unmarshal([]byte(payload), &s); err != nil {
			return nil, fmt.Errorf("presence: decode user %q device %q: %w", user, device, err)
		}
		s.Device = device
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Device < sessions[j].Device })
	return sessions, nil
}

// Lookup returns one device's live session.
func (r *Registry) Lookup(ctx context.Context, user, device string) (Session, bool, error) {
	payload, found, err := r.store.Get(ctx, r.key(user), device)
	if err != nil {
		return Session{}, false, fmt.Errorf("presence: lookup user %q device %q: %w", user, device, err)
	}
	if !found {
		return Session{}, false, nil
	}
	var s Session
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		return Session{}, false, fmt.Errorf("presence: decode user %q device %q: %w", user, device, err)
	}
	s.Device = device
	return s, true, nil
}

// Disconnect removes a device immediately. It reports whether the device was online.
func (r *Registry) Disconnect(ctx context.Context, user, device string) (bool, error) {
	n, err := r.store.Del(ctx, r.key(user), device)
	if err != nil {
		return false, fmt.Errorf("presence: disconnect user %q device %q: %w", user, device, err)
	}
	return n == 1, nil
}
