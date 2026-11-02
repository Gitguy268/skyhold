package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/sync"
)

func (a *App) openNamed(ctx context.Context, name string) (fs.Fs, error) {
	spec, err := a.keys.Get(keyRemote(name))
	if err != nil {
		return nil, fmt.Errorf("remote %s: %w", name, err)
	}
	key := ""
	if a.cfg.EncryptCloud {
		if key, err = a.keys.Get(keyCloud); err != nil {
			return nil, fmt.Errorf("cloud key: %w", err)
		}
	}
	return OpenRemote(ctx, spec, key, "")
}

func (a *App) needRemotes() error {
	if len(a.cfg.Remotes) == 0 {
		return errors.New("no remotes: add one with `skyhold remote add`")
	}
	return nil
}

// PushReport lists what Push did.
type PushReport struct {
	Uploaded int     `json:"uploaded"`
	Problems []Issue `json:"problems"`
}

// Push uploads staged files each remote does not have yet, decrypting them
// from the vault on the fly (rclone crypt re-encrypts them with the cloud
// key when that is on). A copy of the manifest goes to .skyhold/.
func (a *App) Push(ctx context.Context, progress Progress) (*PushReport, error) {
	rep := &PushReport{}
	err := a.locked(func() error {
		if err := a.needInit(); err != nil {
			return err
		}
		if err := a.needRemotes(); err != nil {
			return err
		}
		staged := a.man.staged()
		if len(staged) == 0 {
			return nil
		}
		id, err := a.vaultIdentity()
		if err != nil {
			return err
		}
		defer a.saveManifest()
		total := len(staged) * len(a.cfg.Remotes)
		n := 0
		for _, name := range a.cfg.Remotes {
			f, err := a.openNamed(ctx, name)
			if err != nil {
				rep.Problems = append(rep.Problems, Issue{Remote: name, Problem: err.Error()})
				n += len(staged)
				continue
			}
			for _, e := range staged {
				n++
				if _, done := e.Pushed[name]; done {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				progress.send("push", e.Original, n, total)
				if err := a.upload(ctx, f, e, id); err != nil {
					rep.Problems = append(rep.Problems, Issue{File: e.Original, Remote: name, Problem: err.Error()})
					continue
				}
				if e.Pushed == nil {
					e.Pushed = map[string]time.Time{}
				}
				e.Pushed[name] = time.Now()
				delete(e.Verified, name)
				rep.Uploaded++
				if rep.Uploaded%50 == 0 {
					a.saveManifest()
				}
			}
		}
		if err := a.saveManifest(); err != nil {
			return err
		}
		for _, p := range a.uploadManifest(ctx) {
			rep.Problems = append(rep.Problems, p)
		}
		return nil
	})
	return rep, err
}

func (a *App) upload(ctx context.Context, f fs.Fs, e *Entry, id age.Identity) error {
	r, err := a.openVault(e, id)
	if err != nil {
		return err
	}
	_, err = operations.RcatSize(ctx, f, e.Remote, r, e.Size, e.ModTime, nil)
	return err
}

// uploadManifest copies manifest.json to every remote, so the cloud copy
// says which local path each file came from.
func (a *App) uploadManifest(ctx context.Context) []Issue {
	b, err := json.MarshalIndent(&a.man, "", "  ")
	if err != nil {
		return []Issue{{File: remoteManifest, Problem: err.Error()}}
	}
	var out []Issue
	for _, name := range a.cfg.Remotes {
		f, err := a.openNamed(ctx, name)
		if err == nil {
			_, err = operations.RcatSize(ctx, f, remoteManifest, io.NopCloser(bytes.NewReader(b)), int64(len(b)), time.Now(), nil)
		}
		if err != nil {
			out = append(out, Issue{File: remoteManifest, Remote: name, Problem: err.Error()})
		}
	}
	return out
}

// VerifyReport lists what Verify found.
type VerifyReport struct {
	Checked  int     `json:"checked"`
	Problems []Issue `json:"problems"`
}

// Verify checks every staged file on every remote: size, then MD5 when the
// backend has it, otherwise a full download and SHA-256.
func (a *App) Verify(ctx context.Context, progress Progress) (*VerifyReport, error) {
	var rep *VerifyReport
	err := a.locked(func() error {
		if err := a.needInit(); err != nil {
			return err
		}
		var err error
		rep, err = a.verify(ctx, progress)
		if serr := a.saveManifest(); err == nil {
			err = serr
		}
		return err
	})
	return rep, err
}

func (a *App) verify(ctx context.Context, progress Progress) (*VerifyReport, error) {
	if err := a.needRemotes(); err != nil {
		return nil, err
	}
	rep := &VerifyReport{}
	staged := a.man.staged()
	total := len(staged) * len(a.cfg.Remotes)
	n := 0
	for _, name := range a.cfg.Remotes {
		f, err := a.openNamed(ctx, name)
		for _, e := range staged {
			n++
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			progress.send("verify", e.Original, n, total)
			problem := ""
			switch {
			case err != nil:
				problem = err.Error()
			case e.Pushed[name].IsZero():
				problem = "not uploaded yet"
			default:
				if cerr := check(ctx, f, e); cerr != nil {
					problem = cerr.Error()
					// The copy is gone or wrong, so the next push uploads it again.
					delete(e.Pushed, name)
				}
			}
			if problem != "" {
				delete(e.Verified, name)
				rep.Problems = append(rep.Problems, Issue{File: e.Original, Remote: name, Problem: problem})
				continue
			}
			if e.Verified == nil {
				e.Verified = map[string]time.Time{}
			}
			e.Verified[name] = time.Now()
			rep.Checked++
		}
	}
	return rep, nil
}

func check(ctx context.Context, f fs.Fs, e *Entry) error {
	obj, err := f.NewObject(ctx, e.Remote)
	if err != nil {
		return fmt.Errorf("not found in cloud: %w", err)
	}
	if obj.Size() != e.Size {
		return fmt.Errorf("size in cloud is %d, expected %d", obj.Size(), e.Size)
	}
	if f.Hashes().Contains(hash.MD5) {
		if h, err := obj.Hash(ctx, hash.MD5); err == nil && h != "" {
			if h != e.MD5 {
				return errors.New("MD5 in cloud does not match")
			}
			return nil
		}
	}
	r, err := obj.Open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sh := sha256.New()
	if _, err := io.Copy(sh, r); err != nil {
		return err
	}
	if hex.EncodeToString(sh.Sum(nil)) != e.SHA256 {
		return errors.New("SHA-256 of cloud copy does not match")
	}
	return nil
}

// RestoreOptions picks what Restore brings back and where.
type RestoreOptions struct {
	Paths     []string // originals (or folders of them); empty means every released file
	To        string   // put files under this folder instead of their original paths
	Overwrite bool
}

// RestoreReport lists what Restore did.
type RestoreReport struct {
	Restored []string `json:"restored"`
	Problems []Issue  `json:"problems"`
}

// Restore downloads files back from the first remote that has them and
// checks each against its SHA-256 before putting it in place.
func (a *App) Restore(ctx context.Context, opt RestoreOptions, progress Progress) (*RestoreReport, error) {
	rep := &RestoreReport{}
	err := a.locked(func() error {
		if err := a.needRemotes(); err != nil {
			return err
		}
		var todo []*Entry
		for _, e := range a.man.Entries {
			if (len(opt.Paths) == 0 && e.State == Released) || (len(opt.Paths) > 0 && matchesAny(e.Original, opt.Paths)) {
				todo = append(todo, e)
			}
		}
		remotes := map[string]fs.Fs{}
		for i, e := range todo {
			if err := ctx.Err(); err != nil {
				return err
			}
			target := e.Original
			if opt.To != "" {
				target = filepath.Join(opt.To, filepath.FromSlash(strings.TrimPrefix(e.Remote, "files/")))
			}
			progress.send("restore", target, i+1, len(todo))
			if _, err := os.Lstat(target); err == nil && !opt.Overwrite {
				rep.Problems = append(rep.Problems, Issue{File: target, Problem: "already exists (use overwrite)"})
				continue
			}
			var lastErr error
			for _, name := range a.cfg.Remotes {
				f, ok := remotes[name]
				if !ok {
					var err error
					if f, err = a.openNamed(ctx, name); err != nil {
						lastErr = err
						continue
					}
					remotes[name] = f
				}
				if lastErr = download(ctx, f, e, target); lastErr == nil {
					break
				}
			}
			if lastErr != nil {
				rep.Problems = append(rep.Problems, Issue{File: target, Problem: lastErr.Error()})
				continue
			}
			rep.Restored = append(rep.Restored, target)
		}
		return nil
	})
	return rep, err
}

func download(ctx context.Context, f fs.Fs, e *Entry, target string) error {
	obj, err := f.NewObject(ctx, e.Remote)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	r, err := obj.Open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".skyhold-restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	sh := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, sh), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(sh.Sum(nil)) != e.SHA256 {
		return errors.New("downloaded copy does not match its SHA-256")
	}
	if err := os.Chmod(tmp.Name(), e.Mode); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), e.ModTime, e.ModTime); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// Recover copies everything under files/ on a remote into a local folder,
// without any local state: for a new machine after the old one is gone.
func Recover(ctx context.Context, spec, cloudKey, to string) error {
	src, err := OpenRemote(ctx, spec, cloudKey, "files")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	dst, err := fs.NewFs(ctx, to)
	if err != nil {
		return err
	}
	return sync.CopyDir(ctx, dst, src, false)
}
