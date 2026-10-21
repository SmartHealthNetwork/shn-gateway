package engine

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// evalBundleLine is the IG line the evaluation bundle's gateway builds at: it routes to the
// hosted evaluation payer, a PAS 2.0.1 payer.
const evalBundleLine = "2.0"

// igCanonicalBases names the package each IG canonical base belongs to: an IG's canonical base
// is its package's "canonical", and every profile it defines is <base>/StructureDefinition/<id>.
var igCanonicalBases = map[string]string{
	"http://hl7.org/fhir/us/core":         "hl7.fhir.us.core",
	"http://hl7.org/fhir/us/davinci-crd":  "hl7.fhir.us.davinci-crd",
	"http://hl7.org/fhir/us/davinci-dtr":  "hl7.fhir.us.davinci-dtr",
	"http://hl7.org/fhir/us/davinci-pas":  "hl7.fhir.us.davinci-pas",
	"http://hl7.org/fhir/us/davinci-cdex": "hl7.fhir.us.davinci-cdex",
	"http://hl7.org/fhir/us/davinci-hrex": "hl7.fhir.us.davinci-hrex",
	"http://hl7.org/fhir/uv/sdc":          "hl7.fhir.uv.sdc",
}

// evalBundleIGs reads the IG packages the evaluation bundle's data-HAPI loads, the validator the
// bundle's gateway validates what it sends against (compose.eval.yml's hapi service,
// hapi.fhir.implementationguides.<key>.name, .version and .packageUrl).
func evalBundleIGs(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "deploy", "eval", "compose.eval.yml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ig := regexp.MustCompile(`^\s+hapi\.fhir\.implementationguides\.([a-z0-9]+)\.(name|version|packageUrl):\s*"?([^"\s]+)"?\s*$`)
	service := regexp.MustCompile(`^  ([a-z0-9-]+):\s*$`)
	names, versions, urls := map[string]string{}, map[string]string{}, map[string]string{}
	inHAPI := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := service.FindStringSubmatch(line); m != nil {
			inHAPI = m[1] == "hapi"
			continue
		}
		if m := ig.FindStringSubmatch(line); inHAPI && m != nil {
			switch m[2] {
			case "name":
				names[m[1]] = m[3]
			case "version":
				versions[m[1]] = m[3]
			default:
				urls[m[1]] = m[3]
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	loaded := map[string]string{}
	for key, name := range names {
		// The archive the Dockerfile bakes is named <name>-<version>.tgz under /packages.
		if want := "file:///packages/" + name + "-" + versions[key] + ".tgz"; urls[key] != want {
			t.Errorf("compose.eval.yml's hapi loads %s %s from %q, want %q", name, versions[key], urls[key], want)
		}
		loaded[name] = versions[key]
	}
	if len(loaded) == 0 {
		t.Fatal("compose.eval.yml's hapi service loads no IG packages")
	}
	return loaded
}

// TestEvalBundleValidatorLoadsEveryProfileTheGatewayDeclares: at strict, the evaluation bundle's
// gateway validates what it sends against the profiles it declares, so the bundle's validator
// must load the package that defines each of them, at its version. The set is the gateway's own:
// the profile it declares on a QuestionnaireResponse it sends (dtrQRProfile) and the profiles
// the PA workstream certifies at the bundle's line (paCertification.profile). A profile whose
// package the validator does not load is refused at strict as "could not be found", which is
// how the bundle failed UC03-UC07 before it carried DTR.
func TestEvalBundleValidatorLoadsEveryProfileTheGatewayDeclares(t *testing.T) {
	loaded := evalBundleIGs(t)
	declared := map[string]bool{}
	qr, err := dtrQRProfile(evalBundleLine)
	if err != nil {
		t.Fatal(err)
	}
	declared[qr] = true
	c := paCertification{}
	for _, species := range []string{"PASRequestBundle", "PASResponseBundle", "Claim", "ClaimResponse", "QuestionnaireResponse"} {
		for _, leg := range []string{"pas-claim", "pas-claim-update", "pas-claim-inquire"} {
			if p, ok := c.profile(species, evalBundleLine, leg); ok {
				declared[p] = true
			}
		}
	}
	for p := range declared {
		canonical, version, versioned := strings.Cut(p, "|")
		base, _, ok := strings.Cut(canonical, "/StructureDefinition/")
		if !ok || !versioned {
			t.Errorf("%s: not a versioned profile canonical", p)
			continue
		}
		pkg, known := igCanonicalBases[base]
		if !known {
			t.Errorf("%s: a profile of an IG this test cannot place in a package (add its canonical base)", p)
			continue
		}
		if got := loaded[pkg]; got != version {
			t.Errorf("%s: the evaluation bundle's validator loads %s %q, want %q", p, pkg, got, version)
		}
	}
}
