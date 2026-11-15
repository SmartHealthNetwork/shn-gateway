//go:build unix

package relay

import (
	"syscall"
	"time"
)

// cpuTime is the processor time the test process has used, user and system:
// the clock a cost row reads. Unlike the wall clock it stands still while the
// process waits for a processor, as it does when other tests or processes
// share the machine. What they do to the caches and memory it shares with
// them it still counts, as a slower step; the bound's margin covers that
// (costrow_test.go).
func cpuTime() time.Duration {
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		panic(err)
	}
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}
