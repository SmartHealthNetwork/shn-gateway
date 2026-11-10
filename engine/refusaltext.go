package engine

import (
	"strconv"
	"strings"
)

// quotedBounded is v quoted for a refusal's text, at most 64 bytes of it.
func quotedBounded(v string) string {
	const most = 64
	if len(v) > most {
		return strconv.Quote(v[:most]) + "…"
	}
	return strconv.Quote(v)
}

// bounded is v for a refusal's text, unquoted, at most 64 bytes of it, cut at a character
// boundary.
func bounded(v string) string {
	const most = 64
	if len(v) > most {
		return strings.ToValidUTF8(v[:most], "") + "…"
	}
	return v
}
