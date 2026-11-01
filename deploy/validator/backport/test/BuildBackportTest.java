import java.io.ByteArrayOutputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.LocalDateTime;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.zip.CRC32;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;
import java.util.zip.ZipOutputStream;

/** Deterministic archive preservation and refusal tests; no network or engine needed. */
public final class BuildBackportTest {
    private static int checks;
    public static void main(String[] args) throws Exception {
        Path dir = Files.createTempDirectory("backport-package-test-");
        try {
            Path original = dir.resolve("original.zip");
            archive(original, "target.jar", "untouched.txt");
            // ZIP creator/platform and Unix attributes are not exposed by ZipEntry.
            byte[] originalBytes = Files.readAllBytes(original);
            for (int i = 0; i < originalBytes.length - 46; i++) {
                if (originalBytes[i] == 0x50 && originalBytes[i+1] == 0x4b && originalBytes[i+2] == 1 && originalBytes[i+3] == 2) {
                    originalBytes[i+5] = 3;
                    originalBytes[i+38] = 16; // Preserve the observed DOS directory flag too.
                    originalBytes[i+40] = (byte) 0xa4;
                    originalBytes[i+41] = (byte) 0x81;
                }
            }
            Files.write(original, originalBytes);
            byte[] changed = "replacement bytes".getBytes(java.nio.charset.StandardCharsets.UTF_8);
            Path first = dir.resolve("first.zip"), second = dir.resolve("second.zip");
            BuildBackport.replace(original, first, "target.jar", changed, true);
            BuildBackport.replace(original, second, "target.jar", changed, true);
            check(Arrays.equals(Files.readAllBytes(first), Files.readAllBytes(second)), "repeat bytes differ");
            BuildBackport.verifyDelta(original, first, "target.jar", changed);
            check(Arrays.equals(centralMetadata(original, "untouched.txt"), centralMetadata(first, "untouched.txt")),
                    "raw unchanged creator/attributes/flags metadata changed");
            try (ZipFile before = new ZipFile(original.toFile()); ZipFile after = new ZipFile(first.toFile())) {
                check(after.stream().map(ZipEntry::getName).toList().equals(List.of("target.jar", "untouched.txt")), "entry order changed");
                ZipEntry old = before.getEntry("untouched.txt"), now = after.getEntry("untouched.txt");
                check(old.getMethod() == now.getMethod() && old.getTime() == now.getTime()
                        && old.getComment().equals(now.getComment()) && Arrays.equals(old.getExtra(), now.getExtra()), "unchanged metadata changed");
                check(after.getEntry("target.jar").getMethod() == ZipEntry.STORED, "nested jar not stored");
            }
            rejects(() -> BuildBackport.replace(original, dir.resolve("missing.zip"), "absent.jar", changed, true), "missing target");
            Path tampered = dir.resolve("tampered.zip");
            archive(tampered, "target.jar", "different.txt");
            rejects(() -> BuildBackport.verifyDelta(original, tampered, "target.jar", changed), "unrelated entry mutation");
            Path signed = dir.resolve("signed.jar");
            archive(signed, "target.class", "META-INF/KEY.SF");
            rejects(() -> BuildBackport.requireUnsigned(signed), "signed jar");
            Path duplicate = dir.resolve("duplicate.zip");
            archive(duplicate, "target.jar", "otherx.jar");
            byte[] raw = Files.readAllBytes(duplicate);
            byte[] from = "otherx.jar".getBytes(), to = "target.jar".getBytes();
            for (int i = 0; i <= raw.length - from.length; i++) {
                if (Arrays.equals(Arrays.copyOfRange(raw, i, i + from.length), from)) System.arraycopy(to, 0, raw, i, to.length);
            }
            Files.write(duplicate, raw);
            rejects(() -> BuildBackport.entries(duplicate), "duplicate entry");
            rejects(() -> BuildBackport.requireHash(original, "0".repeat(64)), "wrong input hash");
            rejects(() -> BuildBackport.requireClass(new byte[]{1, 2, 3}, "org/example/Target"), "invalid class");
            byte[] self = BuildBackportTest.class.getResourceAsStream("/BuildBackportTest.class").readAllBytes();
            BuildBackport.requireClass(self, "BuildBackportTest");
            rejects(() -> BuildBackport.requireClass(self, "unexpected/Class"), "wrong class identity");
            byte[] wrongMajor = self.clone(); wrongMajor[7] = 65;
            rejects(() -> BuildBackport.requireClass(wrongMajor, "BuildBackportTest"), "wrong class major");
            for (var tuple : BuildBackport.SOURCES.values()) {
                BuildBackport.requireTuple(tuple.platform(), tuple.child(), tuple.warHash(), tuple.size());
                rejects(() -> BuildBackport.requireTuple("linux/unsupported", tuple.child(), tuple.warHash(), tuple.size()), "unknown platform");
                rejects(() -> BuildBackport.requireTuple(tuple.platform(), "sha256:" + "0".repeat(64), tuple.warHash(), tuple.size()), "wrong child");
                rejects(() -> BuildBackport.requireTuple(tuple.platform(), tuple.child(), "0".repeat(64), tuple.size()), "tampered input");
                rejects(() -> BuildBackport.requireTuple(tuple.platform(), tuple.child(), tuple.warHash(), tuple.size()+1), "wrong size");
                var swapped = BuildBackport.SOURCES.values().stream().filter(t -> !t.platform().equals(tuple.platform())).findFirst().orElseThrow();
                rejects(() -> BuildBackport.requireTuple(tuple.platform(), tuple.child(), swapped.warHash(), swapped.size()), "swapped WAR");
                rejects(() -> BuildBackport.requireTuple("", tuple.child(), tuple.warHash(), tuple.size()), "missing tuple");
            }
            rewrites(dir);
            pins(dir, Path.of(args[0]), Path.of(args[1]));
            System.out.println("PASS packaging checks=" + checks);
        } finally {
            try (var files = Files.walk(dir)) { for (Path p : files.sorted(java.util.Comparator.reverseOrder()).toList()) Files.delete(p); }
        }
    }
    private static void rewrites(Path dir) throws Exception {
        Path original = dir.resolve("set.zip");
        archive(original, "kept.txt", "replaced.txt", "removed.txt", "last.txt");
        byte[] replacement = "new bytes".getBytes(java.nio.charset.StandardCharsets.UTF_8), addition = new byte[4096];
        var delta = new BuildBackport.Delta(Map.of("replaced.txt", replacement),
                new TreeMap<>(Map.of("z.class", addition, "a.class", replacement)), Set.of("removed.txt"));
        Path first = dir.resolve("set-first.zip"), second = dir.resolve("set-second.zip");
        BuildBackport.rewrite(original, first, delta, false);
        BuildBackport.rewrite(original, second, delta, false);
        check(Arrays.equals(Files.readAllBytes(first), Files.readAllBytes(second)), "repeat rewrite bytes differ");
        BuildBackport.verifyDelta(original, first, delta);
        try (ZipFile after = new ZipFile(first.toFile())) {
            check(after.stream().map(ZipEntry::getName).toList().equals(List.of("kept.txt", "replaced.txt", "last.txt", "a.class", "z.class")), "entry order after a rewrite");
            check(after.size() == 5, "entry count after a rewrite");
            check(Arrays.equals(after.getInputStream(after.getEntry("z.class")).readAllBytes(), addition), "added bytes");
            check(after.getEntry("z.class").getMethod() == ZipEntry.DEFLATED, "added entry not deflated");
        }
        check(Arrays.equals(centralMetadata(original, "last.txt"), centralMetadata(first, "last.txt")), "unchanged entry metadata changed in a rewrite");
        BuildBackport.requireUnsigned(first);
        rejects(() -> BuildBackport.rewrite(original, dir.resolve("r1.zip"), new BuildBackport.Delta(Map.of("absent.txt", replacement), Map.of(), Set.of()), false), "missing replacement");
        rejects(() -> BuildBackport.rewrite(original, dir.resolve("r2.zip"), new BuildBackport.Delta(Map.of(), Map.of(), Set.of("absent.txt")), false), "missing removal");
        rejects(() -> BuildBackport.rewrite(original, dir.resolve("r3.zip"), new BuildBackport.Delta(Map.of(), Map.of("kept.txt", addition), Set.of()), false), "addition over an entry");
        rejects(() -> BuildBackport.verifyDelta(original, first, new BuildBackport.Delta(delta.replaced(), delta.added(), Set.of())), "an unexpected removal");
        rejects(() -> BuildBackport.verifyDelta(original, first, new BuildBackport.Delta(delta.replaced(), Map.of("a.class", replacement), delta.removed())), "an unexpected addition");
        rejects(() -> BuildBackport.verifyDelta(original, first, new BuildBackport.Delta(delta.replaced(), Map.of("a.class", replacement, "z.class", replacement), delta.removed())), "tampered addition");
        rejects(() -> BuildBackport.verifyDelta(original, first, new BuildBackport.Delta(Map.of("replaced.txt", addition), delta.added(), delta.removed())), "tampered replacement");
        Map<String, byte[]> outputs = new TreeMap<>();
        for (String name : BuildBackport.REGEX_OUTPUT_CLASSES) outputs.put(name, replacement);
        var regex = BuildBackport.regexDelta(outputs);
        check(regex.replaced().keySet().equals(Set.of(BuildBackport.REGEX_PACKAGE + "RegexTimeout.class")), "regex replacement set");
        check(regex.removed().equals(Set.of(BuildBackport.REGEX_PACKAGE + "RegexTimeout$InterruptibleCharSequence.class")), "regex removal set");
        check(regex.added().size() == 4, "regex addition set");
    }
    /** A mutated pin, or mutated bytes under the true pin, fails: a HAPI bump stops here until someone looks. */
    private static void pins(Path dir, Path utilities, Path source) throws Exception {
        String wrong = "0".repeat(64);
        BuildBackport.requireRegexInputs(utilities, BuildBackport.UTILITIES_JAR_SHA, BuildBackport.REGEX_INPUT_CLASSES);
        rejects(() -> BuildBackport.requireRegexInputs(utilities, wrong, BuildBackport.REGEX_INPUT_CLASSES), "mutated jar pin");
        for (String name : BuildBackport.REGEX_INPUT_CLASSES.keySet()) {
            Map<String, String> mutated = new TreeMap<>(BuildBackport.REGEX_INPUT_CLASSES);
            mutated.put(name, wrong);
            rejects(() -> BuildBackport.requireRegexInputs(utilities, BuildBackport.UTILITIES_JAR_SHA, mutated), "mutated class pin");
        }
        Path tampered = dir.resolve("tampered-utilities.jar");
        BuildBackport.replace(utilities, tampered, BuildBackport.REGEX_INPUT_CLASSES.keySet().iterator().next(), new byte[]{1}, false);
        rejects(() -> BuildBackport.requireRegexInputs(tampered, BuildBackport.UTILITIES_JAR_SHA, BuildBackport.REGEX_INPUT_CLASSES), "mutated jar bytes");
        rejects(() -> BuildBackport.requireRegexInputs(tampered, BuildBackport.hash(tampered), BuildBackport.REGEX_INPUT_CLASSES), "mutated class bytes under a matching jar pin");
        BuildBackport.requireRegexSources(source, BuildBackport.REGEX_SOURCES);
        for (var pin : BuildBackport.REGEX_SOURCES.entrySet()) {
            Map<String, String> mutated = new TreeMap<>(BuildBackport.REGEX_SOURCES);
            mutated.put(pin.getKey(), wrong);
            rejects(() -> BuildBackport.requireRegexSources(source, mutated), "mutated source pin");
            Path copy = dir.resolve("source-copy").resolve(pin.getKey());
            Files.createDirectories(copy.getParent());
            for (String name : BuildBackport.REGEX_SOURCES.keySet()) Files.copy(source.resolve(name), dir.resolve("source-copy").resolve(name), java.nio.file.StandardCopyOption.REPLACE_EXISTING);
            Files.writeString(copy, "// changed\n", java.nio.file.StandardOpenOption.APPEND);
            rejects(() -> BuildBackport.requireRegexSources(dir.resolve("source-copy"), BuildBackport.REGEX_SOURCES), "mutated source bytes");
        }
        Path libs = Files.createDirectory(dir.resolve("libs"));
        String guard = BuildBackport.REGEX_PACKAGE + "RegexTimeout.class";
        rejects(() -> BuildBackport.requireOneRegexGuard(libs), "no regex guard on the classpath");
        archive(libs.resolve("org.hl7.fhir.utilities-6.9.12.jar"), guard, "other.txt");
        BuildBackport.requireOneRegexGuard(libs);
        archive(libs.resolve("shaded.jar"), guard, "other.txt");
        rejects(() -> BuildBackport.requireOneRegexGuard(libs), "a second regex guard on the classpath");
    }
    private static void archive(Path file, String... names) throws Exception {
        try (ZipOutputStream out = new ZipOutputStream(Files.newOutputStream(file))) {
            for (String name : names) {
                byte[] bytes = (name + " retained bytes").getBytes();
                ZipEntry entry = new ZipEntry(name);
                entry.setTimeLocal(LocalDateTime.of(2020, 2, 3, 4, 5, 6));
                entry.setComment("retained comment");
                entry.setExtra(new byte[]{(byte) 0xfe, (byte) 0xca, 2, 0, 1, 2});
                if (name.equals(names[0])) { CRC32 crc = new CRC32(); crc.update(bytes); entry.setMethod(ZipEntry.STORED); entry.setSize(bytes.length); entry.setCrc(crc.getValue()); }
                out.putNextEntry(entry); out.write(bytes); out.closeEntry();
            }
        }
    }
    private static byte[] centralMetadata(Path path, String name) throws Exception {
        byte[] bytes = Files.readAllBytes(path);
        for (int i = 0; i < bytes.length - 46; i++) {
            if (bytes[i] == 0x50 && bytes[i+1] == 0x4b && bytes[i+2] == 1 && bytes[i+3] == 2) {
                int length = (bytes[i+28] & 255) | ((bytes[i+29] & 255) << 8);
                if (new String(bytes, i+46, length, java.nio.charset.StandardCharsets.UTF_8).equals(name)) {
                    byte[] header = Arrays.copyOfRange(bytes, i, i + 46 + length);
                    Arrays.fill(header, 42, 46, (byte) 0);
                    return header;
                }
            }
        }
        throw new AssertionError("no central metadata");
    }
    private static void check(boolean condition, String reason) { checks++; if (!condition) throw new AssertionError(reason); }
    private static void rejects(Checked action, String reason) throws Exception {
        checks++;
        try { action.run(); } catch (IllegalArgumentException | java.io.IOException e) { return; }
        throw new AssertionError("accepted " + reason);
    }
    private interface Checked { void run() throws Exception; }
}
