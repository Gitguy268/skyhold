// Package tui is the terminal UI, built on Bubble Tea.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Gitguy268/skyhold/internal/core"
)

// Run opens the TUI until the user quits.
func Run(ctx context.Context, a *core.App) error {
	m := newModel(ctx, a)
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

type mode int

const (
	mList mode = iota
	mPick
	mRelease
	mBusy
	mInit
	mKey
	mRemote
)

var (
	title  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	cursor = lipgloss.NewStyle().Reverse(true)
	ok     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

type model struct {
	ctx  context.Context
	app  *core.App
	st   core.Status
	mode mode
	h    int

	rows []core.Entry // staged first, then released
	cur  int
	sel  map[string]bool
	msg  string
	err  string

	cwd    string
	ents   []os.DirEntry
	pcur   int
	picked map[string]bool

	prev *core.Preview
	pw   textinput.Model

	busy   string
	ev     core.Event
	events chan core.Event

	inputs  []textinput.Model // init: two passwords; remote: name, spec
	focus   int
	encrypt bool
	key     string
}

type statusMsg struct {
	st  core.Status
	err error
}
type eventMsg core.Event
type doneMsg struct {
	text string
	err  error
}
type previewMsg struct {
	p   *core.Preview
	err error
}

func newModel(ctx context.Context, a *core.App) model {
	home, _ := os.UserHomeDir()
	pw := textinput.New()
	pw.EchoMode, pw.EchoCharacter, pw.Placeholder = textinput.EchoPassword, '•', "release password"
	return model{ctx: ctx, app: a, h: 24, sel: map[string]bool{}, picked: map[string]bool{}, cwd: home, pw: pw, encrypt: true}
}

func (m model) Init() tea.Cmd { return m.loadStatus }

func (m model) loadStatus() tea.Msg {
	st, err := m.app.Status()
	return statusMsg{st, err}
}

func (m *model) setStatus(st core.Status) {
	m.st = st
	m.rows = m.rows[:0]
	for _, state := range []string{core.Staged, core.Released} {
		for _, e := range st.Entries {
			if e.State == state {
				m.rows = append(m.rows, e)
			}
		}
	}
	m.cur = min(m.cur, max(len(m.rows)-1, 0))
	if !st.Initialized && m.mode == mList {
		m.startInit()
	}
}

// run starts a long operation; progress events arrive as eventMsg.
func (m *model) run(name string, fn func(context.Context, core.Progress) (string, error)) tea.Cmd {
	ch := make(chan core.Event, 16)
	m.events, m.busy, m.ev, m.mode, m.msg, m.err = ch, name, core.Event{}, mBusy, "", ""
	ctx := m.ctx
	op := func() tea.Msg {
		text, err := fn(ctx, func(e core.Event) {
			select {
			case ch <- e:
			default:
			}
		})
		close(ch)
		return doneMsg{text, err}
	}
	return tea.Batch(op, wait(ch))
}

func wait(ch chan core.Event) tea.Cmd {
	return func() tea.Msg {
		if e, ok := <-ch; ok {
			return eventMsg(e)
		}
		return nil
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.h = msg.Height
		return m, nil
	case statusMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.setStatus(msg.st)
		}
		return m, nil
	case eventMsg:
		m.ev = core.Event(msg)
		return m, wait(m.events)
	case doneMsg:
		m.mode, m.msg, m.err = mList, msg.text, ""
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, m.loadStatus
	case previewMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.prev, m.mode = msg.p, mRelease
		m.pw.SetValue("")
		return m, m.pw.Focus()
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case mList:
			return m.keyList(msg)
		case mPick:
			return m.keyPick(msg)
		case mRelease:
			return m.keyRelease(msg)
		case mInit, mRemote:
			return m.keyForm(msg)
		case mKey:
			if msg.String() == "enter" {
				m.key, m.mode = "", mList
				return m, m.loadStatus
			}
		}
	}
	return m, nil
}

