package org.hl7.fhir.common.hapi.validation.validator;

import ca.uhn.fhir.context.FhirContext;
import ca.uhn.fhir.context.support.DefaultProfileValidationSupport;
import ca.uhn.fhir.interceptor.executor.InterceptorService;
import ca.uhn.fhir.jpa.dao.BaseHapiFhirResourceDao;
import ca.uhn.fhir.jpa.rp.r4.PatientResourceProvider;
import ca.uhn.fhir.rest.server.RestfulServer;
import org.hl7.fhir.exceptions.FHIRException;
import org.hl7.fhir.instance.model.api.IBaseResource;
import org.hl7.fhir.r4.model.OperationOutcome;
import org.hl7.fhir.r4.model.Patient;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;
import org.springframework.mock.web.MockServletConfig;
import java.lang.reflect.Field;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

/** Uses the shipped servlet, resource provider, DAO validate method, and engine. */
public final class OperationBoundaryTest {
    private static final String PATIENT = "http://hl7.org/fhir/StructureDefinition/Patient";
    private static final String MISSING = "https://example.org/StructureDefinition/unavailable";
    private static final String FAILURE = "https://example.org/StructureDefinition/resolver-execution";
    private static final FHIRException FAULT = new FHIRException("synthetic resolver unavailable",
            new IllegalStateException("retained resolver cause"));
    private static int resolverCalls;

    public static void main(String[] args) throws Exception {
        ExplicitProfileTest.verifyLoadedClass();
        Path evidence = Path.of(args[0]);
        Files.createDirectories(evidence);
        FhirContext context = FhirContext.forR4Cached();
        DefaultProfileValidationSupport support = new DefaultProfileValidationSupport(context) {
            @Override public IBaseResource fetchStructureDefinition(String url) {
                if (FAILURE.equals(url)) { resolverCalls++; throw FAULT; }
                if (MISSING.equals(url) || (PATIENT + "|9.9.9").equals(url)) return null;
                return super.fetchStructureDefinition(url);
            }
        };
        FhirInstanceValidator validator = new FhirInstanceValidator(support);
        validator.setNoTerminologyChecks(true);
        BaseHapiFhirResourceDao<Patient> dao = new BaseHapiFhirResourceDao<>() {};
        dao.setContext(context);
        dao.setResourceType(Patient.class);
        dao.setInterceptorBroadcasterForUnitTest(new InterceptorService());
        Field module = BaseHapiFhirResourceDao.class.getDeclaredField("myInstanceValidator");
        module.setAccessible(true);
        module.set(dao, validator);
        PatientResourceProvider provider = new PatientResourceProvider();
        provider.setContext(context);
        provider.setDao(dao);
        RestfulServer server = new RestfulServer(context);
        server.registerProvider(provider);
        server.init(new MockServletConfig());
        try {
            for (boolean policy : new boolean[]{false, true}) {
                validator.setErrorForUnknownProfiles(policy);
                for (boolean inBand : new boolean[]{false, true}) {
                    String body = "{\"resourceType\":\"Patient\",\"id\":\"synthetic\""
                            + (inBand ? ",\"meta\":{\"profile\":[\"" + PATIENT + "\"]}" : "") + "}";
                    for (String profile : List.of(PATIENT, MISSING, PATIENT + "|9.9.9", FAILURE)) {
                        String row = "policy-" + policy + "-inband-" + inBand + "-"
                                + (profile.equals(PATIENT) ? "valid" : profile.equals(FAILURE) ? "execution"
                                : profile.equals(MISSING) ? "missing-canonical" : "missing-version");
                        MockHttpServletRequest request = new MockHttpServletRequest("POST", "/fhir/Patient/$validate");
                        request.setServletPath("/fhir");
                        request.setPathInfo("/Patient/$validate");
                        request.setContentType("application/fhir+json");
                        request.addHeader("Accept", "application/fhir+json");
                        request.addParameter("profile", profile);
                        request.setQueryString("profile=" + java.net.URLEncoder.encode(profile, StandardCharsets.UTF_8));
                        request.setContent(body.getBytes(StandardCharsets.UTF_8));
                        MockHttpServletResponse response = new MockHttpServletResponse();
                        int before = resolverCalls;
                        server.service(request, response);
                        String raw = response.getContentAsString();
                        Files.writeString(evidence.resolve(row + "-request.json"), body);
                        Files.writeString(evidence.resolve(row + "-response.json"), raw);
                        Files.writeString(evidence.resolve(row + "-call.txt"), "POST /fhir/Patient/$validate?"
                                + request.getQueryString() + "\nstatus=" + response.getStatus() + "\n");
                        OperationOutcome outcome = context.newJsonParser().parseResource(OperationOutcome.class, raw);
                        boolean error = outcome.getIssue().stream().anyMatch(i -> i.getSeverity() == OperationOutcome.IssueSeverity.ERROR
                                || i.getSeverity() == OperationOutcome.IssueSeverity.FATAL);
                        if (profile.equals(FAILURE)) {
                            if (response.getStatus() != 500 || !error || resolverCalls == before
                                    || raw.contains("Invalid profile. Failed to retrieve")
                                    || !raw.contains("synthetic resolver unavailable")) {
                                throw new AssertionError(row + " was not execution failure: " + response.getStatus() + " " + raw);
                            }
                        } else if (profile.equals(PATIENT)) {
                            if (response.getStatus() != 200 || error) throw new AssertionError(row + ": " + raw);
                        } else if (response.getStatus() != 200 || !error || !raw.contains(profile)) {
                            throw new AssertionError(row + " was not ordinary invalid: " + response.getStatus() + " " + raw);
                        }
                        System.out.println("PASS " + row + " HTTP=" + response.getStatus());
                    }
                }
            }
        } finally { server.destroy(); }
    }
}
