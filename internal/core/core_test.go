package core

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const pw = "correct horse battery"

type env struct {
	t     *testing.T
	app   *App
	src   string // folder with the originals
	cloud string // local folder standing in for the cloud
}

func newEnv(t *testing.T, encrypt bool) *env {
	t.Helper()
	t.Setenv("SKYHOLD_KEYRING", "file")
	e := &env{t: t, src: t.TempDir(), cloud: t.TempDir()}
	var err error
	if e.app, err = Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	key, err := e.app.Init(pw, encrypt)
	if err != nil {
		t.Fatal(err)
	}
	if encrypt != (key != "") {
		t.Fatalf("cloud key %q with encrypt=%v", key, encrypt)
	}
	if err := e.app.AddRemote(context.Background(), "disk", e.cloud); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) write(name, body string) string {
	e.t.Helper()
	p := filepath.Join(e.src, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func cloudFiles(t *testing.T, dir string) (names []string, all []byte) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
			b, _ := os.ReadFile(p)
			all = append(all, b...)
		}
		return nil
	})
	return names, all
}

func TestFullCycle(t *testing.T) {
	for _, encrypt := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypt], func(t *testing.T) {
			e := newEnv(t, encrypt)
			ctx := context.Background()
			a := e.write("notes/a.txt", "secret alpha")
			b := e.write("notes/deep/b.txt", "secret beta")

			rep, err := e.app.Add(ctx, []string{e.src}, nil)
			e.must(err)
			if len(rep.Added) != 2 {
				t.Fatalf("added %v, skipped %v", rep.Added, rep.Skipped)
			}
			// Vault copies are encrypted at rest.
			_, vault := cloudFiles(t, e.app.path("vault"))
			if bytes.Contains(vault, []byte("secret")) {
				t.Fatal("plaintext in vault")
			}

			prep, err := e.app.Push(ctx, nil)
			e.must(err)
			if prep.Uploaded != 2 || len(prep.Problems) > 0 {
				t.Fatalf("push: %+v", prep)
			}
			names, cloud := cloudFiles(t, e.cloud)
			hasPlain := bytes.Contains(cloud, []byte("secret alpha"))
			if encrypt == hasPlain {
				t.Fatalf("encrypt=%v but plaintext in cloud=%v", encrypt, hasPlain)
			}
			if encrypt && strings.Contains(strings.Join(names, " "), "a.txt") {
				t.Fatal("file name visible in encrypted cloud")
			}

			vrep, err := e.app.Verify(ctx, nil)
			e.must(err)
			if vrep.Checked != 2 || len(vrep.Problems) > 0 {
				t.Fatalf("verify: %+v", vrep)
			}

			if _, err := e.app.Release(ctx, "wrong password", nil); !errors.Is(err, ErrWrongPassword) {
				t.Fatalf("want wrong password, got %v", err)
			}
			var wait *WaitError
			if _, err := e.app.Release(ctx, pw, nil); !errors.As(err, &wait) {
				t.Fatalf("want wait error, got %v", err)
			}
			if !exists(a) {
				t.Fatal("deleted after wrong password")
			}
			time.Sleep(lockout(1))

			rel, err := e.app.Release(ctx, pw, nil)
			e.must(err)
			if len(rel.Deleted) != 2 || exists(a) || exists(b) {
				t.Fatalf("release: %+v", rel)
			}
			if _, err := e.app.keys.Get(keyVault); !errors.Is(err, ErrNoSecret) {
				t.Fatalf("vault key still there: %v", err)
			}
			if left, _ := os.ReadDir(e.app.path("vault")); len(left) > 0 {
				t.Fatalf("vault not empty: %v", left)
			}
			if !exists(rel.Receipt) {
				t.Fatal("no receipt")
			}

			rrep, err := e.app.Restore(ctx, RestoreOptions{}, nil)
			e.must(err)
			if len(rrep.Restored) != 2 || len(rrep.Problems) > 0 {
				t.Fatalf("restore: %+v", rrep)
			}
			if got, _ := os.ReadFile(b); string(got) != "secret beta" {
				t.Fatalf("restored %q", got)
			}
			if info, _ := os.Stat(a); runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
				t.Fatalf("mode %v", info.Mode())
			}

			// A new machine with only the remote spec and cloud key.
			key, err := e.app.CloudKey()
			e.must(err)
			to := t.TempDir()
			e.must(Recover(ctx, e.cloud, strings.ToLower(key), to))
			if got, _ := os.ReadFile(filepath.Join(to, filepath.FromSlash(strings.TrimPrefix(remotePath(a), "files/")))); string(got) != "secret alpha" {
				t.Fatalf("recovered %q", got)
			}

			// After a release, adding again makes a new vault key.
			c := e.write("c.txt", "gamma")
			_, err = e.app.Add(ctx, []string{c}, nil)
			e.must(err)
			if _, err := e.app.keys.Get(keyVault); err != nil {
				t.Fatal("no new vault key")
			}
		})
	}
}

