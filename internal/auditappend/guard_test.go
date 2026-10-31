package auditappend

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

var update = flag.Bool("update", false, "rewrite testdata/guard_corpus.json from the cases here")

// corpusPath is the corpus the platform's own test of the Audit Plane reads
// too: it runs the Audit Plane's own structure,
// class, binding and member checks over every record here and holds that
// none the guard accepts is one the Plane refuses.
const corpusPath = "testdata/guard_corpus.json"

const holder = "provider-a"

// The SDK's published vectors (shn-sdk testdata/vectors/audit), as records.
func authored() shnsdk.AuditRecord {
	return shnsdk.AuditRecord{
		Timestamp: "2026-06-04T00:00:00Z", Sender: holder, Recipient: "payer-b",
		TransactionType: shnsdk.AuditTypeModuleDetails, AuthorityFrame: "provider-tpo", Scope: "pas-bundle",
		Outcome:           "authored",
		PayloadBundleHash: "2e7c9a1f4b6d8e0a3c5f7b9d1e3a5c7f9b2d4e6a8c0f1b3d5e7a9c2e4b6d8f0a",
		RecordID:          "01JZX5A7B8C9D0E1F2G3H4JA03", Version: 2, CorrelationID: "7d2e4f6a8b0c1d3e5f7a9b2c4d6e8f0a",
		Module: &shnsdk.Module{Body: "request", Leg: "pa.pas-claim", Ownership: "authored", Builder: "sdk-pas-submit", Edits: []string{}, Loss: []shnsdk.ModuleLoss{}},
	}
}

func bridged() shnsdk.AuditRecord {
	r := authored()
	r.Outcome = "edited"
	r.PayloadBundleHash = "4a6c8e0b2d4f6a8c1e3b5d7f9a2c4e6b8d0f1a3c5e7b9d2f4a6c8e0b1d3f5a7c"
	r.RecordID = "01JZX5A7B8C9D0E1F2G3H4JA04"
	r.CorrelationID = "8e3f5a7b9c1d2e4f6a8b0c3d5e7f9a1b"
	r.Module = &shnsdk.Module{Body: "request", Leg: "pa.pas-claim", Ownership: "edited", Builder: "sdk-pas-submit",
		Edits: []string{"E-09"}, LineFrom: "2.1", LineTo: "2.2", Loss: []shnsdk.ModuleLoss{{Kind: "carried", Path: "Claim.item.extension"}}}
	return r
}

func editedNoBuilder() shnsdk.AuditRecord {
	r := authored()
	r.Outcome = "edited"
	r.Scope = "questionnaire-only"
	r.Module = &shnsdk.Module{Body: "answer", Leg: "pa.dtr-questionnaire-fetch", Ownership: "edited", Edits: []string{"E-08"}, Loss: []shnsdk.ModuleLoss{}}
	return r
}

func started() shnsdk.AuditRecord {
	return shnsdk.AuditRecord{
		Timestamp: "2026-06-04T00:00:00Z", Sender: holder, Recipient: holder,
		TransactionType: shnsdk.AuditTypeGatewayStart, Outcome: "started",
		RecordID: "01JZX5A7B8C9D0E1F2G3H4JA05", Version: 2,
		Gateway: &shnsdk.Gateway{Version: "v1.0.0", WireVersions: []string{"1.1.0", "2.0.0"}, Modules: []string{"conformance", "on-behalf"}},
	}
}

// guardCase is one corpus record and the guard's verdict on it.
type guardCase struct {
	Name   string             `json:"name"`
	Holder string             `json:"holder"`
	Accept bool               `json:"accept"`
	Record shnsdk.AuditRecord `json:"record"`
}

func mod(base func() shnsdk.AuditRecord, f func(*shnsdk.AuditRecord)) shnsdk.AuditRecord {
	r := base()
	f(&r)
	return r
}

// flatLeg is a leg's version 1 (flat) name from the SDK's table: one every
// SDK the gateway module is built against names, so the corpus is the same
// whether the module reads the workspace's SDK or its pinned release.
func flatLeg() string {
	const leg = "coverage-eligibility"
	if p, ok := shnsdk.PrefixedTransactionTypes()[leg]; !ok || p == leg {
		panic("auditappend: the SDK's name table has no flat " + leg)
	}
	return leg
}

