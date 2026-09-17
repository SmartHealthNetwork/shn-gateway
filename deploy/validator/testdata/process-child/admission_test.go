package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestControlledChildUsesActualOwnedLaunch(t *testing.T) {
	args := []string{"--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher", "literal space", "$literal;unchanged", "--server.address=127.0.0.1", "--server.port=18080"}
	if address, err := controlledAddress(args); err != nil || address != "127.0.0.1:18080" {
		t.Fatalf("%s %v", address, err)
	}
	for _, i := range []int{0, 1, 2, 3, 6, 7} {
		bad := append([]string{}, args...)
		bad[i] = "changed"
		if _, err := controlledAddress(bad); err == nil {
			t.Fatalf("accepted changed launch argument %d", i)
		}
	}
	if _, err := controlledAddress(args[:4]); err == nil {
		t.Fatal("accepted missing owned suffix")
	}
}

func TestRuntimeChildrenIncludeNonleaderThreadsAndDeduplicate(t *testing.T) {
	root := t.TempDir()
	for thread, children := range map[string]string{"1": "", "17": "8 9", "21": "8"} {
		dir := filepath.Join(root, thread)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "children"), []byte(children), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := childPIDs(root)
	if err != nil || !reflect.DeepEqual(got, []int{8, 9}) {
		t.Fatalf("children=%v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "17", "children"), []byte("not-a-pid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := childPIDs(root); err == nil {
		t.Fatal("malformed process evidence accepted")
	}
}
