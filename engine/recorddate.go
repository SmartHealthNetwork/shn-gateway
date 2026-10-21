package engine

import "encoding/json"

// recordClinicalDate pulls the date a facility record is selected on for FR-24
// (and ordered by as evidence): DiagnosticReport.effectiveDateTime, else the
// end of DiagnosticReport.effectivePeriod, else its start; or
// DocumentReference.date. "" if absent.
func recordClinicalDate(resJSON []byte) string {
	var r struct {
		EffectiveDateTime string `json:"effectiveDateTime"`
		EffectivePeriod   struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"effectivePeriod"`
		Date string `json:"date"`
	}
	_ = json.Unmarshal(resJSON, &r)
	for _, d := range []string{r.EffectiveDateTime, r.EffectivePeriod.End, r.EffectivePeriod.Start} {
		if d != "" {
			return d
		}
	}
	return r.Date
}
