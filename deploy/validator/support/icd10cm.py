"""CMS April 2026 ICD-10-CM fixed-width tables; no inferred clinical semantics."""
import io
import re
import zipfile


def code(field):
    raw = field.rstrip(' ')
    if not re.fullmatch(r'[A-Z][A-Z0-9]{2,6}', raw) or field != raw.ljust(7):
        raise ValueError('invalid ICD-10-CM code field')
    # FHIR R4 ICD representation requires the decimal, absent from the CMS files.
    return raw if len(raw) == 3 else raw[:3] + '.' + raw[3:]


def description(value):
    if not value or value != value.strip() or any(ord(c) < 32 or ord(c) > 126 for c in value):
        raise ValueError('invalid ICD-10-CM description')
    return value


def concepts(data):
    """Preserve all headers and codes, reconciled against the independent codes file."""
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        names = archive.namelist()
        if len(names) != len(set(names)):
            raise ValueError('duplicate ICD-10-CM archive member')
        required = ['Code Descriptions/icd10cm_order_2026.txt', 'Code Descriptions/icd10cm_codes_2026.txt']
        if not set(required).issubset(names):
            raise ValueError('missing ICD-10-CM archive member')
        order, codes = [archive.read(n).decode('ascii').splitlines() for n in required]
    result, seen, valid = [], set(), {}
    for line in order:
        if len(line) < 77 or len(line) > 400 or any(line[i] != ' ' for i in (5, 13, 15, 76)):
            raise ValueError('malformed ICD-10-CM order row')
        if not re.fullmatch(r'[0-9]{5}', line[:5]):
            raise ValueError('invalid ICD-10-CM order number')
        key = code(line[6:13])
        if key in seen:
            raise ValueError('duplicate ICD-10-CM order code')
        seen.add(key)
        if int(line[:5]) != len(result) + 1:
            raise ValueError('nonsequential ICD-10-CM order number')
        if line[14] not in ('0', '1'):
            raise ValueError('invalid ICD-10-CM flag')
        short = description(line[16:76].rstrip(' '))
        long = description(line[77:])
        flag = line[14] == '1'
        if flag:
            valid[key] = long
        result.append(dict(code=key, display=long, designation=[dict(language='en', value=short)], property=[
            dict(code='cmsOrder', valueInteger=int(line[:5])),
            dict(code='cmsValidForHIPAATransactions', valueBoolean=flag)]))
    if len(result) != 98186:
        raise ValueError('incomplete ICD-10-CM April 2026 release')
    listed = {}
    for line in codes:
        if len(line) < 8 or len(line) > 400 or line[7] != ' ':
            raise ValueError('malformed ICD-10-CM codes row')
        key = code(line[:7])
        if key in listed:
            raise ValueError('duplicate ICD-10-CM codes entry')
        listed[key] = description(line[8:])
    if listed != valid:
        raise ValueError('ICD-10-CM codes/order reconciliation failed')
    if len(result) != 98186 or len(valid) != 74719:
        raise ValueError('incomplete ICD-10-CM April 2026 release')
    return result
