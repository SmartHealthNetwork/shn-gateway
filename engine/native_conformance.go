package engine

// The native responder's conformance options: the level it certifies the
// participant's answers at, and where it records what it finds. They are the
// only functions of the core responder that read the level, so a change to
// them owes the level gates (tools/gateroute conformance-levels).

// WithConformancePolicy gives the responder the enforcement policy of the
// gateway it runs in. Unset ⇒ the zero value, strict.
func WithConformancePolicy(p ConformancePolicy) NativeOption {
	return func(n *nativeResponder) { n.conformance = p }
}

// ConformanceLevelForTest exposes this responder's own configured enforcement
// level — test-only introspection (the EndpointEvidenceForTest pattern)
// proving a WithConformancePolicy option (or its absence, which leaves the
// zero value, strict) actually reached this responder, not just whatever
// engine.Config a caller assembled.
func (n *nativeResponder) ConformanceLevelForTest() ConformanceEnforcement {
	return n.module().Level()
}

// module is the responder's module 3: the level WithConformancePolicy gave
// it (the app passes the gateway's own), strict with none. Every payload
// conformance check of the responder asks it.
func (n *nativeResponder) module() conformanceModule { return n.conformance }

// bindFindingEmitter receives the engine's finding emitter after engine.New
// builds the gateway. It cannot be a NativeOption: NewNativeResponder runs
// before engine.New, so no gateway exists when the options are applied.
func (n *nativeResponder) bindFindingEmitter(emit func(ConformanceFinding)) { n.emitFinding = emit }
