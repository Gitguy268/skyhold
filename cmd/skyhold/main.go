// Command skyhold stages files in an encrypted vault, backs them up to any
// rclone remote, and deletes the local copies once the cloud has them.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Gitguy268/skyhold/internal/core"
	"github.com/Gitguy268/skyhold/internal/tui"
	"github.com/Gitguy268/skyhold/internal/web"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "skyhold:", err)
		os.Exit(1)
	}
}

var passwordStdin bool

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skyhold",
		Short: "Stage files, back them up to any cloud, then drop the local copies",
		Long: `Skyhold copies files into an encrypted local vault, uploads them to one or
more rclone remotes (S3, B2, Drive, OneDrive, Dropbox, WebDAV, SFTP and about 60 more),
and, once every file is verified in the cloud and you enter your password,
deletes the vault and the original files. The cloud copies stay.

Run without a command to open the terminal UI.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return cmd.Help()
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			return tui.Run(cmd.Context(), a)
		},
	}
	cmd.PersistentFlags().BoolVar(&passwordStdin, "password-stdin", false, "read passwords from stdin, one per line")
	cmd.AddCommand(initCmd(), remoteCmd(), addCmd(), forgetCmd(), pushCmd(), verifyCmd(), statusCmd(),
		releaseCmd(), restoreCmd(), recoverCmd(), cloudKeyCmd(), passwdCmd(), serveCmd())
	return cmd
}

func openApp() (*core.App, error) {
	home, err := core.DefaultHome()
	if err != nil {
		return nil, err
	}
	return core.Open(home)
}

// withApp turns a func that needs the app into a cobra RunE.
func withApp(fn func(ctx context.Context, a *core.App, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		return fn(cmd.Context(), a, args)
	}
}

var stdin = bufio.NewReader(os.Stdin)

func readPassword(prompt string) (string, error) {
	if passwordStdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func newPassword() (string, error) {
	p1, err := readPassword("New release password: ")
	if err != nil {
		return "", err
	}
	p2, err := readPassword("Repeat it: ")
	if err != nil {
		return "", err
	}
	if p1 != p2 {
		return "", errors.New("passwords do not match")
	}
	return p1, nil
}

// progress prints a one-line counter on stderr when it is a terminal.
func progress() core.Progress {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil
	}
	return func(e core.Event) {
		name := e.File
		if len(name) > 60 {
			name = "…" + name[len(name)-59:]
		}
		fmt.Fprintf(os.Stderr, "\r\033[K%s %d/%d %s", e.Step, e.N, e.Of, name)
	}
}

func doneProgress() {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func printIssues(title string, issues []core.Issue) {
	if len(issues) == 0 {
		return
	}
	fmt.Printf("%s (%d):\n", title, len(issues))
	for _, i := range issues {
		fmt.Println("  " + i.String())
	}
}

func printKey(key string) {
	fmt.Printf(`
Cloud key: %s

Write this down and keep it away from this computer. With it and the remote
you can recover your files on any machine (skyhold recover). Without it the
cloud copies cannot be decrypted, by you or anyone else.
`, key)
}

func initCmd() *cobra.Command {
	var encrypt bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set the release password (and create the cloud key)",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			pw, err := newPassword()
			if err != nil {
				return err
			}
			key, err := a.Init(pw, encrypt)
			if err != nil {
				return err
			}
			fmt.Println("Set up in", a.Home)
			if key != "" {
				printKey(key)
			}
			fmt.Println("\nNext: skyhold remote add <name> <rclone remote>")
			return nil
		}),
	}
	cmd.Flags().BoolVar(&encrypt, "encrypt-cloud", true, "encrypt names and contents in the cloud with a cloud key")
	return cmd
}

func remoteCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "Manage cloud remotes"}
	cmd.AddCommand(&cobra.Command{
		Use:   "add <name> <rclone remote>",
		Short: "Add a remote, e.g. b2:bucket/backup or :s3,provider=AWS,env_auth=true:bucket",
		Long: `Add a remote. The second argument is anything rclone accepts:
