package org.hl7.fhir.utilities.regex;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeoutException;

/** Runs on the original or the replacement classes: the 6.9.12 guard times out at a spent limit, 7.0.3's does not. */
public final class RegexBackportTest {
    private static final int CALLS = 1000;
    private static final long RUNAWAY_CEILING_MILLIS = 60_000;
    private static final List<String> failures = new ArrayList<>();
    private static int rows;

    public static void main(String[] args) throws Exception {
        verifyLoadedClass();
        // The spec's regexes as the validator passes them, with a valid value. The first
        // thirteen have a hand matcher; the rest stay in the regex engine and are short.
        spentLimit("boolean", "true|false", "true");
        spentLimit("canonical uri url", "\\S*", "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction");
        spentLimit("code", "[^\\s]+(\\s[^\\s]+)*", "active now");
        spentLimit("code r5", "[^\\s]+( [^\\s]+)*", "active now");
        spentLimit("id", "[A-Za-z0-9\\-\\.]{1,64}", "claim-1.a");
        spentLimit("integer", "-?([0]|([1-9][0-9]*))", "-12");
        spentLimit("integer r5", "[0]|[-+]?[1-9][0-9]*", "+12");
        spentLimit("oid", "urn:oid:[0-2](\\.(0|[1-9][0-9]*))+", "urn:oid:2.16.840.1.113883");
        spentLimit("positiveInt", "[1-9][0-9]*", "12");
        spentLimit("string markdown", "[ \\r\\n\\t\\S]+", "free text");
        spentLimit("string r5", "^[\\s\\S]+$", "free text");
        spentLimit("unsignedInt", "[0]|([1-9][0-9]*)", "0");
        spentLimit("uuid", "urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", "urn:uuid:c757873d-ec9a-4326-a141-556f43239520");
        spentLimit("date", "([0-9]([0-9]([0-9][1-9]|[1-9]0)|[1-9]00)|[1-9]000)(-(0[1-9]|1[0-2])(-(0[1-9]|[1-2][0-9]|3[0-1]))?)?", "2026-10-31");
        spentLimit("dateTime", "([0-9]([0-9]([0-9][1-9]|[1-9]0)|[1-9]00)|[1-9]000)(-(0[1-9]|1[0-2])(-(0[1-9]|[1-2][0-9]|3[0-1])(T([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\\.[0-9]+)?(Z|(\\+|-)((0[0-9]|1[0-3]):[0-5][0-9]|14:00)))?)?)?", "2026-10-31T12:00:00.000+05:00");
        spentLimit("instant", "([0-9]([0-9]([0-9][1-9]|[1-9]0)|[1-9]00)|[1-9]000)-(0[1-9]|1[0-2])-(0[1-9]|[1-2][0-9]|3[0-1])T([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\\.[0-9]+)?(Z|(\\+|-)((0[0-9]|1[0-3]):[0-5][0-9]|14:00))", "2026-10-31T12:00:00.000Z");
        spentLimit("time", "([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\\.[0-9]+)?", "12:00:00.123");
        spentLimit("decimal", "-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?", "-12.50");
        // FHIRPath's matches(), through find: every regex a loaded constraint uses is off the
        // table, and reads too few characters on a value of this size to reach the clock.
        spentFind("us-core-16", "^[0-9]{10}$", "1234567893");
        spentFind("adf-0", "[A-Z]([A-Za-z0-9_]){0,254}", "ExampleName");
        spentFind("cnl-1", "^[^|# ]+$", "http://hl7.org/fhir/us/core/CodeSystem/example");
        // The guard stays: a runaway regex stops at the limit, on either class set.
        runaway("(.*a){20}");
        runaway("(a*)*\\1b");
        System.out.println("ROWS=" + rows + " FAILURES=" + failures.size());
        failures.forEach(System.out::println);
        System.exit(failures.isEmpty() ? 0 : 1);
    }

    private static void verifyLoadedClass() throws Exception {
        byte[] bytes;
        try (var stream = RegexTimeout.class.getResourceAsStream("RegexTimeout.class")) {
            if (stream == null) throw new AssertionError("loaded class resource unavailable");
            bytes = stream.readAllBytes();
        }
        String actual = java.util.HexFormat.of().formatHex(java.security.MessageDigest.getInstance("SHA-256").digest(bytes));
        String expected = System.getProperty("backport.expectedRegexClass");
        if (expected != null && !actual.equals(expected)) throw new AssertionError("unexpected effective class: " + actual);
        System.out.println("EFFECTIVE_CLASS_SHA256=" + actual);
        System.out.println("EFFECTIVE_CLASS_SOURCE=" + RegexTimeout.class.getProtectionDomain().getCodeSource().getLocation());
    }

    /** One timed-out call in a thousand reds the row, so the original's count holds however its threads race. */
    private static void spentLimit(String name, String regex, String value) {
        rows++;
        try {
            for (int i = 0; i < CALLS; i++) {
                if (!RegexTimeout.matches(value, regex, 0)) { failures.add("no match: " + name); return; }
            }
        } catch (TimeoutException e) {
            failures.add("timed out at a spent limit: " + name);
        }
    }

    private static void spentFind(String name, String regex, String value) {
        rows++;
        try {
            for (int i = 0; i < CALLS; i++) {
                if (!RegexTimeout.find(value, regex, 0)) { failures.add("no match: " + name); return; }
            }
        } catch (TimeoutException e) {
            failures.add("timed out at a spent limit: " + name);
        }
    }

    private static void runaway(String regex) {
        rows++;
        String value = "a".repeat(40) + "!";
        long start = System.nanoTime();
        try {
            RegexTimeout.matches(value, regex);
            failures.add("finished: " + regex);
        } catch (TimeoutException e) {
            long elapsed = (System.nanoTime() - start) / 1_000_000;
            if (elapsed < 500 || elapsed > RUNAWAY_CEILING_MILLIS) failures.add("stopped after " + elapsed + " ms: " + regex);
        }
    }
}
