package tairhash

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Client issues TairHash commands over a valkey-go client. It is safe for
// concurrent use; concurrent calls are auto-pipelined by valkey-go.
type Client struct {
	vk valkey.Client
}

// New wraps vk. The caller owns vk and closes it.
func New(vk valkey.Client) *Client {
	return &Client{vk: vk}
}

// Ping verifies the server has the TairHash module loaded.
func (c *Client) Ping(ctx context.Context) error {
	cmd := c.vk.B().Arbitrary("EXHLEN").Keys("tairhash:ping").ReadOnly()
	if err := c.vk.Do(ctx, cmd).Error(); err != nil {
		return opError("EXHLEN", "tairhash:ping", err)
	}
	return nil
}

// Set writes field=value under key (EXHSET).
func (c *Client) Set(ctx context.Context, key, field, value string, args SetArgs) (SetResult, error) {
	if err := args.Validate(); err != nil {
		return NotSet, err
	}
	extra := make([]string, 0, 7)
	extra = append(extra, field, value)
	extra = args.Expiry.appendArgs(extra)
	extra = args.Condition.appendArgs(extra)
	extra = args.Version.appendArgs(extra)
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHSET").Keys(key).Args(extra...).Build()).ToInt64()
	if err != nil {
		return NotSet, opError("EXHSET", key, err)
	}
	return SetResult(n), nil
}

// SetNX writes field=value only if the field is absent (EXHSETNX). It reports
// whether the field was written.
func (c *Client) SetNX(ctx context.Context, key, field, value string) (bool, error) {
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHSETNX").Keys(key).Args(field, value).Build()).ToInt64()
	if err != nil {
		return false, opError("EXHSETNX", key, err)
	}
	return n == 1, nil
}

// MSet writes several fields, clearing their TTLs and bumping their versions (EXHMSET).
func (c *Client) MSet(ctx context.Context, key string, fields []FieldValue) error {
	if len(fields) == 0 {
		return fmt.Errorf("%w: EXHMSET needs at least one field", ErrInvalidArgument)
	}
	args := make([]string, 0, 2*len(fields))
	for _, fv := range fields {
		args = append(args, fv.Field, fv.Value)
	}
	if err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHMSET").Keys(key).Args(args...).Build()).Error(); err != nil {
		return opError("EXHMSET", key, err)
	}
	return nil
}

// MSetWithOptions writes several fields with per-field version checks and TTLs
// (EXHMSETWITHOPTS). Version checks run before any write, so a stale version
// fails the whole command without partial effects.
func (c *Client) MSetWithOptions(ctx context.Context, key string, entries []MSetEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("%w: EXHMSETWITHOPTS needs at least one field", ErrInvalidArgument)
	}
	args := make([]string, 0, 4*len(entries))
	for _, e := range entries {
		if e.Version < 0 {
			return fmt.Errorf("%w: field %q has negative version %d", ErrInvalidArgument, e.Field, e.Version)
		}
		if e.TTL < time.Second || e.TTL%time.Second != 0 {
			return fmt.Errorf("%w: field %q TTL %s must be a whole number of seconds >= 1s", ErrInvalidArgument, e.Field, e.TTL)
		}
		args = append(args, e.Field, e.Value, strconv.FormatInt(e.Version, 10), strconv.FormatInt(int64(e.TTL/time.Second), 10))
	}
	if err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHMSETWITHOPTS").Keys(key).Args(args...).Build()).Error(); err != nil {
		return opError("EXHMSETWITHOPTS", key, err)
	}
	return nil
}

// IncrBy adds delta to an integer field, creating it at 0 if absent (EXHINCRBY).
// It returns the new value.
func (c *Client) IncrBy(ctx context.Context, key, field string, delta int64, args IncrArgs) (int64, error) {
	if err := args.Validate(); err != nil {
		return 0, err
	}
	extra := make([]string, 0, 11)
	extra = append(extra, field, strconv.FormatInt(delta, 10))
	extra = args.Expiry.appendArgs(extra)
	extra = args.Version.appendArgs(extra)
	if args.Min != nil {
		extra = append(extra, "MIN", strconv.FormatInt(*args.Min, 10))
	}
	if args.Max != nil {
		extra = append(extra, "MAX", strconv.FormatInt(*args.Max, 10))
	}
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHINCRBY").Keys(key).Args(extra...).Build()).ToInt64()
	if err != nil {
		return 0, opError("EXHINCRBY", key, err)
	}
	return n, nil
}

