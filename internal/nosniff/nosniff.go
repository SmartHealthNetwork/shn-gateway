// Package nosniff marks every answer a gateway listener writes with
// X-Content-Type-Options: nosniff, so a browser that fetched one would read
// it as the Content-Type it declares and never guess another.
//
// The header is the gateway's own: it is set before the handler runs and adds
// to an answer, never changing its body or Content-Type, so a peer's answer is
// relayed with the bytes and Content-Type it had before; only the gateway's
// header is added beside it.
package nosniff

import "net/http"

// Header and Value are the header every gateway answer carries.
const (
	Header = "X-Content-Type-Options"
	Value  = "nosniff"
)

// Handler is h with every answer it writes carrying Header: Value, whatever
// its status, its refusals and a ServeMux's own 404 and 405 included.
func Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(Header, Value)
		h.ServeHTTP(w, r)
	})
}
