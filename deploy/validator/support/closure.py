#!/usr/bin/env python3
"""Derive the validation closure of the cross-version and extension canonicals SHN-exchanged
resources carry.

The walk starts at SEEDS (sources.json "closure.seeds") and follows, inside the digest-pinned archives named in sources.json
("closure.archives"): baseDefinition, type.profile, binding.valueSet, element and resource
extension urls, ValueSet.compose systems and included value sets, CodeSystem.supplements and
CodeSystem.valueSet, and type.targetProfile at depth <= 1 (the seed's own value type and that
profile's direct reference targets). It stops at any canonical the engine core provides. It is
version-aware: a reference "url|v" resolves only to an archive member whose own version is v; a
pinned version no archive carries is recorded as a tolerance (the engine's versioned-URL fallback
resolves it to the loaded definition for core-namespace canonicals) and the loaded member is
followed; a canonical no archive carries is recorded as unresolved; a type.targetProfile the depth
cut leaves is recorded as cut.

Outputs, written beside this file so the generator and the tests can read them without the
archives: inputs/closure/<member basename> (bytes copied unchanged from the archive),
closure-members.json (the sorted member list with url and version), closure-tolerances.json
(version fallbacks, cut reference targets, unresolved canonicals, each with the element that
reaches it), and the "closure.members" list in sources.json (file, archive, member path, SHA-256).

Usage: SHN_IG_ARCHIVES=<dir with the two archives> python3 closure.py
Optional: SHN_CORE_INVENTORY=<path to embedded-core.json> (found by searching upward otherwise).
"""
import hashlib
import io
import json
import os
from pathlib import Path
import tarfile

ROOT = Path(__file__).parent
# The seeds are the canonicals SHN-built or SHN-relayed resources carry that no line's own packages
# define: sources.json "closure.seeds" is the one list (the R5 Claim.encounter extension, and the
# artifact-versionAlgorithm extension DTR Questionnaires carry, whose version-algorithm CodeSystem
# the 2.0/2.1 lines otherwise cannot resolve).
SEEDS = json.loads((ROOT / 'sources.json').read_text())['closure']['seeds']
TARGET_DEPTH = 1
FALLBACK_PREFIX = 'http://hl7.org/fhir/StructureDefinition/'


def split(ref):
    url, sep, version = ref.partition('|')
    return url, (version if sep else None)


def core_canonicals(path=None):
    p = Path(path or os.environ.get('SHN_CORE_INVENTORY') or _find_core())
    return {split(u)[0] for u in json.loads(p.read_text())['canonicals']}


def _find_core():
    for parent in [ROOT] + list(ROOT.parents):
        candidate = parent / 'tools' / 'contracts' / 'closure' / 'embedded-core.json'
        if candidate.exists():
            return candidate
    raise FileNotFoundError('embedded-core.json not found above ' + str(ROOT) + '; set SHN_CORE_INVENTORY')


def line_prefixes(path=None):
    """The canonical prefixes of the lines' own IG packages (the manifest's closure.seedPrefixes).
    A closure member under one of them would mean the walk crossed into a package a line already
    loads; the walk refuses that instead of copying it."""
    p = Path(path or os.environ.get('SHN_CONTRACTS_MANIFEST') or _find_core().parent.parent / 'manifest.json')
    return list(json.loads(p.read_text())['closure']['seedPrefixes'])


def load_archives(directory, sources=None):
    """Read the pinned archives after verifying their digests; a mismatch refuses to walk."""
    sources = sources or json.loads((ROOT / 'sources.json').read_text())
    out = {}
    for entry in sources['closure']['archives']:
        data = (Path(directory) / entry['file']).read_bytes()
        if hashlib.sha256(data).hexdigest() != entry['sha256']:
            raise ValueError('archive digest mismatch: ' + entry['file'])
        out[entry['file']] = data
    return out


def members_of(archives):
    """(url, version) -> (archive, member path, bytes, resource) for every JSON resource with a url."""
    index = {}
    for name, data in sorted(archives.items()):
        with tarfile.open(fileobj=io.BytesIO(data)) as tar:
            for m in tar.getmembers():
                if not (m.isfile() and m.name.startswith('package/') and m.name.endswith('.json')) or '/xml/' in m.name:
                    continue
                raw = tar.extractfile(m).read()
                try:
                    resource = json.loads(raw)
                except ValueError:
                    continue
                if not isinstance(resource, dict) or not resource.get('resourceType') or not resource.get('url'):
                    continue
                index.setdefault((resource['url'], resource.get('version')), (name, m.name, raw, resource))
    return index