// IncrByFloat adds delta to a float field, creating it at 0 if absent
// (EXHINCRBYFLOAT). It returns the new value.
func (c *Client) IncrByFloat(ctx context.Context, key, field string, delta float64, args IncrFloatArgs) (float64, error) {
	if err := args.Validate(); err != nil {
		return 0, err
	}
	extra := make([]string, 0, 11)
	extra = append(extra, field, formatFloat(delta))
	extra = args.Expiry.appendArgs(extra)
	extra = args.Version.appendArgs(extra)
	if args.Min != nil {
		extra = append(extra, "MIN", formatFloat(*args.Min))
	}
	if args.Max != nil {
		extra = append(extra, "MAX", formatFloat(*args.Max))
	}
	f, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHINCRBYFLOAT").Keys(key).Args(extra...).Build()).AsFloat64()
	if err != nil {
		return 0, opError("EXHINCRBYFLOAT", key, err)
	}
	return f, nil
}

// Expire sets a field's TTL (EXHPEXPIRE or EXHPEXPIREAT), optionally gated on its
// version. exp must come from TTL or ExpireAt; use Persist to remove a TTL. It
// reports whether the field existed.
func (c *Client) Expire(ctx context.Context, key, field string, exp Expiry, ver VersionCheck) (bool, error) {
	if err := exp.Validate(); err != nil {
		return false, err
	}
	if err := ver.Validate(); err != nil {
		return false, err
	}
	var cmd, when string
	switch exp.kind {
	case expiryTTL:
		cmd, when = "EXHPEXPIRE", strconv.FormatInt(ceilMillis(exp.ttl), 10)
	case expiryAt:
		cmd, when = "EXHPEXPIREAT", strconv.FormatInt(exp.at.UnixMilli(), 10)
	default:
		return false, fmt.Errorf("%w: Expire needs TTL or ExpireAt", ErrInvalidArgument)
	}
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary(cmd).Keys(key).Args(ver.appendArgs([]string{field, when})...).Build()).ToInt64()
	if err != nil {
		return false, opError(cmd, key, err)
	}
	return n == 1, nil
}

// Persist removes a field's TTL (EXHPERSIST). It reports whether a TTL was removed.
func (c *Client) Persist(ctx context.Context, key, field string) (bool, error) {
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHPERSIST").Keys(key).Args(field).Build()).ToInt64()
	if err != nil {
		return false, opError("EXHPERSIST", key, err)
	}
	return n == 1, nil
}

// SetVersion overwrites a field's version (EXHSETVER). version must be > 0. It
// reports whether the field existed.
func (c *Client) SetVersion(ctx context.Context, key, field string, version int64) (bool, error) {
	if version <= 0 {
		return false, fmt.Errorf("%w: version must be > 0, got %d", ErrInvalidArgument, version)
	}
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHSETVER").Keys(key).Args(field, strconv.FormatInt(version, 10)).Build()).ToInt64()
	if err != nil {
		return false, opError("EXHSETVER", key, err)
	}
	return n == 1, nil
}

// Del removes fields and returns how many existed (EXHDEL).
func (c *Client) Del(ctx context.Context, key string, fields ...string) (int64, error) {
	if len(fields) == 0 {
		return 0, nil
	}
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHDEL").Keys(key).Args(fields...).Build()).ToInt64()
	if err != nil {
		return 0, opError("EXHDEL", key, err)
	}
	return n, nil
}

// DelWithVersion removes each field whose version matches, or any version when
// the given version is 0 (EXHDELWITHVER). It returns how many were removed.
func (c *Client) DelWithVersion(ctx context.Context, key string, fields []FieldVersion) (int64, error) {
	if len(fields) == 0 {
		return 0, nil
	}
	args := make([]string, 0, 2*len(fields))
	for _, f := range fields {
		if f.Version < 0 {
			return 0, fmt.Errorf("%w: field %q has negative version %d", ErrInvalidArgument, f.Field, f.Version)
		}
		args = append(args, f.Field, strconv.FormatInt(f.Version, 10))
	}
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHDELWITHVER").Keys(key).Args(args...).Build()).ToInt64()
	if err != nil {
		return 0, opError("EXHDELWITHVER", key, err)
	}
	return n, nil
}

// Get returns a field's value (EXHGET).
func (c *Client) Get(ctx context.Context, key, field string) (string, bool, error) {
	v, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHGET").Keys(key).Args(field).ReadOnly()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return "", false, nil
		}
		return "", false, opError("EXHGET", key, err)
	}
	return v, true, nil
}

