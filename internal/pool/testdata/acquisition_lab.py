"""Private local-repository CLI regression lab. No production pool is opened."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time

PARSER = argparse.ArgumentParser()
PARSER.add_argument('binary')
PARSER.add_argument('--base', action='store_true')
PARSER.add_argument('--evidence')
PARSER.add_argument('--only', choices=['all','same','foreign','concurrent','safety','cache-failure'], default='all')
ARGS = PARSER.parse_args()
BINARY = str(Path(ARGS.binary).resolve())
REAL_GIT = shutil.which('git')
ROOT = Path(tempfile.mkdtemp(prefix='treehouse-acquisition-lab-')).resolve()
REPORT = []


def record(message):
    print(message, flush=True)
    REPORT.append(message)


def git(repo, *args, output=False):
    result = subprocess.run([REAL_GIT, '-C', str(repo), *args], check=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    return result.stdout.strip() if output else None


SHIM = r'''import fcntl,json,os,socket,subprocess,sys,time
args=sys.argv[1:]; cwd=os.getcwd(); root=os.environ['LAB_ROOT']; run=os.environ.get('LAB_ID','single')
f=open(os.environ['LAB_LOCK'],'a+')
try:
 fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB); status='FREE'; fcntl.flock(f,fcntl.LOCK_UN)
except BlockingIOError: status='LOCKED'
event=dict(status=status,cwd=cwd,args=args,run=run)
if 'ls-tree' in args and '-r' in args:
 ref=args[-1]
 result=subprocess.run([os.environ['LAB_REAL_GIT'],'-C',cwd,'rev-parse','--verify',ref+'^{tree}'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
 event['tree']=result.stdout.strip()
with open(os.environ['LAB_LOG'],'a') as log: log.write(json.dumps(event)+'\n')
if os.environ.get('LAB_FAIL_TREE') and event.get('tree')==os.environ['LAB_FAIL_TREE']: sys.exit(1)
kind=os.environ.get('LAB_HOLD',''); target=os.environ.get('LAB_TARGET','')
match=(kind=='selection' and cwd==target and 'status' in args or
       kind=='reset' and cwd==target and 'read-tree' in args or
       kind=='foreign-selection' and cwd==target and 'ls-files' in args and '-v' in args or
       kind=='foreign-remove' and 'worktree' in args and 'remove' in args and target in args)
if match:
 marker=os.path.join(root,'held-'+run)
 try:
  fd=os.open(marker,os.O_CREAT|os.O_EXCL|os.O_WRONLY); os.close(fd)
 except FileExistsError: pass
 else:
  # A shim awaiting the controller is not a tenant of the candidate checkout.
  os.chdir(root)
  s=socket.socket(socket.AF_UNIX); s.connect(os.environ['LAB_SOCKET']); s.sendall(b'H'); s.recv(1); s.close()
  os.chdir(cwd)
if os.environ.get('LAB_DELAY'): time.sleep(float(os.environ['LAB_DELAY']))
os.execv(os.environ['LAB_REAL_GIT'],[os.environ['LAB_REAL_GIT']]+args)
'''


class Fixture:
    serial = 0

    def __init__(self, count, foreign=False, rejected=0):
        Fixture.serial += 1
        self.root = ROOT / str(Fixture.serial)
        self.root.mkdir()
        self.repo = self.root / 'a' / 'repo'
        self.repo.mkdir(parents=True)
        git(self.repo, 'init', '-b', 'main')
        git(self.repo, 'config', 'user.email', 'fixture@example.invalid')
        git(self.repo, 'config', 'user.name', 'Fixture')
        (self.repo / 'README').write_text('recognizable original\n')
        (self.repo / '.gitignore').write_text('ignored.log\n')
        git(self.repo, 'add', '.')
        git(self.repo, 'commit', '-m', 'base')
        remote = self.root / 'remote.git'
        git(self.repo, 'clone', '--bare', str(self.repo), str(remote))
        git(self.repo, 'remote', 'add', 'origin', str(remote))
        git(self.repo, 'fetch', 'origin')
        self.caller = self.repo
        if foreign:
            self.caller = self.root / 'b' / 'repo'
            git(self.repo, 'clone', str(remote), str(self.caller))
        self.env = os.environ.copy()
        self.env.update(TREEHOUSE_ROOT=str(self.root / 'pools'), TREEHOUSE_VCS='git',
                        HOME=str(self.root / 'home'), XDG_CONFIG_HOME=str(self.root / 'config'))
        (self.root / 'home').mkdir()
        self.pool = Path(self.command('pool-dir').stdout.strip())
        self.pool.mkdir(parents=True, exist_ok=True)
        self.paths = []
        entries = []
        for n in range(1, count + 1):
            path = self.pool / str(n) / 'repo'
            git(self.repo, 'worktree', 'add', '--detach', str(path), 'main')
            self.paths.append(str(path))
            if n <= rejected:
                (path / 'README').write_text('unlanded unique content '+str(n)+'\n')
                git(path, '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'add', 'README')
                git(path, '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '-m', 'unlanded')
            entries.append(dict(name=str(n), path=str(path), created_at='2026-01-01T00:00:00Z'))
        (self.pool / 'treehouse-state.json').write_text(json.dumps(dict(worktrees=entries)))
        self.log = self.root / 'git.jsonl'
        shim = self.root / 'bin'
        shim.mkdir()
        (shim / 'git').write_text('#!'+sys.executable+'\n'+SHIM)
        (shim / 'git').chmod(0o755)
        # Unix socket paths have a small fixed limit, independent of pool layout.
        self.socket_path = self.root / 'sync'
        self.env.update(PATH=str(shim)+os.pathsep+self.env['PATH'], LAB_ROOT=str(self.root),
                        LAB_LOCK=str(self.pool / 'treehouse-state.lock'), LAB_LOG=str(self.log),
                        LAB_REAL_GIT=REAL_GIT, LAB_SOCKET=str(self.socket_path))
        (self.caller / 'treehouse.toml').write_text('max_trees = '+str(count)+'\n')
        # The caller's config is untracked only in the main checkout, never a slot.

    def command(self, *args, **kw):
        return subprocess.run([BINARY, *args], cwd=self.caller, env=self.env,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, **kw)

    def start(self, label, hold='', path=''):
        return subprocess.Popen([BINARY, 'get', '--lease', '--no-fetch', '--lease-holder', label],
                                cwd=self.caller, env=dict(self.env, LAB_ID=label, LAB_HOLD=hold, LAB_TARGET=path),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def finish(self, proc):
        out, err = proc.communicate()
        assert proc.returncode == 0, (proc.returncode, out, err)
        return out.strip()

    def state(self):
        return json.loads((self.pool / 'treehouse-state.json').read_text())['worktrees']

    def locked(self):
        with open(self.pool / 'treehouse-state.lock', 'a+') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                fcntl.flock(lock, fcntl.LOCK_UN)
                return False
            except BlockingIOError:
                return True

    def events(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]


def concurrent(kind, count, spare):
    foreign = kind.startswith('foreign')
    f = Fixture(count + spare, foreign=foreign)
    server = socket.socket(socket.AF_UNIX)
    server.bind(str(f.socket_path)); server.listen()
    processes = []
    connection = None
    try:
        first = f.start('first', kind, f.paths[0]); processes.append(first)
        connection, _ = server.accept()
        assert connection.recv(1) == b'H'
        held = f.locked()
        assert held == ARGS.base, (kind, 'pool lock held at stalled check', held)
        others = [f.start('other-'+str(i)) for i in range(count-1)]
        processes.extend(others)
        paths = []
        if not ARGS.base:
            paths = [f.finish(proc) for proc in others]
            assert first.poll() is None, 'first was not still held'
            record('TG2 '+kind+' '+str(count)+': other gets completed while first held: '+repr(paths))
        else:
            record('TG2 BASE '+kind+': kernel lock excludes second get until release')
        connection.sendall(b'R'); connection.close(); connection = None
        paths.append(f.finish(first))
        if ARGS.base:
            paths.extend(f.finish(proc) for proc in others)
        assert len(set(paths)) == count, paths
        reserved = {entry['path']:entry for entry in f.state() if entry.get('leased')}
        assert set(paths) <= reserved.keys(), ('lost persisted reservation', paths, reserved)
        assert all(reserved[path].get('lease_id') for path in paths)
        further = f.command('get', '--lease', '--no-fetch')
        if spare:
            assert further.returncode == 0 and further.stdout.strip() not in paths, (further.stdout, further.stderr)
        else:
            assert further.returncode != 0 and not further.stdout.strip(), (further.stdout, further.stderr)
        record('TG2 '+kind+': distinct allocations and persisted leases retained; spare='+str(spare)+'; fixture='+str(f.root))
    finally:
        if connection is not None:
            connection.sendall(b'R'); connection.close()
        server.close()
        for proc in processes:
            if proc.poll() is None:
                proc.kill()
            proc.communicate()


def exclusive(kind):
    f = Fixture(1, foreign=kind.startswith('foreign'))
    server=socket.socket(socket.AF_UNIX);server.bind(str(f.socket_path));server.listen()
    first=f.start('exclusive-first',kind,f.paths[0]);connection=None
    try:
        connection,_=server.accept();assert connection.recv(1)==b'H'
        assert f.locked()==ARGS.base
        if not ARGS.base:
            refused=f.command('get','--lease','--no-fetch')
            assert refused.returncode!=0 and not refused.stdout.strip(), ('claimed only capacity was handed out',refused.stdout,refused.stderr)
        connection.sendall(b'R');connection.close();connection=None
        path=f.finish(first)
        if ARGS.base:
            refused=f.command('get','--lease','--no-fetch')
            assert refused.returncode!=0 and not refused.stdout.strip()
        state=f.state();assert len(state)==1 and state[0]['path']==path and state[0].get('leased')
        record('TG2 '+kind+' only capacity fenced/reserved, independent capacity absent: refusal preserves first lease; fixture='+str(f.root))
    finally:
        if connection is not None: connection.sendall(b'R');connection.close()
        server.close()
        if first.poll() is None:first.kill()
        first.communicate()


def walk(count=90, foreign=False):
    f = Fixture(count, foreign=foreign, rejected=count-1)
    started = time.monotonic()
    f.env['LAB_DELAY'] = '0.001'
    result = f.command('get', '--lease', '--no-fetch')
    elapsed = time.monotonic()-started
    assert result.returncode == 0, result.stderr
    events = f.events()
    candidates = [event for event in events if event['cwd'] in f.paths]
    assert candidates, 'empty candidate trace'
    locked = [event for event in candidates if event['status'] == 'LOCKED']
    assert bool(locked) == ARGS.base, ('TG1 unexpected candidate lock', locked[:3])
    visited = []
    for event in candidates:
        if 'status' in event['args'] and event['cwd'] not in visited:
            visited.append(event['cwd'])
    if not foreign:
        assert visited == f.paths, ('walk did not visit ordered candidates',visited)
        trees = {}
        for event in candidates:
            if event.get('tree'):
                trees[event['tree']] = trees.get(event['tree'], 0)+1
        assert trees
        if not ARGS.base:
            assert max(trees.values()) <= 1, ('TG3 duplicate immutable tree listing',trees)
        else:
            assert max(trees.values()) >= count-1, ('baseline did not discriminate',trees)
        record('TG3 ordered candidates='+str(len(visited))+' tree counts='+repr(trees)+' fixture='+str(f.root))
    else:
        assert any('rev-list' in e['args'] for e in events), 'foreign final-removal proof unvisited'
        assert sum('ls-files' in e['args'] for e in candidates) >= 20, 'foreign rejection walk unvisited'
    record('TG1 '+('foreign' if foreign else 'same-clone')+' candidate calls='+str(len(candidates))+' LOCKED='+str(len(locked))+' log='+str(f.log))
    record('TGS1 '+('foreign' if foreign else 'same-clone')+' count='+str(count)+' fixed git delay=0.001s elapsed='+str(elapsed))


def failed_tree():
    f=Fixture(21,rejected=20)
    tree=git(f.repo,'rev-parse','HEAD^{tree}',output=True)
    f.env['LAB_FAIL_TREE']=tree
    result=f.command('get','--lease','--no-fetch')
    assert result.returncode==0 and result.stdout.strip()==f.paths[-1], ('clean ancestor must remain usable after failed content proof',result.stdout,result.stderr)
    calls=sum(event.get('tree')==tree for event in f.events())
    expected=20 if ARGS.base else 1
    assert calls==expected, ('TG3 failed immutable listing repeated',calls,expected)
    for i,path in enumerate(f.paths[:-1],1):
        assert (Path(path)/'README').read_text()=='unlanded unique content '+str(i)+'\n'
    record('TG3 failed immutable tree listing calls='+str(calls)+'; clean ancestor allocated; unlanded contents preserved; fixture='+str(f.root))


def safety():
    for risk in ['clean','ancestor','squash','leased','owner','process','tracked','untracked','ignored','unlanded','head-lock','foreign-clean','foreign-ignored']:
        f = Fixture(1 if risk.startswith('foreign') else 2, foreign=risk.startswith('foreign'))
        path = Path(f.paths[0]); marker = path / 'README'
        expected = f.paths[0] if risk in ['clean','ancestor','squash','ignored','foreign-clean'] else (None if risk=='foreign-ignored' else f.paths[1])
        if risk=='foreign-clean': expected=str(f.pool/'2'/'repo')
        tenant = None
        if risk in ['ancestor','squash']:
            if risk=='squash':
                marker.write_text('squashed content\n');git(path,'add','README');git(path,'-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-m','feature')
            (f.repo/'README').write_text('squashed content\n' if risk=='squash' else 'advanced base\n')
            git(f.repo,'add','README');git(f.repo,'commit','-m','landed independently')
        elif risk=='owner':
            tenant=subprocess.Popen([BINARY,'get','--no-fetch'],cwd=f.caller,env=dict(f.env,SHELL='/bin/sh'),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
            tenant.stdin.write("printf 'OWNER_READY\\n'\nread value\n");tenant.stdin.flush()
            assert tenant.stdout.readline().strip()=='OWNER_READY'
            assert f.state()[0].get('owner_pid')==tenant.pid
        elif risk=='process':
            tenant=subprocess.Popen([sys.executable,'-c',"import sys; print('TENANT_READY',flush=True); sys.stdin.read()"],cwd=path,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
            assert tenant.stdout.readline().strip()=='TENANT_READY'
        elif risk=='leased':
            result=f.command('lease','1','--lease-holder','existing lease'); assert result.returncode==0,result.stderr
        elif risk=='tracked': marker.write_text('recognizable dirty checkout\n')
        elif risk=='untracked': (path/'notes').write_text('recognizable untracked\n')
        elif risk in ['ignored','foreign-ignored']: (path/'ignored.log').write_text('recognizable unmanaged ignored\n')
        elif risk=='unlanded':
            marker.write_text('recognizable unlanded commit\n');git(path,'add','README');git(path,'-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-m','unlanded')
        elif risk=='head-lock':
            head=git(path,'rev-parse','--path-format=absolute','--git-path','HEAD',output=True)
            Path(head+'.lock').write_text('another writer\n')
        try:
            result=f.command('get','--lease','--no-fetch')
            if expected is None:
                assert result.returncode!=0 and path.exists(), (risk,result.stdout,result.stderr)
            else:
                assert result.returncode==0 and result.stdout.strip()==expected, (risk,result.stdout,result.stderr)
            if risk in ['ignored','foreign-ignored']:
                assert (path/'ignored.log').read_text()=='recognizable unmanaged ignored\n'
            if risk=='tracked': assert marker.read_text()=='recognizable dirty checkout\n'
            if risk=='foreign-clean':
                assert git(Path(result.stdout.strip()),'rev-parse','--path-format=absolute','--git-common-dir',output=True)==git(f.caller,'rev-parse','--path-format=absolute','--git-common-dir',output=True)
            record('TG4 '+risk+' expected='+repr(expected)+' observed='+repr(result.stdout.strip())+' fixture='+str(f.root))
        finally:
            if tenant is not None:
                if risk=='owner':
                    tenant.stdin.write('release\nexit\n');tenant.stdin.flush()
                    tenant.stdin.close();tenant.stdin=None
                else:
                    tenant.kill()
                tenant.communicate()


try:
    record('mode='+('base' if ARGS.base else 'head')+' binary='+BINARY+' private root='+str(ROOT))
    if ARGS.only in ['all','same']: walk()
    if ARGS.only in ['all','foreign']: walk(21, foreign=True)
    if ARGS.only in ['all','concurrent']:
        for kind in ['selection','reset','foreign-selection','foreign-remove']:
            concurrent(kind,2,0)
        concurrent('selection',3,0)
        concurrent('selection',8,1)
        exclusive('selection')
        exclusive('foreign-selection')
    if ARGS.only in ['all','safety']: safety()
    if ARGS.only in ['all','cache-failure']: failed_tree()
finally:
    if ARGS.evidence:
        Path(ARGS.evidence).write_text('\n'.join(REPORT)+'\n')
