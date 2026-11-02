package core

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// The vault is a folder of age-encrypted copies, one per file. Its key (an
// age X25519 identity) lives only in the keyring; release deletes the key,
// which makes every vault file unreadable at once, even on SSDs where an
// ordinary delete can be undone.

func (a *App) vaultFile(e *Entry) string { return a.path("vault", e.ID+".age") }

// vaultRecipient returns the public vault key, creating a new key pair when
// there is none (first add, or first add after a release).
func (a *App) vaultRecipient() (age.Recipient, error) {
	if a.cfg.VaultRecipient != "" {
		if _, err := a.keys.Get(keyVault); err != nil {
			return nil, fmt.Errorf("vault key missing from keyring (%w): staged files cannot be read", err)
		}
		return age.ParseX25519Recipient(a.cfg.VaultRecipient)
	}
	if len(a.man.staged()) > 0 {
		return nil, errors.New("staged files exist but the vault has no key")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	if err := a.keys.Set(keyVault, id.String()); err != nil {
		return nil, fmt.Errorf("saving vault key to keyring: %w", err)
	}
	a.cfg.VaultRecipient = id.Recipient().String()
	if err := a.saveConfig(); err != nil {
		return nil, err
	}
	return id.Recipient(), nil
}

func (a *App) vaultIdentity() (age.Identity, error) {
	s, err := a.keys.Get(keyVault)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	return age.ParseX25519Identity(s)
}

// openVault returns a reader of the decrypted vault copy of e.
func (a *App) openVault(e *Entry, id age.Identity) (io.ReadCloser, error) {
	f, err := os.Open(a.vaultFile(e))
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(f, id)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{r, f}, nil
}

// AddReport lists what Add did.
type AddReport struct {
	Added   []string `json:"added"`
	Skipped []Issue  `json:"skipped"`
}

// Add copies files (folders are walked) into the vault, encrypted. Adding a
// path again replaces its earlier copy, and it will be uploaded again.
func (a *App) Add(ctx context.Context, paths []string, progress Progress) (*AddReport, error) {
	rep := &AddReport{}
	err := a.locked(func() error {
		if err := a.needInit(); err != nil {
			return err
		}
		rcpt, err := a.vaultRecipient()
		if err != nil {
			return err
		}
		a.cleanVault()
		files, skipped := a.collect(paths)
		rep.Skipped = skipped
		defer a.saveManifest()
		for i, p := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			progress.send("add", p, i+1, len(files))
			if err := a.addFile(p, rcpt); err != nil {
				rep.Skipped = append(rep.Skipped, Issue{File: p, Problem: err.Error()})
				continue
			}
			rep.Added = append(rep.Added, p)
			if i%100 == 99 {
				if err := a.saveManifest(); err != nil {
					return err
				}
			}
		}
		return a.saveManifest()
	})
	return rep, err
}

// collect expands paths into regular files, skipping links, devices and
// anything inside the home folder itself.
func (a *App) collect(paths []string) (files []string, skipped []Issue) {
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			skipped = append(skipped, Issue{File: p, Problem: err.Error()})
			continue
		}
		if within(abs, a.Home) {
			skipped = append(skipped, Issue{File: abs, Problem: "inside the skyhold home folder"})
			continue
		}
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				skipped = append(skipped, Issue{File: path, Problem: err.Error()})
				return nil
			}
			switch {
			case d.IsDir() && within(path, a.Home):
				return filepath.SkipDir
			case d.IsDir():
			case d.Type().IsRegular():
				add(path)
			default:
				skipped = append(skipped, Issue{File: path, Problem: "not a regular file (link or device)"})
			}
			return nil
		})
		if err != nil {
			skipped = append(skipped, Issue{File: abs, Problem: err.Error()})
		}
	}
	return files, skipped
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *App) addFile(path string, rcpt age.Recipient) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(a.path("vault"), ".add-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w, err := age.Encrypt(tmp, rcpt)
	if err != nil {
		tmp.Close()
		return err
	}
	sh, m5 := sha256.New(), md5.New()
	n, err := io.Copy(io.MultiWriter(w, sh, m5), src)
	if err == nil {
		err = w.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	e := &Entry{
		ID: id, Original: path, Remote: remotePath(path),
		Size: n, Mode: info.Mode().Perm(), ModTime: info.ModTime(),
		SHA256: hex.EncodeToString(sh.Sum(nil)), MD5: hex.EncodeToString(m5.Sum(nil)),
		Added: time.Now(), State: Staged,
	}
	if err := os.Rename(tmp.Name(), a.vaultFile(e)); err != nil {
		return err
	}
	if old := a.man.byOriginal(path); old != nil {
		if old.State == Staged {
			os.Remove(a.vaultFile(old))
		}
		*old = *e
	} else {
		a.man.Entries = append(a.man.Entries, e)
	}
	return nil
}

// Forget drops staged files from the vault and the manifest. Originals and
// anything already uploaded are left alone.
func (a *App) Forget(paths []string) (int, error) {
	n := 0
	err := a.locked(func() error {
		keep := a.man.Entries[:0]
		for _, e := range a.man.Entries {
			if e.State == Staged && matchesAny(e.Original, paths) {
				os.Remove(a.vaultFile(e))
				n++
				continue
			}
			keep = append(keep, e)
		}
		a.man.Entries = keep
		return a.saveManifest()
	})
	return n, err
}

// matchesAny reports whether path is one of paths or inside one of them.
func matchesAny(path string, paths []string) bool {
	for _, p := range paths {
		if abs, err := filepath.Abs(p); err == nil && within(path, abs) {
			return true
		}
	}
	return false
}

// cleanVault removes vault files no staged entry points to, left behind by
// a crash between writing a copy and saving the manifest.
func (a *App) cleanVault() {
	want := map[string]bool{}
	for _, e := range a.man.staged() {
		want[e.ID+".age"] = true
	}
	ents, _ := os.ReadDir(a.path("vault"))
	for _, d := range ents {
		if !want[d.Name()] {
			os.Remove(a.path("vault", d.Name()))
		}
	}
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
