//go:build !shn_coreonly

package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// paCertifiedBuilds are the PA builders' certified pairs (certifiedBuilds):
// the PAS inquiry, built by the gateway's own builder at each line and
// validated as a whole against the PAS inquiry-request Bundle profile, with
// refusal controls (the platform conformance suite's
// TestBuildPASInquiryBundle_ValidAtEachLine, in every line's validation-gate
// run list). shn-sdk v0.62.0's inquiry builder
// builds the bytes that test certified.
var paCertifiedBuilds = map[certifiedBuild]certification{
	{relay.BuilderSDKPASInquiry, "pa.pas", "2.0"}: {Test: "TestBuildPASInquiryBundle_ValidAtEachLine", SDK: "v0.62.0"},
	{relay.BuilderSDKPASInquiry, "pa.pas", "2.1"}: {Test: "TestBuildPASInquiryBundle_ValidAtEachLine", SDK: "v0.62.0"},
	{relay.BuilderSDKPASInquiry, "pa.pas", "2.2"}: {Test: "TestBuildPASInquiryBundle_ValidAtEachLine", SDK: "v0.62.0"},
}

func (paWorkstream) certifiedBuilds() map[certifiedBuild]certification { return paCertifiedBuilds }
