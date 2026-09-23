package engine

import (
	"context"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// checkCDSRequestContext enforces the published required context shapes for
// the three CDS hooks this gateway advertises. CRD contract lines select the
// supported exchange, while the hook selects the context schema. This is a
// bounded envelope check, not FHIR validation or patient identity resolution.
func checkCDSRequestContext(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	root, ok := deepDocument(in)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	contract, line, ok := strings.Cut(in.DeclaredVersion, "@")
	if !ok || contract != "pa.crd" {
		return deepUnavailable("version_unavailable")
	}
	if _, ok := shnsdk.CRDLineDef(line); !ok {
		return deepUnavailable("version_unavailable")
	}
	actual, ok := root["hook"].(string)
	if !ok {
		return deepUnavailable("content_unreadable") // structural hook rule owns this shape
	}
	hook := in.Exchange.crdHook
	if hook == "" {
		hook = actual // legacy path without a signed declaration
	}
	if !validCRDHook(in.Exchange.legType, hook) || !knownCDSContextHook(actual) {
		return deepUnavailable("hook_unavailable")
	}
	if actual != hook {
		return checkResult("cds.request.context", false)
	}
	contents, ok := root["context"].(map[string]any)
	if !ok {
		return deepUnavailable("content_unreadable") // structural context rule owns this shape
	}
	stringField := func(key string) bool {
		_, ok := contents[key].(string)
		return ok
	}
	stringArray := func(key string) bool {
		values, ok := contents[key].([]any)
		if !ok {
			return false
		}
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return false
			}
		}
		return true
	}
	good := stringField("patientId")
	switch hook {
	case "order-select":
		bundle, ok := contents["draftOrders"].(map[string]any)
		good = good && stringField("userId") && stringArray("selections") && ok && bundle["resourceType"] == "Bundle"
	case "order-sign":
		bundle, ok := contents["draftOrders"].(map[string]any)
		good = good && stringField("userId") && ok && bundle["resourceType"] == "Bundle"
	case "order-dispatch":
		good = good && stringArray("dispatchedOrders") && stringField("performer")
	default:
		return deepUnavailable("hook_unavailable")
	}
	return checkResult("cds.request.context", good)
}

func knownCDSContextHook(hook string) bool {
	for _, service := range cdsIngressServices {
		if service.Hook == hook {
			return true
		}
	}
	return false
}
