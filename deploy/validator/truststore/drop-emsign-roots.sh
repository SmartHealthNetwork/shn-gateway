#!/usr/bin/env bash
# drop-emsign-roots.sh IN OUT writes the JVM trust store IN to OUT without the
# four eMudhra emSign root CAs, which Oracle's Security Alert for
# CVE-2026-114820 removes from the JRE. The java images' JRE reads its trust
# store (lib/security/cacerts) through links to the runtime base's
# /etc/ssl/certs/java/cacerts, so a newer JRE on the same base would still
# trust them: each image's build runs this on that store, in a JDK build stage
# with no network, and copies OUT over it. A runtime built outside an image (a
# linked JRE shipped on its own) runs it the same way on that JRE's own store,
# with any keytool that can read the store, so the script travels with whatever
# ships it.
#
# drop-emsign-roots.sh --verify reads a store's `keytool -list -v` output on
# standard input and fails if it holds any of the four, or no certificate: run
# on the store a built image's JRE uses, with that image's own keytool
# (`-cacerts`), it checks the image as it will run.
#
# The roots are matched by SHA-256 certificate fingerprint, never by alias or
# name. The build fails if any of them is still in OUT, or if OUT holds no
# certificate at all. A store that never held some of them (a base that has
# dropped them) passes, and the count removed is printed either way.
set -euo pipefail

pass=changeit

# SHA-256 fingerprints, in hex without keytool's colons.
emsign=(
	40F6AF0346A99AA1CD1D555A4E9CCE62C7F9634603EE406615833DC8C8D00367 # emSign Root CA - G1
	86A1ECBA089C4A8D3BBE2734C612BA341D813E043CF9E8A862CD5C57A36BBE6B # emSign ECC Root CA - G3
	125609AA301DA0A249B97A8239CB6A34216F44DCAC9F3954B14292F2E8C8608F # emSign Root CA - C1
	BC4D809B15189D78DB3E1D8CF4F9726A795DA1643CA5F1358E1DDB0EDC0D7EB3 # emSign ECC Root CA - C3
)

# parse reads `keytool -list -v` output and prints "ALIAS<TAB>SHA256" (hex, no
# colons) for every entry in it.
parse() {
	awk -F': ' '/^Alias name: /{alias=substr($0, 13)} /^[ \t]*SHA256: /{fp=toupper($2); gsub(/[: \t]/, "", fp); print alias "\t" fp}'
}

# entries STORE prints parse's lines for every entry in STORE.
entries() {
	keytool -list -v -keystore "$1" -storepass "$pass" | parse
}

# refuse_held ENTRIES WHAT fails if ENTRIES (parse's lines) hold any of the four
# or no entry at all. It compares in the shell, with no pipe a reader could
# close early (a grep -q under pipefail can read a match as none).
refuse_held() {
	local fps want
	fps=$'\n'$(cut -f2 <<<"$1")$'\n'
	for want in "${emsign[@]}"; do
		case $fps in
		*$'\n'"$want"$'\n'*)
			echo "drop-emsign-roots: $want is still in $2" >&2
			exit 1
			;;
		esac
	done
	if [ "$(grep -c . <<<"$1" || true)" -eq 0 ]; then
		echo "drop-emsign-roots: $2 holds no certificate" >&2
		exit 1
	fi
}

if [ "${1:-}" = --verify ] && [ $# -eq 1 ]; then
	listed=$(parse)
	refuse_held "$listed" "the store listed"
	echo "drop-emsign-roots: the store listed holds none of the four emSign roots ($(grep -c . <<<"$listed") entries)"
	exit 0
fi
[ $# -eq 2 ] || { echo "usage: $0 IN OUT | $0 --verify < keytool -list -v output" >&2; exit 2; }
in=$1
out=$2

mkdir -p "$(dirname "$out")"
cp "$in" "$out"
chmod 0644 "$out"
all=$(entries "$out")
before=$(grep -c . <<<"$all" || true)
removed=0
while IFS=$'\t' read -r alias fp; do
	for want in "${emsign[@]}"; do
		if [ "$fp" = "$want" ]; then
			keytool -delete -alias "$alias" -keystore "$out" -storepass "$pass" </dev/null
			removed=$((removed + 1))
		fi
	done
done <<<"$all"

# The check reads the store again.
left=$(entries "$out")
refuse_held "$left" "$out"
after=$(grep -c . <<<"$left")
echo "drop-emsign-roots: removed $removed of the four emSign roots; $before entries before, $after after"