func TestReleaseRefusesUnverified(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	a := e.write("a.txt", "one")
	_, err := e.app.Add(ctx, []string{a}, nil)
	e.must(err)
	var notSafe *NotSafeError
	if _, err := e.app.Release(ctx, pw, nil); !errors.As(err, &notSafe) {
		t.Fatalf("want NotSafeError, got %v", err)
	}
	if !exists(a) {
		t.Fatal("original deleted")
	}
	if _, err := e.app.keys.Get(keyVault); err != nil {
		t.Fatal("vault key destroyed")
	}

	// Pushed, then the cloud copy is damaged: still refused.
	_, err = e.app.Push(ctx, nil)
	e.must(err)
	e.must(os.WriteFile(filepath.Join(e.cloud, remotePath(a)), []byte("two"), 0o644))
	if _, err := e.app.Release(ctx, pw, nil); !errors.As(err, &notSafe) {
		t.Fatalf("want NotSafeError for damaged copy, got %v", err)
	}
	if !exists(a) {
		t.Fatal("original deleted")
	}

	// The failed check makes the next push upload it again.
	prep, err := e.app.Push(ctx, nil)
	e.must(err)
	if prep.Uploaded != 1 {
		t.Fatalf("push after damage: %+v", prep)
	}
	_, err = e.app.Release(ctx, pw, nil)
	e.must(err)
	if exists(a) {
		t.Fatal("original kept after repair")
	}
}

func TestReleaseKeepsChangedOriginal(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	a := e.write("a.txt", "version 1")
	b := e.write("b.txt", "stays the same")
	_, err := e.app.Add(ctx, []string{a, b}, nil)
	e.must(err)
	_, err = e.app.Push(ctx, nil)
	e.must(err)
	e.must(os.WriteFile(a, []byte("version 2"), 0o644))

	rel, err := e.app.Release(ctx, pw, nil)
	e.must(err)
	if !exists(a) || exists(b) || len(rel.Kept) != 1 {
		t.Fatalf("release: %+v", rel)
	}
	if got, _ := os.ReadFile(a); string(got) != "version 2" {
		t.Fatal("changed original was touched")
	}
}

func TestAddSkipsHomeAndLinks(t *testing.T) {
	e := newEnv(t, false)
	a := e.write("a.txt", "x")
	if err := os.Symlink(a, filepath.Join(e.src, "link")); err != nil {
		t.Skip("cannot create symlinks here:", err)
	}
	rep, err := e.app.Add(context.Background(), []string{e.src, e.app.Home}, nil)
	e.must(err)
	if len(rep.Added) != 1 || len(rep.Skipped) != 2 {
		t.Fatalf("add: %+v", rep)
	}
}

func TestRemotePath(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/a.txt":      "files/Users/me/a.txt",
		"/Users/me/x:y.txt":    "files/Users/me/x:y.txt",
		`C:\Users\me\a.txt`:    "files/C/Users/me/a.txt",
		`\\server\share\a.txt`: "files/server/share/a.txt",
	} {
		if got := remotePath(in); got != want {
			t.Errorf("remotePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLockout(t *testing.T) {
	if lockout(0) != 0 || lockout(1) != time.Second || lockout(4) != 8*time.Second || lockout(50) != 5*time.Minute {
		t.Fatal("lockout schedule")
	}
}
