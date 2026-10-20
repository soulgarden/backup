import subprocess,time,uuid
from pathlib import Path
name='backup-es-'+uuid.uuid4().hex[:10]
root=Path(__file__).resolve().parents[1]
try:
    subprocess.run(['docker','run','-d','--name',name,'-p','127.0.0.1::9200','-e','discovery.type=single-node','-e','xpack.security.enabled=false','-e','ES_JAVA_OPTS=-Xms256m -Xmx256m','docker.elastic.co/elasticsearch/elasticsearch:8.15.5'],check=True,stdout=subprocess.DEVNULL)
    port=subprocess.check_output(['docker','port',name,'9200/tcp'],text=True).strip().rsplit(':',1)[1]
    import urllib.request
    deadline=time.monotonic()+90
    while True:
        try:
            with urllib.request.urlopen('http://127.0.0.1:'+port,timeout=2):break
        except Exception:
            if time.monotonic()>deadline:raise
            time.sleep(1)
    subprocess.run(['python3',str(root/'scripts/logalert_contract.py'),'--url','http://127.0.0.1:'+port],check=True)
finally:
    subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,check=False)
