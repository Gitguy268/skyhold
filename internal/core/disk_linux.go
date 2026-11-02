package core

import (
	"context"
	"path/filepath"
	"strings"
)

func diskWarnings(ctx context.Context, paths []string) []string {
	var w []string
	seen := map[string]bool{}
	for _, p := range paths {
		out, err := run(ctx, "findmnt", "-no", "SOURCE,FSTYPE,TARGET", "--target", filepath.Dir(p))
		if err != nil {
			if !seen["?"] {
				seen["?"] = true
				w = append(w, "could not check disk encryption (findmnt failed); if it is off, "+ssdNote)
			}
			continue
		}
		fields := strings.Fields(out)
		if len(fields) < 3 || seen[fields[2]] {
			continue
		}
		source, fstype, mount := fields[0], fields[1], fields[2]
		seen[mount] = true
		if types, err := run(ctx, "lsblk", "-s", "-no", "TYPE", source); err != nil {
			w = append(w, "could not check disk encryption for "+mount+"; if it is off, "+ssdNote)
		} else if !strings.Contains(types, "crypt") {
			w = append(w, mount+" is not on an encrypted (LUKS) volume: "+ssdNote)
		}
		if fstype == "btrfs" || fstype == "zfs" {
			w = append(w, mount+" is "+fstype+": snapshots may still hold the originals")
		}
	}
	return w
}
