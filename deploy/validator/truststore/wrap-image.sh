#!/usr/bin/env bash
# wrap-image.sh UPSTREAM IMAGE builds IMAGE as the image UPSTREAM plus one edit:
# the JVM trust store its JRE reads (/etc/ssl/certs/java/cacerts, reached
# through lib/security/cacerts) without the four eMudhra emSign root CAs that
# Oracle's Security Alert for CVE-2026-114820 removes from the JRE. Everything
# else in UPSTREAM is unchanged. The edit runs drop-emsign-roots.sh (beside this
# script) in a pinned JDK build stage with no network.
#
# The image is built under a candidate tag and checked as it will run: its own
# keytool lists the store its own JRE uses (-cacerts, no network), and
# drop-emsign-roots.sh --verify fails if any of the four is still there, or the
# store is empty. Only a candidate that passes is tagged IMAGE; one that fails
# is removed, and IMAGE is left as it was. IMAGE carries the label
# org.shn.truststore=emsign-removed, so a caller can tell a wrapped image from
# one built before.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 UPSTREAM IMAGE" >&2; exit 2; }
upstream=$1
image=$2
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
candidate="${image}-candidate-$$"
trap 'docker image rm "$candidate" >/dev/null 2>&1 || true' EXIT

docker build --label org.shn.truststore=emsign-removed -t "$candidate" -f - "$here" <<EOF
FROM ${upstream} AS upstream

FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS truststore
COPY --from=upstream /etc/ssl/certs/java/cacerts /in/cacerts
COPY drop-emsign-roots.sh /truststore/drop-emsign-roots.sh
RUN --network=none bash /truststore/drop-emsign-roots.sh /in/cacerts /out/cacerts

FROM upstream
COPY --from=truststore /out/cacerts /etc/ssl/certs/java/cacerts
EOF

home=$(docker run --rm --network none --entrypoint java "$candidate" -XshowSettings:properties -version 2>&1 |
	sed -n 's/^ *java\.home = //p')
case $home in
/*) ;;
*)
	echo "wrap-image: the built image's java reported no java.home; ${image} is unchanged" >&2
	exit 1
	;;
esac
docker run --rm --network none --entrypoint "$home/bin/keytool" "$candidate" -cacerts -list -v -storepass changeit |
	bash "$here/drop-emsign-roots.sh" --verify
docker tag "$candidate" "$image"
echo "wrap-image: built ${image} from ${upstream} with the four emSign roots removed from its JVM trust store"
