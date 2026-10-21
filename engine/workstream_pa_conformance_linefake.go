package engine

// The PA workstream's part in the hermetic line validator (linefake.go).

// lineFakeInquiryProfile reports whether p names one of the PAS INQUIRY profiles
// — its request Bundle or its Claim. See the Claim.item note in linefake.go.
func lineFakeInquiryProfile(p string) bool {
	switch p {
	case lineFakePAS + "profile-pas-inquiry-request-bundle", lineFakePAS + "profile-claim-inquiry":
		return true
	}
	return false
}
