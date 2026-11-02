//go:build !darwin && !linux && !windows

package core

import "context"

func diskWarnings(context.Context, []string) []string {
	return []string{"could not check disk encryption on this OS; if it is off, " + ssdNote}
}
