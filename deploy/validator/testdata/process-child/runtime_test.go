package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Kernel executable evidence can name an emulator outside the container's
// filesystem or retain a deleted executable's suffix. Neither may be rewritten.
func TestRuntimeExecutablePreservesRawTarget(t *testing.T) {
	for _, target := range []string{"unmounted/rosetta", "unmounted/java (deleted)"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exe")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			got, err := runtimeExecutable(path)
			if err != nil || got != target {
				t.Fatalf("executable = %q, %v; want raw target %q", got, err, target)
			}
		})
	}
}

func TestRuntimeExecutablePropagatesMissingAndNonlinkErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		file bool
		want error
	}{
		{name: "missing", want: os.ErrNotExist},
		{name: "not a symlink", file: true, want: syscall.EINVAL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exe")
			if tc.file {
				if err := os.WriteFile(path, []byte("executable"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := runtimeExecutable(path)
			if got != "" || !errors.Is(err, tc.want) {
				t.Fatalf("executable = %q, %v; want empty target and %v", got, err, tc.want)
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) || pathErr.Path != path {
				t.Fatalf("executable error lost failing path: %v", err)
			}
		})
	}
}
