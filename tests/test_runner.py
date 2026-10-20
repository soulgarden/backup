import io,json,signal,subprocess
from pathlib import Path
from unittest.mock import Mock
import pytest
from backup import runner,archive,process,events
from backup.process import Cancelled

RUN='20261020T233000Z-'+'a'*32

def mocks(monkeypatch,tmp_path,inv):
    p=Mock();s=Mock()
    def command(args,*a,**kw):
        if args[0]=='pg_dump':
            file=next(v[7:] for v in args if v.startswith('--file='));Path(file).write_bytes(b'archive')
        if args[0]=='pg_restore' and '--list' in args:kw['output'].write(b'1; 1259 101 TABLE public child owner\n2; 0 101 TABLE DATA public child owner\n')
        return ''
    p.run.side_effect=command
    monkeypatch.setattr(archive,'inventory',lambda *a: inv)
    monkeypatch.setattr(archive,'versions',lambda *a:dict(server_version='16.3',pg_dump_version='16.5',pg_restore_version='16.5'))
    monkeypatch.setattr(runner.retention,'inventory',lambda *a:([],[]))
    return p,s

@pytest.mark.parametrize('stage',['preflight','dump','local_validation','upload','remote_validation','manifest'])
@pytest.mark.parametrize('cancel',[False,True])
def test_failure_no_manifest(settings,env,inv,monkeypatch,tmp_path,stage,cancel,capsys):
    p,s=mocks(monkeypatch,tmp_path,inv)
    error=Cancelled() if cancel else OSError('fixture-secret')
    if stage=='preflight':monkeypatch.setattr(archive,'inventory',Mock(side_effect=error))
    elif stage in ('dump','local_validation'):
        original=p.run.side_effect
        def fail(args,*a,**kw):
            if (args[0]=='pg_dump')==(stage=='dump'):raise error
            return original(args,*a,**kw)
        p.run.side_effect=fail
    else:getattr(s,{'upload':'upload','remote_validation':'verify','manifest':'publish'}[stage]).side_effect=error
    code=runner.run(settings,p,s,tmp_path,RUN)
    assert code==(143 if cancel else 1)
    if stage!='manifest':s.publish.assert_not_called()
    if stage in ('preflight','dump','local_validation'):s.upload.assert_not_called()
    assert 'fixture-secret' not in capsys.readouterr().out

@pytest.mark.parametrize('retention_result',['ok','error','cancel','anomaly'])
def test_success(settings,env,inv,monkeypatch,tmp_path,capsys,retention_result):
    p,s=mocks(monkeypatch,tmp_path,inv)
    rotate=Mock(return_value=[]);monkeypatch.setattr(runner.retention,'rotate',rotate)
    if retention_result=='error':rotate.side_effect=OSError('fixture-secret')
    if retention_result=='cancel':rotate.side_effect=Cancelled()
    monkeypatch.setattr(runner.retention,'size_anomaly',lambda *a:retention_result=='anomaly')
    assert runner.run(settings,p,s,tmp_path,RUN)==(143 if retention_result=='cancel' else 0)
    m=s.publish.call_args.args[1]
    assert m['database_restore_tested'] is False and m['status']=='completed'
    assert m['checks']['full_archive_read'] and m['checks']['remote_sha256']
    assert p.run.call_args_list[-1].args[0][1]=='--file=/dev/null'
    log=capsys.readouterr().out
    assert 'backup_completed' in log and 'fixture-secret' not in log
    if retention_result=='error':assert '"level":"ERROR","event":"backup_retention_failed"' in log
    if retention_result=='anomaly':assert 'backup_size_anomaly' in log

def test_empty_archive(settings,env,inv,monkeypatch,tmp_path):
    p,s=mocks(monkeypatch,tmp_path,inv);monkeypatch.setattr(archive,'hash_file',lambda _: (0,'x'))
    assert runner.run(settings,p,s,tmp_path,RUN)==1
    s.upload.assert_not_called()

@pytest.mark.parametrize('mode',['ok','config','cancel'])
def test_main(settings,monkeypatch,tmp_path,mode):
    monkeypatch.setenv('BACKUP_WORKSPACE',str(tmp_path))
    monkeypatch.setattr(runner.config,'load',Mock(return_value=settings,side_effect=ValueError() if mode=='config' else None))
    monkeypatch.setattr(runner.config,'credentials',lambda _: {})
    monkeypatch.setattr(runner.storage,'client',lambda *a:Mock())
    monkeypatch.setattr(runner,'run',Mock(return_value=0,side_effect=Cancelled() if mode=='cancel' else None))
    assert runner.main()==({'ok':0,'config':1,'cancel':143}[mode])

def test_process_stop_and_signal(monkeypatch):
    p=process.Processes(5);child=Mock(pid=999);child.poll.return_value=None
    child.wait.side_effect=[subprocess.TimeoutExpired('fixture',1),0]
    p.child=child;kill=Mock();monkeypatch.setattr(process.os,'killpg',kill)
    p.stop();assert [call.args[1] for call in kill.call_args_list]==[signal.SIGTERM,signal.SIGKILL]
    p.child=None;monkeypatch.setattr(signal,'alarm',Mock());monkeypatch.setattr(signal,'signal',Mock())
    with pytest.raises(Cancelled):p.handle_signal(signal.SIGTERM,None)
    assert p.cancelled
