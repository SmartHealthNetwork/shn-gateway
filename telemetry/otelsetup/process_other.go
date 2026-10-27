//go:build !unix

package otelsetup

import "time"

// cpuTimes is unknown on a platform without getrusage.
func cpuTimes() (user, system time.Duration, ok bool) { return 0, 0, false }
