#!/usr/bin/env python3
"""Run on an app host; records no passwords or tokens in output."""
import json, os, sys, time, urllib.request, urllib.error, math
phase,region=sys.argv[1:]
os.makedirs('/home/isucon/.replica-validation',mode=0o700,exist_ok=True)
state='/home/isucon/.replica-validation/api-probe.json'
metrics={}
def call(method,path,data=None,token=None,expected=200):
    headers={'Content-Type':'application/json'}
    if token: headers['Authorization']='Bearer '+token
    req=urllib.request.Request('http://127.0.0.1:8080'+path,data=json.dumps(data).encode() if data is not None else None,headers=headers,method=method)
    started=time.monotonic()
    try:
        with urllib.request.urlopen(req,timeout=20) as r: code,body=r.status,r.read()
    except urllib.error.HTTPError as e: code,body=e.code,e.read()
    metrics.setdefault(method+' '+path,[]).append((time.monotonic()-started)*1000)
    assert code==expected,(method,path,code,body[:150])
    return json.loads(body) if body else None
if phase=='before' or not os.path.exists(state):
    username='replicaprobe_'+region+'_'+str(int(time.time()))
    user=call('POST','/api/auth/register',{'username':username,'displayName':'replica probe','region':region},expected=201)
    with open(state,'w') as f: json.dump({'username':username,'secretKey':user['secretKey'],'userId':user['user']['id']},f)
    os.chmod(state,0o600)
with open(state) as f: credentials=json.load(f)
for i in range(10):
    login=call('POST','/api/auth/login',{'username':credentials['username'],'secretKey':credentials['secretKey']})
token=login['token']
for i in range(50): call('GET','/api/me',token=token)
for i in range(10):
    value=phase+'_'+str(i)
    call('PATCH','/api/me',{'displayName':value},token)
    observed=call('GET','/api/me',token=token)['displayName']
    if phase!='async': assert observed==value
    else:
        deadline=time.monotonic()+3
        while observed!=value and time.monotonic()<deadline:
            time.sleep(.025)
            observed=call('GET','/api/me',token=token)['displayName']
        assert observed==value,'profile failed to converge'
call('POST','/api/auth/logout',{},token,204)
call('GET','/api/me',token=token,expected=401)
result={}
for route,values in metrics.items():
    values.sort()
    result[route]={'n':len(values),'p50_ms':round(values[math.ceil(len(values)*.5)-1],3),'p95_ms':round(values[math.ceil(len(values)*.95)-1],3),'p99_ms':round(values[math.ceil(len(values)*.99)-1],3)}
print(json.dumps({'phase':phase,'region':region,'metrics':result},indent=2))
