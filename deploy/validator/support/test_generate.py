import importlib.util
import pathlib
import io
import tarfile
import unittest
from unittest import mock
import copy
import hashlib
from html import escape
import json
import zipfile

ROOT = pathlib.Path(__file__).parent

class SupportTests(unittest.TestCase):
    def test_complete_reproducible_inputs(self):
        self.assertTrue((ROOT / 'generate.py').exists(), 'support generator is required')
        spec = importlib.util.spec_from_file_location('support_generator', ROOT / 'generate.py')
        gen = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(gen)
        resources = gen.resources()
        hcpcs = resources['CodeSystem-cms-hcpcs.json']
        pos = resources['CodeSystem-cms-pos.json']
        self.assertEqual(hcpcs['content'], 'complete')
        self.assertEqual(hcpcs['url'], 'http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets')
        self.assertEqual(pos['url'], 'https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set')
        self.assertEqual(len(hcpcs['concept']), 9109)
        cp = next(c for c in hcpcs['concept'] if c['code'] == 'CP')
        self.assertIn({'code': 'inactive', 'valueBoolean': True}, cp.get('property', []))
        self.assertIn({'code': 'terminationDate', 'valueDateTime': '2017-12-31'}, cp.get('property', []))
        self.assertEqual(len(pos['concept']), 52)
        for resource, valid, invalid in [(hcpcs, 'L8000', 'L9999'), (pos, '11', '98')]:
            codes = {c['code'] for c in resource['concept']}
            self.assertIn(valid, codes)
            self.assertNotIn(invalid, codes)
            self.assertEqual(len(codes), len(resource['concept']))
        sources = json.loads((ROOT / 'sources.json').read_text())
        members = sources['closure']['members']
        with tarfile.open(fileobj=io.BytesIO(gen.package_bytes())) as archive:
            names = sorted(m.name for m in archive.getmembers())
            # The three CMS CodeSystems, the SHN local release, the closure and manifest.
            expected = sorted(['package/CodeSystem-cms-hcpcs.json', 'package/CodeSystem-cms-pos.json', 'package/CodeSystem-cms-icd10cm.json', 'package/CodeSystem-shn-clinical-context.json', 'package/package.json'] + ['package/' + m['file'] for m in members])
            self.assertEqual(names, expected)
            self.assertEqual(len(members), 25)
            for m in members:
                data = (ROOT / 'inputs' / 'closure' / m['file']).read_bytes()
                self.assertEqual(hashlib.sha256(data).hexdigest(), m['sha256'], m['file'])
                self.assertEqual(archive.extractfile('package/' + m['file']).read(), data, m['file'])
                self.assertIn(m['archive'], {a['file'] for a in sources['closure']['archives']})
                self.assertTrue(m['member'].startswith('package/') and m['member'].endswith('/' + m['file']), m['member'])
            manifest = json.loads(archive.extractfile('package/package.json').read())
            self.assertEqual((manifest['name'], manifest['version']), ('shn.fhir.validation-support', '1.4.0'))
        listing = json.loads((ROOT / 'closure-members.json').read_text())
        self.assertEqual([row['file'] for row in listing], [m['file'] for m in members])
        self.assertEqual({row['resourceType'] for row in listing}, {'StructureDefinition', 'ValueSet', 'CodeSystem'})
        self.assertIn('http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter', {row['url'] for row in listing})
        self.assertIn('http://hl7.org/fhir/5.0/StructureDefinition/profile-Encounter', {row['url'] for row in listing})
        for row in listing:
            resource = json.loads((ROOT / 'inputs' / 'closure' / row['file']).read_text())
            self.assertEqual((resource['url'], resource.get('version'), resource['resourceType']), (row['url'], row['version'], row['resourceType']), row['file'])
        self.assertEqual(gen.package_bytes(), gen.package_bytes())
        self.assertEqual(gen.package_bytes(), (ROOT / 'shn.fhir.validation-support-1.4.0.tgz').read_bytes())
        for old in ('1.0.0', '1.1.0', '1.2.0', '1.3.0'):
            self.assertFalse((ROOT / ('shn.fhir.validation-support-' + old + '.tgz')).exists(), 'older archives must be gone')
        self.assertFalse((ROOT / 'inputs' / 'StructureDefinition-ext-R5-Claim.encounter.json').exists())

    def test_closure_member_digest_is_load_bearing(self):
        spec = importlib.util.spec_from_file_location('support_generator', ROOT / 'generate.py')
        gen = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(gen)
        with self.assertRaisesRegex(ValueError, 'digest'):
            gen.verify_input({'file': 'StructureDefinition-profile-Encounter.json', 'sha256': '0' * 64}, 'closure')

    def test_closure_walk_reproduces_the_committed_members(self):
        """Re-run the walk against the digest-checked implementation guide archives named by
        SHN_IG_ARCHIVES, and prove every committed member byte-identical to its archive entry and
        the tolerance record unchanged. Skipped when that directory is not set: the members are
        proved by digest instead."""
        import os
        directory = os.environ.get('SHN_IG_ARCHIVES')
        if not directory:
            self.skipTest('set SHN_IG_ARCHIVES to the directory holding the pinned archives')
        spec = importlib.util.spec_from_file_location('support_closure', ROOT / 'closure.py')
        closure = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(closure)
        members, tolerances = closure.walk(closure.load_archives(directory))
        listing = json.loads((ROOT / 'closure-members.json').read_text())
        self.assertEqual(sorted(members), [row['url'] for row in listing])
        for url, (archive_name, member, raw, resource) in members.items():
            self.assertEqual(raw, (ROOT / 'inputs' / 'closure' / member.rsplit('/', 1)[1]).read_bytes(), url)
        self.assertEqual(tolerances, json.loads((ROOT / 'closure-tolerances.json').read_text()))
        # The walk refuses to copy anything a line's own packages already provide: forbidding a
        # prefix one member carries is red, never a silent duplicate.
        with self.assertRaisesRegex(ValueError, 'crossed into a line package'):
            closure.walk(closure.load_archives(directory), forbid_prefixes=['http://hl7.org/fhir/5.0/StructureDefinition/profile-'])
        with self.assertRaisesRegex(ValueError, 'archive digest mismatch'):
            pinned = json.loads((ROOT / 'sources.json').read_text())
            pinned['closure']['archives'][0]['sha256'] = '0' * 64
            closure.load_archives(directory, pinned)

    def test_corrupt_pinned_input_rejected(self):
        self.assertTrue((ROOT / 'generate.py').exists(), 'support generator is required')
        spec = importlib.util.spec_from_file_location('support_generator', ROOT / 'generate.py')
        gen = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(gen)
        with self.assertRaisesRegex(ValueError, 'digest'):
            gen.verify_input({'file': 'cms-hcpcs-2026-07.zip', 'sha256': '0' * 64})

    def test_parser_rejection_rows(self):
        """Re-pin mutated inputs so each row reaches its parser guard, not SHA-256."""
        spec = importlib.util.spec_from_file_location('support_generator', ROOT / 'generate.py')
        gen = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(gen)
        sources = json.loads((ROOT / 'sources.json').read_text())
        originals = {s['file']: (ROOT / 'inputs' / s['file']).read_bytes() for s in sources['inputs']}
        originals[sources['local']['file']] = (ROOT / 'inputs' / sources['local']['file']).read_bytes()
        hcpcs_name = 'cms-hcpcs-2026-07.zip'
        member = 'HCPC2026_JUL_ANWEB_06172026.txt'
        with zipfile.ZipFile(io.BytesIO(originals[hcpcs_name])) as archive:
            lines = archive.read(member).decode('cp1252').splitlines()
        continuation = next(i for i, line in enumerate(lines) if line[10] in '48')
        table = gen.Table()
        table.feed(originals['cms-pos-2024-05-02.html'].decode())

        def hcpcs(change):
            changed = list(lines)
            change(changed)
            output = io.BytesIO()
            with zipfile.ZipFile(output, 'w') as archive:
                archive.writestr(member, '\r\n'.join(changed).encode('cp1252'))
            return hcpcs_name, output.getvalue()

        def pos(change):
            rows = copy.deepcopy(table.rows)
            change(rows)
            html = '<table>' + ''.join('<tr>' + ''.join('<td>' + escape(cell) + '</td>' for cell in row) + '</tr>' for row in rows) + '</table>'
            return 'cms-pos-2024-05-02.html', html.encode()

        rows = [
            ('primary-length', lambda: hcpcs(lambda x: x.__setitem__(0, x[0][:-1])), 'unexpected HCPCS primary record'),
            ('primary-code', lambda: hcpcs(lambda x: x.__setitem__(0, '!BAD!' + x[0][5:])), 'unexpected HCPCS primary record'),
            ('termination-date', lambda: hcpcs(lambda x: x.__setitem__(0, x[0][:284] + '2026AB01' + x[0][292:])), 'unexpected HCPCS termination date'),
            ('unknown-record-kind', lambda: hcpcs(lambda x: x.__setitem__(0, x[0][:10] + '9' + x[0][11:])), 'unexpected HCPCS continuation'),
            ('orphan-continuation', lambda: hcpcs(lambda x: x.insert(0, x[continuation])), 'unexpected HCPCS continuation'),
            ('mismatched-continuation', lambda: hcpcs(lambda x: x.__setitem__(continuation, 'Z9999' + x[continuation][5:])), 'unexpected HCPCS continuation'),
            ('duplicate-hcpcs', lambda: hcpcs(lambda x: x.__setitem__(1, x[0][:5] + x[1][5:])), 'incomplete or duplicate HCPCS release'),
            ('incomplete-hcpcs', lambda: hcpcs(lambda x: x.pop(0)), 'incomplete or duplicate HCPCS release'),
            ('pos-table-shape', lambda: pos(lambda x: x[1].pop()), 'unexpected CMS POS table shape'),
            ('pos-assigned-code', lambda: pos(lambda x: x[1].__setitem__(0, 'A1')), 'unexpected assigned POS code'),
            ('pos-coverage', lambda: pos(lambda x: x[1].__setitem__(0, '00')), 'incomplete CMS POS table'),
            ('pos-count', lambda: pos(lambda x: x.append(list(x[1]))), 'incomplete CMS POS table'),
        ]
        for name, mutate, diagnostic in rows:
            with self.subTest(name=name):
                filename, data = mutate()
                inputs = dict(originals, **{filename: data})
                pinned = copy.deepcopy(sources)
                for source in pinned['inputs']:
                    source['sha256'] = hashlib.sha256(inputs[source['file']]).hexdigest()
                # All file reads stay in memory; the real digest verifier and parser run.
                with mock.patch.object(pathlib.Path, 'read_bytes', lambda path: inputs[path.name]), mock.patch.object(pathlib.Path, 'read_text', lambda path: json.dumps(pinned)):
                    with self.assertRaisesRegex(ValueError, '^' + diagnostic + '$'):
                        gen.resources()