// cases is the corpus. A Go AuditRecord holds no member outside its schema,
// so an "extra member" is one outside the record's class: a finding, a
// gateway on a module record, a module on a start record.
func cases() []guardCase {
	long := strings.Repeat("a", 129)
	accept := func(name string, r shnsdk.AuditRecord) guardCase {
		return guardCase{Name: name, Holder: holder, Accept: true, Record: r}
	}
	refuse := func(name string, r shnsdk.AuditRecord) guardCase {
		return guardCase{Name: name, Holder: holder, Record: r}
	}
	sixteen := []string{}
	for i := 0; i < 17; i++ {
		sixteen = append(sixteen, "E-08")
	}
	loss33 := []shnsdk.ModuleLoss{}
	for i := 0; i < 33; i++ {
		loss33 = append(loss33, shnsdk.ModuleLoss{Kind: "carried", Path: "Claim.item"})
	}
	return []guardCase{
		accept("module-details authored (SDK vector)", authored()),
		accept("module-details bridged (SDK vector)", bridged()),
		accept("module-details edited, no builder", editedNoBuilder()),
		accept("gateway-start (SDK vector)", started()),
		accept("module-details with no frames", mod(authored, func(r *shnsdk.AuditRecord) { r.AuthorityFrame, r.Scope = "", "" })),
		accept("module-details timestamp with nanoseconds", mod(authored, func(r *shnsdk.AuditRecord) { r.Timestamp = "2026-06-04T00:00:00.123456789Z" })),
		// A producer may leave a list nil: it signs and verifies as the empty list.
		accept("module-details with nil lists", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Edits, r.Module.Loss = nil, nil })),
		accept("gateway-start with nil lists", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway.WireVersions, r.Gateway.Modules = nil, nil })),

		// Version, signer and class.
		refuse("version 1", mod(authored, func(r *shnsdk.AuditRecord) { r.Version = 0 })),
		refuse("version 3", mod(authored, func(r *shnsdk.AuditRecord) { r.Version = 3 })),
		refuse("sender is another holder", mod(authored, func(r *shnsdk.AuditRecord) { r.Sender = "provider-z" })),
		refuse("an exchange record", mod(authored, func(r *shnsdk.AuditRecord) { r.TransactionType, r.Module = "pa.pas-claim", nil })),
		refuse("a conformance-finding record", mod(authored, func(r *shnsdk.AuditRecord) {
			r.TransactionType, r.Module, r.Outcome = shnsdk.AuditTypeConformanceFinding, nil, "relayed"
			r.Finding = &shnsdk.Finding{Kind: "fhir-ingress", Level: "observe", Whose: "peer"}
		})),

		// Bounds on every member.
		refuse("oversized sender", mod(started, func(r *shnsdk.AuditRecord) { r.Sender, r.Recipient = long, long })),
		refuse("oversized recipient", mod(authored, func(r *shnsdk.AuditRecord) { r.Recipient = long })),
		refuse("oversized authorityFrame", mod(authored, func(r *shnsdk.AuditRecord) { r.AuthorityFrame = long })),
		refuse("scope with a space", mod(authored, func(r *shnsdk.AuditRecord) { r.Scope = "pas bundle" })),
		refuse("scope with a control character", mod(authored, func(r *shnsdk.AuditRecord) { r.Scope = "pas\nbundle" })),
		refuse("no recordId", mod(authored, func(r *shnsdk.AuditRecord) { r.RecordID = "" })),
		refuse("lowercase recordId", mod(authored, func(r *shnsdk.AuditRecord) { r.RecordID = strings.ToLower(r.RecordID) })),
		refuse("recordId overflowing 128 bits", mod(authored, func(r *shnsdk.AuditRecord) { r.RecordID = "8" + r.RecordID[1:] })),
		refuse("timestamp with an offset", mod(authored, func(r *shnsdk.AuditRecord) { r.Timestamp = "2026-06-04T00:00:00+01:00" })),
		refuse("timestamp not a time", mod(authored, func(r *shnsdk.AuditRecord) { r.Timestamp = "yesterday" })),
		refuse("correlationId not bounded", mod(authored, func(r *shnsdk.AuditRecord) { r.CorrelationID = long })),
		refuse("payloadBundleHash in capitals", mod(authored, func(r *shnsdk.AuditRecord) { r.PayloadBundleHash = strings.ToUpper(r.PayloadBundleHash) })),

		// Members outside the class.
		refuse("a subjectPCI", mod(authored, func(r *shnsdk.AuditRecord) { r.SubjectPCI = "pci-1" })),
		refuse("an involvement", mod(authored, func(r *shnsdk.AuditRecord) { r.Involvement = "responder-held" })),
		refuse("a consentRef", mod(authored, func(r *shnsdk.AuditRecord) { r.ConsentRef = "consent-1" })),
		refuse("resends", mod(authored, func(r *shnsdk.AuditRecord) { r.Resends = "8e3f5a7b9c1d2e4f6a8b0c3d5e7f9a1b" })),
		refuse("a finding on a module record", mod(authored, func(r *shnsdk.AuditRecord) {
			r.Finding = &shnsdk.Finding{Kind: "fhir-ingress", Level: "observe", Whose: "peer"}
		})),
		refuse("a gateway on a module record", mod(authored, func(r *shnsdk.AuditRecord) { r.Gateway = started().Gateway })),
		refuse("a module on a start record", mod(started, func(r *shnsdk.AuditRecord) { r.Module = authored().Module })),

		// module-details.
		refuse("no module", mod(authored, func(r *shnsdk.AuditRecord) { r.Module = nil })),
		refuse("outcome relayed", mod(authored, func(r *shnsdk.AuditRecord) { r.Outcome = "relayed" })),
		refuse("ownership differs from outcome", mod(authored, func(r *shnsdk.AuditRecord) { r.Outcome = "edited" })),
		refuse("recipient is the sender", mod(authored, func(r *shnsdk.AuditRecord) { r.Recipient = holder })),
		refuse("no payloadBundleHash", mod(authored, func(r *shnsdk.AuditRecord) { r.PayloadBundleHash = "" })),
		refuse("no correlationId", mod(authored, func(r *shnsdk.AuditRecord) { r.CorrelationID = "" })),
		refuse("module.body outside its set", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Body = "response" })),
		refuse("module.leg flat", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Leg = flatLeg() })),
		refuse("module.leg unknown", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Leg = "pa.no-such-leg" })),
		refuse("17 edits", mod(editedNoBuilder, func(r *shnsdk.AuditRecord) { r.Module.Edits = sixteen })),
		refuse("an edit outside its shape", mod(editedNoBuilder, func(r *shnsdk.AuditRecord) { r.Module.Edits = []string{"E-8"} })),
		refuse("builder outside its shape", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Builder = "SDK PAS" })),
		refuse("authored with an edit", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Edits = []string{"E-08"} })),
		refuse("authored with no builder", mod(authored, func(r *shnsdk.AuditRecord) { r.Module.Builder = "" })),
		refuse("edited with no edit", mod(editedNoBuilder, func(r *shnsdk.AuditRecord) { r.Module.Edits = nil })),
		refuse("builder on another edit (SDK refused vector)", mod(editedNoBuilder, func(r *shnsdk.AuditRecord) { r.Module.Builder = "sdk-dtr-package" })),
		refuse("lineFrom without lineTo", mod(bridged, func(r *shnsdk.AuditRecord) { r.Module.LineTo = "" })),
		refuse("a line outside its shape", mod(bridged, func(r *shnsdk.AuditRecord) { r.Module.LineTo = "2 2" })),
		refuse("lines without a line-bridge edit", mod(editedNoBuilder, func(r *shnsdk.AuditRecord) { r.Module.LineFrom, r.Module.LineTo = "2.1", "2.2" })),
		refuse("loss without lines", mod(editedNoBuilder, func(r *shnsdk.AuditRecord) {
			r.Module.Loss = []shnsdk.ModuleLoss{{Kind: "carried", Path: "Claim.item"}}
		})),
		refuse("33 losses", mod(bridged, func(r *shnsdk.AuditRecord) { r.Module.Loss = loss33 })),
		refuse("a loss kind outside its set", mod(bridged, func(r *shnsdk.AuditRecord) { r.Module.Loss[0].Kind = "dropped" })),
		refuse("a loss path outside its shape", mod(bridged, func(r *shnsdk.AuditRecord) { r.Module.Loss[0].Path = "1bad path" })),
		refuse("a truncated loss not last", mod(bridged, func(r *shnsdk.AuditRecord) { r.Module.Loss[0] = shnsdk.ModuleLoss{Kind: "truncated"} })),

		// gateway-start.
		refuse("no gateway", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway = nil })),
		refuse("start outcome not started", mod(started, func(r *shnsdk.AuditRecord) { r.Outcome = "authored" })),
		refuse("start recipient another holder", mod(started, func(r *shnsdk.AuditRecord) { r.Recipient = "payer-b" })),
		refuse("start with a leg member", mod(started, func(r *shnsdk.AuditRecord) { r.CorrelationID = "7d2e4f6a8b0c1d3e5f7a9b2c4d6e8f0a" })),
		refuse("start with a frame", mod(started, func(r *shnsdk.AuditRecord) { r.AuthorityFrame = "provider-tpo" })),
		refuse("gateway.version outside its shape", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway.Version = "1.0.0" })),
		refuse("9 wire versions", mod(started, func(r *shnsdk.AuditRecord) {
			r.Gateway.WireVersions = strings.Fields("1.0.0 1.1.0 1.2.0 1.3.0 1.4.0 1.5.0 1.6.0 1.7.0 2.0.0")
		})),
		refuse("a wire version outside its shape", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway.WireVersions = []string{"2.0"} })),
		refuse("a module outside its set", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway.Modules = []string{"pdex"} })),
		refuse("modules unsorted", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway.Modules = []string{"on-behalf", "conformance"} })),
		refuse("a module twice", mod(started, func(r *shnsdk.AuditRecord) { r.Gateway.Modules = []string{"conformance", "conformance"} })),
	}
}

