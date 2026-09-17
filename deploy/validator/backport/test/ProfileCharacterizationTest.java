package org.hl7.fhir.common.hapi.validation.validator;

import ca.uhn.fhir.context.FhirContext;
import ca.uhn.fhir.context.support.DefaultProfileValidationSupport;
import ca.uhn.fhir.validation.ValidationContext;
import ca.uhn.fhir.validation.ValidationOptions;
import org.hl7.fhir.utilities.validation.ValidationMessage;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

/** Emits stable existing-policy results for byte comparison before and after replacement. */
public final class ProfileCharacterizationTest {
    public static void main(String[] args) throws Exception {
        FhirContext context = FhirContext.forR4Cached();
        var worker = WorkerContextValidationSupportAdapter.newVersionSpecificWorkerContextWrapper(new DefaultProfileValidationSupport(context));
        List<String> output = new ArrayList<>();
        for (boolean policy : new boolean[]{false, true}) {
            for (boolean xml : new boolean[]{false, true}) {
                for (String inBand : List.of("", "http://hl7.org/fhir/StructureDefinition/Patient",
                        "https://example.org/StructureDefinition/unavailable")) {
                    String body = xml ? "<Patient xmlns=\"http://hl7.org/fhir\"><id value=\"synthetic\"/>"
                            + (inBand.isEmpty() ? "" : "<meta><profile value=\"" + inBand + "\"/></meta>") + "</Patient>"
                            : "{\"resourceType\":\"Patient\",\"id\":\"synthetic\""
                            + (inBand.isEmpty() ? "" : ",\"meta\":{\"profile\":[\"" + inBand + "\"]}") + "}";
                    var wrapper = new ValidatorWrapper().setExtensionDomains(List.of()).setNoTerminologyChecks(true)
                            .setErrorForUnknownProfiles(policy).setValidationPolicyAdvisor(new FhirDefaultPolicyAdvisor());
                    List<ValidationMessage> messages = wrapper.validate(worker, ValidationContext.forText(context, body, new ValidationOptions()));
                    output.add(policy + "/" + xml + "/" + inBand + ": " + messages);
                }
                var wrapper = new ValidatorWrapper().setExtensionDomains(List.of()).setNoTerminologyChecks(true)
                        .setErrorForUnknownProfiles(policy).setValidationPolicyAdvisor(new FhirDefaultPolicyAdvisor());
                String body = xml ? "<Patient xmlns=\"http://hl7.org/fhir\"/>" : "{\"resourceType\":\"Patient\"}";
                List<ValidationMessage> wrong = wrapper.validate(worker, ValidationContext.forText(context, body,
                        new ValidationOptions().addProfile("http://hl7.org/fhir/StructureDefinition/Observation")));
                if (wrong.stream().noneMatch(m -> m.getLevel() == ValidationMessage.IssueSeverity.ERROR
                        || m.getLevel() == ValidationMessage.IssueSeverity.FATAL)) throw new AssertionError("wrong-resource profile accepted");
                output.add(policy + "/" + xml + "/wrong-resource: " + wrong);
                try {
                    var malformed = wrapper.validate(worker, ValidationContext.forText(context,
                            xml ? "<Patient xmlns=\"http://hl7.org/fhir\"><" : "{\"resourceType\":\"Patient\",", new ValidationOptions()));
                    output.add(policy + "/" + xml + "/malformed: " + malformed);
                } catch (RuntimeException e) { output.add(policy + "/" + xml + "/malformed: " + e); }
            }
        }
        Files.write(Path.of(args[0]), output);
        System.out.println("PASS characterized rows=" + output.size());
    }
}
