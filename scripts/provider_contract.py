"""Mandatory release gate. Dedicated DigitalOcean fixture bucket ONLY."""
import argparse,hashlib,json,os,sys,tempfile,uuid
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import boto3,botocore
from botocore.exceptions import ClientError
from backup import config,storage

p=argparse.ArgumentParser()
p.add_argument('--bucket',required=True)
p.add_argument('--credentials-dir',required=True)
p.add_argument('--canary-bucket',required=True)
p.add_argument('--scope-evidence',required=True)
p.add_argument('--report',required=True)
a=p.parse_args()
if not a.bucket.startswith('backup-fixture-') or a.bucket in ('swup-backups','kino-backups'):
    raise SystemExit('Only a dedicated backup-fixture-* bucket is allowed')
if not a.canary_bucket.startswith('backup-fixture-') or a.canary_bucket==a.bucket:
    raise SystemExit('A separate provisioned fixture canary bucket is required')
evidence=json.loads(Path(a.scope_evidence).read_text())
if evidence.get('bucket')!=a.bucket or evidence.get('access')!='Read/Write/Delete' or evidence.get('limited_access') is not True:
    raise SystemExit('Provider-side limited key scope evidence is required')
c=json.loads(Path(__file__).resolve().parents[1].joinpath('config.example.json').read_text())['s3']
c.update(bucket=a.bucket,prefix='contract/'+uuid.uuid4().hex+'/',multipartThresholdMiB=5,multipartChunkSizeMiB=5)
s=storage.connect(c,config.credentials(a.credentials_dir))
try:
    s.client.list_objects_v2(Bucket=a.canary_bucket,MaxKeys=1)
except ClientError as error:
    assert error.response['Error']['Code'] in ('AccessDenied','403'), 'Canary must return AccessDenied'
else:
    raise SystemExit('Fixture key can read a different canary bucket; scope gate failed')
assert not s.client.get_bucket_versioning(Bucket=s.bucket).get('Status')
try:
    with tempfile.TemporaryDirectory() as work:
        for name,size in [('single',1024),('multipart',11*1024**2)]:
            path=Path(work)/name;path.write_bytes(os.urandom(size))
            s.upload(path,c['prefix']+name)
            s.verify(c['prefix']+name,size,hashlib.sha256(path.read_bytes()).hexdigest())
        for n in range(1001):s.client.put_object(Bucket=s.bucket,Key=c['prefix']+f'pages/{n}',Body=b'x')
        assert len(s.listing(c['prefix']))==1003
        actual=s.client.upload_part
        calls=0
        def fail(**kw):
            global calls
            calls+=1
            if calls==2:raise OSError('injected_failure')
            return actual(**kw)
        s.client.upload_part=fail
        try:s.upload(Path(work)/'multipart',c['prefix']+'abort')
        except OSError:pass
        else:raise AssertionError('missing fault injection')
        finally:s.client.upload_part=actual
        uploads=s.client.list_multipart_uploads(Bucket=s.bucket,Prefix=c['prefix']).get('Uploads',[])
        assert not uploads
    report=dict(bucket=a.bucket,endpoint=c['endpoint'],sdk=boto3.__version__,botocore=botocore.__version__,
                request_checksum=c['requestChecksumCalculation'],response_checksum=c['responseChecksumValidation'],
                single_put=True,multipart=True,full_sha256=True,pagination=1003,get_versioning=True,mpu_abort=True,
                canary_access_denied=True,canary_bucket=a.canary_bucket,scope_evidence_sha256=hashlib.sha256(Path(a.scope_evidence).read_bytes()).hexdigest())
finally:
    for obj in s.listing(c['prefix']):s.delete(obj['Key'])
assert not s.listing(c['prefix'])
report['fixture_cleanup']=True
Path(a.report).write_text(json.dumps(report,indent=2)+'\n')
print('DigitalOcean fixture contract passed')
