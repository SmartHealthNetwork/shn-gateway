package main

import (
	"reflect"
	"testing"
)

var literalJava = []string{"java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"}

func TestOwnedLaunchPreservesLiteralInputs(t *testing.T) {
	extra := []string{"argument with spaces", "$literal;unchanged", "", "--hapi.fhir.fhir_version=R4"}
	args := append(append([]string{}, literalJava...), extra...)
	env := []string{"JAVA_OPTS=-Dserver.port=9090;$(literal)", "JAVA_TOOL_OPTIONS=-Xmx768m -Ddescription=unrelated", "UNRELATED=value with spaces;$literal", "SERVER_PORT=18080", "server.address=127.0.0.1", `SPRING_APPLICATION_JSON={"server":{"port":18080,"address":"127.0.0.1"},"other":"unchanged"}`}
	want := append(append([]string{}, args...), "--server.address=127.0.0.1", "--server.port=18080")
	before := append([]string{}, env...)
	got, err := launchArgs(args, env)
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(env, before) {
		t.Fatalf("launch=%q %v env=%q", got, err, env)
	}
	if !reflect.DeepEqual(args, append(append([]string{}, literalJava...), extra...)) {
		t.Fatal("caller argv mutated")
	}
}

func TestOwnedLaunchRejectsEveryReservedDirectOption(t *testing.T) {
	for _, arg := range []string{"--server.port=18080", "--server.address=127.0.0.1", "--server.port=9090", "--server.address=0.0.0.0", "--server.port", "--server.port=", "--SERVER_PORT=18080", "--server-address=127.0.0.1"} {
		t.Run(arg, func(t *testing.T) {
			for _, extras := range [][]string{{arg}, {arg, arg}} {
				if _, err := launchArgs(append(append([]string{}, literalJava...), extras...), []string{"SERVER_PORT=18080"}); err == nil {
					t.Fatal("accepted caller owned option")
				}
			}
		})
	}
}

func TestOwnedLaunchSourceRefusals(t *testing.T) {
	for _, env := range []string{"SERVER_PORT=8080", "SERVER_ADDRESS=0.0.0.0", "SERVER_PORT=", "SERVERPORT=18080", "server-address=127.0.0.1", `SPRING_APPLICATION_JSON={"server":{"port":8080}}`, `SPRING_APPLICATION_JSON={"server.port":18080,"server":{"port":18080}}`, `SPRING_APPLICATION_JSON={"server":{"port":18080,"port":18080}}`, `SPRING_APPLICATION_JSON={"server":{"port":null}}`, `SPRING_APPLICATION_JSON={"server":{"port":18080}`, "LOADER_MAIN=other.Main", "LOADER_ARGS=--server.port=8080", "LOADER_CONFIG_LOCATION=file:/tmp/loader.properties", "JDK_JAVA_OPTIONS=@/tmp/options", "JDK_JAVA_OPTIONS=-cp /other", "JAVA_TOOL_OPTIONS=-Dserver.port=8080", `JAVA_TOOL_OPTIONS="-Dserver.port=18080"`, "_JAVA_OPTIONS=-Dserver.address=0.0.0.0", "_JAVA_OPTIONS=-Dloader.main=other.Main", "JAVA_TOOL_OPTIONS=-Dspring.application.json={}", "SPRING_MAIN_ADD_COMMAND_LINE_PROPERTIES=false"} {
		t.Run(env, func(t *testing.T) {
			if _, err := launchArgs(literalJava, []string{env}); err == nil {
				t.Fatal("accepted conflicting/unsupported source")
			}
		})
	}
	for _, env := range [][]string{{"SERVER_PORT=18080", "SERVER_PORT=18080"}, {"SERVER_PORT=18080", "server.port=18080"}} {
		if _, err := launchArgs(literalJava, env); err == nil {
			t.Fatal("accepted ambiguous environment")
		}
	}
	for _, args := range [][]string{{"sh", "-c", "java"}, {"/process-child"}, literalJava[:4], append(append([]string{}, literalJava...), "--loader.main=other.Main")} {
		if _, err := launchArgs(args, nil); err == nil {
			t.Fatalf("accepted unsupported argv %q", args)
		}
	}
}

