#!/usr/bin/env python3
"""Exercise the actual observation CLI with independent ordered corpus fixtures."""
import copy,json,pathlib,subprocess,sys,unittest
SCRIPT=pathlib.Path(__file__).with_name('verify-state.py')
ROWS=['init-pas-request-bundle', 'init-dtr-questionnaireresponse', 'init-pdex-explanationofbenefit', 'init-cdex-task', 'prime-versioned-approved', 'prime-versioned-denied', 'prime-versioned-pended', 'prime-unversioned-approved', 'prime-unversioned-denied', 'prime-unversioned-pended', 'prime-meta-approved', 'prime-meta-denied', 'prime-meta-pended', 'qualify-1-versioned-approved', 'qualify-1-versioned-denied', 'qualify-1-versioned-pended', 'qualify-1-unversioned-approved', 'qualify-1-unversioned-denied', 'qualify-1-unversioned-pended', 'qualify-1-meta-approved', 'qualify-1-meta-denied', 'qualify-1-meta-pended', 'qualify-2-versioned-approved', 'qualify-2-versioned-denied', 'qualify-2-versioned-pended', 'qualify-2-unversioned-approved', 'qualify-2-unversioned-denied', 'qualify-2-unversioned-pended', 'qualify-2-meta-approved', 'qualify-2-meta-denied', 'qualify-2-meta-pended', 'negative-versioned', 'negative-unversioned', 'negative-meta', 'full-response-positive', 'full-response-negative-hcpcs', 'full-response-negative-pos', 'full-response-negative-encounter', 'encounter-positive', 'encounter-target-type', 'explicit-profile-missing-version', 'explicit-profile-missing-canonical']
class StateTests(unittest.TestCase):
 def call(self,mode,line,raw):
  return subprocess.run([sys.executable,str(SCRIPT),mode,line],input=raw,text=True,capture_output=True,timeout=10)
 def state(self,line):return {'schema':2,'line':line,'state':'ready','warm':ROWS[:],'key':'process:boot','fired_at':'0001-01-01T00:00:00Z'}
 def test_ready_exact_all_lines_and_refusals(self):
  for line in ['2.0','2.1','2.2']:
   state=self.state(line);self.assertEqual(self.call('ready',line,json.dumps(state)).returncode,0)
   mutations=[lambda x:x['warm'].pop(),lambda x:x['warm'].pop(0),lambda x:x['warm'].reverse(),lambda x:x['warm'].append(x['warm'][-1]),lambda x:x.update(line='wrong'),lambda x:x.update(schema=1),lambda x:x.update(state='warming'),lambda x:x.update(key=''),lambda x:x.update(row='pending'),lambda x:x.update(failure='failed'),lambda x:x.update(fired_at='2026-01-01T00:00:00Z')]
   for i,mutate in enumerate(mutations):
    bad=copy.deepcopy(state);mutate(bad)
    with self.subTest(line=line,mutation=i):self.assertNotEqual(self.call('ready',line,json.dumps(bad)).returncode,0)
   self.assertNotEqual(self.call('ready',line,'{').returncode,0)
 def log(self,line):return '\n'.join(f'warmup: line={line} row={row} elapsed=1ms outcome={outcome}' for row in ROWS for outcome in ['started','completed'])+'\n'
 def test_logs_exact_all_lines_and_refusals(self):
  for line in ['2.0','2.1','2.2']:
   raw=self.log(line);self.assertEqual(self.call('logs',line,raw).returncode,0)
   lines=raw.splitlines();bad=[lines[:-1],lines[1:],lines+[lines[-1]],list(reversed(lines))]
   for rows in bad:self.assertNotEqual(self.call('logs',line,'\n'.join(rows)).returncode,0)
   for old,new in [('line='+line,'line=wrong'),('elapsed=1ms','elapsed=600s'),('elapsed=1ms','elapsed=invalid'),('outcome=completed','outcome=failed')]:
    with self.subTest(line=line,replacement=new):self.assertNotEqual(self.call('logs',line,raw.replace(old,new,1)).returncode,0)
   self.assertNotEqual(self.call('logs',line,'').returncode,0)
if __name__=='__main__':unittest.main()
