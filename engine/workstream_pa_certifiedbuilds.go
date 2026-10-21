package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// paCertifiedBuilds are the PA builders' certified pairs (certifiedBuilds):
// the PAS inquiry, built by the gateway's own builder at each line and
// validated as a whole against the PAS inquiry-request Bundle profile, with
// refusal controls (test/conformance TestBuildPASInquiryBundle_ValidAtEachLine,
// in every line's make validate run list).
var paCertifiedBuilds = map[certifiedBuild]string{
	{relay.BuilderSDKPASInquiry, "pa.pas", "2.0"}: "TestBuildPASInquiryBundle_ValidAtEachLine",
	{relay.BuilderSDKPASInquiry, "pa.pas", "2.1"}: "TestBuildPASInquiryBundle_ValidAtEachLine",
	{relay.BuilderSDKPASInquiry, "pa.pas", "2.2"}: "TestBuildPASInquiryBundle_ValidAtEachLine",
}

func (paWorkstream) certifiedBuilds() map[certifiedBuild]string { return paCertifiedBuilds }