func TestOwnedLaunchMatchingLowerPrioritySources(t *testing.T) {
	for _, env := range []string{"JDK_JAVA_OPTIONS=-Xmx768m -Dserver.port=18080", "JAVA_TOOL_OPTIONS=-Dserver.address=127.0.0.1 -Xmx768m", "_JAVA_OPTIONS=-Dserver.port=18080", `SPRING_APPLICATION_JSON={"server.port":18080,"server.address":"127.0.0.1"}`, `JAVA_TOOL_OPTIONS=-Dunrelated="literal value"`, "JAVA_TOOL_OPTIONS=literal unrelated invalid JVM option"} {
		if _, err := launchArgs(literalJava, []string{env}); err != nil {
			t.Fatalf("rejected supported lower-priority/unrelated source %q: %v", env, err)
		}
	}
	if _, err := launchArgs(append(append([]string{}, literalJava...), `--spring.application.json={"server":{"port":18080}}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := launchArgs(literalJava, []string{"LOADER_MAIN=", "LOADER_ARGS="}); err != nil {
		t.Fatal("empty loader settings should remain inert", err)
	}
}

func TestOwnedLaunchRejectsRepeatedInlineJSON(t *testing.T) {
	args := append(append([]string{}, literalJava...), `--spring.application.json={"server":{"port":18080}}`, `--spring.application.json={"server":{"port":18080}}`)
	if _, err := launchArgs(args, nil); err == nil {
		t.Fatal("repeated Spring JSON becomes an ambiguous aggregate")
	}
}

// The finite launch-source contract rejects ambiguity even when each value
// would match the owned endpoint by itself.
func TestOwnedLaunchFiniteSourceRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{"duplicate-jdk-source", nil, []string{"JDK_JAVA_OPTIONS=-Dserver.port=18080", "JDK_JAVA_OPTIONS=-Dserver.port=18080"}},
		{"duplicate-tool-source", nil, []string{"JAVA_TOOL_OPTIONS=-Xmx768m", "JAVA_TOOL_OPTIONS=-Xmx768m"}},
		{"duplicate-java-source", nil, []string{"_JAVA_OPTIONS=-Dserver.address=127.0.0.1", "_JAVA_OPTIONS=-Dserver.address=127.0.0.1"}},
		{"repeated-matching-jvm-token", nil, []string{"JAVA_TOOL_OPTIONS=-Dserver.port=18080 -Dserver.port=18080"}},
		{"repeated-conflicting-jvm-token", nil, []string{"JAVA_TOOL_OPTIONS=-Dserver.port=18080 -Dserver.port=9090"}},
		{"escaped-owned-jvm-token", nil, []string{`JDK_JAVA_OPTIONS=-Dserver\.port=18080`}},
		{"owned-jvm-alias", nil, []string{"_JAVA_OPTIONS=-DSERVER_PORT=18080"}},
		{"direct-json-alias", []string{`--SPRING_APPLICATION_JSON={"server":{"port":18080}}`}, nil},
		{"environment-json-alias", nil, []string{`spring-application-json={"server":{"port":18080}}`}},
		{"nested-json-owned-alias", []string{`--spring.application.json={"Server":{"port":18080}}`}, nil},
		{"dotted-json-owned-alias", nil, []string{`SPRING_APPLICATION_JSON={"server.PORT":18080}`}},
		{"json-loader-redirection", []string{`--spring.application.json={"loader":{"main":"other.Main"}}`}, nil},
		{"json-command-line-disabling", nil, []string{`SPRING_APPLICATION_JSON={"spring.main.add-command-line-properties":false}`}},
		{"direct-command-line-disabling", []string{"--spring.main.add-command-line-properties=false"}, nil},
		{"jvm-command-line-disabling", nil, []string{"JAVA_TOOL_OPTIONS=-Dspring.main.add-command-line-properties=false"}},
		{"loader-config-name", nil, []string{"LOADER_CONFIG_NAME=other"}},
		{"loader-home", nil, []string{"LOADER_HOME=/other"}},
		{"loader-path", nil, []string{"LOADER_PATH=/other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, literalJava...), tc.args...)
			if _, err := launchArgs(args, tc.env); err == nil {
				t.Fatal("accepted ambiguous or redirected launch source")
			}
		})
	}
}

func TestOwnedLaunchFiniteLexicalBoundariesPreserveBytes(t *testing.T) {
	for _, source := range []string{"JDK_JAVA_OPTIONS", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS"} {
		t.Run(source, func(t *testing.T) {
			env := []string{source + "=\t -Dserver.port=18080\n-Dserver.address=127.0.0.1\r\n-Dmyserver.port=9090  -Dserver.port.extra=9090\t-Dloader.home.extra=/other"}
			before := append([]string{}, env...)
			args := append([]string{}, literalJava...)
			want := append(append([]string{}, args...), "--server.address=127.0.0.1", "--server.port=18080")
			got, err := launchArgs(args, env)
			if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(env, before) || !reflect.DeepEqual(args, literalJava) {
				t.Fatalf("finite lexical boundary changed literal inputs: argv=%q env=%q err=%v", got, env, err)
			}
		})
	}
}
