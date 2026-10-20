import copy
import io
import json
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock
import pytest
from botocore.exceptions import ClientError

@pytest.fixture
def settings():
    return json.loads(Path('config.example.json').read_text())

@pytest.fixture
def env(monkeypatch):
    for k in ('PGHOST','PGUSER','PGDATABASE','PGPASSWORD'):
        monkeypatch.setenv(k, 'fixture-secret' if k=='PGPASSWORD' else 'fixture')

@pytest.fixture
def inv():
    return dict(server_version='16.3',server_version_num=160003,
                schemas=[dict(oid=2200,name='public'),dict(oid=50,name='odd schema')],
                tables=[dict(oid=100,schema='public',table='root',kind='p',rls=False,select=True,parents=[]),
                        dict(oid=101,schema='public',table='child',kind='r',rls=False,select=True,parents=[100])])

class MemoryStore:
    def __init__(self):
        self.objects={};self.deleted=[]
        self.client=SimpleNamespace(get_bucket_versioning=lambda **_: {})
        self.bucket='fixture'
    def put(self,key,body,date=None):
        if isinstance(body,dict):body=json.dumps(body).encode()
        self.objects[key]=(body,date or datetime.now(timezone.utc))
    def head(self,key):
        if key not in self.objects:
            raise ClientError({'Error':{'Code':'404'}},'HeadObject')
        return dict(ContentLength=len(self.objects[key][0]))
    def absent(self,key):return key not in self.objects
    def read_manifest(self,key):return json.loads(self.objects[key][0])
    def listing(self,prefix):
        return [dict(Key=k,LastModified=d,Size=len(b)) for k,(b,d) in self.objects.items() if k.startswith(prefix)]
    def delete(self,key):self.deleted.append(key);self.objects.pop(key,None)

@pytest.fixture
def store():return MemoryStore()

def add_run(store,settings,n,date,**overrides):
    run_id='20261020T233000Z-'+format(n,'032x')
    prefix=settings['s3']['prefix']+run_id+'/'
    manifest=dict(format_version=2,run_id=run_id,status='completed',verification_scope='archive_read_and_remote_sha256',database_restore_tested=False,archive_key=prefix+'backup.dump',archive_bytes=4,archive_sha256='a'*64,configuration_fingerprint='f'*64)
    fixture=json.loads(Path('config.example.json').read_text())
    manifest.update(database='fixture',dump_started_at='2026-10-20T00:00:00+00:00',
                    dump_finished_at='2026-10-20T01:00:00+00:00',verification_finished_at='2026-10-20T02:00:00+00:00',
                    image_version='0.2.0',dump_major=16,required_restore_major=16,target_server_major=16,
                    filters=fixture['backup'],checks=dict(toc=dict(table_data_entries=1,required_objects_checked=True),
                    full_archive_read=True,remote_sha256=True),server_version='16.3',
                    pg_dump_version='pg_dump (PostgreSQL) 16.15',pg_restore_version='pg_restore (PostgreSQL) 16.15')
    manifest.update(overrides)
    store.put(prefix+'backup.dump',b'data',date)
    store.put(prefix+'manifest.json',manifest,date)
    return run_id,manifest
