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
            # The two CMS CodeSystems, the derived closure (closure.py) and the manifest.
            expected = sorted(['package/CodeSystem-cms-hcpcs.json', 'package/CodeSystem-cms-pos.json', 'package/package.json'] + ['package/' + m['file'] for m in members])
            self.assertEqual(names, expected)
            self.assertEqual(len(members), 25)
            for m in members:
                data = (ROOT / 'inputs' / 'closure' / m['file']).read_bytes()
                self.assertEqual(hashlib.sha256(data).hexdigest(), m['sha256'], m['file'])
                self.assertEqual(archive.extractfile('package/' + m['file']).read(), data, m['file'])
                self.assertIn(m['archive'], {a['file'] for a in sources['closure']['archives']})
                self.assertTrue(m['member'].startswith('package/') and m['member'].endswith('/' + m['file']), m['member'])
            manifest = json.loads(archive.extractfile('package/package.json').read())
            self.assertEqual((manifest['name'], manifest['version']), ('shn.fhir.validation-support', '1.2.0'))
        listing = json.loads((ROOT / 'closure-members.json').read_text())
        self.assertEqual([row['file'] for row in listing], [m['file'] for m in members])
        self.assertEqual({row['resourceType'] for row in listing}, {'StructureDefinition', 'ValueSet', 'CodeSystem'})
        self.assertIn('http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter', {row['url'] for row in listing})
        self.assertIn('http://hl7.org/fhir/5.0/StructureDefinition/profile-Encounter', {row['url'] for row in listing})
        for row in listing:
            resource = json.loads((ROOT / 'inputs' / 'closure' / row['file']).read_text())
            self.assertEqual((resource['url'], resource.get('version'), resource['resourceType']), (row['url'], row['version'], row['resourceType']), row['file'])
        self.assertEqual(gen.package_bytes(), gen.package_bytes())
        self.assertEqual(gen.package_bytes(), (ROOT / 'shn.fhir.validation-support-1.2.0.tgz').read_bytes())
        for old in ('1.0.0', '1.1.0'):
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

if __name__ == '__main__':
    unittest.main()
