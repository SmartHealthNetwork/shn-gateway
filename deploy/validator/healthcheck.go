// healthcheck is the container healthCheck for the distroless HAPI validator image
// (no shell/curl): GET localhost:8080/fhir/metadata, exit 0 on HTTP 200 else 1. The
// validator is single-tenant (no URL_BASED partitioning), so metadata is un-tenanted
// at /fhir/metadata (NOT /fhir/DEFAULT/metadata like the partitioned SoR HAPI).
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	os.Exit(check("http://localhost:8080/fhir/metadata"))
}

func check(url string) int {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
