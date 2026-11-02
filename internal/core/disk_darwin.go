package core

import (
	"context"
	"fmt"
	"strings"
)

func diskWarnings(ctx context.Context, _ []string) []string {
	var w []string
	switch out, err := run(ctx, "fdesetup", "status"); {
	case err != nil:
		w = append(w, "could not check FileVault; if it is off, "+ssdNote)
	case !strings.Contains(out, "FileVault is On"):
		w = append(w, "FileVault is off: "+ssdNote)
	}
	if out, err := run(ctx, "tmutil", "listlocalsnapshots", "/"); err == nil {
		if n := strings.Count(out, "com.apple.TimeMachine"); n > 0 {
			w = append(w, fmt.Sprintf("%d local Time Machine snapshot(s) may still hold the originals (tmutil listlocalsnapshots /)", n))
		}
	}
	if out, err := run(ctx, "tmutil", "destinationinfo"); err == nil && strings.Contains(out, "Name") {
		w = append(w, "Time Machine backups may still hold the originals")
	}
	return w
}
