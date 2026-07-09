// gateway/cmd/evalseed/main.go
package main

import (
	"context"
	"flag"
	"log"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
)

func main() {
	base := flag.String("base", "", "untenanted FHIR base URL, e.g. http://hapi:8080/fhir")
	flag.Parse()
	if *base == "" {
		log.Fatal("evalseed: --base required (untenanted FHIR base)")
	}
	ctx := context.Background()
	c := &fhirseed.Client{Base: *base, Logf: log.Printf}
	for _, s := range seedSteps() {
		if err := s.run(ctx, c); err != nil {
			log.Fatalf("evalseed: %s: %v", s.name, err)
		}
		log.Printf("evalseed: %s OK", s.name)
	}
	log.Print("evalseed: seed complete")
}
