//go:build !shn_coreonly

package engine

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Every record a PAS build places must state its resourceType and id spelled
// exactly: the build addresses each by them, so a participant's record that
// states its id only as "ID" (no FHIR id) is refused, the fidelity of the
// gateway's own build (422), on every path, never answered as the gateway's
// own failure and never sent addressed by an id the record does not give.
// Control: the request whose records state both exactly, the entries SHN builds
// itself among them, is built.
func TestPASBuild_EveryRecordStatesItsIDExactly(t *testing.T) {
	t.Parallel()
	const line = "2.2"
	spelled := func(t *testing.T, record []byte) []byte {
		t.Helper()
		out := bytes.Replace(record, []byte(`"id":`), []byte(`"ID":`), 1)
		if bytes.Equal(out, record) {
			t.Fatalf("fixture: no id in %s", record)
		}
		return out
	}
	const roleRef = "PractitionerRole/role-r"
	role := []byte(`{"resourceType":"PractitionerRole","id":"role-r"}`)
	for _, path := range []struct {
		name      string
		qr        bool
		requester bool
	}{
		{"no requester, with a QR", true, false},
		{"no requester, no QR", false, false},
		{"a requester", true, true},
	} {
		submit := func(edit func(*[]byte, *[]byte, *[]byte, *[]byte, *[]byte)) error {
			in := attachmentInputs("ServiceRequest", line, false)
			if !path.qr {
				in.QR = nil
			}
			var records [][]byte
			if path.requester {
				in.SR = withOrderRequester(t, in.SR, roleRef)
				records = [][]byte{role}
			}
			if edit != nil {
				edit(&in.Coverage, &in.Insurer, &in.Provider, &in.SR, &in.QR)
			}
			_, err := buildModuleOnePASSubmit(line, in, records)
			return err
		}
		if err := submit(nil); err != nil {
			t.Fatalf("%s: the control is refused: %v", path.name, err)
		}
		edits := map[string]func(cov, ins, prov, order, qr *[]byte){
			"Coverage":           func(cov, _, _, _, _ *[]byte) { *cov = spelled(t, *cov) },
			"payer Organization": func(_, ins, _, _, _ *[]byte) { *ins = spelled(t, *ins) },
			"provider":           func(_, _, prov, _, _ *[]byte) { *prov = spelled(t, *prov) },
			// The build gives the order an id of its own; the record's "ID"
			// would ride beside it, naming its id twice: a body the network's
			// message integrity cannot seal, so the re-id cannot be made.
			"order": func(_, _, _, order, _ *[]byte) { *order = spelled(t, *order) },
			// The order's own id equal to the build's: the stray member would
			// still ride beside the build's id.
			"order already convergence-sr": func(_, _, _, order, _ *[]byte) {
				out := bytes.Replace(*order, []byte(`"id":"source-order",`), []byte(`"id":"convergence-sr","ID":"x",`), 1)
				if bytes.Equal(out, *order) {
					t.Fatalf("fixture: no id in %s", *order)
				}
				*order = out
			},
			// The same with the order's own id beside the stray member.
			"order with a stray ID": func(_, _, _, order, _ *[]byte) {
				out := bytes.Replace(*order, []byte(`"id":"source-order",`), []byte(`"id":"source-order","ID":"x",`), 1)
				if bytes.Equal(out, *order) {
					t.Fatalf("fixture: no id in %s", *order)
				}
				*order = out
			},
		}
		if path.qr {
			// The QuestionnaireResponse states no id here: it gains one only as
			// "ID", which the build's own id would ride beside.
			edits["QuestionnaireResponse"] = func(_, _, _, _, qr *[]byte) {
				out := bytes.Replace(*qr, []byte(`{"resourceType":"QuestionnaireResponse",`), []byte(`{"resourceType":"QuestionnaireResponse","ID":"qr-1",`), 1)
				if bytes.Equal(out, *qr) {
					t.Fatalf("fixture: no QuestionnaireResponse head in %s", *qr)
				}
				*qr = out
			}
			edits["QuestionnaireResponse already convergence-qr"] = func(_, _, _, _, qr *[]byte) {
				out := bytes.Replace(*qr, []byte(`{"resourceType":"QuestionnaireResponse",`), []byte(`{"resourceType":"QuestionnaireResponse","id":"convergence-qr","ID":"x",`), 1)
				if bytes.Equal(out, *qr) {
					t.Fatalf("fixture: no QuestionnaireResponse head in %s", *qr)
				}
				*qr = out
			}
		}
		for name, edit := range edits {
			err := submit(edit)
			if !errors.Is(err, errRecordNotCarried) || !isPASBuildFidelity(err) {
				t.Errorf("%s, the %s with its id spelled \"ID\": %v, want the build's fidelity refusal", path.name, name, err)
				continue
			}
			// A record the build re-ids is refused as the re-id it cannot make.
			reID := name == "order" || name == "order with a stray ID" || name == "order already convergence-sr" || strings.HasPrefix(name, "QuestionnaireResponse")
			if got := strings.Contains(err.Error(), "cannot be set beside"); got != reID {
				t.Errorf("%s, the %s: %v, want the re-id named %v", path.name, name, err, reID)
			}
		}
	}
	// A person the member's Coverage names, spelled so, is refused by the
	// build's own party read (shnsdk.ErrPASCoveragePartyNotCarried), a
	// fidelity refusal too, before this check.
	parties := attachmentInputs("ServiceRequest", line, false)
	parties.Coverage = bytes.Replace(parties.Coverage, []byte(`"code":"self"}]},`), []byte(`"code":"child"}]},"subscriber":{"reference":"Patient/parent"},`), 1)
	party := []byte(`{"resourceType":"Patient","id":"parent","name":[{"family":"P"}]}`)
	parties.CoverageParties = [][]byte{party}
	if _, err := buildModuleOnePASSubmit(line, parties, nil); err != nil {
		t.Fatalf("the party control is refused: %v", err)
	}
	parties.CoverageParties = [][]byte{spelled(t, party)}
	if _, err := buildModuleOnePASSubmit(line, parties, nil); !errors.Is(err, shnsdk.ErrPASCoveragePartyNotCarried) || !isPASBuildFidelity(err) {
		t.Errorf("a Coverage party with its id spelled \"ID\": %v, want the build's party refusal", err)
	}
	// The amendment, on the same check.
	update := attachmentUpdateInputs(line, false, false)
	if _, err := buildModuleOnePASUpdate(line, update, nil); err != nil {
		t.Fatalf("the update control is refused: %v", err)
	}
	// The Provenance attributing a report is carried under its own id, not
	// re-identified: one naming its id twice in two spellings is the
	// participant's own ambiguity, which the build refuses as a record it
	// cannot carry as written (shnsdk.ErrPASRecordNotCarried), never as a
	// re-id of the gateway's.
	withReport := attachmentUpdateInputs(line, false, true)
	stray := bytes.Replace(withReport.Provenance, []byte(`"id":"source-prov",`), []byte(`"id":"source-prov","ID":"x",`), 1)
	if bytes.Equal(stray, withReport.Provenance) {
		t.Fatalf("fixture: no id in %s", withReport.Provenance)
	}
	withReport.Provenance = stray
	if _, err := buildModuleOnePASUpdate(line, withReport, nil); !errors.Is(err, shnsdk.ErrPASRecordNotCarried) || strings.Contains(err.Error(), "cannot be set beside") {
		t.Errorf("a report's Provenance naming its id in two spellings: %v, want the participant's own ambiguity refused, not the re-id", err)
	}
	for name, edit := range map[string]func(*shnsdk.ConformantClaimUpdateInputs){
		"Coverage": func(u *shnsdk.ConformantClaimUpdateInputs) { u.Coverage = spelled(t, u.Coverage) },
		// With no report, the build gives the amendment's Provenance an id of
		// its own; the record's "ID" would ride beside it.
		"Provenance": func(u *shnsdk.ConformantClaimUpdateInputs) { u.Provenance = spelled(t, u.Provenance) },
		"Provenance already convergence-prov": func(u *shnsdk.ConformantClaimUpdateInputs) {
			u.Provenance = bytes.Replace(u.Provenance, []byte(`"id":"source-prov",`), []byte(`"id":"convergence-prov","ID":"x",`), 1)
		},
		"order": func(u *shnsdk.ConformantClaimUpdateInputs) { u.SR = spelled(t, u.SR) },
		"QuestionnaireResponse": func(u *shnsdk.ConformantClaimUpdateInputs) {
			u.QR = bytes.Replace(u.QR, []byte(`{"resourceType":"QuestionnaireResponse",`), []byte(`{"resourceType":"QuestionnaireResponse","ID":"qr-1",`), 1)
		},
	} {
		u := attachmentUpdateInputs(line, false, false)
		edit(&u)
		if _, err := buildModuleOnePASUpdate(line, u, nil); !errors.Is(err, errRecordNotCarried) || !isPASBuildFidelity(err) || strings.Contains(err.Error(), "cannot be set beside") != (name != "Coverage") {
			t.Errorf("an update whose %s spells its id \"ID\": %v, want the build's fidelity refusal", name, err)
		}
	}
}

