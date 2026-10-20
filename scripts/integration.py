#!/usr/bin/env python3
"""Disposable Docker network; no host PG/Kubernetes/production credentials."""
import argparse,json,os,subprocess,tempfile,time,uuid
from pathlib import Path

root=Path(__file__).resolve().parents[1]
p=argparse.ArgumentParser();p.add_argument('--image',required=True);args=p.parse_args()
run_id='backup-test-'+uuid.uuid4().hex[:10]
containers=[]

def docker(*cmd,check=True,capture=True,input=None):
    result=subprocess.run(['docker',*map(str,cmd)],check=False,input=input,text=True,
                          stdout=subprocess.PIPE if capture else None,stderr=subprocess.PIPE)
    if check and result.returncode:
        print(result.stdout or '',result.stderr or '',flush=True)
        raise RuntimeError('Disposable Docker fixture command failed')
    return result

def wait(name,command,limit=90):
    deadline=time.monotonic()+limit
    while time.monotonic()<deadline:
        if docker('exec',name,*command,check=False).returncode==0:return
        time.sleep(1)
    raise RuntimeError('fixture startup timeout: '+name)

try:
    docker('network','create',run_id)
    for suffix,image,extra in [
        ('pg','postgres:16-alpine',['-e','POSTGRES_PASSWORD=fixture','-e','POSTGRES_DB=fixture','--network-alias','pg']),
        ('moto','motoserver/moto:5.1.15',['--network-alias','moto'])]:
        name=run_id+'-'+suffix;containers.append(name)
        docker('run','-d','--name',name,'--network',run_id,*extra,image)
    pg,moto=containers
    wait(pg,['pg_isready','-h','127.0.0.1','-U','postgres','-d','fixture'])
    wait(moto,['python','-c',"import urllib.request;urllib.request.urlopen('http://localhost:5000')"])
    sql='''CREATE TABLE public."partition root" (id int, value text) PARTITION BY RANGE(id);
CREATE TABLE public."leaf one" PARTITION OF public."partition root" FOR VALUES FROM (0) TO (100000);
INSERT INTO public."partition root" SELECT n, md5(n::text) FROM generate_series(1,40000) n;
CREATE SCHEMA "work schema";
CREATE TABLE "work schema"."quote space.雪"(id int);
CREATE TABLE "work schema"."new
line"(id int);
CREATE SCHEMA release_archive_fixture;
CREATE TABLE release_archive_fixture.old(id int);
CREATE ROLE fixture_reader;
GRANT SELECT ON public."partition root" TO fixture_reader;
SELECT lo_create(50000);
CREATE TABLE public.parent (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY);
INSERT INTO public.parent DEFAULT VALUES;
CREATE TABLE public.child_ref (parent_id int REFERENCES public.parent(id));
CREATE INDEX child_ref_parent_idx ON public.child_ref(parent_id);
INSERT INTO public.child_ref VALUES (1);
CREATE DATABASE fixture_empty;
CREATE EXTENSION pgcrypto;
CREATE TABLE public.payload AS SELECT n, encode(gen_random_bytes(1024),'hex') value FROM generate_series(1,40000) n;
'''
    docker('exec','-i',pg,'psql','-U','postgres','-d','fixture','-v','ON_ERROR_STOP=1',input=sql)
    env=['-e','PGHOST=pg','-e','PGPORT=5432','-e','PGUSER=postgres','-e','PGDATABASE=fixture','-e','PGPASSWORD=fixture']
    work_disk=tempfile.TemporaryDirectory()
    Path(work_disk.name).chmod(0o777)
    checked=docker('run','--rm','--read-only','--cap-drop=ALL','--security-opt=no-new-privileges',
                   '--network',run_id,*env,'--mount',f'type=bind,src={root},dst=/tests,readonly',
                   '--mount',f'type=bind,src={work_disk.name},dst=/work','--memory','384m','--cpus','2','--entrypoint','python',
                   args.image,'/tests/scripts/container_checks.py')
    print(checked.stdout)
    metrics=json.loads(checked.stdout.strip().splitlines()[-1])
    work_disk.cleanup()
    # Real main/PID1 shutdown while pg_dump waits for an exclusive lock.
    docker('exec','-d',pg,'psql','-U','postgres','-d','fixture','-c',
           'BEGIN; LOCK TABLE public."leaf one" IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(120);')
    with tempfile.TemporaryDirectory() as tmp:
        path=Path(tmp);path.chmod(0o755)
        cfg=json.loads((root/'config.example.json').read_text());cfg['process']['shutdownGraceSeconds']=5
        # PID1 should stop before it reaches S3; no network permission to production.
        (path/'config.json').write_text(json.dumps(cfg));(path/'config.json').chmod(0o444)
        for key in ['access_key','secret_key']:(path/key).write_text('fixture');(path/key).chmod(0o444)
        name=run_id+'-signal';containers.append(name)
        docker('run','-d','--name',name,'--network',run_id,*env,
               '--mount',f'type=bind,src={path},dst=/config,readonly','--mount',f'type=bind,src={path},dst=/credentials,readonly',
               '--tmpfs','/work:uid=10001,gid=10001,size=134217728',args.image)
        # Poll child argv, not a guessed timing, before sending SIGTERM.
        wait(name,['sh','-c',"ps -o args | grep -q '[p]g_dump --no-password'"],limit=30)
        started=time.monotonic();docker('stop','--time','10',name)
        result=json.loads(docker('inspect',name).stdout)[0]
        assert result['State']['ExitCode']==143,result['State']
        assert time.monotonic()-started<10
        log=docker('logs',name).stdout
        assert 'backup_completed' not in log and 'CANCELLED' in log
    report={'image':args.image,'pg_major':16,'sql_restore_executed':False,'pid1_sigterm':True,'fixture_tests':True,'container_metrics':metrics}
    (root/'artifacts').mkdir(exist_ok=True)
    (root/'artifacts/integration.json').write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps(report))
finally:
    for name in reversed(containers):docker('rm','-f',name,check=False)
    docker('network','rm',run_id,check=False)
