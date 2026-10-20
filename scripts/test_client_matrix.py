"""Run real pg_dump/pg_restore 17/18 to prove PG16 preflight rejects each."""
import subprocess,uuid,json
from pathlib import Path
root=Path(__file__).resolve().parents[1]
code='''
from backup.archive import versions
from backup.process import Processes
p=Processes(5)
for selected in ('pg_dump','pg_restore'):
 class Selected:
  def run(self,args):
   if args[0]==selected:return p.run(args)
   return args[0]+' (PostgreSQL) 16.1'
 try:versions(Selected(),{'postgresql':{'majorVersion':16}}, {'server_version':'16.3','server_version_num':160003})
 except ValueError as e:assert str(e)=='CLIENT_MAJOR_MISMATCH'
 else:raise AssertionError('incompatible client accepted')
print('Both actual tools rejected by PG16 preflight')
'''
for major in (17,18):
    image='backup-client-contract-'+uuid.uuid4().hex[:8]+':'+str(major)
    try:
        subprocess.run(['docker','build','--platform','linux/amd64','--build-arg','POSTGRES_MAJOR='+str(major),'-t',image,str(root)],check=True,stdout=subprocess.DEVNULL)
        subprocess.run(['docker','run','--rm','--entrypoint','python',image,'-c',code],check=True)
    finally:
        subprocess.run(['docker','image','rm',image],check=False,stdout=subprocess.DEVNULL)

(root/'artifacts').mkdir(exist_ok=True)
(root/'artifacts/client-matrix.json').write_text(json.dumps({'rejected_majors':[17,18],'tools':['pg_dump','pg_restore'],'expected_major':16},indent=2)+'\n')
