"""Operator read-back gate. Never changes bucket configuration."""
import argparse,json,sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from backup import config,storage
p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('--credentials-dir',required=True);p.add_argument('--report',required=True);a=p.parse_args()
c=config.load(a.config)['s3'];client=storage.client(c,config.credentials(a.credentials_dir))
response=client.get_bucket_lifecycle_configuration(Bucket=c['bucket'])
matching=[]
for rule in response['Rules']:
    f=rule.get('Filter',{})
    prefix=f.get('Prefix',rule.get('Prefix'))
    # Only an enabled pure prefix rule proves coverage without hidden tag constraints.
    if rule.get('Status')=='Enabled' and prefix==c['prefix'] and set(f)<= {'Prefix'} and rule.get('AbortIncompleteMultipartUpload',{}).get('DaysAfterInitiation')==1:
        matching.append(rule.get('ID',''))
if not matching:raise SystemExit('Required scoped one-day MPU lifecycle rule not confirmed')
Path(a.report).write_text(json.dumps(dict(bucket=c['bucket'],prefix=c['prefix'],abort_mpu_days=1,rule_ids=matching,read_back=True),indent=2)+'\n')