a remote from your rclone.conf (b2:bucket/path), or a connection string
(:sftp,host=nas.local,user=me:backups). The string is stored in the OS
keychain, because it can contain credentials.`,
		Args: cobra.ExactArgs(2),
		RunE: withApp(func(ctx context.Context, a *core.App, args []string) error {
			if err := a.AddRemote(ctx, args[0], args[1]); err != nil {
				return err
			}
			fmt.Println("Added remote", args[0])
			return nil
		}),
	}, &cobra.Command{
		Use:   "list",
		Short: "List remotes",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			s, err := a.Status()
			if err != nil {
				return err
			}
			for _, r := range s.Remotes {
				fmt.Printf("%-16s %-10s %s\n", r.Name, r.Backend, r.Path)
			}
			return nil
		}),
	}, &cobra.Command{
		Use:   "rm <name>",
		Short: "Forget a remote (files already uploaded stay there)",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(ctx context.Context, a *core.App, args []string) error {
			return a.RemoveRemote(args[0])
		}),
	}, &cobra.Command{
		Use:   "config",
		Short: "Run rclone's interactive setup to create a named remote in rclone.conf",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return core.EditRcloneConfig(cmd.Context())
		},
	})
	return cmd
}

func addCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <file or folder>...",
		Short: "Copy files into the encrypted vault",
		Args:  cobra.MinimumNArgs(1),
		RunE: withApp(func(ctx context.Context, a *core.App, args []string) error {
			rep, err := a.Add(ctx, args, progress())
			doneProgress()
			if rep != nil {
				fmt.Printf("Added %d file(s)\n", len(rep.Added))
				printIssues("Skipped", rep.Skipped)
			}
			return err
		}),
	}
}

func forgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <file or folder>...",
		Short: "Drop staged files from the vault (originals and cloud copies stay)",
		Args:  cobra.MinimumNArgs(1),
		RunE: withApp(func(ctx context.Context, a *core.App, args []string) error {
			n, err := a.Forget(args)
			fmt.Printf("Forgot %d file(s)\n", n)
			return err
		}),
	}
}

func pushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push",
		Short: "Upload staged files to every remote",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			rep, err := a.Push(ctx, progress())
			doneProgress()
			if rep != nil {
				fmt.Printf("Uploaded %d file(s)\n", rep.Uploaded)
				printIssues("Problems", rep.Problems)
			}
			return err
		}),
	}
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Check that every staged file is complete and correct in the cloud",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			rep, err := a.Verify(ctx, progress())
			doneProgress()
			if err != nil {
				return err
			}
			fmt.Printf("Verified %d copies\n", rep.Checked)
			printIssues("Problems", rep.Problems)
			if len(rep.Problems) > 0 {
				return errors.New("some files are not safely in the cloud: run skyhold push, then verify again")
			}
			return nil
		}),
	}
}

func statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show remotes and staged files",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			s, err := a.Status()
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(s)
			}
			fmt.Println("Home:", s.Home)
			if !s.Initialized {
				fmt.Println("Not set up yet: run skyhold init")
				return nil
			}
			fmt.Println("Cloud encryption:", map[bool]string{true: "on", false: "off"}[s.EncryptCloud])
			for _, r := range s.Remotes {
				fmt.Printf("Remote %s: %s %s\n", r.Name, r.Backend, r.Path)
			}
			var staged, released int
			for _, e := range s.Entries {
				if e.State == core.Released {
					released++
					continue
				}
				staged++
				fmt.Printf("  %-9s %s\n", entryState(e, len(s.Remotes)), e.Original)
			}
			fmt.Printf("%d staged, %d released\n", staged, released)
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func entryState(e core.Entry, remotes int) string {
	switch {
	case remotes > 0 && len(e.Verified) == remotes:
		return "verified"
	case remotes > 0 && len(e.Pushed) == remotes:
		return "uploaded"
	default:
		return "staged"
	}
}

func releaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "release",
		Short: "Verify the cloud copies, then delete the vault and the originals",
		Long: `Shows every file that will be deleted, asks for the release password, checks
