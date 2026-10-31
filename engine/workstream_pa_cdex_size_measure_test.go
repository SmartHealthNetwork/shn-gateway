//go:build !shn_coreonly

package engine

import (
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The decoded length read from base64's length and padding is the length a
// decode gives, for every length, with and without the whitespace FHIR's
// base64Binary allows; text that is not well-formed base64 has none.
func TestBase64DecodedLen(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 0; n < 300; n++ {
		raw := make([]byte, n)
		for i := range raw {
			raw[i] = byte(rng.UintN(256))
		}
		enc := base64.StdEncoding.EncodeToString(raw)
		if got, ok := base64DecodedLen([]byte(enc)); !ok || got != int64(n) {
			t.Fatalf("%d bytes: decoded length %d, %v", n, got, ok)
		}
		spaced := strings.Join(splitEvery(enc, 76), "\r\n ")
		if got, ok := base64DecodedLen([]byte(spaced)); !ok || got != int64(n) {
			t.Fatalf("%d bytes with whitespace: decoded length %d, %v", n, got, ok)
		}
	}
	for _, bad := range []string{"Q", "QQ", "QQQ", "QQ=", "=", "====", "Q===", "QQ=Q", "QQ==QQ==", "QQ-_", "QQ!!", "QUJD\x00"} {
		if n, ok := base64DecodedLen([]byte(bad)); ok {
			t.Errorf("%q: decoded length %d, want none (not well-formed)", bad, n)
		}
	}
}

func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out, s = append(out, s[:n]), s[n:]
	}
	return append(out, s)
}

