"""Executed in the release image, on synthetic source data only. Never restores SQL."""
import copy,hashlib,json,os,signal,subprocess,sys,tempfile,threading,time,resource
from pathlib import Path
sys.path.insert(0,'/opt/backup')
from backup import archive,config,runner,storage
from backup.process import Processes,Cancelled

settings=json.loads(Path('/tests/config.example.json').read_text())
settings['s3'].update(endpoint='http://moto:5000',addressingStyle='path',multipartThresholdMiB=5,multipartChunkSizeMiB=5)
settings['backup'].update(includeSchemas=['public','work schema'],excludeSchemas=['release_archive_*'])
settings['validation'].update(requiredSchemas=['public','work schema'],requiredTables=[dict(schema='public',table='partition root',requireData=True)],minTableDataEntries=3)
settings['retention']['enabled']=True
processes=Processes(5)
client=storage.client(settings['s3'],dict(aws_access_key_id='fixture',aws_secret_access_key='fixture'))
client.create_bucket(Bucket=settings['s3']['bucket'])
store=storage.Storage(client,settings['s3'])
with tempfile.TemporaryDirectory(dir='/work') as work:
    assert runner.run(settings,processes,store,work,'20261020T233000Z-'+'a'*32)==0
    objects=store.listing(settings['s3']['prefix'])
    assert len(objects)==2
    m=store.read_manifest(next(o['Key'] for o in objects if o['Key'].endswith('manifest.json')))
    assert not m['database_restore_tested'] and m['dump_major']==16
    toc_original=processes.run(['pg_restore','--list','--verbose',str(Path(work)/'backup.dump')])
    assert 'SEQUENCE' in toc_original and ' INDEX ' in toc_original and 'FK CONSTRAINT' in toc_original
    assert 'release_archive_fixture' not in toc_original
    env=config.pg_environment(settings)
    # Exercise ACL, large objects, partition data, quoted/unicode/newline names.
    for include in (True,False):
        cfg=copy.deepcopy(settings);cfg['backup'].update(includeAcl=include,includeLargeObjects=include,includeSchemas=[])
        path=Path(work)/'options.dump'
        processes.run(archive.dump_args(cfg,path),env)
        toc=processes.run(['pg_restore','--list','--verbose',str(path)])
        assert (' BLOBS ' in toc or ' BLOB ' in toc)==include
        assert (' ACL ' in toc)==include
        processes.run(['pg_restore','--file=/dev/null',str(path)])
    cfg=copy.deepcopy(settings);cfg['backup']['excludeTableData']=['public."leaf one"']
    path=Path(work)/'excluded.dump';processes.run(archive.dump_args(cfg,path),env)
    toc=processes.run(['pg_restore','--list','--verbose',str(path)])
    inv=archive.inventory(processes,env)
    try:archive.validate_toc(toc,inv,inv,cfg['validation'])
    except ValueError:pass
    else:raise AssertionError('missing leaf data accepted')
    # Read intact TOC, then truncate the data section. --list still succeeds.
    path=Path(work)/'corrupt.dump'
    processes.run(archive.dump_args(settings,path),env)
    raw=path.read_bytes();path.write_bytes(raw[:-200])
    processes.run(['pg_restore','--list',str(path)])
    try:processes.run(['pg_restore','--file=/dev/null',str(path)])
    except RuntimeError:pass
    else:raise AssertionError('corrupt archive accepted')
    # Real multipart completion, abort and paginated list with >1000 objects.
    p=Path(work)/'parts'
    with p.open('wb') as out:
        for _ in range(96):out.write(os.urandom(1024**2))
    store.upload(p,'fixture/multipart');store.verify('fixture/multipart',*archive.hash_file(p))
    for n in range(1001):client.put_object(Bucket=store.bucket,Key=f'pagination/{n}',Body=b'x')
    assert len(store.listing('pagination/'))==1001
    real=client.upload_part
    def fail(**kwargs):raise OSError('injected-part-failure')
    client.upload_part=fail
    try:store.upload(p,'fixture/aborted')
    except OSError:pass
    else:raise AssertionError('failure injection did not fail')
    finally:client.upload_part=real
    assert not client.list_multipart_uploads(Bucket=store.bucket).get('Uploads')
    # SIGTERM during a blocked upload worker must abort BEFORE waiting for it.
    entered=threading.Event();release=threading.Event()
    def delayed(**kwargs):
        entered.set();release.wait(timeout=10)
        return real(**kwargs)
    client.upload_part=delayed
    stop=Processes(5)
    previous=signal.signal(signal.SIGTERM,stop.handle_signal)
    timer=threading.Timer(0.25,lambda: os.kill(os.getpid(),signal.SIGTERM))
    timer.start();started=time.monotonic()
    try:store.upload(p,'fixture/cancelled')
    except Cancelled:
        assert entered.is_set() and time.monotonic()-started<2
        assert not client.list_multipart_uploads(Bucket=store.bucket).get('Uploads')
    else:raise AssertionError('SIGTERM during upload swallowed')
    finally:
        timer.cancel();release.set();client.upload_part=real
        signal.alarm(0);signal.signal(signal.SIGTERM,previous)
    disk_bytes=sum(path.stat().st_size for path in Path(work).iterdir())
    archive_bytes=m['archive_bytes']
    assert archive_bytes>32*1024**2, 'Synthetic archive must exceed the production part buffer budget'
    # Write a failed .part then exit nonzero: no upload or marker.
    original=processes.run
    for partial in (False,True):
        def failed(args,*a,**kw):
            if args[0]=='pg_dump' and '--version' not in args:
                if partial:Path(next(v[7:] for v in args if v.startswith('--file='))).write_bytes(b'partial')
                raise RuntimeError('injected-dump-failure')
            return original(args,*a,**kw)
        processes.run=failed
        run_id='20261020T233000Z-'+('b' if partial else 'c')*32
        assert runner.run(settings,processes,store,work,run_id)==1
        assert not store.listing(settings['s3']['prefix']+run_id+'/')
    processes.run=original
    # Empty source DB is valid for the general profile, rejected by required objects.
    os.environ['PGDATABASE']='fixture_empty'
    empty=copy.deepcopy(settings)
    empty['backup'].update(includeSchemas=[],excludeSchemas=[])
    empty['s3']['prefix']='empty/v2/'
    empty['validation']=dict(requiredSchemas=[],requiredTables=[],minTableDataEntries=0)
    empty['retention']['enabled']=False
    assert runner.run(empty,processes,store,work,'20261020T233000Z-'+'d'*32)==0
    empty['s3']['prefix']='empty-rejected/v2/'
    empty['validation']=settings['validation']
    assert runner.run(empty,processes,store,work,'20261020T233000Z-'+'e'*32)==1
    assert not store.listing(empty['s3']['prefix'])
    os.environ['PGDATABASE']='fixture'
print(json.dumps({'archive_read':True,'sql_restore_executed':False,'multipart':True,'pagination':1001,'mpu_abort':True,'uid':os.getuid(),'archive_bytes':archive_bytes,'stream_object_bytes':96*1024**2,'workspace_peak_bytes':disk_bytes,'python_peak_rss_kib':resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,'mpu_sigterm_abort':True}))
