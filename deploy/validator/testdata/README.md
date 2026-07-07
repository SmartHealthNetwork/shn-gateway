# Offline-proof probe fixtures

Vendored copies of four resources used by `verify.sh` to prove the validator
resolves the Da Vinci PAS/DTR/PDex/CDex profiles **offline** (the network is
isolated during the proof). The check keys only on profile *resolution* (the absence of
HAPI's "Failed to retrieve profile" issue), which is insensitive to resource
content — so harmless drift from the upstream copies cannot affect the gate.
Kept here (rather than referencing a path outside this directory) so the proof
runs from a bare clone of the published gateway repo.
