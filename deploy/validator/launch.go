package main

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

const privateAddress = "127.0.0.1:18080"
const privateBase = "http://" + privateAddress + "/fhir"

var javaPrefix = []string{"java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"}

// launchArgs owns only the image's endpoint and literal launch shape. It neither
// expands arguments nor rewrites inherited JVM options. Spring command-line
// properties outrank config files, environment, JVM properties and inline JSON.
func launchArgs(argv, env []string) ([]string, error) {
	if len(argv) < len(javaPrefix) || !reflect.DeepEqual(argv[:len(javaPrefix)], javaPrefix) {
		return nil, fmt.Errorf("supervisor: unsupported Java launch prefix")
	}
	inlineJSON := false
	for _, arg := range argv[len(javaPrefix):] {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		key, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		switch reservedSetting(key) {
		case "port", "address", "loader", "command-line":
			return nil, launchConflict("application argument", key)
		case "json":
			if key != "spring.application.json" || inlineJSON {
				return nil, launchConflict("application argument", key)
			}
			inlineJSON = true
			if err := checkLaunchJSON(value); err != nil {
				return nil, err
			}
		}
	}
	seen := map[string]bool{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		kind := reservedSetting(key)
		if kind != "" {
			if kind != "loader" && seen[kind] {
				return nil, launchConflict("duplicate environment", key)
			}
			seen[kind] = true
			switch kind {
			case "port", "address":
				canonical := key == "SERVER_PORT" || key == "server.port" || key == "SERVER_ADDRESS" || key == "server.address"
				if !canonical || !matchingEndpoint(kind, value) {
					return nil, launchConflict("environment", key)
				}
			case "json":
				if key != "SPRING_APPLICATION_JSON" && key != "spring.application.json" {
					return nil, launchConflict("environment", key)
				}
				if err := checkLaunchJSON(value); err != nil {
					return nil, err
				}
			case "loader":
				if value != "" {
					return nil, launchConflict("environment", key)
				}
			case "command-line":
				return nil, launchConflict("environment", key)
			}
		}
		switch key {
		case "JDK_JAVA_OPTIONS", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS":
			if seen[key] {
				return nil, launchConflict("duplicate environment", key)
			}
			seen[key] = true
			if err := checkJVMSource(key, value); err != nil {
				return nil, err
			}
		}
	}
	return append(append([]string(nil), argv...), "--server.address=127.0.0.1", "--server.port=18080"), nil
}

func settingName(s string) string {
	return strings.ToLower(strings.NewReplacer(".", "", "_", "", "-", "").Replace(s))
}
func reservedSetting(s string) string {
	switch settingName(s) {
	case "serverport":
		return "port"
	case "serveraddress":
		return "address"
	case "springapplicationjson":
		return "json"
	case "springmainaddcommandlineproperties":
		return "command-line"
	case "loadermain", "loaderargs", "loaderconfiglocation", "loaderconfigname", "loaderhome", "loaderpath":
		return "loader"
	}
	return ""
}
func matchingEndpoint(kind, value string) bool {
	return kind == "port" && value == "18080" || kind == "address" && value == "127.0.0.1"
}
func launchConflict(source, key string) error {
	// Never print supplied values: unrelated configuration can contain credentials.
	return fmt.Errorf("supervisor: unsupported owned endpoint/launch setting in %s (%s)", source, key)
}

// This is deliberately a finite lexical guard, not a Java/shell tokenizer.
// Unrelated strings are untouched, including quoting that only Java interprets.
// Owned JVM settings support only canonical, unquoted atomic -D tokens.
func checkJVMSource(source, value string) error {
	seen := map[string]bool{}
	for _, raw := range strings.Fields(value) {
		token := strings.NewReplacer("\"", "", "'", "", "\\", "").Replace(raw)
		if source == "JDK_JAVA_OPTIONS" {
			if strings.HasPrefix(token, "@") {
				return launchConflict(source, "argument file")
			}
			key, _, _ := strings.Cut(token, "=")
			switch key {
			case "-cp", "-classpath", "--class-path", "-jar", "-m", "--module":
				return launchConflict(source, "launcher redirection")
			}
		}
		if !strings.HasPrefix(token, "-D") {
			continue
		}
		key, val, _ := strings.Cut(strings.TrimPrefix(token, "-D"), "=")
		kind := reservedSetting(key)
		if kind == "" {
			continue
		}
		if kind != "port" && kind != "address" || seen[kind] || raw != token || key != "server."+kind || !matchingEndpoint(kind, val) {
			return launchConflict(source, key)
		}
		seen[kind] = true
	}
	return nil
}

func checkLaunchJSON(value string) error {
	d := json.NewDecoder(strings.NewReader(value))
	d.UseNumber()
	seen := map[string]bool{}
	var walk func(string) error
	walk = func(path string) error {
		token, err := d.Token()
		if err != nil {
			return launchConflict("JSON", "syntax")
		}
		kind := reservedSetting(path)
		if kind != "" {
			if kind != "port" && kind != "address" || seen[kind] || path != "server."+kind {
				return launchConflict("JSON", path)
			}
			seen[kind] = true
			v := ""
			switch x := token.(type) {
			case string:
				v = x
			case json.Number:
				v = string(x)
			}
			if !matchingEndpoint(kind, v) {
				return launchConflict("JSON", path)
			}
			return nil
		}
		delim, ok := token.(json.Delim)
		if !ok {
			if path == "" {
				return launchConflict("JSON", "object required")
			}
			return nil
		}
		switch delim {
		case '{':
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return launchConflict("JSON", "syntax")
				}
				key, ok := k.(string)
				if !ok {
					return launchConflict("JSON", "key")
				}
				if path != "" {
					key = path + "." + key
				}
				if err := walk(key); err != nil {
					return err
				}
			}
		case '[':
			if path == "" {
				return launchConflict("JSON", "object required")
			}
			for d.More() {
				if err := walk(path + "[]"); err != nil {
					return err
				}
			}
		default:
			return launchConflict("JSON", "syntax")
		}
		if _, err := d.Token(); err != nil {
			return launchConflict("JSON", "syntax")
		}
		return nil
	}
	if err := walk(""); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return launchConflict("JSON", "trailing content")
	}
	return nil
}