func (m model) keyList(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.err = "", ""
	switch k.String() {
	case "q", "esc":
		return m, tea.Quit
	case "up", "k":
		m.cur = max(m.cur-1, 0)
	case "down", "j":
		m.cur = min(m.cur+1, max(len(m.rows)-1, 0))
	case " ":
		if len(m.rows) > 0 {
			p := m.rows[m.cur].Original
			m.sel[p] = !m.sel[p]
			m.cur = min(m.cur+1, len(m.rows)-1)
		}
	case "a":
		m.mode, m.picked = mPick, map[string]bool{}
		m.readDir()
	case "n":
		m.startRemote()
	case "p":
		return m, m.run("upload", func(ctx context.Context, p core.Progress) (string, error) {
			rep, err := m.app.Push(ctx, p)
			if rep == nil {
				return "", err
			}
			return fmt.Sprintf("Uploaded %d file(s)", rep.Uploaded) + issueText(rep.Problems), err
		})
	case "v":
		return m, m.run("verify", func(ctx context.Context, p core.Progress) (string, error) {
			rep, err := m.app.Verify(ctx, p)
			if err != nil {
				return "", err
			}
			if len(rep.Problems) > 0 {
				return "", errors.New("not safely in the cloud:" + issueText(rep.Problems))
			}
			return fmt.Sprintf("Verified %d cloud copies", rep.Checked), nil
		})
	case "d":
		paths := m.selected(core.Staged)
		if len(paths) == 0 {
			m.err = "select staged files with space first"
			break
		}
		n, err := m.app.Forget(paths)
		m.msg = fmt.Sprintf("Removed %d file(s) from the vault", n)
		if err != nil {
			m.err = err.Error()
		}
		m.sel = map[string]bool{}
		return m, m.loadStatus
	case "r":
		return m, func() tea.Msg {
			p, err := m.app.Preview(m.ctx)
			if err == nil && len(p.Files) == 0 {
				err = errors.New("nothing staged to release")
			}
			return previewMsg{p, err}
		}
	case "R":
		paths := m.selected(core.Released)
		return m, m.run("restore", func(ctx context.Context, p core.Progress) (string, error) {
			rep, err := m.app.Restore(ctx, core.RestoreOptions{Paths: paths}, p)
			if rep == nil {
				return "", err
			}
			return fmt.Sprintf("Restored %d file(s)", len(rep.Restored)) + issueText(rep.Problems), err
		})
	}
	return m, nil
}

// selected returns the selected originals in state, or none.
func (m model) selected(state string) []string {
	var out []string
	for _, e := range m.rows {
		if e.State == state && m.sel[e.Original] {
			out = append(out, e.Original)
		}
	}
	return out
}

func (m *model) readDir() {
	ents, err := os.ReadDir(m.cwd)
	if err != nil {
		m.err = err.Error()
	}
	m.ents = m.ents[:0]
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			m.ents = append(m.ents, e)
		}
	}
	sort.SliceStable(m.ents, func(i, j int) bool { return m.ents[i].IsDir() && !m.ents[j].IsDir() })
	m.pcur = 0
}

func (m model) keyPick(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.err = ""
	switch k.String() {
	case "esc", "q":
		m.mode = mList
	case "up", "k":
		m.pcur = max(m.pcur-1, 0)
	case "down", "j":
		m.pcur = min(m.pcur+1, max(len(m.ents)-1, 0))
	case "left", "backspace", "h":
		m.cwd = filepath.Dir(m.cwd)
		m.readDir()
	case "right", "enter", "l":
		if len(m.ents) > 0 && m.ents[m.pcur].IsDir() {
			m.cwd = filepath.Join(m.cwd, m.ents[m.pcur].Name())
			m.readDir()
		}
	case " ":
		if len(m.ents) > 0 {
			p := filepath.Join(m.cwd, m.ents[m.pcur].Name())
			if m.picked[p] {
				delete(m.picked, p)
			} else {
				m.picked[p] = true
			}
			m.pcur = min(m.pcur+1, len(m.ents)-1)
		}
	case "a":
		var paths []string
		for p := range m.picked {
			paths = append(paths, p)
		}
		if len(paths) == 0 {
			m.err = "mark files or folders with space first"
			break
		}
		return m, m.run("add", func(ctx context.Context, p core.Progress) (string, error) {
			rep, err := m.app.Add(ctx, paths, p)
			if rep == nil {
				return "", err
			}
			return fmt.Sprintf("Added %d file(s)", len(rep.Added)) + issueText(rep.Skipped), err
		})
	}
	return m, nil
}

