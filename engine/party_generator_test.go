package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The party-shape generator: requests built from a dependent's well-formed request on
// every leg that derives a patient identifier, moved by a seeded grammar of the shapes a
// reader may resolve differently, and held to two properties. Never the parent: no request
// binds the identity the parent's own requests derive. No new false refusal: a request a
// strict FHIR reader resolves unambiguously binds exactly as that reader reads it. The
// reader (strictRead, below) is written from the FHIR resolution rules alone and shares no
// helper with the gateway's walk.
//
// make check runs a fixed seed and case count. SHN_PARTYGEN_SEED and SHN_PARTYGEN_CASES
// (cases per base request) run another.

// genBase is a request the generator moves: a dependent's request on one leg, naming the
// parent (under the member's number) in a Coverage's party slot.
type genBase struct {
	name     string
	doc      any
	provider func(context.Context, *Gateway, []byte) (string, int, string)
}

func partyGenBases(t *testing.T) []genBase {
	t.Helper()
	ref := "Patient/" + strangerMember
	elig := decodeDoc(t, eligibilityRequest(t, strangerMember))
	elig["contained"] = []any{map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active", "beneficiary": reference(ref)}}
	type leg struct {
		original []byte
		partyRef string
		add      func(map[string]any, map[string]any)
		provider func(context.Context, *Gateway, []byte) (string, int, string)
	}
	legs := map[string]leg{
		"CRD": {crdReqJSON(strangerMember, ref, ref), "Patient/" + partyParentID, asPrefetch,
			func(ctx context.Context, g *Gateway, b []byte) (string, int, string) {
				return g.ingressCRDSubjectPCIContext(ctx, b)
			}},
		"DTR": {dtrPackageFor(strangerMember), "Patient/" + partyParentID, asParameter,
			func(ctx context.Context, g *Gateway, b []byte) (string, int, string) {
				out, status, msg := g.prepareDTRPackageRequest(ctx, b)
				return out.pci, status, msg
			}},
		"PAS": {conformantPASBundleWithQR(t, strangerMember), "urn:uuid:parent", asBundleEntry("urn:uuid:parent"),
			func(ctx context.Context, g *Gateway, b []byte) (string, int, string) {
				return g.ingressPASNativeSubjectPCIContext(ctx, b)
			}},
		"inquiry":     {inquiryBundle(strangerMember, "", "TRN-1", "72148"), "Patient/" + partyParentID, asBundleEntry("http://ehr.example/fhir/Patient/" + partyParentID), nil},
		"eligibility": {mustJSON(t, elig), "#" + partyParentID, asContainedSibling, nil},
	}
	var bases []genBase
	names := make([]string, 0, len(legs))
	for name := range legs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		l := legs[name]
		bases = append(bases,
			genBase{name + "/no Patient of the member's own", decodeDoc(t, withParentParty(t, l.original, l.partyRef, reference, l.add)), l.provider},
			genBase{name + "/the member's own Patient", decodeDoc(t, withParentParty(t, withMembersOwnPatient(t, l.original, l.add), l.partyRef, reference, l.add)), l.provider})
	}
	return bases
}

// partyGenOwns reports whether pci is an identity the strict reader's reading allows: that
// of one of the member's own Patients, or, where it reads more than one or none, the member
// id alone.
func partyGenOwns(r srReading, pci string) bool {
	for f := range r.own {
		bd, family, _ := strings.Cut(f, "|")
		if pci == derivedPCI(strangerMember, bd, family) {
			return true
		}
	}
	return len(r.own) != 1 && pci == derivedPCI(strangerMember, "", "")
}

// partyGenSettings are the seed and the number of cases per base request.
func partyGenSettings(t *testing.T) (int64, int) {
	seed, cases := int64(2605), 2000
	if s := os.Getenv("SHN_PARTYGEN_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("SHN_PARTYGEN_SEED: %v", err)
		}
		seed = v
	}
	if s := os.Getenv("SHN_PARTYGEN_CASES"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			t.Fatalf("SHN_PARTYGEN_CASES: %q", s)
		}
		cases = v
	}
	return seed, cases
}

func TestPartyGenerator(t *testing.T) {
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	seed, cases := partyGenSettings(t)
	rng := rand.New(rand.NewSource(seed))
	parent := parentIdentity()
	var failures []string
	total, strict := 0, 0
	for _, base := range partyGenBases(t) {
		for i := 0; i < cases; i++ {
			doc := genCopy(base.doc)
			var trace []string
			for k := 1 + rng.Intn(3); k > 0; k-- {
				if m := genMove(rng, doc); m != "" {
					trace = append(trace, m)
				}
			}
			payload, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			total++
			fail := func(why string) {
				failures = append(failures, fmt.Sprintf("%s case %d [%s]: %s\n%s", base.name, i, strings.Join(trace, "; "), why, payload))
			}
			pci, _, err := g.resolveSubjectPCI(ctx, strangerMember, payload)
			if pci == parent {
				fail("derived the parent's identity")
				continue
			}
			if i%25 == 0 {
				for _, level := range allLevels {
					g.cfg.ConformanceEnforcement = level
					if base.provider != nil {
						if p, _, _ := base.provider(ctx, g, payload); p == parent {
							fail("provider bound the parent's identity at " + level.String())
						}
					}
					if p, _, _ := g.bindInboundSubject(ctx, strangerMember, payload); p == parent {
						fail("payer bound the parent's identity at " + level.String())
					}
				}
				g.cfg.ConformanceEnforcement = EnforcementNone
			}
			reading, ok := strictRead(doc, strangerMember)
			if !ok {
				continue
			}
			strict++
			switch {
			case reading.refused && err == nil:
				fail(fmt.Sprintf("a strict reader cannot identify the member, the gateway binds %s", pci))
			case !reading.refused && err != nil:
				fail(fmt.Sprintf("a strict reader identifies the member, the gateway refuses (%v)", err))
			case err == nil && !partyGenOwns(reading, pci):
				fail(fmt.Sprintf("the gateway binds %s, no Patient a strict reader reads as the member's own (%v)", pci, reading.own))
			}
		}
	}
	t.Logf("seed %d: %d cases, %d a strict reader resolves", seed, total, strict)
	if out := os.Getenv("SHN_PARTYGEN_OUT"); out != "" && len(failures) > 0 {
		if err := os.WriteFile(out, []byte(strings.Join(failures, "\n\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(failures) > 0 {
		for i, f := range failures {
			if i == 5 {
				break
			}
			t.Log(f)
		}
		t.Fatalf("%d of %d cases fail", len(failures), total)
	}
}
