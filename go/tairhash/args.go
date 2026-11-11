package tairhash

import (
	"fmt"
	"strconv"
	"time"
)

type expiryKind uint8

const (
	expiryClear expiryKind = iota
	expiryKeep
	expiryTTL
	expiryAt
)

// Expiry controls what a write does to a field's time-to-live.
//
// The zero value clears any existing TTL, matching EXHSET/EXHINCRBY without
// an expiration option.
type Expiry struct {
	kind expiryKind
	ttl  time.Duration
	at   time.Time
}

// NoExpiry clears the field's TTL. It is the zero value of Expiry.
func NoExpiry() Expiry { return Expiry{} }

// KeepTTL retains the field's existing TTL (KEEPTTL).
func KeepTTL() Expiry { return Expiry{kind: expiryKeep} }

// TTL expires the field d after the server applies the write (PX). Sub-millisecond
// remainders round up so a positive d never becomes "expire now"; d == 0 expires the
// field immediately, as PX 0 does on the server.
func TTL(d time.Duration) Expiry { return Expiry{kind: expiryTTL, ttl: d} }

// ExpireAt expires the field at t (PXAT), using millisecond resolution.
func ExpireAt(t time.Time) Expiry { return Expiry{kind: expiryAt, at: t} }

// IsKeep reports whether e retains the existing TTL.
func (e Expiry) IsKeep() bool { return e.kind == expiryKeep }

// IsClear reports whether e removes any existing TTL.
func (e Expiry) IsClear() bool { return e.kind == expiryClear }

// Relative returns the relative TTL in milliseconds when e was built with TTL.
func (e Expiry) Relative() (ms int64, ok bool) {
	if e.kind != expiryTTL {
		return 0, false
	}
	return ceilMillis(e.ttl), true
}

// Absolute returns the Unix-millisecond deadline when e was built with ExpireAt.
func (e Expiry) Absolute() (unixMs int64, ok bool) {
	if e.kind != expiryAt {
		return 0, false
	}
	return e.at.UnixMilli(), true
}

// Validate rejects expirations the server would refuse.
func (e Expiry) Validate() error {
	switch e.kind {
	case expiryTTL:
		if e.ttl < 0 {
			return fmt.Errorf("%w: negative TTL %s", ErrInvalidArgument, e.ttl)
		}
	case expiryAt:
		if e.at.UnixMilli() < 0 {
			return fmt.Errorf("%w: expiry %s is before the Unix epoch", ErrInvalidArgument, e.at)
		}
	}
	return nil
}

func (e Expiry) appendArgs(args []string) []string {
	switch e.kind {
	case expiryKeep:
		return append(args, "KEEPTTL")
	case expiryTTL:
		return append(args, "PX", strconv.FormatInt(ceilMillis(e.ttl), 10))
	case expiryAt:
		return append(args, "PXAT", strconv.FormatInt(e.at.UnixMilli(), 10))
	}
	return args
}

func ceilMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	if d > time.Duration(ms)*time.Millisecond {
		ms++
	}
	return ms
}

// Condition gates a write on the field's current existence.
type Condition uint8

const (
	// Always writes regardless of whether the field exists.
	Always Condition = iota
	// IfNotExists writes only when the field is absent (NX).
	IfNotExists
	// IfExists writes only when the field is present (XX).
	IfExists
)

func (c Condition) appendArgs(args []string) []string {
	switch c {
	case IfNotExists:
		return append(args, "NX")
	case IfExists:
		return append(args, "XX")
	}
	return args
}

// Validate rejects unknown conditions.
func (c Condition) Validate() error {
	if c > IfExists {
		return fmt.Errorf("%w: unknown condition %d", ErrInvalidArgument, c)
	}
	return nil
}

// VersionMode selects how a write interacts with the field's version.
type VersionMode uint8

const (
	// VersionAny performs no check; the server increments the version.
	VersionAny VersionMode = iota
	// VersionMatch requires the current version to equal the given one (VER).
	VersionMatch
	// VersionForce sets the version unconditionally (ABS).
	VersionForce
	// VersionAdvance requires the given version to exceed the current one and
	// stores it (GT).
	VersionAdvance
)

// VersionCheck is an optimistic-concurrency condition on a write. The zero value
// performs no check.
type VersionCheck struct {
	Mode    VersionMode
	Version int64
}

// MatchVersion applies the write only if the field's version equals v (VER v).
// Absent fields always match, and v == 0 disables the check, as on the server.
func MatchVersion(v int64) VersionCheck { return VersionCheck{Mode: VersionMatch, Version: v} }

// ForceVersion applies the write and sets the field's version to v (ABS v). v must be > 0.
func ForceVersion(v int64) VersionCheck { return VersionCheck{Mode: VersionForce, Version: v} }

// AdvanceVersion applies the write only if v is greater than the field's version,
// then sets the version to v (GT v). v must be > 0.
func AdvanceVersion(v int64) VersionCheck { return VersionCheck{Mode: VersionAdvance, Version: v} }