func (m model) keyRelease(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = mList
		m.pw.Blur()
		return m, nil
	case "enter":
		pw := m.pw.Value()
		m.pw.SetValue("")
		m.pw.Blur()
		return m, m.run("release", func(ctx context.Context, p core.Progress) (string, error) {
			rep, err := m.app.Release(ctx, pw, p)
			if err != nil {
				return "", err
			}
			text := fmt.Sprintf("Deleted %d original(s); vault key and vault copies are gone.", len(rep.Deleted))
			if len(rep.AlreadyGone) > 0 {
				text += fmt.Sprintf(" %d were already gone.", len(rep.AlreadyGone))
			}
			for _, w := range rep.Warnings {
				text += "\nWarning: " + w
			}
			return text + issueText(rep.Kept) + "\nReceipt: " + rep.Receipt, nil
		})
	}
	var cmd tea.Cmd
	m.pw, cmd = m.pw.Update(k)
	return m, cmd
}

func (m *model) startInit() {
	m.mode, m.focus = mInit, 0
	m.inputs = []textinput.Model{textinput.New(), textinput.New()}
	for i, ph := range []string{"release password (8+ characters)", "repeat it"} {
		m.inputs[i].Placeholder, m.inputs[i].EchoMode, m.inputs[i].EchoCharacter = ph, textinput.EchoPassword, '•'
	}
	m.inputs[0].Focus()
}

func (m *model) startRemote() {
	m.mode, m.focus = mRemote, 0
	m.inputs = []textinput.Model{textinput.New(), textinput.New()}
	m.inputs[0].Placeholder, m.inputs[1].Placeholder = "name", "rclone remote, e.g. b2:bucket/backup"
	m.inputs[1].Width = 60
	m.inputs[0].Focus()
}

func (m model) keyForm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		if m.mode == mInit {
			return m, tea.Quit
		}
		m.mode = mList
		return m, nil
	case "tab", "shift+tab", "up", "down":
		m.inputs[m.focus].Blur()
		m.focus = (m.focus + 1) % len(m.inputs)
		return m, m.inputs[m.focus].Focus()
	case "ctrl+e":
		if m.mode == mInit {
			m.encrypt = !m.encrypt
		}
		return m, nil
	case "enter":
		if m.focus < len(m.inputs)-1 {
			m.inputs[m.focus].Blur()
			m.focus++
			return m, m.inputs[m.focus].Focus()
		}
		a, b := m.inputs[0].Value(), m.inputs[1].Value()
		if m.mode == mInit {
			if a != b {
				m.err = "passwords do not match"
				return m, nil
			}
			key, err := m.app.Init(a, m.encrypt)
			if err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.err, m.key, m.mode = "", key, mKey
			if key == "" {
				m.mode = mList
			}
			return m, m.loadStatus
		}
		if err := m.app.AddRemote(m.ctx, a, b); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.err, m.msg, m.mode = "", "Added cloud "+a, mList
		return m, m.loadStatus
	}
	var cmd tea.Cmd
	m.inputs[m.focus], cmd = m.inputs[m.focus].Update(k)
	return m, cmd
}

func issueText(issues []core.Issue) string {
	var b strings.Builder
	for i, is := range issues {
		if i == 8 {
			fmt.Fprintf(&b, "\n  … and %d more", len(issues)-8)
			break
		}
		b.WriteString("\n  " + is.String())
	}
	return b.String()
}

