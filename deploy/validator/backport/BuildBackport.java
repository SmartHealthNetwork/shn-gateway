import java.io.ByteArrayInputStream;
import java.io.DataInputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.time.LocalDateTime;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Comparator;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TimeZone;
import java.util.TreeMap;
import java.util.zip.CRC32;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;
import java.util.zip.ZipOutputStream;

/** Replaces one verified class in one verified nested JAR without runtime overlays. */
public final class BuildBackport {
    static final String JAR = "WEB-INF/lib/hapi-fhir-validation-8.10.0.jar";
    static final String CLASS = "org/hl7/fhir/common/hapi/validation/validator/ValidatorWrapper";
    static final String ARM_WAR_SHA = "94a8a650d470060025ef8c1c404b5451a9b91efa860a118e69bbdcda3c8221e4";
    static final String JAR_SHA = "cf4d24f810bcd29760296fc5f9f5908196e4253755d8194afd610dc456009deb";
    static final String CLASS_SHA = "b7055a835c7fdae7b3e6834d63743b912394fa631feda3bde015c3f8ed1edf0c";
    static final String SOURCE_SHA = "5050c7693096e31a9f26c9fbd68c60780bd16db7fc51b404a2095e9b95568be0";
    static final String COMPILER = "eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7";
    static final Map<String, String> PLATFORMS = Map.of(
            "aarch64", "sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3",
            "amd64", "sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892");

    record SourceTuple(String platform, String child, String warHash, long size) {}
    static final Map<String, SourceTuple> SOURCES = Map.of(
        "linux/arm64", new SourceTuple("linux/arm64", "sha256:ac871f44ee311bdd4d3533f7ea9e81b3a744ca709726b74d5a97d29663a7916c", ARM_WAR_SHA, 378563064),
        "linux/amd64", new SourceTuple("linux/amd64", "sha256:f4aa83b7102a52f19a42c4c27c5f63fa3d1ded4f393c7083e53a39dc37b2a4c5", "378be32e09f643db6c01c703851e5a639d9ea17ebf7a0aafeeefaa66a0452009", 378563063));
    static SourceTuple requireTuple(String platform, String child, String hash, long size) throws IOException {
        SourceTuple tuple = SOURCES.get(platform);
        if (tuple == null || !tuple.child().equals(child) || !tuple.warHash().equals(hash) || tuple.size() != size)
            throw new IOException("unsupported or mismatched HAPI source tuple");
        return tuple;
    }
    public static void main(String[] args) throws Exception {
        TimeZone.setDefault(TimeZone.getTimeZone("UTC"));
        if (args.length != 7 || !args[0].equals("build")) throw new IllegalArgumentException("build SOURCE_DIR ORIGINAL_WAR OUTPUT_DIR HAPI_PLATFORM HAPI_CHILD COMPILER_CHILD");
        build(Path.of(args[1]), Path.of(args[2]), Path.of(args[3]), args[4], args[5], args[6]);
    }

