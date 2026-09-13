#!/usr/bin/env python3
import json, os, pathlib, shutil, subprocess, tempfile

ROOT = pathlib.Path('/Users/kunchen/.no-mistakes/worktrees/8c0b202f5740/01M2D0AB1VPEGB1BN6TWSFGCQN')
TMP = pathlib.Path(tempfile.mkdtemp(prefix='treehouse-live-'))
BIN = TMP / 'treehouse'
records = []
results = []

def raw(argv, cwd=None, env=None, stdin=None, check=True):
    merged = os.environ.copy()
    merged.update({'TREEHOUSE_NO_UPDATE_CHECK':'1', 'GIT_AUTHOR_NAME':'Live Test', 'GIT_AUTHOR_EMAIL':'live@example.com', 'GIT_COMMITTER_NAME':'Live Test', 'GIT_COMMITTER_EMAIL':'live@example.com'})
    if env: merged.update(env)
    p = subprocess.run([str(x) for x in argv], cwd=str(cwd or ROOT), env=merged, input=stdin, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and p.returncode != 0:
        raise RuntimeError(f"command failed {argv}: {p.returncode}\nstdout={p.stdout}\nstderr={p.stderr}")
    return p

def drive(label, home, cwd, *args, stdin=None, check=True):
    env = {'HOME':str(home), 'TREEHOUSE_ROOT':str(home/'pools')}
    p = raw([BIN, *args], cwd=cwd, env=env, stdin=stdin, check=check)
    records.append({'label':label, 'cwd':str(cwd), 'command':'treehouse '+' '.join(map(str,args)), 'exit':p.returncode, 'stdout':p.stdout, 'stderr':p.stderr})
    return p

def git(cwd, *args):
    return raw(['git', *args], cwd=cwd).stdout.strip()

def make_repo(base, name='project', remote=None):
    base.mkdir(parents=True, exist_ok=True)
    if remote is None:
        remote = base / 'remote.git'
        git(base, 'init', '--bare', str(remote))
        seed = base / 'seed'
        git(base, 'clone', str(remote), str(seed))
        (seed/'README').write_text('seed\n')
        git(seed, 'add', 'README')
        git(seed, 'commit', '-m', 'seed')
        git(seed, 'branch', '-M', 'main')
        git(seed, 'push', '-u', 'origin', 'main')
        git(remote, 'symbolic-ref', 'HEAD', 'refs/heads/main')
        shutil.rmtree(seed)
    repo = base / name
    git(base, 'clone', str(remote), str(repo))
    return repo, pathlib.Path(remote)

def repo_hooks(repo, marker=None):
    post = f'printf repo-post > {marker}' if marker else './scripts/setup.sh'
    (repo/'treehouse.toml').write_text('[hooks]\npost_create = ['+json.dumps(post)+']\npre_destroy = ["./scripts/teardown.sh"]\n')

def assert_true(v, msg):
    if not v: raise AssertionError(msg)

def acquire(label, home, repo):
    p = drive(label, home, repo, 'get', '--lease')
    path = pathlib.Path(p.stdout.strip())
    assert_true(p.stdout == str(path)+'\n' and path.is_dir(), f'{label}: stdout path contract failed: {p.stdout!r}')
    return path, p

def scenario(name, fn):
    try:
        fn()
        results.append({'name':name,'result':'pass'})
    except Exception as e:
        results.append({'name':name,'result':'fail','error':str(e)})

raw(['go','build','-o',BIN,'.'])

# 1: warning visibility, stderr contract, and repo hooks remain untrusted/ignored.
def s1():
    base=TMP/'s1'; home=base/'home'; repo,_=make_repo(base/'repos'); marker=base/'repo-hook-ran'
    repo_hooks(repo, marker)
    wt,p=acquire('ignored repo hooks warning on get --lease',home,repo)
    for text in ('[hooks]',str(repo/'treehouse.toml'),'post_create','pre_destroy','~/.config/treehouse/config.toml'):
        assert_true(text in p.stderr, f'missing warning detail {text!r}')
    assert_true(str(repo/'treehouse.toml') not in p.stdout and not marker.exists(), 'warning polluted stdout or repo post_create executed')
scenario('get --lease warns on stderr, preserves path-only stdout, and does not execute repository hooks',s1)

# 2: absent/non-hook repository configuration remains quiet.
def s2():
    base=TMP/'s2'; home=base/'home'; repo,_=make_repo(base/'r1')
    _,p1=acquire('no repo config stays quiet',home,repo)
    assert_true('ignored' not in p1.stderr.lower() and '[hooks]' not in p1.stderr, 'unexpected warning without config')
    repo2,_=make_repo(base/'r2')
    (repo2/'treehouse.toml').write_text('max_trees = 4\n')
    _,p2=acquire('repo config without hooks stays quiet',home,repo2)
    assert_true('ignored' not in p2.stderr.lower() and '[hooks]' not in p2.stderr, 'unexpected warning without hooks')
scenario('repositories with no config or no hooks produce no ignored-hooks warning',s2)

# 3: repeated checks in one bulk command dedupe by file and name all guidance.
def s3():
    base=TMP/'s3'; home=base/'home'; repo,_=make_repo(base/'repos')
    w1,_=acquire('dedupe setup lease 1',home,repo); w2,_=acquire('dedupe setup lease 2',home,repo)
    repo_hooks(repo)
    pool=w1.parent.parent
    p=drive('bulk dry-run repeated warning checks',home,base,'destroy',pool,'--all')
    cp=str(repo/'treehouse.toml')
    assert_true(p.stderr.count(cp)==1, f'warning count for config was {p.stderr.count(cp)}')
    for text in ('post_create','pre_destroy','~/.config/treehouse/config.toml'): assert_true(text in p.stderr, f'missing {text}')
    assert_true(cp not in p.stdout, 'warning polluted stdout')
scenario('bulk destroy deduplicates repeated checks while naming ignored keys and the user config destination',s3)

# 4: single target is resolved from its owning repo, not cwd or first shared-pool entry.
def s4():
    base=TMP/'s4'; home=base/'home'; repoA,remote=make_repo(base/'cloneA')
    repoB,_=make_repo(base/'cloneB',remote=remote)
    wa,_=acquire('shared pool clone A lease',home,repoA); wb,_=acquire('shared pool clone B lease',home,repoB)
    repo_hooks(repoA); repo_hooks(repoB)
    outside=base/'outside'; outside.mkdir()
    p=drive('single destroy from outside targets clone B config',home,outside,'destroy',wb)
    assert_true(str(repoB/'treehouse.toml') in p.stderr, 'target repository warning absent')
    assert_true(str(repoA/'treehouse.toml') not in p.stderr, 'wrong shared-pool repository warned')
scenario('destroying a named worktree from outside its repository warns only for that worktree owner',s4)

# 5: bulk target snapshot warns once for every distinct owning repository.
def s5():
    base=TMP/'s5'; home=base/'home'; repoA,remote=make_repo(base/'cloneA'); repoB,_=make_repo(base/'cloneB',remote=remote)
    wa,_=acquire('bulk clone A lease',home,repoA); wb,_=acquire('bulk clone B lease',home,repoB)
    repo_hooks(repoA); repo_hooks(repoB)
    p=drive('shared pool bulk warning dry-run',home,base,'destroy',wa.parent.parent,'--all')
    for repo in (repoA,repoB):
        cp=str(repo/'treehouse.toml'); assert_true(p.stderr.count(cp)==1, f'{cp} warning count {p.stderr.count(cp)}')
        assert_true(cp not in p.stdout, 'warning polluted stdout')
scenario('bulk destroy warns exactly once for each owning repository in its target snapshot',s5)

# 6: malformed repository TOML does not block an actual destroy.
def s6():
    base=TMP/'s6'; home=base/'home'; repo,_=make_repo(base/'repos'); wt,_=acquire('malformed config setup lease',home,repo)
    (repo/'treehouse.toml').write_text('invalid toml <<<\n')
    p=drive('destroy survives malformed repo config',home,base,'destroy',wt,'--include-leased','--yes')
    assert_true(p.returncode==0 and not wt.exists() and 'Destroyed 1 worktree' in p.stdout, 'destroy did not succeed/remove target')
scenario('destroy remains non-fatal and removes its target when repository configuration is malformed',s6)

# 7: user-level pre_destroy runs for each destroy class and not in dry-run.
def s7():
    base=TMP/'s7'; home=base/'home'; repo,_=make_repo(base/'repos'); hooklog=base/'pre-destroy.log'
    cfg=home/'.config/treehouse'; cfg.mkdir(parents=True)
    (cfg/'config.toml').write_text('[hooks]\npre_destroy = ['+json.dumps('pwd >> '+str(hooklog))+']\n')
    # disposable
    disposable,_=acquire('matrix disposable acquire',home,repo)
    drive('matrix disposable release',home,base,'return',disposable)
    drive('matrix disposable destroy',home,base,'destroy',disposable,'--yes')
    # leased
    leased,_=acquire('matrix leased acquire',home,repo)
    drive('matrix leased destroy',home,base,'destroy',leased,'--include-leased','--yes')
    # dirty and idle
    dirty,_=acquire('matrix dirty acquire',home,repo)
    drive('matrix dirty release',home,base,'return',dirty)
    (dirty/'untracked.txt').write_text('dirty\n')
    drive('matrix dirty destroy',home,base,'destroy',dirty,'--include-unlanded','--yes')
    # dry run
    dry,_=acquire('matrix dry-run acquire',home,repo)
    drive('matrix dry-run release',home,base,'return',dry)
    before=hooklog.read_text().splitlines()
    drive('matrix dry-run does not invoke hook',home,base,'destroy',dry)
    after=hooklog.read_text().splitlines()
    assert_true(len(before)==3 and set(before)=={str(disposable),str(leased),str(dirty)}, f'actual hook log wrong: {before}')
    assert_true(after==before and dry.exists(), 'dry-run invoked hook or removed target')
scenario('user pre_destroy runs for disposable, leased, and dirty removals but not for dry-run',s7)

out={'temporary_root':str(TMP),'results':results,'commands':records}
print(json.dumps(out,indent=2))
if any(x['result']!='pass' for x in results): raise SystemExit(1)
shutil.rmtree(TMP)