def refs(resource, follow_targets):
    """(reference, element id or '', kind) in a deterministic order."""
    out = []
    rt = resource.get('resourceType')
    if rt == 'StructureDefinition':
        if resource.get('baseDefinition'):
            out.append((resource['baseDefinition'], '', 'baseDefinition'))
        for part in ('snapshot', 'differential'):
            for el in (resource.get(part) or {}).get('element') or []:
                eid = el.get('id') or el.get('path') or ''
                for t in el.get('type') or []:
                    for p in t.get('profile') or []:
                        out.append((p, eid, 'type.profile'))
                    for p in t.get('targetProfile') or []:
                        out.append((p, eid, 'type.targetProfile' if follow_targets else 'cut'))
                binding = el.get('binding') or {}
                if binding.get('valueSet'):
                    out.append((binding['valueSet'], eid, 'binding.valueSet'))
                for e in el.get('extension') or []:
                    if str(e.get('url', '')).startswith('http'):
                        out.append((e['url'], eid, 'element.extension'))
    elif rt == 'ValueSet':
        compose = resource.get('compose') or {}
        for inc in (compose.get('include') or []) + (compose.get('exclude') or []):
            if inc.get('system'):
                out.append((inc['system'], '', 'compose.system'))
            for vs in inc.get('valueSet') or []:
                out.append((vs, '', 'compose.valueSet'))
    elif rt == 'CodeSystem':
        for key in ('supplements', 'valueSet'):
            if resource.get(key):
                out.append((resource[key], '', key))
    for e in resource.get('extension') or []:
        if str(e.get('url', '')).startswith('http'):
            out.append((e['url'], '', 'extension'))
    seen, ordered = set(), []
    for item in out:
        if item not in seen:
            seen.add(item)
            ordered.append(item)
    return ordered


def walk(archives, seeds=SEEDS, core=None, forbid_prefixes=None):
    """Return (members, tolerances). members: url -> (archive, member path, bytes, resource).
    Refuses (ValueError) a member whose canonical carries one of forbid_prefixes — by default the
    manifest's line-IG prefixes — so the closure never duplicates what a line's own packages load."""
    core = core if core is not None else core_canonicals()
    forbid = list(forbid_prefixes) if forbid_prefixes is not None else line_prefixes()
    index = members_of(archives)
    by_url = {}
    for (url, version), entry in index.items():
        by_url.setdefault(url, []).append((version, entry))
    members, fallbacks, cut, unresolved = {}, [], [], []
    queue = [(s, 0, '', '', 'seed') for s in seeds]
    visited = set()
    while queue:
        ref, depth, from_url, element, kind = queue.pop(0)
        url, version = split(ref)
        if url in core:
            continue
        if kind == 'cut':
            if url not in members:
                cut.append(dict(reference=ref, from_url=from_url, element=element))
            continue
        key = (ref, from_url, element)
        if key in visited:
            continue
        visited.add(key)
        candidates = by_url.get(url)
        if not candidates:
            unresolved.append(dict(reference=ref, from_url=from_url, element=element, kind=kind))
            continue
        exact = [e for v, e in candidates if version is None or v == version]
        if not exact:
            loaded = sorted(v for v, _ in candidates if v)
            fallbacks.append(dict(reference=ref, from_url=from_url, element=element, kind=kind,
                                  loaded=loaded, fallback=url.startswith(FALLBACK_PREFIX)))
            exact = [e for _, e in candidates]
        entry = sorted(exact, key=lambda e: (e[0], e[1]))[0]
        if url not in members:
            members[url] = entry
            follow_targets = depth <= TARGET_DEPTH
            for child, eid, child_kind in refs(entry[3], follow_targets):
                queue.append((child, depth + 1, url, eid, child_kind))
    crossed = sorted(u for u in members if any(u.startswith(p) for p in forbid))
    if crossed:
        raise ValueError('closure crossed into a line package: ' + ', '.join(crossed))
    tolerances = dict(versionFallbacks=sorted(fallbacks, key=lambda d: (d['reference'], d['from_url'], d['element'])),
                      cutReferenceTargets=sorted(cut, key=lambda d: (d['reference'], d['from_url'], d['element'])),
                      unresolved=sorted(unresolved, key=lambda d: (d['reference'], d['from_url'], d['element'])))
    return members, tolerances


def write(members, tolerances):
    out_dir = ROOT / 'inputs' / 'closure'
    out_dir.mkdir(parents=True, exist_ok=True)
    for stale in out_dir.glob('*.json'):
        stale.unlink()
    listing, sources_members = [], []
    for url in sorted(members):
        archive, member, raw, resource = members[url]
        basename = member.rsplit('/', 1)[1]
        (out_dir / basename).write_bytes(raw)
        listing.append(dict(url=url, version=resource.get('version'), resourceType=resource['resourceType'], file=basename))
        sources_members.append(dict(file=basename, archive=archive, member=member, sha256=hashlib.sha256(raw).hexdigest()))
    (ROOT / 'closure-members.json').write_text(json.dumps(listing, indent=2) + '\n')
    (ROOT / 'closure-tolerances.json').write_text(json.dumps(tolerances, indent=2) + '\n')
    sources = json.loads((ROOT / 'sources.json').read_text())
    sources['closure']['members'] = sources_members
    (ROOT / 'sources.json').write_text(json.dumps(sources, indent=2) + '\n')
    return listing


if __name__ == '__main__':
    directory = os.environ.get('SHN_IG_ARCHIVES')
    if not directory:
        raise SystemExit('set SHN_IG_ARCHIVES to the directory holding the pinned archives')
    found, tol = walk(load_archives(directory))
    rows = write(found, tol)
    print('closure members: %d (%s)' % (len(rows), ', '.join('%s %d' % (t, sum(1 for r in rows if r['resourceType'] == t)) for t in sorted({r['resourceType'] for r in rows}))))
    print('version fallbacks: %d, cut reference targets: %d, unresolved: %d' % (len(tol['versionFallbacks']), len(tol['cutReferenceTargets']), len(tol['unresolved'])))
