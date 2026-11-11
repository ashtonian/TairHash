package tairhash

import (
	"errors"
	"fmt"
	"strings"

	"github.com/valkey-io/valkey-go"
)

var (
	// ErrStaleVersion means a VER/GT version check failed.
	ErrStaleVersion = errors.New("tairhash: update version is stale")
	// ErrOverflow means an increment overflowed int64 or left its MIN/MAX bounds.
	ErrOverflow = errors.New("tairhash: increment would overflow or leave bounds")
	// ErrNotInteger means the field holds a value that is not an integer.
	ErrNotInteger = errors.New("tairhash: value is not an integer")
	// ErrNotFloat means the field holds a value that is not a float.
	ErrNotFloat = errors.New("tairhash: value is not a float")
	// ErrWrongType means the key holds a value that is not a TairHash.
	ErrWrongType = errors.New("tairhash: key holds a different type")
	// ErrInvalidArgument means the arguments were rejected before or by the server.
	ErrInvalidArgument = errors.New("tairhash: invalid argument")
	// ErrModuleNotLoaded means the server does not have the TairHash module loaded.
	ErrModuleNotLoaded = errors.New("tairhash: module not loaded")
)

// serverErrors maps the module's error replies (src/tairhash.h) to sentinels. The
// module exposes no machine-readable codes, so its reply text is the contract.
// Keys omit the "ERR " prefix, which valkey-go may already have stripped.
var serverErrors = map[string]error{
	"update version is stale":                                           ErrStaleVersion,
	"increment or decrement would overflow":                             ErrOverflow,
	"value is not an integer":                                           ErrNotInteger,
	"value is not an float":                                             ErrNotFloat,
	"syntax error":                                                      ErrInvalidArgument,
	"min or max is specified, but value is not an integer":              ErrNotInteger,
	"min or max is specified, but value is not a float":                 ErrNotFloat,
	"min value is bigger than max value":                                ErrInvalidArgument,
	"WRONGTYPE Operation against a key holding the wrong kind of value": ErrWrongType,
}

// MapServerError translates a server error reply into this package's sentinels,
// leaving transport and unknown errors untouched. Backends emulating TairHash
// semantics reuse it so callers branch on one set of errors.
func MapServerError(err error) error {
	return MapServerErrorWith(err, nil)
}

// MapServerErrorWith is MapServerError with extra reply-text mappings consulted
// first, for backends whose server words the same conditions differently.
func MapServerErrorWith(err error, extra map[string]error) error {
	if err == nil {
		return nil
	}
	ve, ok := valkey.IsValkeyErr(err)
	if !ok {
		return err
	}
	msg := strings.TrimPrefix(ve.Error(), "ERR ")
	if sentinel, found := extra[msg]; found {
		return fmt.Errorf("%w (%s)", sentinel, msg)
	}
	if sentinel, found := serverErrors[msg]; found {
		return fmt.Errorf("%w (%s)", sentinel, msg)
	}
	if strings.HasPrefix(msg, "unknown command") {
		return fmt.Errorf("%w (%s)", ErrModuleNotLoaded, msg)
	}
	return err
}

func opError(cmd, key string, err error) error {
	return fmt.Errorf("%s %q: %w", cmd, key, MapServerError(err))
}
