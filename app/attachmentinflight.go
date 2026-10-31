//go:build !shn_coreonly

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A task's memory limit, as its cgroup states it: version 2, then version 1.
const (
	cgroupMemoryMax   = "/sys/fs/cgroup/memory.max"
	cgroupMemoryLimit = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// attachmentInflightFallback is the budget when the task's memory cannot be
// read, and attachmentInflightShare the share of the task's memory it takes: a
// body held costs about five times its bytes at the provider's gateway, so this
// holds about half the task.
const (
	attachmentInflightFallback = 64 << 20
	attachmentInflightShare    = 10
)

// cgroupUnlimited is the least a version 1 cgroup states for no limit; a real
// task's memory is far under it.
const cgroupUnlimited = 1 << 50

// Where the budget for large bodies came from, as the boot log states it.
const (
	inflightFromSetting  = "ATTACHMENT_INFLIGHT_BYTES"
	inflightFromCgroup   = "a tenth of the task's memory, from its cgroup"
	inflightFromMetadata = "a tenth of the task's memory, from its ECS task metadata"
	inflightFromFallback = "the default: neither the cgroup nor ECS task metadata states the task's memory"
)

// taskMetadataEnv names the task's metadata endpoint, which ECS sets. An ECS
// Fargate container's cgroup states no limit when only its task has one, so its
// memory is read from there. It is the platform's, never a participant's
// configuration.
const taskMetadataEnv = "ECS_CONTAINER_METADATA_URI_V4"

// taskMetadataTimeout bounds the one read of the metadata endpoint at boot: the
// endpoint is on the task's own link, and a gateway never waits on it to start.
var taskMetadataTimeout = 2 * time.Second

// taskMetadataClient is the client of the one metadata read: no proxy, since the
// endpoint is on the task's own link, and no connection kept after it.
func taskMetadataClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// attachmentInflightDefault is the budget for large bodies when no setting
// states one, and where it came from: a share of the task's memory, the fallback
// when that cannot be read, and never below the ceiling, so a full submission
// always fits.
func attachmentInflightDefault(ctx context.Context, ceiling int64, read func(string) ([]byte, error), metadataURL string, c *http.Client) (int64, string) {
	if memory, ok := taskMemoryBytes(read); ok {
		return max(ceiling, memory/attachmentInflightShare), inflightFromCgroup
	}
	if memory, ok := taskMetadataMemoryBytes(ctx, metadataURL, c); ok {
		return max(ceiling, memory/attachmentInflightShare), inflightFromMetadata
	}
	return max(ceiling, attachmentInflightFallback), inflightFromFallback
}

// taskMetadataMemoryBytes reads the task's memory from its metadata endpoint
// (Limits.Memory, in MiB). No endpoint, an endpoint that fails or does not answer
// in time, and an answer that states no usable figure are unknown, never a limit.
func taskMetadataMemoryBytes(ctx context.Context, metadataURL string, c *http.Client) (int64, bool) {
	if metadataURL == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, taskMetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL+"/task", nil)
	if err != nil {
		return 0, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var task struct {
		Limits struct {
			Memory int64
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&task); err != nil {
		return 0, false
	}
	if n := task.Limits.Memory; n >= 1 && n < cgroupUnlimited>>20 {
		return n << 20, true
	}
	return 0, false
}

// taskMemoryBytes reads the task's memory limit from its cgroup. A file that is
// absent or unreadable, "max", or a value no limit would take is unknown, never
// a limit.
func taskMemoryBytes(read func(string) ([]byte, error)) (int64, bool) {
	for _, path := range []string{cgroupMemoryMax, cgroupMemoryLimit} {
		raw, err := read(path)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || n < 1 || n >= cgroupUnlimited {
			continue
		}
		return n, true
	}
	return 0, false
}
