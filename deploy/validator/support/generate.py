#!/usr/bin/env python3
"""Build the offline validation support package from digest-pinned official inputs."""
import gzip
import hashlib
from html.parser import HTMLParser
import io
import json
from pathlib import Path
import re
import tarfile
import zipfile

ROOT = Path(__file__).parent
NAME = 'shn.fhir.validation-support'
VERSION = '1.0.0'

def verify_input(source):
    data = (ROOT / 'inputs' / source['file']).read_bytes()
    if hashlib.sha256(data).hexdigest() != source['sha256']:
        raise ValueError('input digest mismatch: ' + source['file'])
    return data

class Table(HTMLParser):
    def __init__(self):
        super().__init__()
        self.rows, self.row, self.cell = [], None, None
    def handle_starttag(self, tag, attrs):
        if tag == 'tr':
            self.row = []
        if tag in ('td', 'th'):
            self.cell = []
    def handle_data(self, data):
        if self.cell is not None:
            self.cell.append(data)
    def handle_endtag(self, tag):
        if tag in ('td', 'th') and self.cell is not None:
            self.row.append(' '.join(''.join(self.cell).split()))
            self.cell = None
        if tag == 'tr' and self.row:
            self.rows.append(self.row)
            self.row = None

def code_system(identifier, url, version, concepts):
    return dict(resourceType='CodeSystem', id=identifier, url=url, version=version,
                name=identifier.replace('-', '_'), status='active', experimental=False,
                publisher='Centers for Medicare & Medicaid Services', caseSensitive=True,
                content='complete', count=len(concepts), concept=sorted(concepts, key=lambda c: c['code']))

def resources():
    inputs = {s['file']: verify_input(s) for s in json.loads((ROOT / 'sources.json').read_text())['inputs']}
    archive = zipfile.ZipFile(io.BytesIO(inputs['cms-hcpcs-2026-07.zip']))
    lines = archive.read('HCPC2026_JUL_ANWEB_06172026.txt').decode('cp1252').splitlines()
    concepts = []
    current = None
    for line in lines:
        code, kind = line[:5].strip(), line[10]
        if kind in ('3', '7'):
            if len(line) != 293 or not re.fullmatch(r'[A-Z][0-9]{4}|[A-Z0-9]{2}', code):
                raise ValueError('unexpected HCPCS primary record')
            current = dict(code=code, display=line[91:119].strip(), definition=line[11:91].strip())
            termination = line[284:292].strip()
            if termination:
                if not re.fullmatch(r'[0-9]{8}', termination):
                    raise ValueError('unexpected HCPCS termination date')
                current['property'] = [
                    {'code': 'inactive', 'valueBoolean': termination < '20260701'},
                    {'code': 'terminationDate', 'valueDateTime': termination[:4] + '-' + termination[4:6] + '-' + termination[6:]},
                ]
            concepts.append(current)
        elif kind in ('4', '8') and current and current['code'] == code:
            current['definition'] += ' ' + line[11:91].strip()
        else:
            raise ValueError('unexpected HCPCS continuation')
    if len(concepts) != 9109 or len({c['code'] for c in concepts}) != 9109:
        raise ValueError('incomplete or duplicate HCPCS release')
    hcpcs = code_system('cms-hcpcs', 'http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets', '2026-07', concepts)
    hcpcs['property'] = [
        {'code': 'inactive', 'uri': 'http://hl7.org/fhir/concept-properties#inactive', 'type': 'boolean',
         'description': 'CMS termination date precedes the July 2026 release effective date.'},
        {'code': 'terminationDate', 'type': 'dateTime', 'description': 'CMS last date for use, from source positions 285-292.'},
    ]
    table = Table()
    table.feed(inputs['cms-pos-2024-05-02.html'].decode('utf-8'))
    concepts = []
    covered = set()
    for row in table.rows[1:]:
        if len(row) != 3:
            raise ValueError('unexpected CMS POS table shape')
        code, name, definition = row
        if name == 'Unassigned':
            lo, _, hi = code.partition('-')
            covered.update(range(int(lo), int(hi or lo) + 1))
            continue
        if not re.fullmatch(r'[0-9]{2}', code):
            raise ValueError('unexpected assigned POS code')
        covered.add(int(code))
        concepts.append(dict(code=code, display=name, definition=definition))
    if covered != set(range(1, 100)) or len(concepts) != 52:
        raise ValueError('incomplete CMS POS table')
    pos = code_system('cms-pos', 'https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set', '2024-05-02', concepts)
    return {'CodeSystem-cms-hcpcs.json': hcpcs, 'CodeSystem-cms-pos.json': pos}

def package_bytes():
    data = {name: (json.dumps(resource, indent=2, ensure_ascii=False) + '\n').encode() for name, resource in resources().items()}
    sd = 'StructureDefinition-ext-R5-Claim.encounter.json'
    data[sd] = (ROOT / 'inputs' / sd).read_bytes()
    data['package.json'] = (json.dumps(dict(name=NAME, version=VERSION, type='fhir.ig',
        fhirVersions=['4.0.1'], description='Offline CMS terminology and scoped official cross-version definition support',
        dependencies={'hl7.fhir.r4.core': '4.0.1'}), indent=2) + '\n').encode()
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode='wb', filename='', mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode='w', format=tarfile.USTAR_FORMAT) as tar:
            for name, content in sorted(data.items()):
                info = tarfile.TarInfo('package/' + name)
                info.size, info.mode, info.mtime = len(content), 0o644, 0
                tar.addfile(info, io.BytesIO(content))
    return output.getvalue()

if __name__ == '__main__':
    (ROOT / (NAME + '-' + VERSION + '.tgz')).write_bytes(package_bytes())
