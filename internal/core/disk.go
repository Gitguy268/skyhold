package core

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// diskWarnings is implemented per OS in disk_<os>.go. Every check is best
// effort: a command that is missing or needs admin rights becomes a
// "could not check" warning, never an error.

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

const ssdNote = "deleted originals may be recoverable from the disk"
