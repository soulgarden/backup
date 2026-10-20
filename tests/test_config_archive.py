import copy,json,os,subprocess,sys
from pathlib import Path
from unittest.mock import Mock
import pytest
from backup import archive,config
from backup.process import Processes,Cancelled


def test_config(settings,tmp_path,env):
    path=tmp_path/'config.json';path.write_text(json.dumps(settings))
    assert config.load(path)==settings
    assert config.pg_environment(settings)['PGCONNECT_TIMEOUT']=='10'
    for name in ('access_key','secret_key'): (tmp_path/name).write_text('test-secret\n')
    assert config.credentials(tmp_path)['aws_session_token'] is None
    (tmp_path/'session_token').write_text('token')
    assert config.credentials(tmp_path)['aws_session_token']=='token'
    (tmp_path/'access_key').write_text('')
    with pytest.raises(ValueError): config.credentials(tmp_path)

@pytest.mark.parametrize('section,key,value',[
 ('s3','verifyRemoteSha256',False),('backup','readEntireArchive',False),
 ('s3','host_bucket','legacy'),('retention','keepLast',0),('retention','maxAgeDays',0),
 ('backup','compressionLevel',10),('backup','includeSchemas','public'),
 ('s3','prefix','/'),('s3','prefix','a/../b/'),('s3','prefix','a//b/'),
 ('s3','endpoint','https://user:password@example.com'),('s3','endpoint','http://example.com'),
 ('postgresql','majorVersion',18),('retention','dryRun',False)])
def test_invalid(settings,section,key,value):
    settings[section][key]=value
    with pytest.raises(ValueError):config.validate(settings)

@pytest.mark.parametrize('prefix,pattern',[('','^x$'),('x/','x'),('x/','^x')])
def test_legacy_scope(settings,prefix,pattern):
    settings['retention']['legacy'].update(enabled=True,prefix=prefix,filenamePattern=pattern)
    with pytest.raises(ValueError):config.validate(settings)

def test_missing_pg(settings,monkeypatch):
    monkeypatch.delenv('PGHOST',raising=False)
    with pytest.raises(ValueError):config.pg_environment(settings)

def test_args(settings):
    b=settings['backup'];b.update(includeSchemas=['-odd','space schema','$(touch /tmp/no)'],excludeSchemas=['release_archive_*'],excludeTables=['a.b'],excludeTableData=['a.c'],includeAcl=False,includeLargeObjects=False)
    args=archive.dump_args(settings,'/work/a')
    assert '--schema=-odd' in args and '--schema=$(touch /tmp/no)' in args
    assert '--no-large-objects' in args and '--no-acl' in args
    b.update(includeSchemas=[],strictNames=False)
    assert not any(x.startswith('--schema=') for x in archive.dump_args(settings,'x'))
    assert '--strict-names' not in archive.dump_args(settings,'x')

def test_versions(settings,inv):
    p=Mock();p.run.side_effect=['pg_dump (PostgreSQL) 16.5','pg_restore (PostgreSQL) 16.5']
    assert archive.versions(p,settings,inv)['server_version']=='16.3'
    for version in ['17.1','18.0','garbage']:
        p.run.side_effect=None;p.run.return_value=f'pg_dump (PostgreSQL) {version}'
        with pytest.raises(ValueError):archive.versions(p,settings,inv)
    inv['server_version_num']=170001
    with pytest.raises(ValueError):archive.versions(p,settings,inv)

def test_inventory(inv):
    p=Mock();p.run.return_value=json.dumps(inv)
    assert archive.inventory(p,{})==inv

TOC='0; 2615 2200 SCHEMA - public owner\n1; 2615 50 SCHEMA odd schema owner\n2; 1259 100 TABLE public root owner\n3; 1259 101 TABLE public child owner\n4; 0 101 TABLE DATA public child owner\n'

def test_toc(settings,inv,tmp_path):
    policy=dict(requiredSchemas=['public','odd schema'],requiredTables=[dict(schema='public',table='root',requireData=True)],minTableDataEntries=1)
    assert archive.validate_toc(TOC,inv,inv,policy)['table_data_entries']==1
    policy['requiredTables'][0]['requireData']=False
    archive.validate_toc(TOC,inv,inv,policy)
    p=tmp_path/'a';p.write_bytes(b'data')
    assert archive.hash_file(p,2)[0]==4
    assert archive.toc_entries('1; 0 42 TABLE DATA schema "a\nnewline" owner\n')==[(0,42,'TABLE DATA')]

@pytest.mark.parametrize('change',['ddl','minimum','schema','table','data','rls','permissions'])
def test_toc_fail(inv,change):
    after=copy.deepcopy(inv);text=TOC
    policy=dict(requiredSchemas=['public'],requiredTables=[dict(schema='public',table='root',requireData=True)],minTableDataEntries=0)
    if change=='ddl':after['tables'][1]['oid']=102
    if change=='minimum':policy['minTableDataEntries']=5
    if change=='schema':policy['requiredSchemas']=['missing']
    if change=='table':policy['requiredTables'][0]['table']='missing'
    if change=='data':text=text.replace('TABLE DATA','OTHER')
    if change in ('rls','permissions'):
        inv['tables'][1]['rls']=change=='rls';inv['tables'][1]['select']=False;after=copy.deepcopy(inv)
    with pytest.raises(ValueError):archive.validate_toc(text,inv,after,policy)

def test_process(tmp_path):
    p=Processes(5)
    assert p.run([sys.executable,'-c','print("ok")'])=='ok\n'
    with (tmp_path/'out').open('wb') as out:p.run([sys.executable,'-c','print("ok")'],output=out)
    with pytest.raises(RuntimeError):p.run([sys.executable,'-c','raise SystemExit(2)'])
    p.cancelled=True
    with pytest.raises(Cancelled):p.run(['true'])
    p.stop()

def test_foreign_data_required(settings,inv):
    inv['tables'][1]['kind']='f'
    policy=dict(requiredSchemas=[],requiredTables=[dict(schema='public',table='root',requireData=True)],minTableDataEntries=0)
    with pytest.raises(ValueError):archive.validate_toc(TOC,inv,inv,policy)

@pytest.mark.parametrize('field,value',[('minSamples',10),('minRatio',3)])
def test_invalid_size_policy(settings,field,value):
    settings['observability']['sizeAnomaly'][field]=value
    with pytest.raises(ValueError):config.validate(settings)

def test_toc_descriptor_ambiguity():
    text='1; 1259 42 TABLE DATA named_table owner\n2; 0 42 TABLE DATA DATA named_table owner\n3; 0 0 TABLE ATTACH DATA named_table owner\n4; 1259 43 FOREIGN TABLE DATA foreign owner\n'
    assert archive.toc_entries(text)==[(1259,42,'TABLE'),(0,42,'TABLE DATA'),(1259,43,'TABLE')]
