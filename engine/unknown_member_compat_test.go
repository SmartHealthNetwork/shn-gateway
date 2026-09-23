package engine

import (
	"context"
	"net/http"
	"testing"
)

// The compatibility switch cannot invent an identity or authorize
// record disclosure; native signed-context carriage is tested independently.
func TestUnknownMemberCompatibilityIsNoOp(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, payload := range [][]byte{nil, []byte(`{"prefetch":{"patient":` + requestPatient(strangerMember, "1962-03-11", "Nakamura") + `}}`)} {
			g := &Gateway{cfg: Config{SoR: newCensusSoR(), AcceptUnknownMembers: enabled}}
			pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
			if err != nil || found || pci != "" {
				t.Errorf("compatibility=%v derived identity %q found=%v err=%v", enabled, pci, found, err)
			}
		}
		g := &Gateway{cfg: Config{SoR: noMemberSoR{newPrefetchSoR()}, AcceptUnknownMembers: enabled}}
		_, status, _ := g.ingressEnsureSelfContainedContext(context.Background(), "crd-order-select", strangerEHRRequest(supported), strangerMember)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("compatibility=%v missing local source status=%d", enabled, status)
		}
	}
}
