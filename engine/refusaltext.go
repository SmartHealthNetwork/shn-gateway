package engine

import "strconv"

// quotedBounded is v quoted for a refusal's text, at most 64 bytes of it.
func quotedBounded(v string) string {
	const most = 64
	if len(v) > most {
		return strconv.Quote(v[:most]) + "…"
	}
	return strconv.Quote(v)
}
