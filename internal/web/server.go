// Package web is the browser UI: a JSON API over core plus one embedded page.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Gitguy268/skyhold/internal/core"
)

//go:embed static
var static embed.FS

// Serve runs the web UI until ctx ends. Every API call needs the token that
// is printed in the URL, and the Host header must be the listen address, so
// other websites open in the same browser cannot drive it.
func Serve(ctx context.Context, a *core.App, addr string, open bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// SKYHOLD_TOKEN fixes the token for scripts and tests; otherwise it is
	// new on every start.
	token := os.Getenv("SKYHOLD_TOKEN")
	if token == "" {
		tok := make([]byte, 16)
		if _, err := rand.Read(tok); err != nil {
			return err
		}
		token = hex.EncodeToString(tok)
	}
	s := &server{app: a, token: token, addr: ln.Addr().String()}
	host, _, _ := net.SplitHostPort(s.addr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fmt.Fprintln(os.Stderr, "Warning: listening beyond this computer; anyone with the URL can use it.")
	}
	url := "http://" + s.addr + "/#t=" + s.token
	fmt.Println("Skyhold web UI:", url)
	if open {
		openBrowser(url)
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type server struct {
	app   *core.App
	token string
	addr  string

	mu  sync.Mutex
	job job
}

// job is the one long operation that may run at a time.
type job struct {
	Name    string     `json:"name"`
	Running bool       `json:"running"`
	Event   core.Event `json:"event"`
	Result  any        `json:"result,omitempty"`
	Error   string     `json:"error,omitempty"`
	// WaitSeconds is set when a wrong password locked the release.
	WaitSeconds int `json:"wait_seconds,omitempty"`
	// Problems is set when release refused because of unverified files.
	Problems []core.Issue `json:"problems,omitempty"`
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	page, _ := static.ReadFile("static/index.html")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Write(page)
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /api/status", s.status)
	api.HandleFunc("GET /api/ls", s.ls)
	api.HandleFunc("GET /api/preview", s.preview)
	api.HandleFunc("GET /api/job", s.getJob)
	api.HandleFunc("POST /api/init", s.init)
	api.HandleFunc("POST /api/remotes", s.addRemote)
	api.HandleFunc("DELETE /api/remotes/{name}", s.removeRemote)
	api.HandleFunc("POST /api/forget", s.forget)
	api.HandleFunc("POST /api/add", s.add)
	api.HandleFunc("POST /api/push", s.push)
	api.HandleFunc("POST /api/verify", s.verify)
	api.HandleFunc("POST /api/release", s.release)
	api.HandleFunc("POST /api/restore", s.restore)
	mux.Handle("/api/", s.guard(api))
	return s.checkHost(mux)
}

func (s *server) checkHost(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.addr)
	ok := []string{s.addr, "localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(ok, r.Host) {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Skyhold-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or wrong token: open the URL printed by skyhold serve"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	st, err := s.app.Status()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, st)
}

type lsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

// ls lists a local folder for the file picker. Browsers cannot hand a page
// real file paths, so the picker browses through the server instead.
func (s *server) ls(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out := []lsEntry{}
	for _, d := range ents {
		if strings.HasPrefix(d.Name(), ".") && r.URL.Query().Get("hidden") == "" {
			continue
		}
		e := lsEntry{Name: d.Name(), Path: filepath.Join(dir, d.Name()), Dir: d.IsDir()}
		if info, err := d.Info(); err == nil && !e.Dir {
			e.Size = info.Size()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	parent := filepath.Dir(dir)
	if parent == dir {
		parent = ""
	}
	writeJSON(w, map[string]any{"path": dir, "parent": parent, "entries": out})
}

func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	p, err := s.app.Preview(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, p)
}

func (s *server) init(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Encrypt  bool   `json:"encrypt"`
	}
	if !readBody(w, r, &req) {
		return
	}
	key, err := s.app.Init(req.Password, req.Encrypt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"cloud_key": key})
}

func (s *server) addRemote(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Spec string }
	if !readBody(w, r, &req) {
		return
	}
	if err := s.app.AddRemote(r.Context(), req.Name, req.Spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) removeRemote(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RemoveRemote(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) forget(w http.ResponseWriter, r *http.Request) {
	var req struct{ Paths []string }
	if !readBody(w, r, &req) {
		return
	}
	n, err := s.app.Forget(req.Paths)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]int{"forgotten": n})
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.job)
}

// start runs fn as the current job; the page polls /api/job for progress.
func (s *server) start(w http.ResponseWriter, name string, fn func(ctx context.Context, p core.Progress) (any, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.Running {
		writeErr(w, http.StatusConflict, fmt.Errorf("%s is still running", s.job.Name))
		return
	}
	s.job = job{Name: name, Running: true}
	progress := func(e core.Event) {
		s.mu.Lock()
		s.job.Event = e
		s.mu.Unlock()
	}
	go func() {
		res, err := fn(context.Background(), progress)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.job.Running, s.job.Result = false, res
		if err != nil {
			s.job.Error = err.Error()
			var wait *core.WaitError
			var notSafe *core.NotSafeError
			if errors.As(err, &wait) {
				s.job.WaitSeconds = int(wait.Wait.Seconds()) + 1
			}
			if errors.As(err, &notSafe) {
				s.job.Problems = notSafe.Problems
			}
		}
	}()
	writeJSON(w, map[string]bool{"started": true})
}

func (s *server) add(w http.ResponseWriter, r *http.Request) {
	var req struct{ Paths []string }
	if !readBody(w, r, &req) {
		return
	}
	s.start(w, "add", func(ctx context.Context, p core.Progress) (any, error) { return s.app.Add(ctx, req.Paths, p) })
}

func (s *server) push(w http.ResponseWriter, r *http.Request) {
	s.start(w, "push", func(ctx context.Context, p core.Progress) (any, error) { return s.app.Push(ctx, p) })
}

func (s *server) verify(w http.ResponseWriter, r *http.Request) {
	s.start(w, "verify", func(ctx context.Context, p core.Progress) (any, error) { return s.app.Verify(ctx, p) })
}

func (s *server) release(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if !readBody(w, r, &req) {
		return
	}
	s.start(w, "release", func(ctx context.Context, p core.Progress) (any, error) {
		return s.app.Release(ctx, req.Password, p)
	})
}

func (s *server) restore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths     []string
		To        string
		Overwrite bool
	}
	if !readBody(w, r, &req) {
		return
	}
	s.start(w, "restore", func(ctx context.Context, p core.Progress) (any, error) {
		return s.app.Restore(ctx, core.RestoreOptions{Paths: req.Paths, To: req.To, Overwrite: req.Overwrite}, p)
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}
