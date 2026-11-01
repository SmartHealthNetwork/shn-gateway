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
import java.util.Set;
import java.util.TimeZone;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.zip.CRC32;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;
import java.util.zip.ZipOutputStream;

/** Replaces verified classes in two verified nested JARs without runtime overlays. */
public final class BuildBackport {
    static final String JAR = "WEB-INF/lib/hapi-fhir-validation-8.12.0.jar";
    static final String CLASS = "org/hl7/fhir/common/hapi/validation/validator/ValidatorWrapper";
    static final String ARM_WAR_SHA = "d784462f9a5de7385c4f6b2fcfeec3d7d2316270d5bb2e6fb73cb6ed0c5de749";
    static final String JAR_SHA = "d6ff8774a4de5dd2156d903e260ea3c1dc734f58b2bb759f3f249cb52bec7fd4";
    static final String CLASS_SHA = "e7f50925b9703e25bf1ef1d461f22381ab8e1a3a28b990ab2ff0c6e83bdba4ce";
    static final String SOURCE_SHA = "5050c7693096e31a9f26c9fbd68c60780bd16db7fc51b404a2095e9b95568be0";
    // The validator's regex guard, from org.hl7.fhir.core 7.0.3, verbatim: the pins fail any
    // HAPI bump until someone looks, and a bump to core 7.0 or later deletes this backport.
    static final String UTILITIES_JAR = "WEB-INF/lib/org.hl7.fhir.utilities-6.9.12.jar";
    static final String UTILITIES_JAR_SHA = "f36f062e9cb561143691bf7908c6fb6ecbcefacda53e478fada82b1b534788eb";
    static final String REGEX_PACKAGE = "org/hl7/fhir/utilities/regex/";
    static final Map<String, String> REGEX_INPUT_CLASSES = new TreeMap<>(Map.of(
            REGEX_PACKAGE + "RegexTimeout.class", "61ae2a5da8f8336cfd85a435a3baf1b1aca9ec876ff7f9691ca5cbe6a14b95ab",
            REGEX_PACKAGE + "RegexTimeout$InterruptibleCharSequence.class", "73915b24697314c9c398a4912424c8b3f5bafef63a5bf38c142e4db774171cdd"));
    static final Map<String, String> REGEX_SOURCES = new TreeMap<>(Map.of(
            "upstream/RegexTimeout.java", "8372619facf152c831901c50ec3fe165e66047634bee039415ebcb7fb9cb43a0",
            "upstream/PrimitiveRegexes.java", "b0e595bf1ced4bc14fee48fa58bc4af8c3446b60c8a4d951a2ce3d93876b82c7"));
    static final List<String> REGEX_OUTPUT_CLASSES = List.of(
            REGEX_PACKAGE + "PrimitiveRegexes$PrimitiveMatcher.class", REGEX_PACKAGE + "PrimitiveRegexes.class",
            REGEX_PACKAGE + "RegexTimeout$DeadlineCharSequence.class", REGEX_PACKAGE + "RegexTimeout$RegexTimeoutException.class",
            REGEX_PACKAGE + "RegexTimeout.class");
    static final String COMPILER = "eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7";
    static final Map<String, String> PLATFORMS = Map.of(
            "aarch64", "sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3",
            "amd64", "sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892");

    // On the 6.9.12 classes every row at a limit of 0 ms is red, and the two runaway rows stay green.
    static final int REGEX_ROWS = 23, REGEX_ORIGINAL_FAILURES = 21;

