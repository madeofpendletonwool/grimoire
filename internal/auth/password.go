package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argon2Params are the cost settings for a hash. They travel inside every
// encoded hash rather than living only in this file, so raising the cost later
// leaves passwords hashed under the old settings verifiable.
type argon2Params struct {
	memory  uint32 // KiB
	time    uint32 // passes
	threads uint8
	keyLen  uint32
	saltLen uint32
}

// defaultParams follow the OWASP argon2id guidance (64 MiB, 3 passes). The
// thread count is fixed rather than derived from the host so a hash produced on
// a big machine still verifies on a small one — argon2 output depends on it.
var defaultParams = argon2Params{memory: 64 * 1024, time: 3, threads: 2, keyLen: 32, saltLen: 16}

var errBadHash = errors.New("malformed password hash")

// dummyHash is verified against when an account does not exist, so a login
// attempt for an unknown name costs the same as one for a real one. It is
// derived once, lazily, from the current parameters rather than pinned to a
// literal, so it stays representative if the cost settings change.
var dummyHash = sync.OnceValue(func() string {
	h, err := hashPassword("not-a-real-passphrase", defaultParams)
	if err != nil {
		return ""
	}
	return h
})

// hashPassword returns a PHC-format argon2id string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func hashPassword(password string, p argon2Params) (string, error) {
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, p.keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword recomputes the hash using the parameters recorded in encoded
// and compares in constant time.
func verifyPassword(encoded, password string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, p.keyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decodeHash(encoded string) (p argon2Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, errBadHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, errBadHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return p, nil, nil, errBadHash
	}
	p.saltLen = uint32(len(salt))
	p.keyLen = uint32(len(key))
	return p, salt, key, nil
}
