//go:build !unix

package relay

import "time"

var costEpoch = time.Now()

// cpuTime is the wall clock where the process's processor time is not read
// (costclock_unix_test.go).
func cpuTime() time.Duration { return time.Since(costEpoch) }
