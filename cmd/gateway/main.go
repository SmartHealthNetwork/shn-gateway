// Binary gateway is the PUBLIC, federation-capable Smart Gateway: it boots
// config-only, loads its `shn register` bundle, federates against the live
// /discovery + the registrar feed, and runs prior auth for its ROLE. The runtime
// lives in gateway/app (a thin main keeps it hermetically testable).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/SmartHealthNetwork/shn-gateway/app"
)

func main() {
	if err := app.Run(context.Background(), os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}