// builtEntriesNamed names the re-id only for a type the build re-identifies
// (pasBuildReIDs), whatever id the record stated, even one equal to the
// build's own: the stray spelling would still ride beside the build's id. A
// type the build carries under its own id is left to the network's message
// integrity, never refused as the gateway's re-id.
func TestBuiltEntriesNamed_TheReIDIsTheBuildsOnly(t *testing.T) {
	t.Parallel()
	body := []byte(`{"resourceType":"Bundle","entry":[{"fullUrl":"https://shn.example/fhir/Provenance/convergence-prov","resource":{"resourceType":"Provenance","id":"convergence-prov","ID":"x"}}]}`)
	if err := builtEntriesNamed(body, pasBuildReIDs(nil, true)); !errors.Is(err, errRecordNotCarried) || !strings.Contains(err.Error(), "cannot be set beside") {
		t.Errorf("a Provenance the build re-identifies, its own id equal to the build's: %v, want the re-id refused", err)
	}
	if err := builtEntriesNamed(body, pasBuildReIDs(nil, false)); err != nil {
		t.Errorf("a Provenance carried under its own id (it attributes a report): %v, want it left to message integrity", err)
	}
	if got := pasBuildReIDs([]byte(`{"resourceType":"DeviceRequest","id":"o"}`), false); !got["DeviceRequest"] || !got["QuestionnaireResponse"] || got["Provenance"] || got["ServiceRequest"] {
		t.Errorf("the re-identified types of a DeviceRequest submission: %v", got)
	}
}
