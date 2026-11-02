// Package core is everything Skyhold does; the CLI, TUI and web UI are
// thin layers over it.
package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fspath"
)

// Keyring item names.
const (
	keyVault = "vault-identity"
	keyCloud = "cloud-key"
)

func keyRemote(name string) string { return "remote/" + name }

// DefaultHome is $SKYHOLD_HOME, or a skyhold folder in the user's config
// directory (~/Library/Application Support, %AppData%, ~/.config).
func DefaultHome() (string, error) {
	if h := os.Getenv("SKYHOLD_HOME"); h != "" {
		return filepath.Abs(h)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "skyhold"), nil
}

// App is one Skyhold home folder: config, manifest and encrypted vault.
type App struct {
	Home  string
	keys  Keyring
	mu    sync.Mutex
	flock *flock.Flock
	cfg   Config
	man   Manifest
}

// Open opens (and creates if needed) the home folder.
func Open(home string) (*App, error) {
	if err := os.MkdirAll(filepath.Join(home, "vault"), 0o700); err != nil {
		return nil, err
	}
	a := &App{Home: home, keys: newKeyring(home), flock: flock.New(filepath.Join(home, "lock"))}
	return a, a.load()
}

func (a *App) path(name ...string) string { return filepath.Join(append([]string{a.Home}, name...)...) }

func (a *App) load() error {
	a.cfg, a.man = Config{}, Manifest{}
	if err := readJSON(a.path("config.json"), &a.cfg); err != nil {
		return fmt.Errorf("config.json: %w", err)
	}
	if err := readJSON(a.path("manifest.json"), &a.man); err != nil {
		return fmt.Errorf("manifest.json: %w", err)
	}
	return nil
}

func (a *App) saveConfig() error {
	a.cfg.Version = 1
	return writeJSON(a.path("config.json"), &a.cfg)
}

func (a *App) saveManifest() error {
	a.man.Version = 1
	return writeJSON(a.path("manifest.json"), &a.man)
}

// ErrBusy means another Skyhold process holds the home folder.
var ErrBusy = errors.New("another skyhold process is using this home folder")

// locked runs fn with the home folder locked against other processes and
// with config and manifest freshly loaded.
func (a *App) locked(fn func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	ok, err := a.flock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return ErrBusy
	}
	defer a.flock.Unlock()
	if err := a.load(); err != nil {
		return err
	}
	return fn()
}

// ErrNotInitialized means `skyhold init` has not run for this home.
var ErrNotInitialized = errors.New("not set up yet: run `skyhold init` first")

func (a *App) needInit() error {
	if len(a.cfg.Password.Hash) == 0 {
		return ErrNotInitialized
	}
	return nil
}

// Init sets the release password and, with encryptCloud, creates the cloud
// key. It returns the cloud key so the user can write it down.
func (a *App) Init(password string, encryptCloud bool) (cloudKey string, err error) {
	err = a.locked(func() error {
		if a.needInit() == nil {
			return errors.New("already set up")
		}
		h, err := hashPassword(password)
		if err != nil {
			return err
		}
		if encryptCloud {
			if cloudKey, err = newCloudKey(); err != nil {
				return err
			}
			if err := a.keys.Set(keyCloud, cloudKey); err != nil {
				return fmt.Errorf("saving cloud key to keyring: %w", err)
			}
		}
		a.cfg.Password, a.cfg.EncryptCloud = h, encryptCloud
		return a.saveConfig()
	})
	return cloudKey, err
}

// CloudKey returns the cloud key, or "" when the cloud copy is not encrypted.
func (a *App) CloudKey() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(); err != nil || !a.cfg.EncryptCloud {
		return "", err
	}
	return a.keys.Get(keyCloud)
}

var remoteName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// AddRemote checks that spec opens and saves it under name. The spec can
// hold credentials, so it goes into the keyring, not config.json.
func (a *App) AddRemote(ctx context.Context, name, spec string) error {
	if !remoteName.MatchString(name) {
		return errors.New("remote name: use 1-40 letters, digits, - or _")
	}
	if _, err := OpenRemote(ctx, spec, "", ""); err != nil {
		if errors.Is(err, fs.ErrorIsFile) {
			return errors.New(`opening remote: the path is a file, not a folder; if the spec has a value with : or , in it, quote it, e.g. endpoint="http://host:9000"`)
		}
		return fmt.Errorf("opening remote: %w", err)
	}
	return a.locked(func() error {
		if err := a.needInit(); err != nil {
			return err
		}
		if slices.Contains(a.cfg.Remotes, name) {
			return fmt.Errorf("remote %q already exists", name)
		}
		if err := a.keys.Set(keyRemote(name), spec); err != nil {
			return fmt.Errorf("saving remote to keyring: %w", err)
		}
		a.cfg.Remotes = append(a.cfg.Remotes, name)
		return a.saveConfig()
	})
}

// RemoveRemote forgets a remote. Files already uploaded there stay.
func (a *App) RemoveRemote(name string) error {
	return a.locked(func() error {
		i := slices.Index(a.cfg.Remotes, name)
		if i < 0 {
			return fmt.Errorf("no remote %q", name)
		}
		a.cfg.Remotes = slices.Delete(a.cfg.Remotes, i, i+1)
		for _, e := range a.man.Entries {
			delete(e.Pushed, name)
			delete(e.Verified, name)
		}
		if err := a.saveManifest(); err != nil {
			return err
		}
		if err := a.saveConfig(); err != nil {
			return err
		}
		return a.keys.Delete(keyRemote(name))
	})
}

// RemoteInfo describes a remote without its credentials.
type RemoteInfo struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Path    string `json:"path"`
}

// Status is a snapshot for the UIs.
type Status struct {
	Home         string       `json:"home"`
	Initialized  bool         `json:"initialized"`
	EncryptCloud bool         `json:"encrypt_cloud"`
	Remotes      []RemoteInfo `json:"remotes"`
	Entries      []Entry      `json:"entries"`
}

// Status reads the current state.
func (a *App) Status() (Status, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(); err != nil {
		return Status{}, err
	}
	s := Status{Home: a.Home, Initialized: a.needInit() == nil, EncryptCloud: a.cfg.EncryptCloud}
	for _, name := range a.cfg.Remotes {
		info := RemoteInfo{Name: name}
		if spec, err := a.keys.Get(keyRemote(name)); err == nil {
			if p, err := fspath.Parse(spec); err == nil {
				info.Backend, info.Path = p.Name, p.Path
			}
			if info.Backend == "" {
				info.Backend = "local"
			}
		}
		s.Remotes = append(s.Remotes, info)
	}
	for _, e := range a.man.Entries {
		s.Entries = append(s.Entries, *e)
	}
	return s, nil
}

// Event reports progress of a long operation.
type Event struct {
	Step string `json:"step"`
	File string `json:"file"`
	N    int    `json:"n"`
	Of   int    `json:"of"`
}

// Progress receives events; nil is fine.
type Progress func(Event)

func (p Progress) send(step, file string, n, of int) {
	if p != nil {
		p(Event{Step: step, File: file, N: n, Of: of})
	}
}

// Issue is a problem with one file.
type Issue struct {
	File    string `json:"file"`
	Remote  string `json:"remote,omitempty"`
	Problem string `json:"problem"`
}

func (i Issue) String() string {
	if i.Remote != "" {
		return fmt.Sprintf("%s (%s): %s", i.File, i.Remote, i.Problem)
	}
	return i.File + ": " + i.Problem
}
