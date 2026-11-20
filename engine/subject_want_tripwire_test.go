package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every token this gateway verifies is held to a stated subject
// (verifyBoundWant): no engine file but subject_want.go calls the SDK's
// VerifyBound or VerifyBoundNoPayload, whose empty expected subject reads as
// "do not check". A new verify site cannot pass an empty one.
func TestEveryVerifyStatesItsSubject(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the engine's sources: %v, %v", files, err)
	}
	call := regexp.MustCompile(`\bshnsdk\.VerifyBound(NoPayload)?\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "subject_want.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if call.Match(b) {
			t.Errorf("%s calls shnsdk.VerifyBound directly: state the token's subject through verifyBoundWant", f)
		}
	}
}