// inlineFile is base64 of n bytes.
func inlineFile(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

// submissionWith is a $submit-attachment whose Attachment parameter's parts
// are parts.
func submissionWith(parts ...string) []byte {
	return []byte(`{"resourceType":"Parameters","parameter":[` +
		`{"name":"MemberId","valueIdentifier":{"system":"http://example.org/member","value":"MBR-COVERED"}},` +
		`{"name":"Attachment","part":[` + strings.Join(parts, ",") + `]}]}`)
}

func documentReference(attachments ...string) string {
	content := make([]string, len(attachments))
	for i, a := range attachments {
		content[i] = `{"attachment":` + a + `}`
	}
	return `{"name":"Content","resource":{"resourceType":"DocumentReference","status":"current","content":[` + strings.Join(content, ",") + `]}}`
}

// Each inline file is found by structure and measured at its decoded size; the
// largest is returned. A file given by url, content with no file, and data
// that is not well-formed base64 are not measured.
func TestLargestInlineAttachment(t *testing.T) {
	for name, row := range map[string]struct {
		body     []byte
		want     int64
		measured bool
	}{
		"a DocumentReference's files, the largest": {
			submissionWith(documentReference(`{"contentType":"application/pdf","data":"`+inlineFile(1000)+`"}`, `{"contentType":"application/pdf","data":"`+inlineFile(70000)+`"}`)), 70000, true},
		"a valueAttachment": {
			submissionWith(`{"name":"Content","valueAttachment":{"contentType":"application/pdf","data":"` + inlineFile(4097) + `"}}`), 4097, true},
		"the files of every Attachment parameter": {
			[]byte(`{"resourceType":"Parameters","parameter":[{"name":"Attachment","part":[` + documentReference(`{"data":"`+inlineFile(10)+`"}`) + `]},{"name":"Attachment","part":[` + documentReference(`{"data":"`+inlineFile(20)+`"}`) + `]}]}`), 20, true},
		"a file whose / is escaped": {
			submissionWith(documentReference(`{"data":"` + strings.ReplaceAll(base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xfb, 0xef, 0xbe}), "/", `\/`) + `"}`)), 6, true},
		"a file with line breaks": {
			submissionWith(documentReference(`{"data":"` + strings.Join(splitEvery(inlineFile(300), 76), `\r\n`) + `"}`)), 300, true},
		"an empty file": {
			submissionWith(documentReference(`{"data":""}`)), 0, true},
		"a file by url": {
			submissionWith(documentReference(`{"contentType":"application/pdf","url":"https://provider.example/docs/1.pdf","size":99999999}`)), 0, false},
		"a QuestionnaireResponse": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"QuestionnaireResponse","status":"completed"}}`), 0, false},
		"data that is not well-formed base64": {
			submissionWith(documentReference(`{"data":"not base64!"}`)), 0, false},
		"a declared size is never read": {
			submissionWith(documentReference(`{"data":"` + inlineFile(12) + `","size":5}`)), 12, true},
		"a QuestionnaireResponse's answer attachment": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"QuestionnaireResponse","status":"completed","item":[{"linkId":"1","item":[{"linkId":"1.1","answer":[{"valueAttachment":{"contentType":"application/pdf","data":"` + inlineFile(3000) + `"}}]}]}]}}`), 3000, true},
		"a QuestionnaireResponse's answer attachment by url": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"QuestionnaireResponse","status":"completed","item":[{"linkId":"1","answer":[{"valueAttachment":{"url":"https://provider.example/q/1.pdf"}}]}]}}`), 0, false},
		"a DocumentReference a resource contains": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"QuestionnaireResponse","status":"completed","contained":[` + documentReference(`{"data":"` + inlineFile(777) + `"}`)[len(`{"name":"Content","resource":`):len(documentReference(`{"data":"`+inlineFile(777)+`"}`))-1] + `]}}`), 777, true},
		"a DocumentReference in a Bundle": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + documentReference(`{"data":"` + inlineFile(901) + `"}`)[len(`{"name":"Content","resource":`):len(documentReference(`{"data":"`+inlineFile(901)+`"}`))-1] + `}]}}`), 901, true},
		"data that is a number is no file": {
			submissionWith(documentReference(`{"data":123456789012345678}`)), 0, false},
		"data that is null is no file": {
			submissionWith(documentReference(`{"data":null}`)), 0, false},
		"a content attachment of another resource is not measured": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"Composition","content":[{"attachment":{"data":"` + inlineFile(50) + `"}}]}}`), 0, false},
		"an attachment that is no DocumentReference's is not a file of the submission": {
			submissionWith(`{"name":"Content","resource":{"resourceType":"Binary","data":"` + inlineFile(50) + `"}}`), 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, measured, err := largestInlineAttachment(row.body)
			if err != nil || got != row.want || measured != row.measured {
				t.Fatalf("largest %d measured %v err %v, want %d %v", got, measured, err, row.want, row.measured)
			}
		})
	}
	if _, _, err := largestInlineAttachment([]byte(`{"parameter":[`)); err == nil {
		t.Fatal("a body that does not scan measured nothing and no error")
	}
}

// cdexAttachmentFixtures are the synthetic documentation submissions under
// testdata/cdex-attachments, each with what its manifest says of it: the
// limit they are built around, and each one's largest inline file.
type cdexAttachmentFixtures struct {
	DeclaredLimit int64 `json:"declaredLimit"`
	Fixtures      []struct {
		File          string `json:"file"`
		What          string `json:"what"`
		Files         int    `json:"files"`
		LargestInline int64  `json:"largestInline"`
		Measured      bool   `json:"measured"`
	} `json:"fixtures"`
}

func readCDexAttachmentFixtures(t *testing.T) cdexAttachmentFixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cdex-attachments", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m cdexAttachmentFixtures
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Fixtures) == 0 {
		t.Fatalf("manifest: %v", err)
	}
	return m
}

// Each fixture is what its manifest says, by an independent reading: its
// files decoded by encoding/base64, never by the measurement under test. And
// the measurement reads each the same way.
func TestCDexAttachmentFixtures_AreWhatTheManifestSays(t *testing.T) {
	m := readCDexAttachmentFixtures(t)
	for _, f := range m.Fixtures {
		t.Run(f.File, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "cdex-attachments", f.File))
			if err != nil {
				t.Fatal(err)
			}
			var root any
			if err := json.Unmarshal(body, &root); err != nil {
				t.Fatal(err)
			}
			// Every file, read with encoding/json and encoding/base64: each
			// object-valued valueAttachment, and each attachment of a
			// DocumentReference's content, at any depth.
			files, measured, largest := 0, false, int64(0)
			file := func(a map[string]any) {
				files++
				data, ok := a["data"].(string)
				if !ok {
					return
				}
				if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
					measured, largest = true, max(largest, int64(len(raw)))
				}
			}
			var walk func(v any)
			walk = func(v any) {
				switch v := v.(type) {
				case []any:
					for _, e := range v {
						walk(e)
					}
				case map[string]any:
					if a, ok := v["valueAttachment"].(map[string]any); ok {
						file(a)
					}
					if v["resourceType"] == "DocumentReference" {
						content, _ := v["content"].([]any)
						for _, c := range content {
							entry, _ := c.(map[string]any)
							if a, ok := entry["attachment"].(map[string]any); ok {
								file(a)
							}
						}
					}
					for _, e := range v {
						walk(e)
					}
				}
			}
			walk(root)
			if files != f.Files || measured != f.Measured || largest != f.LargestInline {
				t.Fatalf("%d files, measured %v, largest %d; the manifest says %d, %v, %d", files, measured, largest, f.Files, f.Measured, f.LargestInline)
			}
			got, gotMeasured, err := largestInlineAttachment(body)
			if err != nil || got != f.LargestInline || gotMeasured != f.Measured {
				t.Fatalf("measured %d %v (%v), want %d %v", got, gotMeasured, err, f.LargestInline, f.Measured)
			}
		})
	}
	if m.DeclaredLimit != 65536 {
		t.Fatalf("the fixtures are built around %d", m.DeclaredLimit)
	}
}