// GetWithVersion returns a field's value and version (EXHGETWITHVER).
func (c *Client) GetWithVersion(ctx context.Context, key, field string) (Field, error) {
	msg, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHGETWITHVER").Keys(key).Args(field).ReadOnly()).ToMessage()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return Field{}, nil
		}
		return Field{}, opError("EXHGETWITHVER", key, err)
	}
	f, err := parseVersioned(msg)
	if err != nil {
		return Field{}, fmt.Errorf("EXHGETWITHVER %q: %w", key, err)
	}
	return f, nil
}

// MGet returns the values of several fields in request order (EXHMGET).
func (c *Client) MGet(ctx context.Context, key string, fields ...string) ([]Field, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	msgs, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHMGET").Keys(key).Args(fields...).ReadOnly()).ToArray()
	if err != nil {
		return nil, opError("EXHMGET", key, err)
	}
	out := make([]Field, len(msgs))
	for i, m := range msgs {
		if m.IsNil() {
			continue
		}
		v, err := m.ToString()
		if err != nil {
			return nil, fmt.Errorf("EXHMGET %q field %d: %w", key, i, err)
		}
		out[i] = Field{Value: v, Found: true}
	}
	return out, nil
}

// MGetWithVersion returns the values and versions of several fields in request
// order (EXHMGETWITHVER).
func (c *Client) MGetWithVersion(ctx context.Context, key string, fields ...string) ([]Field, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	msgs, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHMGETWITHVER").Keys(key).Args(fields...).ReadOnly()).ToArray()
	if err != nil {
		return nil, opError("EXHMGETWITHVER", key, err)
	}
	out := make([]Field, len(msgs))
	for i, m := range msgs {
		if m.IsNil() {
			continue
		}
		if out[i], err = parseVersioned(m); err != nil {
			return nil, fmt.Errorf("EXHMGETWITHVER %q field %d: %w", key, i, err)
		}
	}
	return out, nil
}

// GetAll returns every unexpired field (EXHGETALL).
func (c *Client) GetAll(ctx context.Context, key string) (map[string]string, error) {
	m, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHGETALL").Keys(key).ReadOnly()).AsStrMap()
	if err != nil {
		return nil, opError("EXHGETALL", key, err)
	}
	return m, nil
}

// GetAllWithVersion returns every unexpired field with its version (EXHGETALLWITHVER).
func (c *Client) GetAllWithVersion(ctx context.Context, key string) (map[string]Entry, error) {
	msgs, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHGETALLWITHVER").Keys(key).ReadOnly()).ToArray()
	if err != nil {
		return nil, opError("EXHGETALLWITHVER", key, err)
	}
	if len(msgs)%3 != 0 {
		return nil, fmt.Errorf("EXHGETALLWITHVER %q: reply has %d elements, want a multiple of 3", key, len(msgs))
	}
	out := make(map[string]Entry, len(msgs)/3)
	for i := 0; i < len(msgs); i += 3 {
		f, err := msgs[i].ToString()
		if err != nil {
			return nil, fmt.Errorf("EXHGETALLWITHVER %q: %w", key, err)
		}
		v, err := msgs[i+1].ToString()
		if err != nil {
			return nil, fmt.Errorf("EXHGETALLWITHVER %q field %q: %w", key, f, err)
		}
		ver, err := msgs[i+2].AsInt64()
		if err != nil {
			return nil, fmt.Errorf("EXHGETALLWITHVER %q field %q: %w", key, f, err)
		}
		out[f] = Entry{Value: v, Version: ver}
	}
	return out, nil
}

// Keys returns every unexpired field name (EXHKEYS).
func (c *Client) Keys(ctx context.Context, key string) ([]string, error) {
	s, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHKEYS").Keys(key).ReadOnly()).AsStrSlice()
	if err != nil {
		return nil, opError("EXHKEYS", key, err)
	}
	return s, nil
}

// Vals returns every unexpired field value (EXHVALS).
func (c *Client) Vals(ctx context.Context, key string) ([]string, error) {
	s, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHVALS").Keys(key).ReadOnly()).AsStrSlice()
	if err != nil {
		return nil, opError("EXHVALS", key, err)
	}
	return s, nil
}

// Len returns the number of fields (EXHLEN). Without excludeExpired the count is
// O(1) but may include expired fields not yet reclaimed; with it the server walks
// the hash (EXHLEN NOEXP).
func (c *Client) Len(ctx context.Context, key string, excludeExpired bool) (int64, error) {
	b := c.vk.B().Arbitrary("EXHLEN").Keys(key)
	if excludeExpired {
		b = b.Args("NOEXP")
	}
	n, err := c.vk.Do(ctx, b.ReadOnly()).ToInt64()
	if err != nil {
		return 0, opError("EXHLEN", key, err)
	}
	return n, nil
}

