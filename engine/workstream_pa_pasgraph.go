// workstream_pa_pasgraph.go — the engine's names for the PAS response-graph
// REFERENCE-CLOSURE RULE (FR-G28), which lives in internal/pasgraph so that
// the Prior Authorization workstream's readers of a native PAS answer
// (pa/pasanswer) read an answer by the same definition as the gateway.
//
// It is a PRODUCT GUARD: a receiver that accepts an answer naming records it
// does not carry has accepted a message it cannot read. It is held to answers
// a REAL reference payer emitted, unpatched and corrected, by
// test/adversarial/payer_graph_capture_test.go. The names below keep the
// engine's callers unchanged; the rule itself is internal/pasgraph's.
package engine

import "github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"

const (
	pasGraphMaxBytes      = pasgraph.MaxBytes
	pasGraphMaxResources  = pasgraph.MaxResources
	pasGraphMaxReferences = pasgraph.MaxReferences
	pasGraphMaxDepth      = pasgraph.MaxDepth
)

type (
	pasGraph           = pasgraph.Graph
	pasGraphEntry      = pasgraph.Entry
	pasGraphRefusal    = pasgraph.Refusal
	pasGraphCauseError = pasgraph.CauseError
)

func pasGraphError(cause string) error { return pasgraph.Refuse(cause) }

func pasGraphStructural(cause, format string, args ...any) error {
	return pasgraph.Structural(cause, format, args...)
}

func pasGraphRefusalOf(err error) *pasGraphRefusal { return pasgraph.RefusalOf(err) }

func readPASGraph(raw []byte) (*pasGraph, error) { return pasgraph.Read(raw) }

func readPASGraphOf(raw []byte, needResponse bool) (*pasGraph, error) {
	return pasgraph.ReadOf(raw, needResponse)
}

func validatePASBundleGraph(raw []byte) error { return pasgraph.ValidateBundle(raw) }

func validatePASBundleGraphAt(raw []byte, line string) error {
	return pasgraph.ValidateBundleAt(raw, line)
}

func claimResponseRequestNamesClaim(cr map[string]any, entryType func(ref string) string) bool {
	return pasgraph.ClaimResponseRequestNamesClaim(cr, entryType)
}

func pasReferenceTarget(ref string) string       { return pasgraph.ReferenceTarget(ref) }
func pasResourceTypeName(typ string) bool        { return pasgraph.ResourceTypeName(typ) }
func pasSafeResourceID(id string) bool           { return pasgraph.SafeResourceID(id) }
func pasSortedKeys(x map[string]any) []string    { return pasgraph.SortedKeys(x) }
func pasReferencePath(parent, key string) string { return pasgraph.ReferencePath(parent, key) }
func decodePASObject(raw []byte, dst *map[string]any) error {
	return pasgraph.DecodeObject(raw, dst)
}
