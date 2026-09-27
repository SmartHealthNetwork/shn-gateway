package lanequalify

// CertificationRow is line's PAS request bundle from the readiness corpus and
// the versioned request-bundle profile a gateway certifies such a bundle
// against, for a warm-up that sends one request rather than the corpus. ok is
// false for a line the corpus does not cover. It lives outside the files the
// validator image and the Kit share with this package.
func CertificationRow(line string) (body []byte, profile string, ok bool) {
	version, ok := pasVersion(line)
	if !ok {
		return nil, "", false
	}
	for _, row := range warmups(line) {
		if row.identity != "init-pas-request-bundle" {
			continue
		}
		raw, err := fixtures.ReadFile(row.file)
		if err != nil {
			return nil, "", false
		}
		return raw, row.profile + "|" + version, true
	}
	return nil, "", false
}