every file in the cloud again and refuses unless all of them are there. Then it
destroys the vault key, deletes the vault and deletes the original files. An
original that changed since it was added is kept.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			p, err := a.Preview(ctx)
			if err != nil {
				return err
			}
			if len(p.Files) == 0 {
				return errors.New("nothing staged to release")
			}
			fmt.Printf("These %d file(s) will be deleted from this computer (copies stay on %s):\n",
				len(p.Files), strings.Join(p.Remotes, ", "))
			for _, e := range p.Files {
				fmt.Println("  " + e.Original)
			}
			if p.Unpushed > 0 {
				fmt.Printf("\n%d file(s) are not uploaded yet; release will refuse until they are (skyhold push).\n", p.Unpushed)
			}
			for _, w := range p.Warnings {
				fmt.Println("Warning: " + w)
			}
			pw, err := readPassword("\nRelease password (Ctrl-C to cancel): ")
			if err != nil {
				return err
			}
			rep, err := a.Release(ctx, pw, progress())
			doneProgress()
			if err != nil {
				return err
			}
			fmt.Printf("Deleted %d original(s); the vault key and vault copies are gone.\n", len(rep.Deleted))
			if len(rep.AlreadyGone) > 0 {
				fmt.Printf("%d original(s) were already gone.\n", len(rep.AlreadyGone))
			}
			printIssues("Kept", rep.Kept)
			for _, w := range rep.Warnings {
				fmt.Println("Warning: " + w)
			}
			fmt.Println("Receipt:", rep.Receipt)
			return nil
		}),
	}
}

func restoreCmd() *cobra.Command {
	var opt core.RestoreOptions
	cmd := &cobra.Command{
		Use:   "restore [file or folder]...",
		Short: "Download files back (all released files when none are given)",
		RunE: withApp(func(ctx context.Context, a *core.App, args []string) error {
			opt.Paths = args
			rep, err := a.Restore(ctx, opt, progress())
			doneProgress()
			if rep != nil {
				fmt.Printf("Restored %d file(s)\n", len(rep.Restored))
				printIssues("Problems", rep.Problems)
			}
			return err
		}),
	}
	cmd.Flags().StringVar(&opt.To, "to", "", "restore under this folder instead of the original paths")
	cmd.Flags().BoolVar(&opt.Overwrite, "overwrite", false, "replace files that already exist")
	return cmd
}

func recoverCmd() *cobra.Command {
	var spec, to string
	var plain bool
	cmd := &cobra.Command{
		Use:   "recover --remote <rclone remote> --to <folder>",
		Short: "Copy everything from a remote to a folder, without local state (new machine)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := ""
			if !plain {
				var err error
				if key, err = readPassword("Cloud key: "); err != nil {
					return err
				}
			}
			if err := core.Recover(cmd.Context(), spec, key, to); err != nil {
				return err
			}
			fmt.Println("Recovered into", to)
			return nil
		},
	}
	cmd.Flags().StringVar(&spec, "remote", "", "rclone remote the backup is on")
	cmd.Flags().StringVar(&to, "to", "", "local folder to copy into")
	cmd.Flags().BoolVar(&plain, "no-encryption", false, "the backup was made without a cloud key")
	cmd.MarkFlagRequired("remote")
	cmd.MarkFlagRequired("to")
	return cmd
}

func cloudKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cloud-key",
		Short: "Show the cloud key again",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			key, err := a.CloudKey()
			if err != nil {
				return err
			}
			if key == "" {
				return errors.New("cloud encryption is off")
			}
			printKey(key)
			return nil
		}),
	}
}

func passwdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "passwd",
		Short: "Change the release password",
		Args:  cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			old, err := readPassword("Current release password: ")
			if err != nil {
				return err
			}
			pw, err := newPassword()
			if err != nil {
				return err
			}
			if err := a.ChangePassword(old, pw); err != nil {
				return err
			}
			fmt.Println("Password changed")
			return nil
		}),
	}
}

func serveCmd() *cobra.Command {
	var addr string
	var open bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the web UI on this computer",
		Long: `Start the web UI. Open the printed URL: it carries a token that every
request needs. Set SKYHOLD_TOKEN to use a fixed token instead of a new one
on every start.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(ctx context.Context, a *core.App, _ []string) error {
			return web.Serve(ctx, a, addr, open)
		}),
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7480", "address to listen on")
	cmd.Flags().BoolVar(&open, "open", false, "open the web UI in the browser")
	return cmd
}
