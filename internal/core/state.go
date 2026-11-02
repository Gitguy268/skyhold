package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Config is config.json in the home folder. It holds no secrets: the vault
// key, cloud key and remote specs live in the keyring.
type Config struct {
	Version      int          `json:"version"`
	Remotes      []string     `json:"remotes"`
	EncryptCloud bool         `json:"encrypt_cloud"`
	Password     PasswordHash `json:"password"`
	// VaultRecipient is the public half of the vault key. Empty after a
	// release: the next add creates a new key.
	VaultRecipient string   `json:"vault_recipient,omitempty"`
	Attempts       Attempts `json:"attempts"`
}

// Attempts tracks wrong passwords so the delay holds across processes.
type Attempts struct {
	Failed int       `json:"failed"`
	Last   time.Time `json:"last"`
}

// Entry is one file that went through the vault.
type Entry struct {
	ID       string               `json:"id"`
	Original string               `json:"original"`
	Remote   string               `json:"remote"`
	Size     int64                `json:"size"`
	Mode     os.FileMode          `json:"mode"`
	ModTime  time.Time            `json:"mod_time"`
	SHA256   string               `json:"sha256"`
	MD5      string               `json:"md5"`
	Added    time.Time            `json:"added"`
	State    string               `json:"state"`
	Pushed   map[string]time.Time `json:"pushed,omitempty"`
	Verified map[string]time.Time `json:"verified,omitempty"`
	Released time.Time            `json:"released,omitzero"`
}

// Entry states.
const (
	Staged   = "staged"
	Released = "released"
)

// Manifest is manifest.json in the home folder; a copy goes to each remote.
type Manifest struct {
	Version int      `json:"version"`
	Entries []*Entry `json:"entries"`
}

func (m *Manifest) byOriginal(path string) *Entry {
	for _, e := range m.Entries {
		if e.Original == path {
			return e
		}
	}
	return nil
}

func (m *Manifest) staged() []*Entry {
	var out []*Entry
	for _, e := range m.Entries {
		if e.State == Staged {
			out = append(out, e)
		}
	}
	return out
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

// writeFileAtomic writes to a temp file in the same folder and renames it,
// so a crash never leaves a half-written config or manifest.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), perm); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