class ICD10CMTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location('support_generator', ROOT / 'generate.py')
        cls.gen = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.gen)
        cls.input_name = 'cms-icd10cm-april-2026-order.zip'
        cls.sources = json.loads((ROOT / 'sources.json').read_text())
        cls.originals = {s['file']: (ROOT / 'inputs' / s['file']).read_bytes() for s in cls.sources['inputs']}
        cls.originals[cls.sources['local']['file']] = (ROOT / 'inputs' / cls.sources['local']['file']).read_bytes()

    def test_full_official_release(self):
        cs = self.gen.resources()['CodeSystem-cms-icd10cm.json']
        self.assertEqual((cs['url'], cs['version'], cs['content'], cs['count']),
                         ('http://hl7.org/fhir/sid/icd-10-cm', '2026-04-01', 'complete', 98186))
        self.assertNotIn('hierarchyMeaning', cs)
        concepts = {c['code']: c for c in cs['concept']}
        self.assertEqual(len(concepts), 98186)
        self.assertEqual(concepts['M51.16']['display'], 'Intervertebral disc disorders with radiculopathy, lumbar region')
        self.assertNotIn('M51.16X', concepts)
        self.assertNotIn('M5116', concepts)
        self.assertIn('QA0.0101', concepts)
        # Independently split each official row by the documented fixed positions.
        with zipfile.ZipFile(io.BytesIO(self.originals[self.input_name])) as z:
            order = z.read('Code Descriptions/icd10cm_order_2026.txt').decode().splitlines()
            codes = z.read('Code Descriptions/icd10cm_codes_2026.txt').decode().splitlines()
        valid = {line[:7].strip(): line[8:] for line in codes}
        flags = {False: 0, True: 0}
        for row in order:
            key = row[6:13].strip()
            dotted = key if len(key) == 3 else key[:3] + '.' + key[3:]
            c = concepts[dotted]
            self.assertEqual(c['display'], row[77:])
            self.assertEqual(c['designation'], [{'language': 'en', 'value': row[16:76].rstrip()}])
            expected_flag = row[14] == '1'
            self.assertEqual(c['property'], [
                {'code': 'cmsOrder', 'valueInteger': int(row[:5])},
                {'code': 'cmsValidForHIPAATransactions', 'valueBoolean': expected_flag}])
            self.assertNotIn('concept', c)
            flags[expected_flag] += 1
            if expected_flag:
                self.assertEqual(valid.pop(key), c['display'])
        self.assertEqual(flags, {False: 23467, True: 74719})
        self.assertEqual(valid, {})
        self.assertEqual(concepts['A00']['property'][1]['valueBoolean'], False)
        self.assertEqual(concepts['A00.0']['property'][1]['valueBoolean'], True)

    def test_source_digest_rejects(self):
        with self.assertRaisesRegex(ValueError, 'digest'):
            self.gen.verify_input({'file': self.input_name, 'sha256': '0' * 64})

    def test_official_release_mutations_reject(self):
        with zipfile.ZipFile(io.BytesIO(self.originals[self.input_name])) as z:
            members = {n: z.read(n) for n in z.namelist()}
        order_name = 'Code Descriptions/icd10cm_order_2026.txt'
        codes_name = 'Code Descriptions/icd10cm_codes_2026.txt'
        order = members[order_name].decode().splitlines()
        codes = members[codes_name].decode().splitlines()
        def at(lines, i, start, end, value):
            lines[i] = lines[i][:start] + value + lines[i][end:]
        rows = [
            ('missing-order-member', None, order_name, 'missing'),
            ('missing-codes-member', None, codes_name, 'missing'),
            ('duplicate-zip-member', None, order_name, 'duplicate'),
            ('missing-order-row', lambda x: x.pop(), order_name, 'incomplete'),
            ('duplicate-order-row', lambda x: x.__setitem__(1, x[0]), order_name, 'duplicate'),
            ('duplicate-order-number', lambda x: at(x, 1, 0, 5, '00001'), order_name, 'order'),
            ('order-number', lambda x: at(x, 0, 0, 5, '0000X'), order_name, 'order'),
            ('malformed-row', lambda x: x.__setitem__(0, 'short'), order_name, 'row'),
            ('separator', lambda x: at(x, 0, 13, 14, '!'), order_name, 'row'),
            ('invalid-flag', lambda x: at(x, 0, 14, 15, '2'), order_name, 'flag'),
            ('invalid-code', lambda x: at(x, 0, 6, 13, '100    '), order_name, 'code'),
            ('dotted-input', lambda x: at(x, 1, 6, 13, 'A00.0  '), order_name, 'code'),
            ('interior-code-space', lambda x: at(x, 1, 6, 13, 'A0 0   '), order_name, 'code'),
            ('missing-short', lambda x: at(x, 0, 16, 76, ' '*60), order_name, 'description'),
            ('missing-long', lambda x: x.__setitem__(0, x[0][:77]), order_name, 'description'),
            ('control-description', lambda x: at(x, 0, 77, 78, '\t'), order_name, 'description'),
            ('longer-than-format', lambda x: x.__setitem__(0, x[0] + 'x'*401), order_name, 'row'),
            ('wrong-flag', lambda x: at(x, 0, 14, 15, '1'), order_name, 'reconcil'),
            ('missing-code-row', lambda x: x.pop(), codes_name, 'reconcil'),
            ('duplicate-code-row', lambda x: x.__setitem__(1, x[0]), codes_name, 'duplicate'),
            ('codes-bad-separator', lambda x: at(x, 0, 7, 8, '!'), codes_name, 'row'),
            ('codes-bad-code', lambda x: at(x, 0, 0, 7, 'A00.0  '), codes_name, 'code'),
            ('codes-empty-description', lambda x: x.__setitem__(0, x[0][:8]), codes_name, 'description'),
            ('different-display', lambda x: x.__setitem__(0, x[0] + ' changed'), codes_name, 'reconcil'),
        ]
        for name, mutate, member, diagnostic in rows:
            with self.subTest(name=name):
                changed = dict(members)
                if mutate:
                    lines = list(order if member == order_name else codes)
                    mutate(lines)
                    changed[member] = ('\r\n'.join(lines) + '\r\n').encode()
                elif name.startswith('missing-'):
                    del changed[member]
                output = io.BytesIO()
                with zipfile.ZipFile(output, 'w') as z:
                    for n, data in changed.items():
                        z.writestr(n, data)
                    if name == 'duplicate-zip-member':
                        import warnings
                        with warnings.catch_warnings():
                            warnings.simplefilter('ignore', UserWarning)
                            z.writestr(member, members[member])
                inputs = dict(self.originals, **{self.input_name: output.getvalue()})
                pinned = copy.deepcopy(self.sources)
                for source in pinned['inputs']:
                    source['sha256'] = hashlib.sha256(inputs[source['file']]).hexdigest()
                with mock.patch.object(pathlib.Path, 'read_bytes', lambda path: inputs[path.name]), mock.patch.object(pathlib.Path, 'read_text', lambda path: json.dumps(pinned)):
                    with self.assertRaisesRegex(ValueError, diagnostic):
                        self.gen.resources()

