# Vendored relay fixtures

Every file under `valid/` and `invalid/` is a **byte-for-byte copy** of a
file at the same relative path in the platform repository's relay fixture
corpus. This package's tests read them (in the gateway module, so do the
tests of the enclosing `relay` package; one copy serves both):

- `valid/` — hostile-but-legal Da Vinci documents (CRD, DTR, PAS and CDex).
  Each must scan cleanly and survive an identity splice unchanged; they also
  seed both fuzz targets.
- `invalid/` — the documents whose JSON itself must be refused, each with the
  specific refusal the scanner returns:

  | File | Refusal |
  |---|---|
  | `duplicate-key.json` | `ErrDuplicateKey` (nested) |
  | `escaped-duplicate-key.json` | `ErrDuplicateKey` (names equal after unescaping) |
  | `casefold-duplicate-key.json` | `ErrDuplicateKey` (names equal under case folding) |
  | `trailing-document.json` | `ErrTrailingData` |
  | `bad-utf8.bin` | `ErrInvalidUTF8` |
  | `lone-surrogate.json` | `ErrLoneSurrogate` |
  | `deep-nesting.json` | `ErrDepthLimit` (depth 65) |

## Why copies

The module that holds this package (the gateway or the SDK; each has an
identical copy of this package) is published on its own and does not carry
the platform repository's fixture corpus. A test that reached outside the module would
pass in the platform repository and break (or silently skip) in the published
module, so this package reads its own copies instead.

## Drift guard

The copies are not maintained independently. A test in the platform
repository asserts byte equality between every file here and its original on
every run of its default gate. If it fails, re-copy the named file from the original;
do not hand-edit the copy.
