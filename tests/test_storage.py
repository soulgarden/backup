import hashlib,io,json
from unittest.mock import Mock
import boto3
import pytest
from moto import mock_aws
from botocore.exceptions import ClientError
from backup import storage


def response(raw):return dict(Body=io.BytesIO(raw),ContentLength=len(raw))


def test_client(settings):
    s=storage.client(settings['s3'],dict(aws_access_key_id='fixture',aws_secret_access_key='fixture'))
    assert s.meta.config.signature_version=='s3v4'
    assert s.meta.config.request_checksum_calculation=='when_required'
    assert s.meta.config.response_checksum_validation=='when_required'

@pytest.mark.parametrize('size',[100,6*1024**2])
@mock_aws
def test_real_sdk(settings,tmp_path,size):
    c=boto3.client('s3',region_name='us-east-1',aws_access_key_id='test',aws_secret_access_key='test')
    c.create_bucket(Bucket=settings['s3']['bucket'])
    settings['s3']['multipartThresholdMiB']=5;settings['s3']['multipartChunkSizeMiB']=5
    s=storage.Storage(c,settings['s3']);raw=b'a'*size;p=tmp_path/'file';p.write_bytes(raw)
    s.upload(p,'archive');s.verify('archive',size,hashlib.sha256(raw).hexdigest())
    s.publish('manifest',{'a':1});assert s.read_manifest('manifest')=={'a':1}
    assert len(s.listing(''))==2
    assert s.absent('missing');assert not s.absent('archive')
    s.delete('archive');assert s.absent('archive')

@pytest.mark.parametrize('kind',['head','short','long','hash','interrupted'])
def test_verify_fail(settings,kind):
    c=Mock();c.head_object.return_value={'ContentLength':3 if kind=='head' else 4}
    c.get_object.side_effect=lambda **_:response({'short':b'da','long':b'dataextra','hash':b'evil'}.get(kind,b'data'))
    if kind=='interrupted':c.get_object.side_effect=OSError('fixture-secret')
    s=storage.Storage(c,settings['s3'])
    with pytest.raises((ValueError,OSError)):s.verify('key',4,hashlib.sha256(b'data').hexdigest())

def test_get_retry_resets(settings):
    c=Mock();c.head_object.return_value={'ContentLength':4}
    broken=Mock();broken.read.side_effect=[b'da',OSError()]
    c.get_object.side_effect=[dict(Body=broken,ContentLength=4),response(b'data')]
    storage.Storage(c,settings['s3']).verify('k',4,hashlib.sha256(b'data').hexdigest())
    broken.close.assert_called_once()

@pytest.mark.parametrize('match',[True,False])
def test_uncertain_publish(settings,match):
    c=Mock();c.put_object.side_effect=TimeoutError()
    c.get_object.return_value=response(b'{"a":1}' if match else b'{"a":2}')
    s=storage.Storage(c,settings['s3'])
    if match:s.publish('k',{'a':1})
    else:
        with pytest.raises(ValueError):s.publish('k',{'a':1})

@pytest.mark.parametrize('raw,length',[(b'{}',1024**2+1),(b'{}',5)])
def test_manifest_length(settings,raw,length):
    c=Mock();c.get_object.return_value=dict(Body=io.BytesIO(raw),ContentLength=length)
    with pytest.raises(ValueError):storage.Storage(c,settings['s3']).read_manifest('k')

def test_manifest_mismatch(settings):
    c=Mock();c.get_object.return_value=response(b'{}')
    with pytest.raises(ValueError):storage.Storage(c,settings['s3']).publish('k',{'a':1})

def test_absent_denied(settings):
    c=Mock();c.head_object.side_effect=ClientError({'Error':{'Code':'403'}},'HEAD')
    with pytest.raises(ClientError):storage.Storage(c,settings['s3']).absent('k')

def test_mpu_abort(settings,tmp_path):
    c=Mock();c.create_multipart_upload.return_value={'UploadId':'fixture'}
    c.upload_part.side_effect=OSError()
    settings['s3']['multipartThresholdMiB']=5
    p=tmp_path/'big';p.write_bytes(b'a'*(6*1024**2))
    with pytest.raises(OSError):storage.Storage(c,settings['s3']).upload(p,'k')
    c.abort_multipart_upload.assert_called_once()

def test_incomplete_page(settings):
    c=Mock();c.get_paginator.return_value.paginate.return_value=[{'IsTruncated':True,'Contents':[]}]
    with pytest.raises(ValueError):storage.Storage(c,settings['s3']).listing('prefix/')

@pytest.mark.parametrize('operation',['verify_get','publish_put','publish_get'])
def test_cancel_never_retried_or_confirmed(settings,operation):
    from backup.process import Cancelled
    c=Mock();c.head_object.return_value={'ContentLength':4}
    if operation=='verify_get':c.get_object.side_effect=Cancelled()
    if operation=='publish_put':c.put_object.side_effect=Cancelled()
    if operation=='publish_get':c.get_object.side_effect=Cancelled()
    s=storage.Storage(c,settings['s3'])
    with pytest.raises(Cancelled):
        if operation=='verify_get':s.verify('k',4,hashlib.sha256(b'data').hexdigest())
        else:s.publish('k',{'a':1})
    if operation=='publish_put':c.get_object.assert_not_called()
    else:assert c.get_object.call_count==1

def test_bounded_abort_client(settings):
    s=storage.connect(settings['s3'],dict(aws_access_key_id='fixture',aws_secret_access_key='fixture'))
    assert s.abort_client.meta.config.read_timeout==3
    assert s.abort_client.meta.config.connect_timeout==2
    assert s.abort_client.meta.config.retries['total_max_attempts']==1

def test_manifest_readback_preserves_boolean_types(settings):
    c=Mock();c.get_object.return_value=response(b'{"database_restore_tested":0}')
    with pytest.raises(ValueError):storage.Storage(c,settings['s3']).publish('k',{'database_restore_tested':False})

def test_manifest_duplicate_keys(settings):
    c=Mock();c.get_object.return_value=response(b'{"a":1,"a":2}')
    with pytest.raises(ValueError):storage.Storage(c,settings['s3']).read_manifest('k')
