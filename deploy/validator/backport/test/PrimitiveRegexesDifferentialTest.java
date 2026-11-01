package org.hl7.fhir.utilities.regex;

import java.util.List;
import java.util.Random;
import java.util.regex.Pattern;

/** On this build's Java, each hand matcher answers as java.util.regex does for the regex it stands in for. */
public final class PrimitiveRegexesDifferentialTest {
    private static final List<String> TABLE = List.of(
            "^[\\s\\S]+$", "[ \\r\\n\\t\\S]+", "\\S*", "[A-Za-z0-9\\-\\.]{1,64}", "true|false", "[1-9][0-9]*",
            "[0]|([1-9][0-9]*)", "-?([0]|([1-9][0-9]*))", "[0]|[-+]?[1-9][0-9]*", "[^\\s]+(\\s[^\\s]+)*", "[^\\s]+( [^\\s]+)*",
            "urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", "urn:oid:[0-2](\\.(0|[1-9][0-9]*))+");
    // Each class of character a matcher separates: word, digit, sign, separator, every ASCII
    // whitespace regex treats as \s, a Unicode space it does not, and half a surrogate pair.
    private static final char[] ALPHABET = {'a', 'Z', '0', '1', '9', 'f', '-', '+', '.', ':', ' ', '\t', '\n', '\r', '\u000B', '\f', ' ', '\u0085', ' ', '\uD83D'};
    // One step either side of each range a matcher checks.
    private static final char[] BOUNDARIES = "/09:@AZ[`afgz{123-. ".toCharArray();
    // Each value one character from these: an exhaustive run this short never gets past a prefix.
    private static final String[] VALID = {"urn:oid:0.0", "urn:oid:1.0", "urn:oid:2.16.840.1.10", "urn:uuid:c757873d-ec9a-4326-a141-556f43239520",
            "urn:uuid:01234567-89ab-cdef-0123-456789abcdef", "true", "false", "-10", "+10", "109", "a b", "a\tb", "Az09-.z"};
    private static final String[] FRAGMENTS = {"true", "false", "urn:oid:", "urn:oid:1", ".0", ".1", ".12", ".01", "urn:uuid:",
            "c757873d-ec9a-4326-a141-556f43239520", "c757873d", "-ec9a", "0", "1", "12", "-", "+", " ", "\t", "\n", "\r", "\u000B", "\f", " ", "a", "Z",
            "http://example.org/", "a".repeat(64), "a".repeat(65), "😀"};
    private static final int RANDOM_INPUTS = 200_000, EXHAUSTIVE_LENGTH = 4;
    private static long compared;

    public static void main(String[] args) throws Exception {
        for (String regex : TABLE) {
            if (PrimitiveRegexes.matchesRegex(regex, "") == null) throw new AssertionError("no hand matcher for " + regex);
            Pattern pattern = Pattern.compile(regex);
            exhaustive(pattern, regex, ALPHABET, new char[EXHAUSTIVE_LENGTH], 0);
            exhaustive(pattern, regex, BOUNDARIES, new char[EXHAUSTIVE_LENGTH], 0);
            for (String valid : VALID) oneAway(pattern, regex, valid);
            Random random = new Random(3230L * regex.hashCode());
            for (int i = 0; i < RANDOM_INPUTS; i++) compare(pattern, regex, generated(random));
        }
        if (PrimitiveRegexes.matchesRegex("[\\d]{4}", "1234") != null) throw new AssertionError("a regex outside the table must reach the engine");
        operations();
        longValues();
        System.out.println("PASS differential regexes=" + TABLE.size() + " compared=" + compared);
    }

    private static void exhaustive(Pattern pattern, String regex, char[] alphabet, char[] value, int length) {
        compare(pattern, regex, new String(value, 0, length));
        if (length == value.length) return;
        for (char c : alphabet) { value[length] = c; exhaustive(pattern, regex, alphabet, value, length + 1); }
    }

    private static void oneAway(Pattern pattern, String regex, String valid) {
        compare(pattern, regex, valid);
        for (int i = 0; i < valid.length(); i++) {
            compare(pattern, regex, valid.substring(0, i) + valid.substring(i + 1));
            for (char c : BOUNDARIES) {
                compare(pattern, regex, valid.substring(0, i) + c + valid.substring(i + 1));
                compare(pattern, regex, valid.substring(0, i) + c + valid.substring(i));
            }
        }
    }

    private static String generated(Random random) {
        StringBuilder value = new StringBuilder();
        for (int parts = random.nextInt(6); parts >= 0; parts--) value.append(FRAGMENTS[random.nextInt(FRAGMENTS.length)]);
        if (random.nextBoolean()) value.setCharAt(random.nextInt(value.length()), ALPHABET[random.nextInt(ALPHABET.length)]);
        return value.toString();
    }

    private static void compare(Pattern pattern, String regex, String value) {
        compared++;
        if (pattern.matcher(value).matches() != PrimitiveRegexes.matchesRegex(regex, value)) throw new AssertionError("differs on " + regex + ": " + value);
    }

    /** Off the table the operations answer as String's do; only their guard changed. */
    private static void operations() throws Exception {
        String value = "a1 b2  c3";
        require(RegexTimeout.find(value, "[0-9] ") == Pattern.compile("[0-9] ").matcher(value).find(), "find");
        require(value.replaceAll("[0-9]", "#").equals(RegexTimeout.replaceAll(value, "[0-9]", "#")), "replaceAll");
        require(value.replaceFirst("[0-9]", "#").equals(RegexTimeout.replaceFirst(value, "[0-9]", "#")), "replaceFirst");
        require(List.of(value.split(" +")).equals(List.of(RegexTimeout.split(value, " +"))), "split");
        require(List.of(value.split(" ", 2)).equals(List.of(RegexTimeout.split(value, " ", 2))), "split with a limit");
    }

    /** These overflowed the regex engine's stack before the table: the guard itself answers them now. */
    private static void longValues() throws Exception {
        StringBuilder oid = new StringBuilder("urn:oid:1");
        for (int i = 0; i < 5000; i++) oid.append(".1");
        require(RegexTimeout.matches(oid, "urn:oid:[0-2](\\.(0|[1-9][0-9]*))+"), "5,000-part oid");
        require(RegexTimeout.matches("w" + " w".repeat(5000), "[^\\s]+(\\s[^\\s]+)*"), "5,000-word code");
        require(RegexTimeout.matches("x".repeat(1 << 20), "\\S*"), "1 MB uri");
    }

    private static void require(boolean condition, String reason) { if (!condition) throw new AssertionError(reason); }
}
