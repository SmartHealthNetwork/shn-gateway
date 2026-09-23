package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const payerEOBProfile = "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization"
const payerEOBWriteScope = "system/ExplanationOfBenefit.write"

var ErrPayerEOBSourceUnavailable = errors.New("payer's own EOB source or authority is unavailable")

// handlePayerEOBRecord is a private, participant-authenticated action. A
// registered payer connector names an EOB already written in its own SoR; the
// gateway never accepts caller-supplied EOB bytes or projects a native reply.
func (g *Gateway) handlePayerEOBRecord(w http.ResponseWriter, r *http.Request) {
	principal, authenticated, unavailable := g.ingressPrincipal(r)
	if !authenticated || principal.ClientID == "" || g.ingressAuth == nil {
		if unavailable {
			writeStoreUnavailable(w, "ingress key store unavailable")
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "payer source authentication required"})
		return
	}
	registration, found := g.ingressAuth.clients[principal.ClientID]
	if !found || !registration.PayerEOBRecord || principal.Scope != payerEOBWriteScope {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "payer EOB action not granted"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes+1))
	if err != nil || len(body) > shnsdk.MaxRequestBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payer EOB action request"})
		return
	}
	var in struct {
		SubjectPCI string `json:"subjectPCI"`
		SourceRef  string `json:"sourceRef"`
	}
	if decodeMessage(body, &in) != nil || in.SubjectPCI == "" || in.SourceRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payer EOB action request"})
		return
	}
	if err := g.RecordPayerEOBFromSource(r.Context(), in.SubjectPCI, in.SourceRef); err != nil {
		if errors.Is(err, ErrEOBSubjectMismatch) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "EOB id belongs to another patient"})
			return
		}
		if errors.Is(err, ErrPayerEOBSourceUnavailable) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "payer EOB source or authority unavailable"})
			return
		}
		writeStoreUnavailable(w, "payer EOB record unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"recorded": in.SourceRef})
}

// RecordPayerEOBFromSource is an explicit payer-owned clinical action. The payer
// system has already authored the complete EOB; the gateway reads its original
// bytes and records those bytes for the payer's Patient Access API. Native PAS
// delivery never calls this method. A source miss or failed proof writes nothing.
func (g *Gateway) RecordPayerEOBFromSource(ctx context.Context, subjectPCI, sourceRef string) error {
	if g == nil || g.cfg.Role != "payer" || g.cfg.SoR == nil || g.cfg.Store == nil ||
		subjectPCI == "" || g.cfg.PayerEOBValidator == nil {
		return ErrPayerEOBSourceUnavailable
	}
	if _, err := localEOBRef(sourceRef, "ExplanationOfBenefit"); err != nil {
		return err
	}
	reader := ReadSystemOfRecord(g.cfg.SoR)
	raw, found, err := reader.ResolveByReferenceContext(ctx, sourceRef)
	if err != nil {
		return err
	}
	if !found {
		return ErrPayerEOBSourceUnavailable
	}
	var eob struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Status       string `json:"status"`
		Type         struct {
			Coding []struct {
				System string `json:"system"`
				Code   string `json:"code"`
			} `json:"coding"`
		} `json:"type"`
		Use     string `json:"use"`
		Created string `json:"created"`
		Outcome string `json:"outcome"`
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
		Insurer struct {
			Reference string `json:"reference"`
		} `json:"insurer"`
		Provider struct {
			Reference string `json:"reference"`
		} `json:"provider"`
		Insurance []struct {
			Focal    bool `json:"focal"`
			Coverage struct {
				Reference string `json:"reference"`
			} `json:"coverage"`
		} `json:"insurance"`
		Item []struct {
			ProductOrService struct {
				Coding []struct {
					System string `json:"system"`
					Code   string `json:"code"`
				} `json:"coding"`
			} `json:"productOrService"`
			Adjudication []struct {
				Category struct {
					Coding []struct {
						Code string `json:"code"`
					} `json:"coding"`
				} `json:"category"`
				Extension []struct {
					URL       string `json:"url"`
					Extension []struct {
						URL                  string `json:"url"`
						ValueCodeableConcept struct {
							Coding []struct {
								Code string `json:"code"`
							} `json:"coding"`
						} `json:"valueCodeableConcept"`
					} `json:"extension"`
				} `json:"extension"`
			} `json:"adjudication"`
		} `json:"item"`
		PreAuthRef []string `json:"preAuthRef"`
	}
	if decodeMessage(raw, &eob) != nil || eob.ResourceType != "ExplanationOfBenefit" ||
		eob.ID == "" || sourceRef != "ExplanationOfBenefit/"+eob.ID || eob.Status != "active" || eob.Use != "preauthorization" ||
		len(eob.Type.Coding) == 0 || eob.Type.Coding[0].System == "" || eob.Type.Coding[0].Code == "" ||
		eob.Created == "" || eob.Outcome == "" || len(eob.Item) == 0 {
		return ErrPayerEOBSourceUnavailable
	}
	if err := provePayerEOBPatient(ctx, reader, eob.Patient.Reference, subjectPCI); err != nil {
		return err
	}
	if err := provePayerEOBReference(ctx, reader, eob.Insurer.Reference, "Organization"); err != nil {
		return err
	}
	if err := provePayerEOBReference(ctx, reader, eob.Provider.Reference, "Organization", "Practitioner", "PractitionerRole"); err != nil {
		return err
	}
	if len(eob.Insurance) != 1 || !eob.Insurance[0].Focal {
		return ErrPayerEOBSourceUnavailable
	}
	coverageRef := eob.Insurance[0].Coverage.Reference
	if err := provePayerEOBReference(ctx, reader, coverageRef, "Coverage"); err != nil {
		return err
	}
	coverageRaw, _, err := reader.ResolveByReferenceContext(ctx, coverageRef)
	if err != nil {
		return err
	}
	var coverage struct {
		Beneficiary struct {
			Reference string `json:"reference"`
		} `json:"beneficiary"`
	}
	if decodeMessage(coverageRaw, &coverage) != nil || coverage.Beneficiary.Reference != eob.Patient.Reference {
		return ErrPayerEOBSourceUnavailable
	}
	denied := false
	for _, item := range eob.Item {
		if len(item.ProductOrService.Coding) == 0 || item.ProductOrService.Coding[0].System == "" || item.ProductOrService.Coding[0].Code == "" {
			return ErrPayerEOBSourceUnavailable
		}
		for _, adj := range item.Adjudication {
			for _, category := range adj.Category.Coding {
				if category.Code == "denialreason" {
					denied = true
				}
			}
			for _, ext := range adj.Extension {
				if ext.URL != "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction" {
					continue
				}
				for _, sub := range ext.Extension {
					if sub.URL != "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewActionCode" {
						continue
					}
					for _, coding := range sub.ValueCodeableConcept.Coding {
						if coding.Code == "A3" {
							denied = true
						}
					}
				}
			}
		}
	}
	if denied && len(eob.PreAuthRef) > 0 {
		return ErrPayerEOBSourceUnavailable
	}
	for _, number := range eob.PreAuthRef {
		if strings.TrimSpace(number) == "" {
			return ErrPayerEOBSourceUnavailable
		}
	}
	evidence, err := delegateValidatorEvidence(ctx, g.cfg.PayerEOBValidator, raw, payerEOBProfile)
	// The FHIR $validate adapter proves profile execution but deliberately does
	// not claim complete terminology coverage. A source-owned code is retained;
	// a reported invalid code or unproven profile still refuses this local write.
	if err != nil || !evidence.ExecutionAttempted || evidence.Profile.State != shnsdk.ValidationValid || evidence.Terminology.State == shnsdk.ValidationInvalid {
		return ErrPayerEOBSourceUnavailable
	}
	return g.cfg.Store.RecordEOB(subjectPCI, eob.ID, raw)
}

