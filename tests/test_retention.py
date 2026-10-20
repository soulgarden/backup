import copy,json
from datetime import datetime,timedelta,timezone
from unittest.mock import Mock
import pytest
from backup import retention
from conftest import add_run

NOW=datetime(2026,10,20,tzinfo=timezone.utc)

def setup_runs(store,settings,count=9):
    settings['retention'].update(enabled=True,dryRun=False,deletionApproved=True)
    return [add_run(store,settings,n,NOW-timedelta(days=400+n))[0] for n in range(count)]

@pytest.mark.parametrize('age,expected',[(364,False),(365,False),(365+1/86400,True)])
def test_age_boundary(settings,store,age,expected):
    runs=setup_runs(store,settings)
    run,manifest=add_run(store,settings,90,NOW-timedelta(days=age))
    # Keep the seven newest, plus the explicitly protected current run.
    settings['retention']['keepLast']=1
    current,_=add_run(store,settings,100,NOW)
    retention.rotate(store,settings,current,NOW)
    assert (manifest['archive_key'] in store.deleted)==expected

def test_last_seven(settings,store):
    runs=setup_runs(store,settings)
    retention.rotate(store,settings,runs[0],NOW)
    assert len(store.deleted)==4
    assert store.deleted[0].endswith('manifest.json')
    for run in runs[:7]:assert settings['s3']['prefix']+run+'/manifest.json' in store.objects

@pytest.mark.parametrize('count',[1,6,7])
def test_minimum_reserve(settings,store,count):
    runs=setup_runs(store,settings,count)
    assert retention.rotate(store,settings,runs[0],NOW)==[]
    assert store.deleted==[]

@pytest.mark.parametrize('failure',['future','json','scope','missing_archive','size','read','layout','listed404','unknown_hash'])
def test_ambiguous_no_delete(settings,store,failure):
    runs=setup_runs(store,settings)
    marker=settings['s3']['prefix']+runs[-1]+'/manifest.json'
    archive=marker.replace('manifest.json','backup.dump')
    m=store.read_manifest(marker)
    if failure=='future':m['format_version']=3
    if failure=='scope':m['verification_scope']='other'
    if failure=='unknown_hash':m['archive_sha256']='missing'
    if failure=='size':m['archive_bytes']=999
    store.put(marker,m,NOW-timedelta(days=800))
    if failure=='json':store.put(marker,b'{invalid',NOW-timedelta(days=800))
    if failure=='missing_archive':store.objects.pop(archive)
    if failure=='read':store.read_manifest=Mock(side_effect=TimeoutError())
    if failure=='listed404':store.head=Mock(side_effect=TimeoutError())
    if failure=='layout':store.put(settings['s3']['prefix']+'unknown/file',b'a')
    with pytest.raises(Exception):retention.rotate(store,settings,runs[0],NOW)
    assert store.deleted==[]

@pytest.mark.parametrize('versioning',['Enabled','Suspended'])
def test_versioning(settings,store,versioning):
    runs=setup_runs(store,settings)
    store.client.get_bucket_versioning=lambda **_:dict(Status=versioning)
    with pytest.raises(ValueError):retention.rotate(store,settings,runs[0],NOW)
    assert store.deleted==[]

@pytest.mark.parametrize('dry,legacydry',[(True,True),(True,False),(False,True),(False,False)])
def test_dry_orphan_legacy(settings,store,dry,legacydry):
    runs=setup_runs(store,settings)
    settings['retention'].update(dryRun=dry)
    settings['retention']['legacy'].update(enabled=True,dryRun=legacydry,prefix='example/',filenamePattern=r'^old\.sql\.gz$')
    store.put('example/old.sql.gz',b'old',NOW-timedelta(days=366))
    store.put('example/nested/old.sql.gz',b'old',NOW-timedelta(days=900))
    orphan=settings['s3']['prefix']+'20261020T233000Z-'+('a'*32)+'/backup.dump'
    store.put(orphan,b'orphan',NOW-timedelta(days=8))
    fresh=orphan.replace('a'*32,'b'*32);store.put(fresh,b'new',NOW-timedelta(days=6))
    retention.rotate(store,settings,runs[0],NOW)
    assert (orphan in store.deleted)==(not dry)
    assert ('example/old.sql.gz' in store.deleted)==(not dry and not legacydry)
    assert fresh not in store.deleted and 'example/nested/old.sql.gz' not in store.deleted
    if dry:assert not store.deleted

def test_disabled(settings,store):assert retention.rotate(store,settings,'x',NOW)==[]

