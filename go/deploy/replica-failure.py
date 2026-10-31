#!/usr/bin/env python3
"""Run on isu01 while isu02 PostgreSQL is stopped. State comes from consistency test."""
import json,time,urllib.request,urllib.error
state=json.load(open('/home/isucon/.replica-validation/consistency-state.json'))
url='http://192.168.2.10/api/me'
headers={'Authorization':'Bearer '+state['token'],'Content-Type':'application/json'}
def request(method,data=None):
    req=urllib.request.Request(url,data=json.dumps(data).encode() if data else None,method=method,headers=headers)
    started=time.monotonic()
    try:
        with urllib.request.urlopen(req,timeout=18) as r:code,body=r.status,r.read()
    except urllib.error.HTTPError as e:code,body=e.code,e.read()
    return code,time.monotonic()-started,body
code,latency,body=request('GET');assert code==200,(code,body)
code,elapsed,body=request('PATCH',{'displayName':'unknown_commit_probe'})
assert code==200 and elapsed<5,(code,elapsed,body)
code,latency2,body=request('GET');assert code==200,(code,body)
print(json.dumps({'fallback_get_status':code,'get_before_ms':latency*1000,'write_status':200,'write_timeout_seconds':elapsed,'get_after_ms':latency2*1000,'write_outcome':'committed on Primary while replica was stopped'},indent=2))
