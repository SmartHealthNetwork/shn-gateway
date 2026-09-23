package engine

// localPayerRoutingError is this gateway's own refusal before any Hub leg.
// It carries only the routing diagnostic from recipientForWith; it is
// never a peer ApplicationReply or RelayError (FR-G40 / PCV-06).
type localPayerRoutingError struct {
	status  int
	message string
}

func (e *localPayerRoutingError) Error() string { return e.message }