// Validate rejects version checks the server would refuse.
func (v VersionCheck) Validate() error {
	switch v.Mode {
	case VersionAny:
		return nil
	case VersionMatch:
		if v.Version < 0 {
			return fmt.Errorf("%w: negative version %d", ErrInvalidArgument, v.Version)
		}
	case VersionForce, VersionAdvance:
		if v.Version <= 0 {
			return fmt.Errorf("%w: ABS/GT version must be > 0, got %d", ErrInvalidArgument, v.Version)
		}
	default:
		return fmt.Errorf("%w: unknown version mode %d", ErrInvalidArgument, v.Mode)
	}
	return nil
}

func (v VersionCheck) appendArgs(args []string) []string {
	var tok string
	switch v.Mode {
	case VersionMatch:
		tok = "VER"
	case VersionForce:
		tok = "ABS"
	case VersionAdvance:
		tok = "GT"
	default:
		return args
	}
	return append(args, tok, strconv.FormatInt(v.Version, 10))
}

// SetArgs are the options of EXHSET.
type SetArgs struct {
	Expiry    Expiry
	Condition Condition
	Version   VersionCheck
}

// Validate rejects option combinations the server would refuse.
func (a SetArgs) Validate() error {
	if err := a.Expiry.Validate(); err != nil {
		return err
	}
	if err := a.Condition.Validate(); err != nil {
		return err
	}
	return a.Version.Validate()
}

// SetResult is the outcome of EXHSET.
type SetResult int8

const (
	// NotSet means the NX/XX condition was not met and nothing was written.
	NotSet SetResult = -1
	// Updated means an existing field was overwritten.
	Updated SetResult = 0
	// Created means a new field was written.
	Created SetResult = 1
)

// Applied reports whether the write took effect.
func (r SetResult) Applied() bool { return r != NotSet }

func (r SetResult) String() string {
	switch r {
	case NotSet:
		return "not-set"
	case Updated:
		return "updated"
	case Created:
		return "created"
	}
	return "SetResult(" + strconv.Itoa(int(r)) + ")"
}

// IncrArgs are the options of EXHINCRBY. Min and Max, when set, bound the
// resulting value; a result outside them fails with ErrOverflow.
type IncrArgs struct {
	Expiry  Expiry
	Version VersionCheck
	Min     *int64
	Max     *int64
}

// Validate rejects option combinations the server would refuse.
func (a IncrArgs) Validate() error {
	if err := a.Expiry.Validate(); err != nil {
		return err
	}
	if err := a.Version.Validate(); err != nil {
		return err
	}
	if a.Min != nil && a.Max != nil && *a.Max < *a.Min {
		return fmt.Errorf("%w: max %d is below min %d", ErrInvalidArgument, *a.Max, *a.Min)
	}
	return nil
}

// IncrFloatArgs are the options of EXHINCRBYFLOAT.
type IncrFloatArgs struct {
	Expiry  Expiry
	Version VersionCheck
	Min     *float64
	Max     *float64
}

// Validate rejects option combinations the server would refuse.
func (a IncrFloatArgs) Validate() error {
	if err := a.Expiry.Validate(); err != nil {
		return err
	}
	if err := a.Version.Validate(); err != nil {
		return err
	}
	if a.Min != nil && a.Max != nil && *a.Max < *a.Min {
		return fmt.Errorf("%w: max %g is below min %g", ErrInvalidArgument, *a.Max, *a.Min)
	}
	return nil
}

// FieldTTL describes a field's remaining lifetime.
type FieldTTL struct {
	// Exists is false when the key or field is absent or already expired.
	Exists bool
	// Expires is true when the field has a TTL.
	Expires bool
	// Remaining is the time left before expiry; zero unless Expires.
	Remaining time.Duration
}

// Field is a single field read, optionally with its version.
type Field struct {
	Value   string
	Version int64
	Found   bool
}

// Entry is a field value paired with its version.
type Entry struct {
	Value   string
	Version int64
}

// FieldVersion names a field and the version a conditional delete expects.
type FieldVersion struct {
	Field   string
	Version int64
}

// FieldValue is one field/value pair.
type FieldValue struct {
	Field string
	Value string
}

// MSetEntry is one field written by EXHMSETWITHOPTS.
type MSetEntry struct {
	Field string
	Value string
	// Version, when non-zero, must equal the field's current version or the
	// whole command fails with ErrStaleVersion. Absent fields always match.
	Version int64
	// TTL is relative and has whole-second resolution on the server. It must be
	// at least one second: the server treats 0 as "expire immediately".
	TTL time.Duration
}

// ScanArgs are the options of EXHSCAN.
type ScanArgs struct {
	Match string
	Count int64
}

// ScanPage is one page of EXHSCAN results. A zero Cursor ends the iteration.
type ScanPage struct {
	Cursor uint64
	Fields []FieldValue
}
