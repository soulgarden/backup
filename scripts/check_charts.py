import argparse,json,subprocess,tempfile,sys
from pathlib import Path
import yaml

p=argparse.ArgumentParser();p.add_argument('--ci-root');a=p.parse_args()
root=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(root))
charts=[root/'helm/backup']
if a.ci_root:charts.append(Path(a.ci_root)/'helm/backup')
for chart in charts:
    values=yaml.safe_load((chart/'values.yaml').read_text())
    metadata=yaml.safe_load((chart/'Chart.yaml').read_text())
    assert values['image']['tag']==metadata['appVersion'], 'image.tag and appVersion differ'
    subprocess.run(['helm','lint','--strict',str(chart)],check=True)
    rendered=subprocess.check_output(['helm','template','backup',str(chart),'--namespace','fixture'],text=True)
    docs=[d for d in yaml.safe_load_all(rendered) if d]
    cron=next(d for d in docs if d['kind']=='CronJob')['spec']
    job=cron['jobTemplate']['spec'];pod=job['template']['spec']
    assert job['activeDeadlineSeconds']==10800 and pod['terminationGracePeriodSeconds']==90
    assert cron['timeZone']=='Etc/UTC' and cron['concurrencyPolicy']=='Forbid'
    assert pod['securityContext']['runAsUser']==10001 and not pod['automountServiceAccountToken']
    conf=json.loads(next(d for d in docs if d['kind']=='ConfigMap')['data']['config.json'])
    from backup.config import validate
    validate(conf)
    assert conf['retention']['maxAgeDays']==365 and conf['retention']['dryRun']
    with tempfile.TemporaryDirectory() as tmp:
        override=Path(tmp)/'values.yaml'
        override.write_text(yaml.safe_dump({'backup':{'includeSchemas':['space schema','a."b','$(unsafe)']},'nodeSelector':{'fixture':'yes'},'image':{'digest':'sha256:'+'a'*64}}))
        changed=subprocess.check_output(['helm','template','backup',str(chart),'-f',str(override)],text=True)
        assert '@sha256:'+'a'*64 in changed and 'fixture: ' in changed
    invalid=subprocess.run(['helm','template','backup',str(chart),'--set','s3.host_bucket=old'],capture_output=True)
    assert invalid.returncode!=0
print('Chart configuration and activation gates passed')