    record SourceTuple(String platform, String child, String warHash, long size) {}
    static final Map<String, SourceTuple> SOURCES = Map.of(
        "linux/arm64", new SourceTuple("linux/arm64", "sha256:4d59628d42b3556b2dcd274d26054a43ba53697ebdae0fd71e6a5475065ed22d", ARM_WAR_SHA, 388197072),
        "linux/amd64", new SourceTuple("linux/amd64", "sha256:88489ad986f38d35ec0e216a0660db42cb55329275efdc5913dfe1d2f2afbe53", "a256639a442672571f948bb6502788ae5a93b2be92bf64cb7e97f7104ac3f678", 388197072));
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
        requireRegexSources(source, REGEX_SOURCES);
        String manifest = Files.readString(source.resolve("provenance.json"));
        requireHash(source.resolve("src/ValidatorWrapper.java"), stringField(manifest, "patched_source_sha256"));
        requireHash(source.resolve("LICENSE.txt"), stringField(manifest, "license_sha256"));
        if (!SOURCE_SHA.equals(stringField(manifest, "upstream_source_sha256"))
                || !COMPILER.equals(stringField(manifest, "compiler_image"))
                || !REGEX_SOURCES.get("upstream/RegexTimeout.java").equals(stringField(manifest, "regex_timeout_source_sha256"))
                || !REGEX_SOURCES.get("upstream/PrimitiveRegexes.java").equals(stringField(manifest, "primitive_regexes_source_sha256"))) throw new IOException("source provenance mismatch");
        Map<String, ZipEntry> members = entries(war);
        ZipEntry jarMember = members.get(JAR);
        if (jarMember == null || jarMember.getMethod() != ZipEntry.STORED) throw new IOException("expected stored validation JAR");
        ZipEntry utilitiesMember = members.get(UTILITIES_JAR);
        if (utilitiesMember == null || utilitiesMember.getMethod() != ZipEntry.STORED) throw new IOException("expected stored utilities JAR");
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
            Path originalUtilities = libs.resolve(Path.of(UTILITIES_JAR).getFileName());
            requireRegexInputs(originalUtilities, UTILITIES_JAR_SHA, REGEX_INPUT_CLASSES);
            requireOneRegexGuard(libs);
            Path original = Files.createDirectory(work.resolve("original"));
            Path patched = Files.createDirectory(work.resolve("patched"));
            Path regex = Files.createDirectory(work.resolve("regex"));
            Path tests = Files.createDirectory(work.resolve("tests"));
            Path logs = Files.createDirectory(work.resolve("logs"));
            String cp;
            try (var stream = Files.list(libs)) { cp = String.join(java.io.File.pathSeparator, stream.sorted().map(Path::toString).toList()); }
            compile(source.resolve("upstream/ValidatorWrapper.java"), original, cp, logs.resolve("compile-original.log"));
            compile(source.resolve("src/ValidatorWrapper.java"), patched, cp, logs.resolve("compile-patched.log"));
            compile(REGEX_SOURCES.keySet().stream().map(source::resolve).toList(), regex, cp, logs.resolve("compile-regex.log"));
            List<Path> testSources;
            try (var stream = Files.list(source.resolve("test"))) { testSources = stream.filter(p -> p.toString().endsWith(".java")).sorted().toList(); }
            List<String> javac = new ArrayList<>(List.of("javac", "--release", "17", "-g", "-encoding", "UTF-8", "-proc:none", "-classpath", regex + ":" + cp,
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
            requireExit(run(List.of("java", "-cp", tests.toString(), "BuildBackportTest", originalUtilities.toString(), source.toString()), logs.resolve("packaging.log")), 0, "packaging tests");
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
            Map<String, byte[]> regexClasses = regexClasses(regex);
            Path newJar = work.resolve("patched.jar"), newUtilities = work.resolve("utilities.jar"), newWar = work.resolve("main.war");
            replace(originalJar, newJar, CLASS + ".class", patchedClass, false);
            verifyDelta(originalJar, newJar, CLASS + ".class", patchedClass);
            Delta regexDelta = regexDelta(regexClasses);
            rewrite(originalUtilities, newUtilities, regexDelta, false);
            verifyDelta(originalUtilities, newUtilities, regexDelta);
            // The packaged JAR takes the original's place, so a reader of a removed class fails here.
            String jarCP = patched + ":" + tests + ":" + cp.replace(originalUtilities.toString(), newUtilities.toString());
            String regexIdentity = "-Dbackport.expectedRegexClass=" + hash(regexClasses.get(REGEX_PACKAGE + "RegexTimeout.class"));
            String regexTest = "org.hl7.fhir.utilities.regex.RegexBackportTest";
            requireExit(run(List.of("java", "-Xmx1g", "-Dbackport.expectedRegexClass=" + REGEX_INPUT_CLASSES.get(REGEX_PACKAGE + "RegexTimeout.class"), "-cp", tests + ":" + cp, regexTest), logs.resolve("regex-original-red.log")), 1, "original regex regression");
            if (!Files.readString(logs.resolve("regex-original-red.log")).contains("ROWS=" + REGEX_ROWS + " FAILURES=" + REGEX_ORIGINAL_FAILURES)) throw new IOException("unexpected original regex failures");
            requireExit(run(List.of("java", "-Xmx1g", regexIdentity, "-cp", jarCP, regexTest), logs.resolve("regex-backport-green.log")), 0, "backport regex regression");
            requireExit(run(List.of("java", "-Xmx1g", "-cp", jarCP, "org.hl7.fhir.utilities.regex.PrimitiveRegexesDifferentialTest"), logs.resolve("regex-differential.log")), 0, "primitive regex differential");
            requireExit(run(List.of("java", "-Xmx1g", patchedIdentity, "-cp", jarCP, testClass), logs.resolve("regex-explicit-profile.log")), 0, "explicit profile on the packaged JAR");
            requireExit(run(List.of("java", "-Xmx1g", patchedIdentity, "-cp", jarCP,
                    "org.hl7.fhir.common.hapi.validation.validator.OperationBoundaryTest", logs.resolve("regex-operation").toString()), logs.resolve("regex-operation.log")), 0, "operation boundary on the packaged JAR");
            requireExit(run(List.of("java", "-Xmx1g", "-cp", jarCP, characterization, logs.resolve("characterization-regex.txt").toString()), logs.resolve("characterization-regex.log")), 0, "backport characterization");
            if (!Arrays.equals(Files.readAllBytes(before), Files.readAllBytes(logs.resolve("characterization-regex.txt")))) throw new IOException("existing validation behavior changed under the regex backport");
            byte[] jarBytes = Files.readAllBytes(newJar), utilitiesBytes = Files.readAllBytes(newUtilities);
            Delta warDelta = new Delta(new TreeMap<>(Map.of(JAR, jarBytes, UTILITIES_JAR, utilitiesBytes)), Map.of(), Set.of());
            rewrite(war, newWar, warDelta, true);
            verifyDelta(war, newWar, warDelta);
            Map<String, String> provenance = new TreeMap<>();
            provenance.put("format", "validator-backport-v2");
            provenance.put("upstream_image", "hapiproject/hapi@sha256:0ff86aac4c7bd13a1ee226444a70263bb46ed1c28ef543c30a44f865b80ca04e");
            provenance.put("hapi_source_platform", inputTuple.platform()); provenance.put("hapi_source_child", inputTuple.child());
            provenance.put("input_war_size", Long.toString(inputTuple.size()));
            provenance.put("input_war_sha256", inputTuple.warHash()); provenance.put("input_jar_sha256", JAR_SHA);
            provenance.put("input_class_sha256", CLASS_SHA); provenance.put("upstream_source_sha256", SOURCE_SHA);
            provenance.put("input_utilities_jar_sha256", UTILITIES_JAR_SHA);
            provenance.put("input_regex_classes_sha256", classesIdentity(REGEX_INPUT_CLASSES));
            provenance.put("regex_timeout_source_sha256", REGEX_SOURCES.get("upstream/RegexTimeout.java"));
            provenance.put("primitive_regexes_source_sha256", REGEX_SOURCES.get("upstream/PrimitiveRegexes.java"));
            provenance.put("patched_source_sha256", hash(Files.readAllBytes(source.resolve("src/ValidatorWrapper.java"))));
            provenance.put("compiler_image", COMPILER); provenance.put("compiler_platform", "linux/" + (System.getProperty("os.arch").equals("aarch64") ? "arm64" : "amd64"));
            provenance.put("compiler_platform_digest", PLATFORMS.get(System.getProperty("os.arch")));
            provenance.put("compiler_runtime", System.getProperty("java.runtime.version"));
            provenance.put("builder_sha256", hash(Files.readAllBytes(source.resolve("BuildBackport.java"))));
            provenance.put("source_manifest_sha256", hash(Files.readAllBytes(source.resolve("provenance.json"))));
            provenance.put("classpath_sha256", hash((String.join("\n", classpath) + "\n").getBytes(StandardCharsets.UTF_8)));
            provenance.put("java_class_major", "61"); provenance.put("changed_war_entry", JAR); provenance.put("changed_jar_entry", CLASS + ".class");
            provenance.put("changed_utilities_war_entry", UTILITIES_JAR);
            provenance.put("output_class_sha256", hash(patchedClass)); provenance.put("output_jar_sha256", hash(jarBytes));
            provenance.put("output_utilities_jar_sha256", hash(utilitiesBytes));
            Map<String, String> regexHashes = new TreeMap<>();
            for (var entry : regexClasses.entrySet()) regexHashes.put(entry.getKey(), hash(entry.getValue()));
            provenance.put("output_regex_classes_sha256", classesIdentity(regexHashes));
            provenance.put("output_war_sha256", hash(newWar));
            Map<String, String> key = new TreeMap<>(provenance);
            key.keySet().removeIf(k -> k.startsWith("output_"));
            provenance.put("build_key", hash(json(key).getBytes(StandardCharsets.UTF_8)));
            Files.createDirectory(output);
            Files.copy(newWar, output.resolve("main.war")); Files.copy(newJar, output.resolve("hapi-fhir-validation-8.12.0.jar"));
            Files.copy(newUtilities, output.resolve("org.hl7.fhir.utilities-6.9.12.jar"));
            Files.writeString(output.resolve("provenance.json"), json(provenance));
            Files.writeString(output.resolve("classpath.sha256"), String.join("\n", classpath) + "\n");
            copyTree(logs, output.resolve("evidence"));
            Files.writeString(output.resolve("complete"), provenance.get("build_key") + "\n");
            System.out.println(json(provenance));
        } finally { deleteTree(work); }
    }