class LocalClinicalContextTests(unittest.TestCase):
    """PCV-06: the first SHN release has only the three source-backed concepts."""

    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location('support_generator', ROOT / 'generate.py')
        cls.gen = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.gen)

    @staticmethod
    def expected():
        return {
            'resourceType': 'CodeSystem', 'id': 'shn-clinical-context',
            'url': 'urn:shn:clinical-context', 'version': '1.0.0',
            'name': 'SHNClinicalContext', 'status': 'active', 'experimental': False,
            'publisher': 'Smart Health Network', 'caseSensitive': True,
            'content': 'complete', 'count': 3,
            'description': 'First SHN-maintained release of three local clinical-context concepts used by the lumbar prior-authorization workflow. Membership does not establish a suitable standard clinical code or the truth of a participant-reported value.',
            'concept': [
                {'code': 'conservative-therapy-weeks', 'display': 'Weeks of completed conservative therapy',
                 'definition': 'Number of weeks of conservative therapy completed, as reported by the participant source system.'},
                {'code': 'neuro-deficit', 'display': 'Progressive neurological deficit present',
                 'definition': 'Boolean flag for whether a progressive neurological deficit is present, as reported by the participant source system.'},
                {'code': 'patient-reported-required', 'display': 'Patient-reported functional-status attestation required',
                 'definition': 'Boolean workflow requirement for patient-reported functional-status attestation; this signal is not the attestation itself.'},
            ],
        }

    def test_local_release_membership_and_meaning(self):
        resource = self.gen.resources()['CodeSystem-shn-clinical-context.json']
        self.assertEqual(resource, self.expected())
        for field, kind in [('caseSensitive', bool), ('experimental', bool), ('count', int)]:
            self.assertIs(type(resource[field]), kind, field)
        with tarfile.open(fileobj=io.BytesIO(self.gen.package_bytes())) as archive:
            packaged = json.load(archive.extractfile('package/CodeSystem-shn-clinical-context.json'))
            self.assertEqual(packaged, self.expected())
            manifest = json.load(archive.extractfile('package/package.json'))
            self.assertEqual((manifest['name'], manifest['version']), ('shn.fhir.validation-support', '1.4.0'))

    def test_mutated_local_release_rejected(self):
        valid = self.expected()
        rows = [
            ('missing', lambda x: x['concept'].pop(), 'membership'),
            ('extra', lambda x: x['concept'].append({'code': 'prior-imaging', 'display': 'Prior imaging', 'definition': 'Unreviewed'}), 'membership'),
            ('duplicate', lambda x: x['concept'].append(copy.deepcopy(x['concept'][0])), 'duplicate'),
            ('case', lambda x: x['concept'][0].__setitem__('code', 'Conservative-therapy-weeks'), 'membership'),
            ('resource-type', lambda x: x.__setitem__('resourceType', 'ValueSet'), 'identity'),
            ('id', lambda x: x.__setitem__('id', 'other'), 'identity'),
            ('canonical', lambda x: x.__setitem__('url', 'urn:shn:other'), 'identity'),
            ('version', lambda x: x.__setitem__('version', '1.0.1'), 'identity'),
            ('name', lambda x: x.__setitem__('name', 'Other'), 'identity'),
            ('status', lambda x: x.__setitem__('status', 'draft'), 'identity'),
            ('publisher', lambda x: x.__setitem__('publisher', 'Centers for Medicare & Medicaid Services'), 'identity'),
            ('case-sensitive-false', lambda x: x.__setitem__('caseSensitive', False), 'identity'),
            ('case-sensitive-numeric-true', lambda x: x.__setitem__('caseSensitive', 1), 'identity'),
            ('experimental-true', lambda x: x.__setitem__('experimental', True), 'identity'),
            ('experimental-numeric-false', lambda x: x.__setitem__('experimental', 0), 'identity'),
            ('content', lambda x: x.__setitem__('content', 'fragment'), 'identity'),
            ('count', lambda x: x.__setitem__('count', 4), 'identity'),
            ('count-float-alias', lambda x: x.__setitem__('count', 3.0), 'identity'),
            ('extra-top-level-field', lambda x: x.__setitem__('date', '2026-01-01'), 'shape'),
            ('missing-description', lambda x: x.pop('description'), 'shape'),
            ('blank-description', lambda x: x.__setitem__('description', ' '), 'shape'),
            ('non-string-description', lambda x: x.__setitem__('description', []), 'shape'),
            ('concept-container', lambda x: x.__setitem__('concept', {}), 'concept shape'),
            ('concept-member', lambda x: x['concept'].__setitem__(0, []), 'concept shape'),
            ('non-flat-concept', lambda x: x['concept'][0].__setitem__('concept', []), 'concept shape'),
            ('missing-display', lambda x: x['concept'][0].pop('display'), 'concept shape'),
            ('blank-display', lambda x: x['concept'][0].__setitem__('display', ''), 'concept shape'),
            ('non-string-display', lambda x: x['concept'][0].__setitem__('display', 7), 'concept shape'),
            ('missing-definition', lambda x: x['concept'][0].pop('definition'), 'concept shape'),
            ('blank-definition', lambda x: x['concept'][0].__setitem__('definition', '  '), 'concept shape'),
            ('non-string-definition', lambda x: x['concept'][0].__setitem__('definition', False), 'concept shape'),
        ]
        for name, mutate, diagnostic in rows:
            with self.subTest(name=name):
                changed = copy.deepcopy(valid)
                mutate(changed)
                with self.assertRaisesRegex(ValueError, diagnostic):
                    self.gen.shn_clinical_context(json.dumps(changed).encode())

    def test_duplicate_raw_json_field_rejected(self):
        valid = json.dumps(self.expected()).encode()
        duplicate = valid.replace(b'"resourceType": "CodeSystem",',
                                  b'"resourceType": "CodeSystem", "resourceType": "CodeSystem",', 1)
        self.assertNotEqual(valid, duplicate)
        with self.assertRaisesRegex(ValueError, 'duplicate local CodeSystem field'):
            self.gen.shn_clinical_context(duplicate)

    def test_local_input_digest_rejects_corruption(self):
        source = json.loads((ROOT / 'sources.json').read_text())['local']
        with self.assertRaisesRegex(ValueError, 'digest'):
            self.gen.verify_input(dict(source, sha256='0' * 64))

    def test_prior_resource_bytes(self):
        baseline = json.loads((ROOT / 'prior-resource-sha256.json').read_text())
        self.assertEqual(len(baseline), 28)
        generated = self.gen.package_bytes()
        with tarfile.open(fileobj=io.BytesIO(generated)) as archive:
            current = {m.name.removeprefix('package/'): hashlib.sha256(archive.extractfile(m).read()).hexdigest()
                       for m in archive.getmembers() if m.name != 'package/package.json'}
        self.assertEqual({name: current[name] for name in baseline}, baseline)


if __name__ == '__main__':
    unittest.main()
