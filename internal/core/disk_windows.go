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
		vol := strings.ToUpper(filepath.VolumeName(p))
		if vol == "" || seen[vol] {
			continue
		}
		seen[vol] = true
		// Works without admin rights, unlike manage-bde. 1 and 3 mean on or
		// encrypting; anything else means not protected.
		out, err := run(ctx, "powershell", "-NoProfile", "-Command",
			"(New-Object -ComObject Shell.Application).NameSpace('"+vol+"\\').Self.ExtendedProperty('System.Volume.BitLockerProtection')")
		switch {
		case err != nil:
			w = append(w, "could not check BitLocker on "+vol+"; if it is off, "+ssdNote)
		case out != "1" && out != "3":
			w = append(w, "BitLocker is off on "+vol+": "+ssdNote)
		}
	}
	w = append(w, "System Restore points, shadow copies or File History may still hold the originals")
	return w
}