    static void build(Path source, Path war, Path output, String sourcePlatform, String sourceChild, String compilerChild) throws Exception {
        if (!"Eclipse Adoptium".equals(System.getProperty("java.vendor"))
                || !"21.0.11+10-LTS".equals(System.getProperty("java.runtime.version"))
                || !"Linux".equals(System.getProperty("os.name"))
                || !PLATFORMS.containsKey(System.getProperty("os.arch"))) {
            throw new IllegalArgumentException("requires the pinned Linux Temurin compiler");
        }
        if (Files.exists(output)) throw new IllegalArgumentException("output directory must be fresh");
        if (!PLATFORMS.get(System.getProperty("os.arch")).equals(compilerChild)) throw new IOException("compiler child/platform mismatch");
        SourceTuple inputTuple = requireTuple(sourcePlatform, sourceChild, hash(war), Files.size(war));
        requireHash(source.resolve("upstream/ValidatorWrapper.java"), SOURCE_SHA);
        String manifest = Files.readString(source.resolve("provenance.json"));
        requireHash(source.resolve("src/ValidatorWrapper.java"), stringField(manifest, "patched_source_sha256"));
        requireHash(source.resolve("LICENSE.txt"), stringField(manifest, "license_sha256"));
        if (!SOURCE_SHA.equals(stringField(manifest, "upstream_source_sha256"))
                || !COMPILER.equals(stringField(manifest, "compiler_image"))) throw new IOException("source provenance mismatch");
        Map<String, ZipEntry> members = entries(war);
        ZipEntry jarMember = members.get(JAR);
        if (jarMember == null || jarMember.getMethod() != ZipEntry.STORED) throw new IOException("expected stored validation JAR");
        Path work = Files.createTempDirectory("validator-backport-");
        try {
            Path libs = Files.createDirectory(work.resolve("libs"));
            List<String> classpath = new ArrayList<>();
            try (ZipFile input = new ZipFile(war.toFile())) {
                for (String name : members.keySet().stream().filter(n -> n.endsWith(".jar")).sorted().toList()) {
                    if (!(name.startsWith("WEB-INF/lib/") || name.startsWith("WEB-INF/lib-provided/"))) throw new IOException("unexpected compile library " + name);
                    String base = Path.of(name).getFileName().toString();
                    Path target = libs.resolve(base);
                    if (Files.exists(target)) throw new IOException("duplicate compile library " + base);
                    byte[] bytes = input.getInputStream(input.getEntry(name)).readAllBytes();
                    Files.write(target, bytes);
                    classpath.add(name + "\t" + hash(bytes));
                }
            }
            Path originalJar = libs.resolve(Path.of(JAR).getFileName());
            requireHash(originalJar, JAR_SHA);
            requireUnsigned(originalJar);
            entries(originalJar);
            byte[] originalClass = member(originalJar, CLASS + ".class");
            requireHash(originalClass, CLASS_SHA);
            requireClass(originalClass, CLASS);
            Path original = Files.createDirectory(work.resolve("original"));
            Path patched = Files.createDirectory(work.resolve("patched"));
            Path tests = Files.createDirectory(work.resolve("tests"));
            Path logs = Files.createDirectory(work.resolve("logs"));
            String cp;
            try (var stream = Files.list(libs)) { cp = String.join(java.io.File.pathSeparator, stream.sorted().map(Path::toString).toList()); }
            compile(source.resolve("upstream/ValidatorWrapper.java"), original, cp, logs.resolve("compile-original.log"));
            compile(source.resolve("src/ValidatorWrapper.java"), patched, cp, logs.resolve("compile-patched.log"));
            List<Path> testSources;
            try (var stream = Files.list(source.resolve("test"))) { testSources = stream.filter(p -> p.toString().endsWith(".java")).sorted().toList(); }
            List<String> javac = new ArrayList<>(List.of("javac", "--release", "17", "-g", "-encoding", "UTF-8", "-proc:none", "-classpath", cp,
                    "-d", tests.toString(), source.resolve("BuildBackport.java").toString()));
            javac.addAll(testSources.stream().map(Path::toString).toList());
            requireExit(run(javac, logs.resolve("compile-tests.log")), 0, "compile tests");
            Path originalDisassembly = logs.resolve("original-instructions.txt"), rebuiltDisassembly = logs.resolve("rebuilt-instructions.txt");
            requireExit(run(List.of("javap", "-c", "-p", "-classpath", originalJar.toString(), CLASS.replace('/', '.')), originalDisassembly), 0, "original javap");
            requireExit(run(List.of("javap", "-c", "-p", "-classpath", original.toString(), CLASS.replace('/', '.')), rebuiltDisassembly), 0, "rebuilt javap");
            if (!Arrays.equals(Files.readAllBytes(originalDisassembly), Files.readAllBytes(rebuiltDisassembly))) throw new IOException("upstream recompilation instruction drift");
            String originalIdentity = "-Dbackport.expectedClass=" + CLASS_SHA;
            String patchedIdentity = "-Dbackport.expectedClass=" + hash(Files.readAllBytes(patched.resolve(CLASS + ".class")));
            String testClass = "org.hl7.fhir.common.hapi.validation.validator.ExplicitProfileTest";
            requireExit(run(List.of("java", "-Xmx1g", originalIdentity, "-cp", tests + ":" + cp, testClass), logs.resolve("original-red.log")), 1, "original regression");
            if (!Files.readString(logs.resolve("original-red.log")).contains("ROWS=56 FAILURES=36")) throw new IOException("unexpected original regression failures");
            String patchedCP = patched + ":" + tests + ":" + cp;
            requireExit(run(List.of("java", "-Xmx1g", patchedIdentity, "-cp", patchedCP, testClass), logs.resolve("patched-green.log")), 0, "patched regression");
            requireExit(run(List.of("java", "-Xmx1g", patchedIdentity, "-cp", patchedCP,
                    "org.hl7.fhir.common.hapi.validation.validator.OperationBoundaryTest", logs.resolve("operation").toString()), logs.resolve("operation.log")), 0, "operation boundary");
            requireExit(run(List.of("java", "-cp", tests.toString(), "BuildBackportTest"), logs.resolve("packaging.log")), 0, "packaging tests");
            String characterization = "org.hl7.fhir.common.hapi.validation.validator.ProfileCharacterizationTest";
            Path before = logs.resolve("characterization-original.txt"), after = logs.resolve("characterization-patched.txt");
            requireExit(run(List.of("java", "-Xmx1g", "-cp", tests + ":" + cp, characterization, before.toString()), logs.resolve("characterization-original.log")), 0, "original characterization");
            requireExit(run(List.of("java", "-Xmx1g", "-cp", patchedCP, characterization, after.toString()), logs.resolve("characterization-patched.log")), 0, "patched characterization");
            if (!Arrays.equals(Files.readAllBytes(before), Files.readAllBytes(after))) throw new IOException("existing validation behavior changed");
            List<Path> produced;
            try (var stream = Files.walk(patched)) { produced = stream.filter(Files::isRegularFile).toList(); }
            if (produced.size() != 1 || !produced.get(0).equals(patched.resolve(CLASS + ".class"))) throw new IOException("unexpected additional compiled classes");
            byte[] patchedClass = Files.readAllBytes(produced.get(0));
            requireClass(patchedClass, CLASS);
            Path newJar = work.resolve("patched.jar"), newWar = work.resolve("main.war");
            replace(originalJar, newJar, CLASS + ".class", patchedClass, false);
            verifyDelta(originalJar, newJar, CLASS + ".class", patchedClass);
            byte[] jarBytes = Files.readAllBytes(newJar);
            replace(war, newWar, JAR, jarBytes, true);
            verifyDelta(war, newWar, JAR, jarBytes);
            Map<String, String> provenance = new TreeMap<>();
            provenance.put("format", "explicit-profile-backport-v1");
            provenance.put("upstream_image", "hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1");
            provenance.put("hapi_source_platform", inputTuple.platform()); provenance.put("hapi_source_child", inputTuple.child());
            provenance.put("input_war_size", Long.toString(inputTuple.size()));
            provenance.put("input_war_sha256", inputTuple.warHash()); provenance.put("input_jar_sha256", JAR_SHA);
            provenance.put("input_class_sha256", CLASS_SHA); provenance.put("upstream_source_sha256", SOURCE_SHA);
            provenance.put("patched_source_sha256", hash(Files.readAllBytes(source.resolve("src/ValidatorWrapper.java"))));
            provenance.put("compiler_image", COMPILER); provenance.put("compiler_platform", "linux/" + (System.getProperty("os.arch").equals("aarch64") ? "arm64" : "amd64"));
            provenance.put("compiler_platform_digest", PLATFORMS.get(System.getProperty("os.arch")));
            provenance.put("compiler_runtime", System.getProperty("java.runtime.version"));
            provenance.put("builder_sha256", hash(Files.readAllBytes(source.resolve("BuildBackport.java"))));
            provenance.put("source_manifest_sha256", hash(Files.readAllBytes(source.resolve("provenance.json"))));
            provenance.put("classpath_sha256", hash((String.join("\n", classpath) + "\n").getBytes(StandardCharsets.UTF_8)));
            provenance.put("java_class_major", "61"); provenance.put("changed_war_entry", JAR); provenance.put("changed_jar_entry", CLASS + ".class");
            provenance.put("output_class_sha256", hash(patchedClass)); provenance.put("output_jar_sha256", hash(jarBytes));
            provenance.put("output_war_sha256", hash(newWar));
            Map<String, String> key = new TreeMap<>(provenance);
            key.keySet().removeIf(k -> k.startsWith("output_"));
            provenance.put("build_key", hash(json(key).getBytes(StandardCharsets.UTF_8)));
            Files.createDirectory(output);
            Files.copy(newWar, output.resolve("main.war")); Files.copy(newJar, output.resolve("hapi-fhir-validation-8.10.0.jar"));
            Files.writeString(output.resolve("provenance.json"), json(provenance));
            Files.writeString(output.resolve("classpath.sha256"), String.join("\n", classpath) + "\n");
            copyTree(logs, output.resolve("evidence"));
            Files.writeString(output.resolve("complete"), provenance.get("build_key") + "\n");
            System.out.println(json(provenance));
        } finally { deleteTree(work); }
    }

