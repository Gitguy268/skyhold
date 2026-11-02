package core

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"sync"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fspath"
)

var rcloneOnce sync.Once

// setupRclone loads the user's rclone.conf, so named rclone remotes work as
// well as connection strings, and keeps rclone quiet unless it fails.
func setupRclone() {
	rcloneOnce.Do(func() {
		configfile.Install()
		fs.GetConfig(context.Background()).LogLevel = fs.LogLevelError
	})
}

// OpenRemote opens an rclone remote spec (a named remote like "b2:bucket/x"
// or a connection string like ":s3,provider=Minio,...:bucket"). With a cloud
// key, the remote is wrapped in rclone crypt so the provider only sees
// encrypted names and contents. sub is a folder inside the remote.
func OpenRemote(ctx context.Context, spec, cloudKey, sub string) (fs.Fs, error) {
	setupRclone()
	if cloudKey == "" {
		return fs.NewFs(ctx, fspath.JoinRootPath(spec, sub))
	}
	key := NormalizeCloudKey(cloudKey)
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	// The "skyhold-salt:" prefix is frozen: changing it makes every existing
	// cloud backup unreadable.
	conn := fmt.Sprintf(":crypt,remote=%s,password=%s,password2=%s:%s",
		q(spec), q(obscure.MustObscure(key)), q(obscure.MustObscure("skyhold-salt:"+key)), sub)
	return fs.NewFs(ctx, conn)
}

// newCloudKey returns 160 random bits as dash-separated base32, easy to
// write down: ABCD-EFGH-…
func newCloudKey() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	var groups []string
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return strings.Join(groups, "-"), nil
}

// NormalizeCloudKey accepts a cloud key typed with any case, spaces or dashes.
func NormalizeCloudKey(k string) string {
	k = strings.ToUpper(k)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, k)
}

// remotePath maps an absolute local path to a path under files/ on the
// remote: /Users/me/a.txt → files/Users/me/a.txt, C:\x\a.txt → files/C/x/a.txt.
func remotePath(abs string) string {
	p := strings.ReplaceAll(abs, `\`, "/")
	if len(p) >= 2 && p[1] == ':' { // drive letter
		p = p[:1] + p[2:]
	}
	p = strings.TrimLeft(p, "/")
	return "files/" + p
}

const remoteManifest = ".skyhold/manifest.json"

// EditRcloneConfig runs rclone's interactive remote setup, for users who
// would rather name a remote in rclone.conf than type a connection string.
func EditRcloneConfig(ctx context.Context) error {
	setupRclone()
	return config.EditConfig(ctx)
}
