package core

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"time"

	"golang.org/x/crypto/argon2"
)

// PasswordHash is an argon2id hash of the release password.
type PasswordHash struct {
	Salt    []byte `json:"salt"`
	Hash    []byte `json:"hash"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

// MinPasswordLen is the shortest release password accepted.
const MinPasswordLen = 8

func hashPassword(pw string) (PasswordHash, error) {
	if len(pw) < MinPasswordLen {
		return PasswordHash{}, fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	h := PasswordHash{Salt: make([]byte, 16), Time: 3, Memory: 64 * 1024, Threads: 4}
	if _, err := rand.Read(h.Salt); err != nil {
		return h, err
	}
	h.Hash = argon2.IDKey([]byte(pw), h.Salt, h.Time, h.Memory, h.Threads, 32)
	return h, nil
}

func (h PasswordHash) matches(pw string) bool {
	if len(h.Hash) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), h.Salt, h.Time, h.Memory, h.Threads, uint32(len(h.Hash)))
	return subtle.ConstantTimeCompare(got, h.Hash) == 1
}

// WaitError means a wrong password was entered too recently.
type WaitError struct{ Wait time.Duration }

func (e *WaitError) Error() string {
	return fmt.Sprintf("too many wrong passwords: try again in %s", e.Wait.Round(time.Second))
}

// ErrWrongPassword is returned for a wrong release password.
var ErrWrongPassword = fmt.Errorf("wrong password")

// lockout is how long to wait after n wrong passwords: 1s, 2s, 4s … 5 min.
func lockout(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n > 9 {
		return 5 * time.Minute
	}
	return min(time.Second<<(n-1), 5*time.Minute)
}
