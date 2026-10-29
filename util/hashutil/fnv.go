package hashutil

import (
	"encoding/hex"
	"encoding/json"
	"hash/fnv"

	"github.com/cockroachdb/errors"
)

// FNV hashes the given object using a 64-bit FNV-1 hash. It does this by first
// JSON encoding the object, which sorts keys and therefore provides a deterministic
// byte array, and then hashing the result. The returned string is hex-encoded.
//
// See: https://pkg.go.dev/encoding/json#Marshal for more info about sorting keys.
func FNV(objects ...any) (string, error) {
	hasher := fnv.New64()
	for _, obj := range objects {
		// json.Marshal sorts the keys, so we get a deterministic raw byte array.
		raw, err := json.Marshal(obj)
		if err != nil {
			return "", errors.AssertionFailedf("unexpected json marshalling error: %v", err)
		}
		// "It never returns an error." - https://pkg.go.dev/hash#Hash
		_, _ = hasher.Write(raw)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// MustFNV is the same as FNV, but panics if it fails to return the hash of the
// given object.
func MustFNV(objects ...any) string {
	res, err := FNV(objects...)
	if err != nil {
		panic(err)
	}
	return res
}
