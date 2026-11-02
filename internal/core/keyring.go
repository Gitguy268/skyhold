package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
)

// ErrNoSecret is returned when a key is not in the keyring.
var ErrNoSecret = errors.New("secret not found in keyring")

// Keyring stores the vault key, cloud key and remote credentials.
type Keyring interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// newKeyring returns the OS keychain (macOS Keychain, Windows Credential
// Manager, Secret Service on Linux), or a 0600 file in home when
// SKYHOLD_KEYRING=file. The file store is for machines without a keychain
// and for tests; it is weaker, because the keys sit next to the vault.
func newKeyring(home string) Keyring {
	if os.Getenv("SKYHOLD_KEYRING") == "file" {
		return &fileKeyring{path: filepath.Join(home, "keyring.json")}
	}
	// One service per home, so two homes (or a test run) never share keys.
	sum := sha256.Sum256([]byte(home))
	return osKeyring{service: "skyhold-" + hex.EncodeToString(sum[:6])}
}

type osKeyring struct{ service string }

func (k osKeyring) Get(key string) (string, error) {
	v, err := keyring.Get(k.service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoSecret
	}
	return v, err
}

func (k osKeyring) Set(key, value string) error { return keyring.Set(k.service, key, value) }

func (k osKeyring) Delete(key string) error {
	err := keyring.Delete(k.service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

type fileKeyring struct {
	mu   sync.Mutex
	path string
}

func (k *fileKeyring) load() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

func (k *fileKeyring) Get(key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}

func (k *fileKeyring) Set(key, value string) error {
	return k.update(func(m map[string]string) { m[key] = value })
}

func (k *fileKeyring) Delete(key string) error {
	return k.update(func(m map[string]string) { delete(m, key) })
}

func (k *fileKeyring) update(fn func(map[string]string)) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return err
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(k.path, b, 0o600)
}