    static void compile(Path source, Path destination, String cp, Path log) throws Exception {
        requireExit(run(List.of("javac", "--release", "17", "-g", "-encoding", "UTF-8", "-proc:none", "-classpath", cp,
                "-d", destination.toString(), source.toString()), log), 0, "compile " + source.getFileName());
    }
    static int run(List<String> command, Path log) throws Exception {
        Files.writeString(Path.of(log + ".command"), String.join("\n", command) + "\n");
        Process p = new ProcessBuilder(command).redirectErrorStream(true).redirectOutput(log.toFile()).start();
        if (!p.waitFor(180, java.util.concurrent.TimeUnit.SECONDS)) { p.destroyForcibly(); p.waitFor(); throw new IOException("command timeout " + command.get(0)); }
        int code = p.exitValue(); Files.writeString(Path.of(log + ".exit"), code + "\n"); return code;
    }
    static void requireExit(int actual, int expected, String step) throws IOException { if (actual != expected) throw new IOException(step + " exit " + actual + " expected " + expected); }
    static String hash(byte[] bytes) throws Exception { return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(bytes)); }
    static String hash(Path path) throws Exception {
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        try (var stream = Files.newInputStream(path)) { byte[] chunk = new byte[65536]; int n; while ((n = stream.read(chunk)) != -1) digest.update(chunk, 0, n); }
        return HexFormat.of().formatHex(digest.digest());
    }
    static void requireHash(Path path, String expected) throws Exception { if (!hash(path).equals(expected)) throw new IOException("SHA256 mismatch expected " + expected); }
    static void requireHash(byte[] bytes, String expected) throws Exception { if (!hash(bytes).equals(expected)) throw new IOException("SHA256 mismatch expected " + expected); }
    static Map<String, ZipEntry> entries(Path path) throws IOException {
        Map<String, ZipEntry> result = new LinkedHashMap<>();
        try (ZipFile zip = new ZipFile(path.toFile())) {
            var all = zip.entries();
            while (all.hasMoreElements()) {
                ZipEntry e = all.nextElement();
                if (result.putIfAbsent(e.getName(), e) != null) throw new IOException("duplicate ZIP entry " + e.getName());
            }
        }
        return result;
    }
    static byte[] member(Path zipPath, String name) throws IOException {
        try (ZipFile zip = new ZipFile(zipPath.toFile())) {
            ZipEntry e = zip.getEntry(name); if (e == null) throw new IOException("missing ZIP entry " + name);
            return zip.getInputStream(e).readAllBytes();
        }
    }
    static void requireUnsigned(Path jar) throws IOException {
        for (String name : entries(jar).keySet()) if (name.matches("(?i)META-INF/[^/]+\\.(SF|RSA|DSA|EC)")) throw new IOException("signed JAR " + name);
    }
    static void requireClass(byte[] bytes, String expected) throws IOException {
        try (DataInputStream in = new DataInputStream(new ByteArrayInputStream(bytes))) {
            if (in.readInt() != 0xcafebabe || in.readUnsignedShort() != 0 || in.readUnsignedShort() != 61) throw new IOException("wrong class magic/version");
            int count = in.readUnsignedShort(); Object[] pool = new Object[count];
            for (int i = 1; i < count; i++) {
                int tag = in.readUnsignedByte();
                switch (tag) {
                    case 1 -> pool[i] = in.readUTF();
                    case 7 -> pool[i] = in.readUnsignedShort();
                    case 3, 4, 9, 10, 11, 12, 17, 18 -> in.skipNBytes(4);
                    case 5, 6 -> { in.skipNBytes(8); i++; }
                    case 8, 16, 19, 20 -> in.skipNBytes(2);
                    case 15 -> in.skipNBytes(3);
                    default -> throw new IOException("invalid constant pool tag " + tag);
                }
            }
            in.readUnsignedShort(); int self = in.readUnsignedShort();
            if (self <= 0 || self >= count || !(pool[self] instanceof Integer name) || name <= 0 || name >= count
                    || !expected.equals(pool[name])) throw new IOException("unexpected class identity");
        }
    }
    static void replace(Path input, Path output, String target, byte[] bytes, boolean stored) throws IOException {
        Map<String, ZipEntry> members = entries(input);
        ZipEntry old = members.get(target);
        if (old == null) throw new IOException("missing replacement target " + target);
        if (stored && old.getMethod() != ZipEntry.STORED) throw new IOException("nested JAR must already be stored");
        Path single = Files.createTempFile("replacement-entry-", ".zip");
        try {
            // Only the changed member is serialized. Every other local record and
            // central header is copied raw, retaining metadata ZipEntry cannot expose.
            try (ZipOutputStream zip = new ZipOutputStream(Files.newOutputStream(single))) {
                ZipEntry changed = new ZipEntry(target);
                changed.setTimeLocal(LocalDateTime.of(2000, 1, 1, 0, 0));
                changed.setMethod(old.getMethod());
                CRC32 crc = new CRC32(); crc.update(bytes);
                changed.setSize(bytes.length); changed.setCrc(crc.getValue());
                zip.putNextEntry(changed); zip.write(bytes); zip.closeEntry();
            }
            ZipLayout layout = layout(input), replacement = layout(single);
            Map<String, Long> positions = new LinkedHashMap<>();
            List<String> localOrder = layout.headers().keySet().stream()
                    .sorted(Comparator.comparingLong(n -> u32(layout.headers().get(n),42))).toList();
            try (var from = java.nio.channels.FileChannel.open(input);
                    var one = java.nio.channels.FileChannel.open(single);
                    var out = java.nio.channels.FileChannel.open(output, java.nio.file.StandardOpenOption.CREATE_NEW,
                            java.nio.file.StandardOpenOption.WRITE)) {
                long first = u32(layout.headers().get(localOrder.get(0)),42);
                copyRange(from,out,0,first);
                for (int i = 0; i < localOrder.size(); i++) {
                    String name = localOrder.get(i);
                    long begin = u32(layout.headers().get(name),42);
                    long finish = i+1 == localOrder.size() ? layout.offset() : u32(layout.headers().get(localOrder.get(i+1)),42);
                    if (finish <= begin) throw new IOException("overlapping local ZIP records");
                    positions.put(name,out.position());
                    if (name.equals(target)) copyRange(one,out,0,replacement.offset());
                    else copyRange(from,out,begin,finish-begin);
                }
                long directory = out.position();
                for (String name : layout.headers().keySet()) {
                    byte[] header = (name.equals(target) ? replacement.headers().get(target) : layout.headers().get(name)).clone();
                    putU32(header,42,positions.get(name));
                    write(out,header);
                }
                byte[] ending = layout.ending().clone();
                putU32(ending,12,out.position()-directory); putU32(ending,16,directory);
                write(out,ending);
            }
        } finally { Files.delete(single); }
    }
    static void write(java.nio.channels.FileChannel out, byte[] bytes) throws IOException {
        var buffer = java.nio.ByteBuffer.wrap(bytes); while (buffer.hasRemaining()) out.write(buffer);
    }
    static void copyRange(java.nio.channels.FileChannel from, java.nio.channels.FileChannel to, long offset, long length) throws IOException {
        long copied = 0;
        while (copied < length) { long n = from.transferTo(offset+copied,length-copied,to); if (n <= 0) throw new IOException("short ZIP record copy"); copied += n; }
    }
    static void putU32(byte[] bytes, int offset, long value) throws IOException {
        if (value < 0 || value >= 0xffffffffL) throw new IOException("ZIP64 output unsupported");
        for (int i=0;i<4;i++) bytes[offset+i]=(byte)(value >>> (8*i));
    }
    static void verifyDelta(Path original, Path patched, String target, byte[] replacement) throws IOException {
        Map<String, ZipEntry> before = entries(original), after = entries(patched);
        if (!new ArrayList<>(before.keySet()).equals(new ArrayList<>(after.keySet()))) throw new IOException("ZIP entry set/order changed");
        Map<String, byte[]> oldHeaders = centralMetadata(original), newHeaders = centralMetadata(patched);
        for (String name : before.keySet()) if (!name.equals(target) && !Arrays.equals(oldHeaders.get(name), newHeaders.get(name)))
            throw new IOException("raw central ZIP metadata changed " + name);
        try (ZipFile a = new ZipFile(original.toFile()); ZipFile b = new ZipFile(patched.toFile())) {
            if (!java.util.Objects.equals(a.getComment(), b.getComment())) throw new IOException("archive comment changed");
            for (String name : before.keySet()) {
                ZipEntry old = before.get(name), now = after.get(name);
                byte[] expected = name.equals(target) ? replacement : a.getInputStream(old).readAllBytes();
                if (!Arrays.equals(expected, b.getInputStream(now).readAllBytes())) throw new IOException("entry bytes changed " + name);
                if (old.getMethod() != now.getMethod()) throw new IOException("compression method changed " + name);
                if (!name.equals(target) && (old.getTime() != now.getTime()
                        || !java.util.Objects.equals(old.getComment(), now.getComment()) || !Arrays.equals(old.getExtra(), now.getExtra()))) {
                    throw new IOException("entry metadata changed " + name);
                }
            }
        }
    }
    record ZipLayout(Map<String, byte[]> headers, long offset, byte[] ending) {}
    static Map<String, byte[]> centralMetadata(Path archive) throws IOException {
        Map<String, byte[]> result = new LinkedHashMap<>();
        for (var entry : layout(archive).headers().entrySet()) {
            byte[] metadata = entry.getValue().clone();
            Arrays.fill(metadata,20,24,(byte)0); Arrays.fill(metadata,42,46,(byte)0);
            result.put(entry.getKey(),metadata);
        }
        return result;
    }
    static ZipLayout layout(Path archive) throws IOException {
        Map<String, byte[]> result = new LinkedHashMap<>();
        try (var file = new java.io.RandomAccessFile(archive.toFile(), "r")) {
            int tailSize = (int) Math.min(file.length(), 65557);
            byte[] tail = new byte[tailSize]; file.seek(file.length() - tailSize); file.readFully(tail);
            int end = -1;
            for (int i = tail.length - 22; i >= 0; i--) {
                if (u32(tail, i) == 0x06054b50L && i + 22 + u16(tail, i+20) == tail.length) { end = i; break; }
            }
            if (end < 0 || u16(tail,end+4) != 0 || u16(tail,end+6) != 0 || u16(tail,end+8) != u16(tail,end+10)
                    || u16(tail,end+10) == 0 || u16(tail,end+10) == 65535 || u32(tail,end+16) == 0xffffffffL) throw new IOException("unsupported ZIP directory");
            long directory = u32(tail,end+16), length = u32(tail,end+12);
            if (directory+length != file.length()-tailSize+end) throw new IOException("unexpected ZIP directory trailer");
            file.seek(directory);
            for (int i = 0; i < u16(tail,end+10); i++) {
                byte[] header = new byte[46]; file.readFully(header);
                if (u32(header,0) != 0x02014b50L || (u16(header,8)&1) != 0 || u16(header,34) != 0
                        || u32(header,20) == 0xffffffffL || u32(header,24) == 0xffffffffL || u32(header,42) >= directory)
                    throw new IOException("unsupported central ZIP header");
                int names = u16(header,28), extras = u16(header,30), comments = u16(header,32);
                byte[] rest = new byte[names + extras + comments]; file.readFully(rest);
                String name = new String(rest, 0, names, StandardCharsets.UTF_8);
                byte[] metadata = new byte[header.length + rest.length];
                System.arraycopy(header,0,metadata,0,header.length); System.arraycopy(rest,0,metadata,header.length,rest.length);
                if (result.putIfAbsent(name,metadata) != null) throw new IOException("duplicate central ZIP entry");
            }
            if (file.getFilePointer() != directory + length) throw new IOException("central ZIP length mismatch");
            return new ZipLayout(result,directory,Arrays.copyOfRange(tail,end,tail.length));
        }
    }
    static int u16(byte[] b, int p) { return (b[p]&255) | ((b[p+1]&255)<<8); }
    static long u32(byte[] b, int p) { return Integer.toUnsignedLong((b[p]&255) | ((b[p+1]&255)<<8) | ((b[p+2]&255)<<16) | ((b[p+3]&255)<<24)); }
    static String stringField(String json, String key) throws IOException {
        var matcher = java.util.regex.Pattern.compile("\"" + java.util.regex.Pattern.quote(key) + "\"\\s*:\\s*\"([^\"\\\\]*)\"").matcher(json);
        if (!matcher.find()) throw new IOException("missing provenance field " + key);
        String value = matcher.group(1);
        if (matcher.find()) throw new IOException("duplicate provenance field " + key);
        return value;
    }
    static String json(Map<String, String> fields) {
        return "{\n" + String.join(",\n", fields.entrySet().stream().map(e -> "  \"" + e.getKey() + "\": \""
                + e.getValue().replace("\\", "\\\\").replace("\"", "\\\"").replace("\n", "\\n") + "\"").toList()) + "\n}\n";
    }
    static void copyTree(Path from, Path to) throws IOException {
        try (var files = Files.walk(from)) { for (Path p : files.toList()) { Path dest = to.resolve(from.relativize(p)); if (Files.isDirectory(p)) Files.createDirectories(dest); else Files.copy(p, dest); } }
    }
    static void deleteTree(Path dir) throws IOException {
        try (var files = Files.walk(dir)) { for (Path p : files.sorted(Comparator.reverseOrder()).toList()) Files.delete(p); }
    }
}
