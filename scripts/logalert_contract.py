"""Real Elasticsearch query + mock sender; no production notification transport."""
import argparse,json,urllib.request,uuid
from unittest.mock import Mock

QUERY='namespace:swup AND (message:error OR message:fatal OR message:panic OR message:critical) AND NOT (message:warn OR message:warning OR message:info OR message:debug)'
p=argparse.ArgumentParser();p.add_argument('--url',default='http://localhost:19200');a=p.parse_args()
index='backup-contract-'+uuid.uuid4().hex

def call(method,path,body=None):
    req=urllib.request.Request(a.url+'/'+path,data=json.dumps(body).encode() if body is not None else None,headers={'Content-Type':'application/json'},method=method)
    with urllib.request.urlopen(req,timeout=10) as r:return json.load(r)

try:
    call('PUT',index,{'mappings':{'properties':{'namespace':{'type':'keyword'},'message':{'type':'text'}}}})
    documents=[{'namespace':'swup','message':json.dumps({'level':'ERROR','event':event,'stage':'retention','code':'CLEANUP_FAILED','run_id':'20261020T233000Z-'+'a'*32},separators=(',',':'))} for event in ['backup_retention_failed','backup_size_anomaly']]
    documents += [{'namespace':'swup','message':'INFO normal completion'},{'namespace':'kino','message':'ERROR backup_retention_failed'}]
    for n,doc in enumerate(documents):call('PUT',f'{index}/_doc/{n}',doc)
    call('POST',index+'/_refresh')
    result=call('POST',index+'/_search',{'query':{'query_string':{'query':QUERY}},'size':100,'track_total_hits':True})
    assert not result.get('timed_out') and result['_shards']['failed']==0
    hits=result['hits']['hits'];assert len(hits)==2
    sender=Mock()
    for hit in hits:sender(hit['_source'])
    assert sender.call_count==2
    print('Exact logalert query matched both ERROR events; mock delivery passed')
finally:
    call('DELETE',index)
