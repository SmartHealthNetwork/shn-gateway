package engine

import (
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const certificationPAS = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/"
const certificationDTR = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/"

func profileFor(species, line, legType string) (string, bool) {
	if legType != "" && legType != "pas-claim" && legType != "pas-claim-update" && legType != "pas-claim-inquire" {
		return "", false
	}
	if species == "QuestionnaireResponse" {
		d, ok := shnsdk.DTRLineDef(line)
		if !ok {
			return "", false
		}
		return certificationDTR + "dtr-questionnaireresponse|" + d.PackageVersion, true
	}
	d, ok := shnsdk.PASLineDef(line)
	if !ok {
		return "", false
	}
	// An inquiry is its own message family: the request Claim, its request Bundle and
	// the answer's response Bundle each have an inquiry profile of their own, and
	// certifying an inquiry against the submit profiles would report it invalid for a
	// profile it was never meant to meet. The 2.2.1 answer's Parameters wrapper is not
	// certified as a whole: the IG governs that
	// wrapper by its OperationDefinition and declares no profile for it.
	inquiry := legType == "pas-claim-inquire"
	name := ""
	switch species {
	case "Claim":
		switch {
		case inquiry:
			name = "profile-claim-inquiry"
		case legType == "pas-claim-update":
			name = "profile-claim-update"
		default:
			name = "profile-claim"
		}
	case "ClaimResponse":
		name = "profile-claimresponse"
		if inquiry {
			name = "profile-claiminquiryresponse"
		}
	case "PASRequestBundle":
		name = "profile-pas-request-bundle"
		if inquiry {
			name = "profile-pas-inquiry-request-bundle"
		}
	case "PASResponseBundle":
		name = "profile-pas-response-bundle"
		if inquiry {
			name = "profile-pas-inquiry-response-bundle"
		}
	default:
		return "", false
	}
	return certificationPAS + name + "|" + d.PackageVersion, true
}