func entryState(e core.Entry, remotes int) string {
	switch {
	case e.State == core.Released:
		return dim.Render("released")
	case remotes > 0 && len(e.Verified) == remotes:
		return ok.Render("verified")
	case remotes > 0 && len(e.Pushed) == remotes:
		return warn.Render("uploaded")
	default:
		return "staged  "
	}
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(title.Render("Skyhold") + dim.Render("  stage · upload · verify · release") + "\n")
	var remotes []string
	for _, r := range m.st.Remotes {
		remotes = append(remotes, r.Name+" ("+r.Backend+")")
	}
	if len(remotes) == 0 {
		remotes = []string{"none — press n"}
	}
	enc := "off"
	if m.st.EncryptCloud {
		enc = "on"
	}
	b.WriteString(dim.Render("clouds: "+strings.Join(remotes, ", ")+" · cloud encryption "+enc) + "\n\n")

	room := max(m.h-12, 5)
	switch m.mode {
	case mList:
		if len(m.rows) == 0 {
			b.WriteString(dim.Render("  Vault is empty. Press a to pick files.") + "\n")
		}
		start := max(0, m.cur-room+1)
		for i := start; i < len(m.rows) && i < start+room; i++ {
			e := m.rows[i]
			mark := "  "
			if m.sel[e.Original] {
				mark = "● "
			}
			line := mark + entryState(e, len(m.st.Remotes)) + "  " + e.Original
			if i == m.cur {
				line = cursor.Render(line)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n" + dim.Render("a add · p upload · v verify · r release · R restore · space select · d remove · n add cloud · q quit"))
	case mPick:
		b.WriteString("Pick files  " + dim.Render(m.cwd) + "\n")
		start := max(0, m.pcur-room+1)
		for i := start; i < len(m.ents) && i < start+room; i++ {
			e := m.ents[i]
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			mark := "  "
			if m.picked[filepath.Join(m.cwd, e.Name())] {
				mark = "● "
			}
			line := mark + name
			if i == m.pcur {
				line = cursor.Render(line)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString(fmt.Sprintf("\n%d marked  ", len(m.picked)) + dim.Render("space mark · enter/→ open · ← up · a add marked · esc back"))
	case mRelease:
		p := m.prev
		b.WriteString(bad.Render(fmt.Sprintf("Delete %d file(s) from this computer", len(p.Files))) +
			fmt.Sprintf(" (copies stay on %s)\n", strings.Join(p.Remotes, ", ")))
		for i, e := range p.Files {
			if i == room-4 {
				b.WriteString(dim.Render(fmt.Sprintf("  … and %d more", len(p.Files)-i)) + "\n")
				break
			}
			b.WriteString("  " + e.Original + "\n")
		}
		if p.Unpushed > 0 {
			b.WriteString(warn.Render(fmt.Sprintf("%d file(s) not uploaded yet: release will refuse.", p.Unpushed)) + "\n")
		}
		for _, w := range p.Warnings {
			b.WriteString(warn.Render("Warning: "+w) + "\n")
		}
		b.WriteString("\n" + m.pw.View() + "\n\n" + dim.Render("enter release · esc cancel"))
	case mBusy:
		bar := ""
		if m.ev.Of > 0 {
			w := 30
			n := m.ev.N * w / m.ev.Of
			bar = "[" + strings.Repeat("█", n) + strings.Repeat("·", w-n) + fmt.Sprintf("] %d/%d", m.ev.N, m.ev.Of)
		}
		b.WriteString(title.Render(m.busy+"…") + " " + bar + "\n" + dim.Render(m.ev.File))
	case mInit:
		b.WriteString("Set up: the release password is asked every time Skyhold deletes local files.\n\n")
		for _, in := range m.inputs {
			b.WriteString(in.View() + "\n")
		}
		box := "[ ]"
		if m.encrypt {
			box = "[x]"
		}
		b.WriteString("\n" + box + " encrypt names and contents in the cloud (ctrl+e)\n\n" + dim.Render("tab next · enter confirm · esc quit"))
	case mKey:
		b.WriteString("Your cloud key:\n\n  " + title.Render(m.key) + "\n\n" +
			"Write it down and keep it away from this computer. With it and the remote\n" +
			"you can recover your files anywhere; without it nobody can decrypt them.\n\n" + dim.Render("enter when done"))
	case mRemote:
		b.WriteString("Add a cloud: a remote from rclone.conf (b2:bucket/x) or a connection string\n(:sftp,host=nas.local,user=me:backups). Stored in the OS keychain.\n\n")
		for _, in := range m.inputs {
			b.WriteString(in.View() + "\n")
		}
		b.WriteString("\n" + dim.Render("tab next · enter add · esc back"))
	}
	if m.msg != "" {
		b.WriteString("\n\n" + ok.Render(m.msg))
	}
	if m.err != "" {
		b.WriteString("\n\n" + bad.Render(m.err))
	}
	return b.String() + "\n"
}
