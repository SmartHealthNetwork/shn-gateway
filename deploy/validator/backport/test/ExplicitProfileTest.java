package org.hl7.fhir.common.hapi.validation.validator;

import ca.uhn.fhir.context.FhirContext;
import ca.uhn.fhir.context.support.DefaultProfileValidationSupport;
import ca.uhn.fhir.validation.ValidationContext;
import ca.uhn.fhir.validation.ValidationOptions;
import org.hl7.fhir.exceptions.FHIRException;
import org.hl7.fhir.r5.context.IWorkerContext;
import org.hl7.fhir.utilities.validation.ValidationMessage;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.List;

/** Runs with the original or replacement class first on the exact engine classpath. */
public final class ExplicitProfileTest {
    private static final String PATIENT = "http://hl7.org/fhir/StructureDefinition/Patient";
    private static final String MISSING = "https://example.org/StructureDefinition/unavailable";
    private static final FhirContext CONTEXT = FhirContext.forR4Cached();
    private static final IWorkerContext BASE = WorkerContextValidationSupportAdapter.newVersionSpecificWorkerContextWrapper(
            new DefaultProfileValidationSupport(CONTEXT));
    private static final List<String> failures = new ArrayList<>();
    private static int rows;

    static void verifyLoadedClass() throws Exception {
        byte[] bytes;
        try (var stream = ValidatorWrapper.class.getResourceAsStream("ValidatorWrapper.class")) {
            if (stream == null) throw new AssertionError("loaded class resource unavailable");
            bytes = stream.readAllBytes();
        }
        String actual = java.util.HexFormat.of().formatHex(java.security.MessageDigest.getInstance("SHA-256").digest(bytes));
        String expected = System.getProperty("backport.expectedClass");
        if (expected != null && !actual.equals(expected)) throw new AssertionError("unexpected effective class: " + actual);
        System.out.println("EFFECTIVE_CLASS_SHA256=" + actual);
        System.out.println("EFFECTIVE_CLASS_SOURCE=" + ValidatorWrapper.class.getProtectionDomain().getCodeSource().getLocation());
    }
    public static void main(String[] args) throws Exception {
        verifyLoadedClass();
        for (boolean policy : new boolean[]{false, true}) {
            for (boolean inBand : new boolean[]{false, true}) {
                for (boolean xml : new boolean[]{false, true}) {
                    for (String missing : List.of(MISSING, PATIENT + "|9.9.9")) {
                        for (boolean validExplicit : new boolean[]{false, true}) {
                            String name = "missing policy=" + policy + " inBand=" + inBand + " xml=" + xml
                                    + " validExplicit=" + validExplicit + " profile=" + missing;
                            run(name, () -> {
                                ValidationOptions options = new ValidationOptions().addProfile(missing);
                                if (validExplicit) options.addProfile(PATIENT);
                                List<ValidationMessage> messages = validate(policy, inBand, xml, options, missing, null);
                                if (messages.stream().noneMatch(m -> m.getLevel() == ValidationMessage.IssueSeverity.ERROR
                                        && m.getMessage().contains(missing))) {
                                    throw new AssertionError("explicit missing profile did not produce ERROR: " + messages);
                                }
                            });
                        }
                    }
                    for (RuntimeException fault : List.of(
                            new FHIRException("synthetic resolver unavailable", new IllegalStateException("retained cause")),
                            new IllegalStateException("synthetic resolver execution failed"))) {
                        run("execution " + fault.getClass().getSimpleName() + " policy=" + policy + " inBand=" + inBand + " xml=" + xml,
                                () -> {
                                    try {
                                        validate(policy, inBand, xml, new ValidationOptions().addProfile(MISSING), MISSING, fault);
                                    } catch (RuntimeException actual) {
                                        if (actual != fault) throw new AssertionError("resolver exception identity lost", actual);
                                        return;
                                    }
                                    throw new AssertionError("resolver execution exception swallowed");
                                });
                    }
                    run("valid policy=" + policy + " inBand=" + inBand + " xml=" + xml, () -> {
                        List<ValidationMessage> messages = validate(policy, inBand, xml,
                                new ValidationOptions().addProfile(PATIENT), MISSING, null);
                        if (messages.stream().anyMatch(m -> m.getLevel() == ValidationMessage.IssueSeverity.ERROR
                                || m.getLevel() == ValidationMessage.IssueSeverity.FATAL)) {
                            throw new AssertionError("valid Patient rejected: " + messages);
                        }
                    });
                }
            }
        }
        System.out.println("ROWS=" + rows + " FAILURES=" + failures.size());
        failures.forEach(System.out::println);
        if (!failures.isEmpty()) throw new AssertionError("explicit-profile matrix failed");
    }

    private static List<ValidationMessage> validate(boolean policy, boolean inBand, boolean xml,
            ValidationOptions options, String intercepted, RuntimeException fault) {
        IWorkerContext worker = (IWorkerContext) Proxy.newProxyInstance(IWorkerContext.class.getClassLoader(),
                new Class<?>[]{IWorkerContext.class}, (proxy, method, args) -> {
                    if (method.getName().startsWith("fetchResource") && args != null && args.length >= 2
                            && intercepted.equals(args[1])) {
                        if (fault != null) throw fault;
                        return null;
                    }
                    try { return method.invoke(BASE, args); }
                    catch (InvocationTargetException e) { throw e.getCause(); }
                });
        String payload = xml
                ? "<Patient xmlns=\"http://hl7.org/fhir\"><id value=\"synthetic\"/>"
                    + (inBand ? "<meta><profile value=\"" + PATIENT + "\"/></meta>" : "") + "</Patient>"
                : "{\"resourceType\":\"Patient\",\"id\":\"synthetic\""
                    + (inBand ? ",\"meta\":{\"profile\":[\"" + PATIENT + "\"]}" : "") + "}";
        return new ValidatorWrapper().setExtensionDomains(List.of()).setNoTerminologyChecks(true)
                .setValidationPolicyAdvisor(new FhirDefaultPolicyAdvisor())
                .setErrorForUnknownProfiles(policy).validate(worker, ValidationContext.forText(CONTEXT, payload, options));
    }

    private static void run(String name, Runnable row) {
        rows++;
        try { row.run(); System.out.println("PASS " + name); }
        catch (Throwable e) { failures.add("FAIL " + name + ": " + e); }
    }
}
