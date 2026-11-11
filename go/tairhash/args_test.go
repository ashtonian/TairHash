package tairhash

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSetArgsEncoding(t *testing.T) {
	at := time.UnixMilli(1_700_000_000_123)
	tests := []struct {
		name string
		args SetArgs
		want []string
	}{
		{"zero value clears ttl", SetArgs{}, nil},
		{"keepttl", SetArgs{Expiry: KeepTTL()}, []string{"KEEPTTL"}},
		{"ttl in ms", SetArgs{Expiry: TTL(1500 * time.Millisecond)}, []string{"PX", "1500"}},
		{"sub-ms ttl rounds up", SetArgs{Expiry: TTL(1500 * time.Microsecond)}, []string{"PX", "2"}},
		{"zero ttl expires now", SetArgs{Expiry: TTL(0)}, []string{"PX", "0"}},
		{"absolute", SetArgs{Expiry: ExpireAt(at)}, []string{"PXAT", "1700000000123"}},
		{"nx", SetArgs{Condition: IfNotExists}, []string{"NX"}},
		{"xx", SetArgs{Condition: IfExists}, []string{"XX"}},
		{"ver", SetArgs{Version: MatchVersion(3)}, []string{"VER", "3"}},
		{"abs", SetArgs{Version: ForceVersion(4)}, []string{"ABS", "4"}},
		{"gt", SetArgs{Version: AdvanceVersion(5)}, []string{"GT", "5"}},
		{
			"combined",
			SetArgs{Expiry: TTL(time.Second), Condition: IfExists, Version: MatchVersion(9)},
			[]string{"PX", "1000", "XX", "VER", "9"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, tt.args.Validate())
			var got []string
			got = tt.args.Expiry.appendArgs(got)
			got = tt.args.Condition.appendArgs(got)
			got = tt.args.Version.appendArgs(got)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestArgsValidation(t *testing.T) {
	one, zero := int64(1), int64(0)
	tests := []struct {
		name string
		err  error
	}{
		{"negative ttl", SetArgs{Expiry: TTL(-time.Second)}.Validate()},
		{"pre-epoch deadline", SetArgs{Expiry: ExpireAt(time.UnixMilli(-5))}.Validate()},
		{"unknown condition", SetArgs{Condition: Condition(9)}.Validate()},
		{"negative VER", SetArgs{Version: MatchVersion(-1)}.Validate()},
		{"zero ABS", SetArgs{Version: ForceVersion(0)}.Validate()},
		{"zero GT", SetArgs{Version: AdvanceVersion(0)}.Validate()},
		{"unknown version mode", SetArgs{Version: VersionCheck{Mode: 9}}.Validate()},
		{"max below min", IncrArgs{Min: &one, Max: &zero}.Validate()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.err, ErrInvalidArgument)
		})
	}
}

func TestParsePTTL(t *testing.T) {
	assert.Equal(t, FieldTTL{}, ParsePTTL(-2))
	assert.Equal(t, FieldTTL{}, ParsePTTL(-3))
	assert.Equal(t, FieldTTL{Exists: true}, ParsePTTL(-1))
	assert.Equal(t, FieldTTL{Exists: true, Expires: true, Remaining: 1500 * time.Millisecond}, ParsePTTL(1500))
	assert.Equal(t, FieldTTL{Exists: true, Expires: true}, ParsePTTL(0))
}