    static void compile(Path source, Path destination, String cp, Path log) throws Exception {
        compile(List.of(source), destination, cp, log);
    }
    static void compile(List<Path> sources, Path destination, String cp, Path log) throws Exception {
        List<String> javac = new ArrayList<>(List.of("javac", "--release", "17", "-g", "-encoding", "UTF-8", "-proc:none", "-classpath", cp,
                "-d", destination.toString()));
        javac.addAll(sources.stream().map(Path::toString).toList());
        requireExit(run(javac, log), 0, "compile " + sources.get(0).getFileName());
    }
    static void requireRegexSources(Path source, Map<String, String> pins) throws Exception {
        for (var pin : pins.entrySet()) requireHash(source.resolve(pin.getKey()), pin.getValue());
    }
    static void requireRegexInputs(Path jar, String jarHash, Map<String, String> classes) throws Exception {
        requireHash(jar, jarHash);
        requireUnsigned(jar);
        entries(jar);
        for (var pin : classes.entrySet()) requireHash(member(jar, pin.getKey()), pin.getValue());
    }
    // A second copy on the classpath could shadow the replacement.
    static void requireOneRegexGuard(Path libs) throws IOException {
        List<String> carriers = new ArrayList<>();
        try (var stream = Files.list(libs)) {
            for (Path jar : stream.sorted().toList()) if (entries(jar).containsKey(REGEX_PACKAGE + "RegexTimeout.class")) carriers.add(jar.getFileName().toString());
        }
        if (!carriers.equals(List.of(Path.of(UTILITIES_JAR).getFileName().toString()))) throw new IOException("regex guard carriers " + carriers);
    }
    static Map<String, byte[]> regexClasses(Path compiled) throws IOException {
        Map<String, byte[]> classes = new TreeMap<>();
        try (var stream = Files.walk(compiled)) {
            for (Path file : stream.filter(Files::isRegularFile).toList()) classes.put(compiled.relativize(file).toString(), Files.readAllBytes(file));
        }
        if (!new ArrayList<>(classes.keySet()).equals(REGEX_OUTPUT_CLASSES)) throw new IOException("unexpected compiled regex classes " + classes.keySet());
        for (var entry : classes.entrySet()) requireClass(entry.getValue(), entry.getKey().replaceAll("\\.class$", ""));
        return classes;
    }
    /** The 7.0.3 class set exactly: a 6.9.12 class it no longer has goes, one it adds comes. */
    static Delta regexDelta(Map<String, byte[]> classes) {
        Map<String, byte[]> replaced = new TreeMap<>(), added = new TreeMap<>();
        Set<String> removed = new TreeSet<>(REGEX_INPUT_CLASSES.keySet());
        for (var entry : classes.entrySet()) {
            if (removed.remove(entry.getKey())) replaced.put(entry.getKey(), entry.getValue());
            else added.put(entry.getKey(), entry.getValue());
        }
        return new Delta(replaced, added, removed);
    }
    static String classesIdentity(Map<String, String> hashes) throws Exception {
        StringBuilder lines = new StringBuilder();
        for (var entry : new TreeMap<>(hashes).entrySet()) lines.append(entry.getKey()).append('\t').append(entry.getValue()).append('\n');
        return hash(lines.toString().getBytes(StandardCharsets.UTF_8));
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
    /** What one archive rewrite changes; added entries follow every original entry, in name order. */
    record Delta(Map<String, byte[]> replaced, Map<String, byte[]> added, Set<String> removed) {
        static Delta of(String target, byte[] bytes) { return new Delta(Map.of(target, bytes), Map.of(), Set.of()); }
    }
    static void replace(Path input, Path output, String target, byte[] bytes, boolean stored) throws IOException {
        rewrite(input, output, Delta.of(target, bytes), stored);
    }
    static void rewrite(Path input, Path output, Delta delta, boolean stored) throws IOException {
        Map<String, ZipEntry> members = entries(input);
        Map<String, byte[]> serialized = new TreeMap<>(delta.replaced());
        serialized.putAll(delta.added());
        for (String target : delta.replaced().keySet()) {
            ZipEntry old = members.get(target);
            if (old == null) throw new IOException("missing replacement target " + target);
            if (stored && old.getMethod() != ZipEntry.STORED) throw new IOException("nested JAR must already be stored");
        }
        for (String target : delta.removed()) if (!members.containsKey(target)) throw new IOException("missing removal target " + target);
        for (String target : delta.added().keySet()) if (members.containsKey(target)) throw new IOException("addition already present " + target);
        Path single = Files.createTempFile("replacement-entry-", ".zip");
        try {
            // Only the changed members are serialized. Every other local record and
            // central header is copied raw, retaining metadata ZipEntry cannot expose.
            try (ZipOutputStream zip = new ZipOutputStream(Files.newOutputStream(single))) {
                for (var entry : serialized.entrySet()) {
                    ZipEntry changed = new ZipEntry(entry.getKey());
                    byte[] bytes = entry.getValue();
                    changed.setTimeLocal(LocalDateTime.of(2000, 1, 1, 0, 0));
                    changed.setMethod(members.containsKey(entry.getKey()) ? members.get(entry.getKey()).getMethod() : ZipEntry.DEFLATED);
                    CRC32 crc = new CRC32(); crc.update(bytes);
                    changed.setSize(bytes.length); changed.setCrc(crc.getValue());
                    zip.putNextEntry(changed); zip.write(bytes); zip.closeEntry();
                }
            }
            ZipLayout layout = layout(input), replacement = layout(single);
            List<String> serializedOrder = replacement.headers().keySet().stream()
                    .sorted(Comparator.comparingLong(n -> u32(replacement.headers().get(n),42))).toList();
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
                    if (delta.removed().contains(name)) continue;
                    positions.put(name,out.position());
                    if (serialized.containsKey(name)) copySerialized(one,out,replacement,serializedOrder,name);
                    else copyRange(from,out,begin,finish-begin);
                }
                for (String name : delta.added().keySet().stream().sorted().toList()) {
                    positions.put(name,out.position());
                    copySerialized(one,out,replacement,serializedOrder,name);
                }
                long directory = out.position();
                List<String> central = new ArrayList<>(layout.headers().keySet());
                central.removeAll(delta.removed());
                central.addAll(new TreeSet<>(delta.added().keySet()));
                for (String name : central) {
                    byte[] header = (serialized.containsKey(name) ? replacement.headers().get(name) : layout.headers().get(name)).clone();
                    putU32(header,42,positions.get(name));
                    write(out,header);
                }
                byte[] ending = layout.ending().clone();
                putU16(ending,8,positions.size()); putU16(ending,10,positions.size());
                putU32(ending,12,out.position()-directory); putU32(ending,16,directory);
                write(out,ending);
            }
        } finally { Files.delete(single); }
    }
    static void copySerialized(java.nio.channels.FileChannel one, java.nio.channels.FileChannel out, ZipLayout replacement, List<String> order, String name) throws IOException {
        int i = order.indexOf(name);
        long begin = u32(replacement.headers().get(name),42);
        long finish = i+1 == order.size() ? replacement.offset() : u32(replacement.headers().get(order.get(i+1)),42);
        copyRange(one,out,begin,finish-begin);
    }
    static void write(java.nio.channels.FileChannel out, byte[] bytes) throws IOException {
        var buffer = java.nio.ByteBuffer.wrap(bytes); while (buffer.hasRemaining()) out.write(buffer);
    }
    static void copyRange(java.nio.channels.FileChannel from, java.nio.channels.FileChannel to, long offset, long length) throws IOException {
        long copied = 0;
        while (copied < length) { long n = from.transferTo(offset+copied,length-copied,to); if (n <= 0) throw new IOException("short ZIP record copy"); copied += n; }
    }
    static void putU16(byte[] bytes, int offset, int value) throws IOException {
        if (value < 0 || value >= 65535) throw new IOException("ZIP entry count unsupported");
        bytes[offset]=(byte)value; bytes[offset+1]=(byte)(value >>> 8);
    }
    static void putU32(byte[] bytes, int offset, long value) throws IOException {
        if (value < 0 || value >= 0xffffffffL) throw new IOException("ZIP64 output unsupported");
        for (int i=0;i<4;i++) bytes[offset+i]=(byte)(value >>> (8*i));
    }
    static void verifyDelta(Path original, Path patched, String target, byte[] replacement) throws IOException {
        verifyDelta(original, patched, Delta.of(target, replacement));
    }
    static void verifyDelta(Path original, Path patched, Delta delta) throws IOException {
        Map<String, ZipEntry> before = entries(original), after = entries(patched);
        List<String> expectedOrder = new ArrayList<>(before.keySet());
        expectedOrder.removeAll(delta.removed());
        expectedOrder.addAll(new TreeSet<>(delta.added().keySet()));
        if (!expectedOrder.equals(new ArrayList<>(after.keySet()))) throw new IOException("ZIP entry set/order changed");
        Map<String, byte[]> oldHeaders = centralMetadata(original), newHeaders = centralMetadata(patched);
        for (String name : expectedOrder) if (before.containsKey(name) && !delta.replaced().containsKey(name) && !Arrays.equals(oldHeaders.get(name), newHeaders.get(name)))
            throw new IOException("raw central ZIP metadata changed " + name);
        try (ZipFile a = new ZipFile(original.toFile()); ZipFile b = new ZipFile(patched.toFile())) {
            if (!java.util.Objects.equals(a.getComment(), b.getComment())) throw new IOException("archive comment changed");
            for (String name : expectedOrder) {
                ZipEntry old = before.get(name), now = after.get(name);
                byte[] expected = delta.replaced().containsKey(name) ? delta.replaced().get(name)
                        : delta.added().containsKey(name) ? delta.added().get(name) : a.getInputStream(old).readAllBytes();
                if (!Arrays.equals(expected, b.getInputStream(now).readAllBytes())) throw new IOException("entry bytes changed " + name);
                if (old == null) continue;
                if (old.getMethod() != now.getMethod()) throw new IOException("compression method changed " + name);
                if (!delta.replaced().containsKey(name) && (old.getTime() != now.getTime()
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
