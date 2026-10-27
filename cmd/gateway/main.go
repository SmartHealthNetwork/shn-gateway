//go:build !shn_coreonly

// Binary gateway is the PUBLIC, federation-capable Smart Gateway: it boots
// config-only, loads its `shn register` bundle, federates against the live
// /discovery + the registrar feed, and runs prior auth for its ROLE. The runtime
// lives in gateway/app (a thin main keeps it hermetically testable).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/SmartHealthNetwork/shn-gateway/app"
	"github.com/SmartHealthNetwork/shn-gateway/internal/buildversion"
)

func main() {
	if printVersion(os.Args[1:], os.Stdout) {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

// printVersion answers `gateway -version` with the build's version and where
// it came from ("v1.0.0 stamp"), before any configuration is read, so a build
// can be checked by running it. It reports whether it answered.
func printVersion(args []string, w io.Writer) bool {
	if len(args) != 1 || args[0] != "-version" {
		return false
	}
	v, src := buildversion.Version()
	fmt.Fprintln(w, v, src)
	return true
}
