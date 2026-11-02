package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Preview is what the release screen shows before asking for the password.
type Preview struct {
	Files    []Entry  `json:"files"`
	Remotes  []string `json:"remotes"`
	Unpushed int      `json:"unpushed"`
	Warnings []string `json:"warnings"`
}

// Preview lists what Release would delete, plus warnings about copies that
// may survive on disk (no full-disk encryption, snapshots).
func (a *App) Preview(ctx context.Context) (*Preview, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(); err != nil {
		return nil, err
	}
	p := &Preview{Remotes: a.cfg.Remotes}
	var paths []string
	for _, e := range a.man.staged() {
		p.Files = append(p.Files, *e)
		paths = append(paths, e.Original)
		for _, r := range a.cfg.Remotes {
			if e.Pushed[r].IsZero() {
				p.Unpushed++
				break
			}
		}
	}
	if len(paths) > 0 {
		p.Warnings = diskWarnings(ctx, paths)
	}
	return p, nil
}

// ReleaseReport lists what Release did.
type ReleaseReport struct {
	Deleted     []string `json:"deleted"`
	AlreadyGone []string `json:"already_gone"`
	Kept        []Issue  `json:"kept"`
	Receipt     string   `json:"receipt"`
	Warnings    []string `json:"warnings"`
}

// NotSafeError means some file is not verified in the cloud, so nothing was
// deleted.
type NotSafeError struct{ Problems []Issue }

func (e *NotSafeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "nothing deleted: %d file(s) not safely in the cloud", len(e.Problems))
	for i, p := range e.Problems {
		if i == 5 {
			fmt.Fprintf(&b, "\n  … and %d more", len(e.Problems)-5)
			break
		}
		b.WriteString("\n  " + p.String())
	}
	return b.String()
}

// Release checks the password, verifies every staged file on every remote
// again, and only then destroys the vault key, deletes the vault copies and
// deletes the originals. An original that changed since it was added is
// kept, because the cloud has the older version.
func (a *App) Release(ctx context.Context, password string, progress Progress) (*ReleaseReport, error) {
	rep := &ReleaseReport{}
	err := a.locked(func() error {
		if err := a.needInit(); err != nil {
			return err
		}
		if err := a.checkPassword(password); err != nil {
			return err
		}
		staged := a.man.staged()
		if len(staged) == 0 {
			return errors.New("nothing staged to release")
		}
		vrep, err := a.verify(ctx, progress)
		a.saveManifest()
		if err != nil {
			return err
		}
		if len(vrep.Problems) > 0 {
			return &NotSafeError{Problems: vrep.Problems}
		}

		var deleteOriginal []*Entry
		for i, e := range staged {
			progress.send("check original", e.Original, i+1, len(staged))
			switch same, err := sameAsAdded(e); {
			case errors.Is(err, os.ErrNotExist):
				rep.AlreadyGone = append(rep.AlreadyGone, e.Original)
			case err != nil:
				rep.Kept = append(rep.Kept, Issue{File: e.Original, Problem: err.Error()})
			case !same:
				rep.Kept = append(rep.Kept, Issue{File: e.Original, Problem: "changed since it was added; the cloud has the older version"})
			default:
				deleteOriginal = append(deleteOriginal, e)
			}
		}

		// Key first: if anything below fails, the vault is already unreadable
		// and the cloud copies are verified, so nothing is lost or exposed.
		if err := a.keys.Delete(keyVault); err != nil {
			return fmt.Errorf("destroying vault key (nothing deleted): %w", err)
		}
		a.cfg.VaultRecipient = ""
		if err := a.saveConfig(); err != nil {
			return err
		}
		for _, e := range staged {
			if err := os.Remove(a.vaultFile(e)); err != nil && !errors.Is(err, os.ErrNotExist) {
				rep.Warnings = append(rep.Warnings, "vault copy: "+err.Error())
			}
		}
		for i, e := range deleteOriginal {
			progress.send("delete", e.Original, i+1, len(deleteOriginal))
			if err := os.Remove(e.Original); err != nil {
				rep.Kept = append(rep.Kept, Issue{File: e.Original, Problem: err.Error()})
				continue
			}
			rep.Deleted = append(rep.Deleted, e.Original)
		}
		now := time.Now()
		for _, e := range staged {
			e.State, e.Released = Released, now
		}
		if err := a.saveManifest(); err != nil {
			return err
		}
		rep.Receipt = a.path("receipts", now.Format("20060102-150405")+".json")
		if err := writeJSON(rep.Receipt, receipt{Time: now, Remotes: a.cfg.Remotes, Report: rep, Files: staged}); err != nil {
			rep.Warnings = append(rep.Warnings, "receipt: "+err.Error())
		}
		for _, p := range a.uploadManifest(ctx) {
			rep.Warnings = append(rep.Warnings, p.String())
		}
		return nil
	})
	return rep, err
}

type receipt struct {
	Time    time.Time      `json:"time"`
	Remotes []string       `json:"remotes"`
	Report  *ReleaseReport `json:"report"`
	Files   []*Entry       `json:"files"`
}

// checkPassword enforces the growing delay after wrong passwords. The count
// is saved in config.json, so restarting the tool does not reset it.
func (a *App) checkPassword(pw string) error {
	if wait := lockout(a.cfg.Attempts.Failed) - time.Since(a.cfg.Attempts.Last); wait > 0 {
		return &WaitError{Wait: wait}
	}
	if !a.cfg.Password.matches(pw) {
		a.cfg.Attempts.Failed++
		a.cfg.Attempts.Last = time.Now()
		if err := a.saveConfig(); err != nil {
			return err
		}
		return ErrWrongPassword
	}
	if a.cfg.Attempts.Failed > 0 {
		a.cfg.Attempts = Attempts{}
		return a.saveConfig()
	}
	return nil
}

// ChangePassword replaces the release password.
func (a *App) ChangePassword(old, new string) error {
	return a.locked(func() error {
		if err := a.needInit(); err != nil {
			return err
		}
		if err := a.checkPassword(old); err != nil {
			return err
		}
		h, err := hashPassword(new)
		if err != nil {
			return err
		}
		a.cfg.Password = h
		return a.saveConfig()
	})
}

func sameAsAdded(e *Entry) (bool, error) {
	f, err := os.Open(e.Original)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != e.Size {
		return false, nil
	}
	sh := sha256.New()
	if _, err := io.Copy(sh, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(sh.Sum(nil)) == e.SHA256, nil
}
