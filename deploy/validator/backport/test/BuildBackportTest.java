import java.io.ByteArrayOutputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.LocalDateTime;
import java.util.Arrays;
import java.util.List;
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
            System.out.println("PASS packaging checks=" + checks);
        } finally {
            try (var files = Files.walk(dir)) { for (Path p : files.sorted(java.util.Comparator.reverseOrder()).toList()) Files.delete(p); }
        }
    }
    private static void archive(Path file, String a, String b) throws Exception {
        try (ZipOutputStream out = new ZipOutputStream(Files.newOutputStream(file))) {
            for (String name : List.of(a, b)) {
                byte[] bytes = (name + " retained bytes").getBytes();
                ZipEntry entry = new ZipEntry(name);
                entry.setTimeLocal(LocalDateTime.of(2020, 2, 3, 4, 5, 6));
                entry.setComment("retained comment");
                entry.setExtra(new byte[]{(byte) 0xfe, (byte) 0xca, 2, 0, 1, 2});
                if (name.equals(a)) { CRC32 crc = new CRC32(); crc.update(bytes); entry.setMethod(ZipEntry.STORED); entry.setSize(bytes.length); entry.setCrc(crc.getValue()); }
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
