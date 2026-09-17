// Binary gateway is the PUBLIC, federation-capable Smart Gateway: it boots
// config-only, loads its `shn register` bundle, federates against the live
// /discovery + the registrar feed, and runs prior auth for its ROLE. The runtime
// lives in gateway/app (a thin main keeps it hermetically testable).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SmartHealthNetwork/shn-gateway/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}