// recordIDSamples are recordIds minted at fixed clocks and entropy, for the
// root module's strict parse.
func recordIDSamples(t *testing.T) []string {
	var out []string
	for _, c := range []struct {
		ms   int64
		fill byte
	}{{0, 0x00}, {1, 0x01}, {1781827200000, 0x5a}, {1<<48 - 1, 0xff}} {
		id, err := NewRecordID(time.UnixMilli(c.ms), bytes.NewReader(bytes.Repeat([]byte{c.fill}, 10)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

type corpus struct {
	RecordIDs []string `json:"recordIds"`
	// SignPub and Signed are the append requests Append stores for the
	// accepted cases, signed with a fixed test key: the root module's test
	// verifies each with the Plane's own signature check.
	SignPub string            `json:"signPub"`
	Signed  []json.RawMessage `json:"signed"`
	Cases   []guardCase       `json:"cases"`
}

// signedBodies are the signed append requests a send makes of each accepted
// case's queued record.
func signedBodies(t *testing.T, cs []guardCase) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, c := range cs {
		if !c.Accept {
			continue
		}
		s := NewMemStore()
		a := newAppender(t, "https://audit.example", s, newCounts())
		if got := a.Append(c.Record); got != Queued {
			t.Fatalf("%s: Append = %s", c.Name, got)
		}
		// The request a send makes of the queued record (signed then).
		req, err := a.signed(s.entries[0].Entry)
		if err != nil {
			t.Fatalf("%s: signed: %v", c.Name, err)
		}
		out = append(out, json.RawMessage(req))
	}
	return out
}

// TestGuardCorpus pins the corpus file to the cases and the guard's verdict
// on each; -update rewrites it.
func TestGuardCorpus(t *testing.T) {
	cs := cases()
	for _, c := range cs {
		err := Check(c.Record, c.Holder)
		if (err == nil) != c.Accept {
			t.Errorf("%s: Check = %v, want accept %v", c.Name, err, c.Accept)
		}
	}
	want, err := json.MarshalIndent(corpus{
		RecordIDs: recordIDSamples(t),
		SignPub:   base64.StdEncoding.EncodeToString(testKey.Public().(ed25519.PublicKey)),
		Signed:    signedBodies(t, cs),
		Cases:     cs,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(corpusPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(corpusPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("%v (go test -run TestGuardCorpus -update writes it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not the corpus the cases make: rerun with -update and commit it", corpusPath)
	}
}

// TestGuardCorpusCoversBothVerdicts: a corpus with no accepted record, or no
// refused one, would make the root module's parity check vacuous.
func TestGuardCorpusCoversBothVerdicts(t *testing.T) {
	var a, r int
	for _, c := range cases() {
		if c.Accept {
			a++
		} else {
			r++
		}
	}
	if a < 4 || r < 40 {
		t.Fatalf("corpus holds %d accepted and %d refused records", a, r)
	}
}

// TestGuardRefusesTheEmptyHolder: an appender with no holder signs nothing.
func TestGuardRefusesTheEmptyHolder(t *testing.T) {
	r := authored()
	r.Sender = ""
	if Check(r, "") == nil {
		t.Fatal("a record with no sender passed for a gateway with no holder")
	}
}
