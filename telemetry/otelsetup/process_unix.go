//go:build unix

package otelsetup

import (
	"syscall"
	"time"
)

// cpuTimes is this process's user and system CPU time, from getrusage.
func cpuTimes() (user, system time.Duration, ok bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0, false
	}
	return time.Duration(ru.Utime.Nano()), time.Duration(ru.Stime.Nano()), true
}
