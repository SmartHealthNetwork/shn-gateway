package subscription

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/paedits"
)

// schemaStrings is every path to a string member the closed
// SubscriptionStatus Bundle schema admits, as "Bundle.ENTRY[].FULLURL" (the
// schema's member names are folded).
func schemaStrings(s *jschema, at string, out map[string]bool) {
	switch s.kind {
	case '"':
		out[at] = true
	case '[':
		schemaStrings(s.elem, at+"[]", out)
	case '{':
		for name, m := range s.members {
			schemaStrings(m, at+"."+name, out)
		}
	}
}

// notificationStringTypes is the type of each string member the closed
// SubscriptionStatus Bundle schema admits, by its path with "[]" for every
// array index, lower case: its FHIR type, but "url" for a Bundle's locators
// (link.url, fullUrl, request.url and response.location, which FHIR types as
// uri, and which a client follows) and "reference" for a Reference's
// reference. A path the schema gains that is not here fails
// TestNotificationShapeAddressesAreE17Places until it is typed.
var notificationStringTypes = func() map[string]string {
	t := map[string]string{
		"bundle.resourcetype": "code", "bundle.id": "id", "bundle.implicitrules": "uri", "bundle.language": "code",
		"bundle.type": "code", "bundle.timestamp": "instant",
		"bundle.link[].relation": "string", "bundle.link[].url": "url",
		"bundle.entry[].fullurl": "url", "bundle.entry[].search.mode": "code",
		"bundle.entry[].request.method": "code", "bundle.entry[].request.url": "url",
		"bundle.entry[].response.status": "string", "bundle.entry[].response.location": "url",
		"bundle.entry[].response.etag": "string", "bundle.entry[].response.lastmodified": "instant",
		"bundle.entry[].resource.resourcetype": "code", "bundle.entry[].resource.id": "id",
		"bundle.entry[].resource.implicitrules": "uri", "bundle.entry[].resource.language": "code",
	}
	meta := func(at string) {
		t[at+".id"], t[at+".versionid"], t[at+".lastupdated"], t[at+".source"], t[at+".profile[]"] = "string", "id", "instant", "uri", "canonical"
		for _, list := range []string{".security[]", ".tag[]"} {
			coding(t, at+list)
		}
	}
	meta("bundle.meta")
	meta("bundle.entry[].resource.meta")
	for _, at := range []string{"bundle.entry[].resource.parameter[]", "bundle.entry[].resource.parameter[].part[]"} {
		t[at+".id"], t[at+".name"] = "string", "string"
		t[at+".valuecanonical"], t[at+".valueuri"], t[at+".valuecode"] = "canonical", "uri", "code"
		t[at+".valuestring"], t[at+".valueinstant"], t[at+".valuedatetime"] = "string", "instant", "dateTime"
		t[at+".valuereference.reference"], t[at+".valuereference.type"] = "reference", "uri"
		t[at+".valuecodeableconcept.id"], t[at+".valuecodeableconcept.text"] = "string", "string"
		coding(t, at+".valuecodeableconcept.coding[]")
	}
	return t
}()

func coding(t map[string]string, at string) {
	t[at+".id"], t[at+".system"], t[at+".version"], t[at+".code"], t[at+".display"] = "string", "uri", "string", "code", "string"
}

// A notification is held to the closed SubscriptionStatus Bundle schema
// (CheckShape), so its uri-like members are known by construction, and each
// is placed by its element, never by guessing its meaning: a locator (a url,
// or a reference) is one of E-17's places, as its registry row names them,
// and an identity (a uri or canonical that names: a profile, a code system,
// a source, a type) is on notificationIdentityMembers with its reason,
// carried as sent. Every string member the schema admits is typed here, so
// one it gains fails this row until it is typed and placed; a url or a
// reference on the identity list fails it, and so does an identity among
// E-17's places.
func TestNotificationShapeAddressesAreE17Places(t *testing.T) {
	got := map[string]bool{}
	schemaStrings(statusAnswerSchema, "Bundle", got)
	var e17 relay.Edit
	for _, e := range paedits.Rows() {
		if e.ID == relay.EditCDexTaskNotificationAddressRekey {
			e17 = e
		}
	}
	if len(e17.Paths) == 0 {
		t.Fatal("E-17 is not in the registry")
	}
	index := regexp.MustCompile(`\[[a-z0-9]\]`)
	places := map[string]bool{}
	for _, p := range e17.Paths {
		p, _, _ = strings.Cut(p, " (")
		places[strings.ToLower(strings.Replace(index.ReplaceAllString(p, "[]"), "entry[0]", "entry[]", 1))] = true
	}
	var faults []string
	for path := range got {
		p := strings.ToLower(path)
		typ, ok := notificationStringTypes[p]
		reason, identity := notificationIdentityMembers[p]
		switch {
		case !ok:
			faults = append(faults, path+": not typed")
		case typ == "url" || typ == "reference":
			if identity {
				faults = append(faults, path+": a "+typ+" on the identity list")
			}
			if !places[p] {
				faults = append(faults, path+": a "+typ+" E-17 does not re-key")
			}
		case typ == "uri" || typ == "canonical":
			if places[p] {
				faults = append(faults, path+": an identity ("+typ+") among E-17's places")
			}
			if !identity || reason == "" {
				faults = append(faults, path+": a "+typ+" neither E-17's nor on the identity list with a reason")
			}
		case places[p] || identity:
			faults = append(faults, path+": a "+typ+" listed as a locator or an identity")
		}
	}
	for p := range notificationIdentityMembers {
		if !containsFold(got, p) {
			faults = append(faults, p+": on the identity list, not admitted by the shape")
		}
	}
	for p := range places {
		// An entry's link is one of E-17's places, which the shape refuses.
		if p != "bundle.entry[].link[].url" && !containsFold(got, p) {
			faults = append(faults, p+": one of E-17's places, not admitted by the shape")
		}
	}
	sort.Strings(faults)
	for _, f := range faults {
		t.Error(f)
	}
}

func containsFold(m map[string]bool, p string) bool {
	for k := range m {
		if strings.EqualFold(k, p) {
			return true
		}
	}
	return false
}