// provePayerEOBPatient binds the EOB to one already-issued PCI in one Patient
// source snapshot. A separate resolver call followed by a second Patient read
// could attest two different versions if the source changed between calls.
func provePayerEOBPatient(ctx context.Context, reader ContextSystemOfRecord, ref, subjectPCI string) error {
	id, err := localEOBRef(ref, "Patient")
	if err != nil {
		return err
	}
	raw, found, err := reader.ResolveByReferenceContext(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		return ErrPayerEOBSourceUnavailable
	}
	var patient struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Identifier   []struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
	}
	if decodeMessage(raw, &patient) != nil || patient.ResourceType != "Patient" || patient.ID != id {
		return ErrPayerEOBSourceUnavailable
	}
	count := 0
	for _, identifier := range patient.Identifier {
		if identifier.System != "urn:shn:pci" {
			continue
		}
		count++
		if identifier.Value != subjectPCI || !strings.HasPrefix(identifier.Value, "pci:") {
			return ErrPayerEOBSourceUnavailable
		}
	}
	if count != 1 {
		return ErrPayerEOBSourceUnavailable
	}
	return nil
}

func localEOBRef(ref string, types ...string) (string, error) {
	typ, id, ok := strings.Cut(ref, "/")
	if !ok || !pasSafeResourceID(id) || strings.Contains(id, "/") {
		return "", ErrPayerEOBSourceUnavailable
	}
	for _, allowed := range types {
		if typ == allowed {
			return id, nil
		}
	}
	return "", ErrPayerEOBSourceUnavailable
}

func provePayerEOBReference(ctx context.Context, reader ContextSystemOfRecord, ref string, types ...string) error {
	id, err := localEOBRef(ref, types...)
	if err != nil {
		return err
	}
	raw, found, err := reader.ResolveByReferenceContext(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		return ErrPayerEOBSourceUnavailable
	}
	var resource struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if decodeMessage(raw, &resource) != nil || resource.ID != id {
		return ErrPayerEOBSourceUnavailable
	}
	for _, allowed := range types {
		if resource.ResourceType == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: source resource type mismatch", ErrPayerEOBSourceUnavailable)
}
