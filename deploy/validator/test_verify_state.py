#!/usr/bin/env python3
"""Exercise the actual observation CLI with independent ordered corpus fixtures."""
import copy,json,pathlib,subprocess,sys,unittest
SCRIPT=pathlib.Path(__file__).with_name('verify-state.py')
ROWS=['init-pas-request-bundle', 'init-dtr-questionnaireresponse', 'init-cdex-task', 'prime-versioned-approved', 'prime-versioned-denied', 'prime-versioned-pended', 'prime-unversioned-approved', 'prime-unversioned-denied', 'prime-unversioned-pended', 'prime-meta-approved', 'prime-meta-denied', 'prime-meta-pended', 'qualify-1-versioned-approved', 'qualify-1-versioned-denied', 'qualify-1-versioned-pended', 'qualify-1-unversioned-approved', 'qualify-1-unversioned-denied', 'qualify-1-unversioned-pended', 'qualify-1-meta-approved', 'qualify-1-meta-denied', 'qualify-1-meta-pended', 'qualify-2-versioned-approved', 'qualify-2-versioned-denied', 'qualify-2-versioned-pended', 'qualify-2-unversioned-approved', 'qualify-2-unversioned-denied', 'qualify-2-unversioned-pended', 'qualify-2-meta-approved', 'qualify-2-meta-denied', 'qualify-2-meta-pended', 'negative-versioned', 'negative-unversioned', 'negative-meta', 'full-response-positive', 'full-response-negative-hcpcs', 'full-response-negative-pos', 'full-response-negative-encounter', 'encounter-positive', 'encounter-target-type', 'explicit-profile-missing-version', 'explicit-profile-missing-canonical']
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
JAVA=['java','--class-path','/app/main.war','-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes','org.springframework.boot.loader.PropertiesLauncher']
PAS={'2.0':'2.0.1','2.1':'2.1.0','2.2':'2.2.1'}
COMMON=['PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt','LANG=C.UTF-8']
class ImageTests(unittest.TestCase):
 """The built image against the engine image its WAR comes from and the runtime base it runs on.
 The runtime base sets the same variables as the engine's, differing only in the JRE's version
 within its feature release; the image keeps the runtime base's environment."""
 def engine(self,env=None):return [{'Config':{'Entrypoint':JAVA,'User':'65532:65532','WorkingDir':'/app','Env':env if env is not None else COMMON+['JAVA_VERSION=21.0.12.1']}}]
 def runtime(self,env=None):return [{'Config':{'Entrypoint':['/usr/bin/java','-jar'],'User':'65532','WorkingDir':'/home/nonroot','Env':env if env is not None else COMMON+['JAVA_VERSION=21.0.13']}}]
 def built(self,line,env=None):return [{'Config':{'Entrypoint':['/healthcheck','supervise']+JAVA,'User':'65532:65532','WorkingDir':'/app','Env':env if env is not None else COMMON+['JAVA_VERSION=21.0.13','SPRING_AI_MCP_SERVER_ENABLED=false','SHN_IG_LINE='+line,'hapi.fhir.implementationguides.pas.version='+PAS[line]]}}]
 def call(self,line,engine,runtime,built,tmp):
  paths=[]
  for name,doc in [('engine',engine),('runtime',runtime),('built',built)]:
   path=tmp/(name+'.json');path.write_text(json.dumps(doc));paths.append(str(path))
  return subprocess.run([sys.executable,str(SCRIPT),'image',*paths,line],text=True,capture_output=True,timeout=10)
 def run_rows(self,line,rows):
  import tempfile
  with tempfile.TemporaryDirectory() as d:
   tmp=pathlib.Path(d)
   for name,(engine,runtime,built,ok,*why) in rows.items():
    with self.subTest(line=line,row=name):
     r=self.call(line,engine,runtime,built,tmp)
     (self.assertEqual if ok else self.assertNotEqual)(r.returncode,0,r.stderr)
     for w in why:self.assertIn('AssertionError: '+w,r.stderr)
 def env(self,line,drop=(),**set_):
  e=dict(x.split('=',1) for x in self.built(line)[0]['Config']['Env'])
  for k in drop:e.pop(k)
  e.update(set_);return [k+'='+v for k,v in e.items()]
 def test_image_all_lines_and_refusals(self):
  for line in ['2.0','2.1','2.2']:
   b=lambda env=None,**cfg:[{'Config':{**self.built(line,env)[0]['Config'],**cfg}}]
   rows={
    # the runtime base's JRE differs from the engine's: its environment is the image's
    'runtime JRE newer than the engine': (self.engine(),self.runtime(),b(),True),
    'runtime and engine agree': (self.engine(COMMON+['JAVA_VERSION=21.0.13']),self.runtime(),b(),True),
    'image claims the engine JRE, not the one it runs': (self.engine(),self.runtime(),b(self.env(line,JAVA_VERSION='21.0.12.1')),False),
    'runtime value dropped': (self.engine(),self.runtime(),b(self.env(line,drop=['JAVA_VERSION'])),False),
    'runtime value changed': (self.engine(),self.runtime(),b(self.env(line,LANG='C')),False),
    'engine sets a variable the runtime does not': (self.engine(COMMON+['JAVA_VERSION=21.0.12.1','HAPI_ONLY=1']),self.runtime(),b(self.env(line,HAPI_ONLY='1')),False),
    'wrong line': (self.engine(),self.runtime(),b(self.env(line,SHN_IG_LINE='9.9')),False),
    'wrong PAS version': (self.engine(),self.runtime(),b(self.env(line,**{'hapi.fhir.implementationguides.pas.version':'0.0.0'})),False),
    'engine argv changed': ([{'Config':{**self.engine()[0]['Config'],'Entrypoint':['java','-jar','/app/main.war']}}],self.runtime(),b(),False),
    'image argv not the engine argv': (self.engine(),self.runtime(),b(Entrypoint=['/healthcheck','supervise','java','-jar']),False),
    'image user changed': (self.engine(),self.runtime(),b(User='0:0'),False),
    'image workdir changed': (self.engine(),self.runtime(),b(WorkingDir='/'),False),
    'appended command': (self.engine(),self.runtime(),b(Cmd=['--debug']),False),
    # only the JRE's version may differ between the engine's base and the runtime base, and only within its feature release
    'runtime on another Java feature release': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=17.0.1']),b(self.env(line,JAVA_VERSION='17.0.1')),False),
    'runtime PATH differs from the engine': (self.engine(),self.runtime(['PATH=/x']+COMMON[1:]+['JAVA_VERSION=21.0.13']),b(self.env(line,PATH='/x')),False),
    'runtime LANG differs from the engine': (self.engine(),self.runtime(COMMON[:2]+['LANG=C','JAVA_VERSION=21.0.13']),b(self.env(line,LANG='C')),False),
    'runtime without a JRE version': (self.engine(),self.runtime(COMMON),b(self.env(line,drop=['JAVA_VERSION'])),False),
    'neither states a JRE version': (self.engine(COMMON),self.runtime(COMMON),b(self.env(line,drop=['JAVA_VERSION'])),False,'JRE version not stated'),
    'unreadable JRE version': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=unknown']),b(self.env(line,JAVA_VERSION='unknown')),False,'unreadable JRE version'),
    'runtime sets a variable the engine does not': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=21.0.13','JAVA_TOOL_OPTIONS=-Xmx1m']),b(self.env(line,JAVA_TOOL_OPTIONS='-Xmx1m')),False),
    'runtime JRE version with a build suffix': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=21.0.13+9']),b(self.env(line,JAVA_VERSION='21.0.13+9')),True),
    'runtime JRE version in the GA form': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=21+35']),b(self.env(line,JAVA_VERSION='21+35')),True),
    'runtime JRE version in the jdk- form': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=jdk-21.0.13+9']),b(self.env(line,JAVA_VERSION='jdk-21.0.13+9')),True),
    'runtime on a feature release sharing a prefix': (self.engine(),self.runtime(COMMON+['JAVA_VERSION=210.0.1']),b(self.env(line,JAVA_VERSION='210.0.1')),False),
   }
   self.run_rows(line,rows)
if __name__=='__main__':unittest.main()