// Exists reports whether an unexpired field exists (EXHEXISTS).
func (c *Client) Exists(ctx context.Context, key, field string) (bool, error) {
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHEXISTS").Keys(key).Args(field).ReadOnly()).ToInt64()
	if err != nil {
		return false, opError("EXHEXISTS", key, err)
	}
	return n == 1, nil
}

// StrLen returns the length of a field's value, or 0 if absent (EXHSTRLEN).
func (c *Client) StrLen(ctx context.Context, key, field string) (int64, error) {
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHSTRLEN").Keys(key).Args(field).ReadOnly()).ToInt64()
	if err != nil {
		return 0, opError("EXHSTRLEN", key, err)
	}
	return n, nil
}

// PTTL returns a field's remaining lifetime (EXHPTTL).
func (c *Client) PTTL(ctx context.Context, key, field string) (FieldTTL, error) {
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHPTTL").Keys(key).Args(field).ReadOnly()).ToInt64()
	if err != nil {
		return FieldTTL{}, opError("EXHPTTL", key, err)
	}
	return ParsePTTL(n), nil
}

// ParsePTTL decodes a TairHash EXHPTTL reply: -2 missing key, -3 missing field,
// -1 no TTL, otherwise the remaining milliseconds.
func ParsePTTL(n int64) FieldTTL {
	switch {
	case n == -1:
		return FieldTTL{Exists: true}
	case n < 0:
		return FieldTTL{}
	default:
		return FieldTTL{Exists: true, Expires: true, Remaining: time.Duration(n) * time.Millisecond}
	}
}

// Version returns a field's version (EXHVER).
func (c *Client) Version(ctx context.Context, key, field string) (int64, bool, error) {
	n, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHVER").Keys(key).Args(field).ReadOnly()).ToInt64()
	if err != nil {
		return 0, false, opError("EXHVER", key, err)
	}
	if n < 0 {
		return 0, false, nil
	}
	return n, true, nil
}

// Scan returns one page of fields (EXHSCAN). Start with cursor 0 and continue
// until the returned cursor is 0.
func (c *Client) Scan(ctx context.Context, key string, cursor uint64, args ScanArgs) (ScanPage, error) {
	extra := []string{strconv.FormatUint(cursor, 10)}
	if args.Match != "" {
		extra = append(extra, "MATCH", args.Match)
	}
	if args.Count > 0 {
		extra = append(extra, "COUNT", strconv.FormatInt(args.Count, 10))
	}
	msgs, err := c.vk.Do(ctx, c.vk.B().Arbitrary("EXHSCAN").Keys(key).Args(extra...).ReadOnly()).ToArray()
	if err != nil {
		return ScanPage{}, opError("EXHSCAN", key, err)
	}
	if len(msgs) != 2 {
		return ScanPage{}, fmt.Errorf("EXHSCAN %q: reply has %d elements, want 2", key, len(msgs))
	}
	next, err := msgs[0].ToString()
	if err != nil {
		return ScanPage{}, fmt.Errorf("EXHSCAN %q cursor: %w", key, err)
	}
	page := ScanPage{}
	if page.Cursor, err = strconv.ParseUint(next, 10, 64); err != nil {
		return ScanPage{}, fmt.Errorf("EXHSCAN %q cursor %q: %w", key, next, err)
	}
	flat, err := msgs[1].AsStrSlice()
	if err != nil {
		return ScanPage{}, fmt.Errorf("EXHSCAN %q fields: %w", key, err)
	}
	if len(flat)%2 != 0 {
		return ScanPage{}, fmt.Errorf("EXHSCAN %q: odd field/value list length %d", key, len(flat))
	}
	page.Fields = make([]FieldValue, 0, len(flat)/2)
	for i := 0; i < len(flat); i += 2 {
		page.Fields = append(page.Fields, FieldValue{Field: flat[i], Value: flat[i+1]})
	}
	return page, nil
}

func parseVersioned(m valkey.ValkeyMessage) (Field, error) {
	pair, err := m.ToArray()
	if err != nil {
		return Field{}, err
	}
	if len(pair) != 2 {
		return Field{}, fmt.Errorf("versioned reply has %d elements, want 2", len(pair))
	}
	v, err := pair[0].ToString()
	if err != nil {
		return Field{}, err
	}
	ver, err := pair[1].AsInt64()
	if err != nil {
		return Field{}, err
	}
	return Field{Value: v, Version: ver, Found: true}, nil
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