def test_listing_error(settings,store):
    runs=setup_runs(store,settings)
    store.listing=Mock(side_effect=TimeoutError())
    with pytest.raises(TimeoutError):retention.rotate(store,settings,runs[0],NOW)
    assert store.deleted==[]

def test_protected_changed(settings,store):
    runs=setup_runs(store,settings)
    original=store.read_manifest;calls=0
    def read(key):
        nonlocal calls
        calls+=1
        return original(key) if calls<=9 else {}
    store.read_manifest=read
    with pytest.raises(ValueError):retention.rotate(store,settings,runs[0],NOW)
    assert store.deleted==[]

def test_delete_error(settings,store):
    runs=setup_runs(store,settings);store.delete=Mock(side_effect=OSError())
    with pytest.raises(OSError):retention.rotate(store,settings,runs[0],NOW)
    assert store.delete.call_count==1

@pytest.mark.parametrize('ratio,expected',[(0.1,True),(1,False),(3,True)])
def test_size(settings,store,ratio,expected):
    for n in range(5):add_run(store,settings,n,NOW)
    complete,_=retention.inventory(store,settings['s3']['prefix'])
    m=copy.deepcopy(complete[0][2]);m['archive_bytes']=4*ratio
    assert retention.size_anomaly(complete,m,settings['observability']['sizeAnomaly'])==expected
    assert not retention.size_anomaly(complete[:1],m,settings['observability']['sizeAnomaly'])

def test_marker_appears_after_listing(settings,store):
    runs=setup_runs(store,settings)
    orphan=settings['s3']['prefix']+'20261020T233000Z-'+('d'*32)+'/backup.dump'
    store.put(orphan,b'orphan',NOW-timedelta(days=10))
    store.absent=Mock(return_value=False)
    with pytest.raises(ValueError):retention.rotate(store,settings,runs[0],NOW)
    assert not store.deleted

def test_no_current(settings,store):
    runs=setup_runs(store,settings)
    with pytest.raises(ValueError):retention.rotate(store,settings,'unknown',NOW)
    assert not store.deleted

def test_fewer_legacy(settings,store):
    runs=setup_runs(store,settings,6)
    settings['retention']['legacy'].update(enabled=True,dryRun=False,prefix='legacy/',filenamePattern=r'^file$')
    store.put('legacy/file',b'old',NOW-timedelta(days=500))
    retention.rotate(store,settings,runs[0],NOW)
    assert not store.deleted

@pytest.mark.parametrize('mutate',['checks','restore','bool','fingerprint','timestamp','db'])
def test_manifest_contract(settings,store,mutate):
    runs=setup_runs(store,settings)
    key=settings['s3']['prefix']+runs[-1]+'/manifest.json'
    m=store.read_manifest(key)
    if mutate=='checks':m['checks']['full_archive_read']=False
    if mutate=='restore':m['required_restore_major']=18
    if mutate=='bool':m['database_restore_tested']=0
    if mutate=='fingerprint':m['configuration_fingerprint']='short'
    if mutate=='timestamp':m['dump_started_at']='yesterday'
    if mutate=='db':del m['database']
    store.put(key,m,NOW-timedelta(days=500))
    with pytest.raises(ValueError):retention.rotate(store,settings,runs[0],NOW)
    assert not store.deleted

def test_freshest_orphan_object_protects_run(settings,store):
    runs=setup_runs(store,settings)
    prefix=settings['s3']['prefix']+'20261020T233000Z-'+('d'*32)+'/'
    store.put(prefix+'backup.dump',b'old',NOW-timedelta(days=8))
    # A recognized archive is fresh: GC cannot use the run_id date instead.
    store.put(prefix+'backup.dump',b'fresh',NOW-timedelta(days=1))
    retention.rotate(store,settings,runs[0],NOW)
    assert prefix+'backup.dump' not in store.deleted

def test_current_protected_with_clock_skew(settings,store):
    runs=setup_runs(store,settings)
    current,_=add_run(store,settings,100,NOW-timedelta(days=999))
    retention.rotate(store,settings,current,NOW)
    assert settings['s3']['prefix']+current+'/manifest.json' not in store.deleted

def test_late_pagination_error(settings):
    from backup.storage import Storage
    c=Mock()
    def pages():
        yield {'IsTruncated':True,'NextContinuationToken':'next','Contents':[{'Key':'valid'}]}
        raise TimeoutError()
    c.get_paginator.return_value.paginate.return_value=pages()
    c.get_bucket_versioning.return_value={}
    s=Storage(c,settings['s3']);settings['retention']['enabled']=True
    with pytest.raises(TimeoutError):retention.rotate(s,settings,'x',NOW)
    c.delete_object.assert_not_called()
