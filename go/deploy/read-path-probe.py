#!/usr/bin/env python3
"""Compare read paths using a disposable account; run on each app host."""
import json,sys,os,time,math,urllib.request,urllib.error
phase,region=sys.argv[1:]
state='/home/isucon/.replica-validation/read-path-'+region+'.json'
os.makedirs(os.path.dirname(state),mode=0o700,exist_ok=True)
def call(method,path,data=None,token=None,expect=200):
 headers={'Content-Type':'application/json'}
 if token:headers['Authorization']='Bearer '+token
 req=urllib.request.Request('http://127.0.0.1:8080'+path,data=json.dumps(data).encode() if data is not None else None,headers=headers,method=method)
 started=time.monotonic()
 try:
  with urllib.request.urlopen(req,timeout=15) as r:code,body=r.status,r.read()
 except urllib.error.HTTPError as e:code,body=e.code,e.read()
 assert code==expect,(path,code,body[:200])
 return (json.loads(body) if body else None),(time.monotonic()-started)*1000
if phase=='before':
 name='readpath_'+region+'_'+str(int(time.time()))
 user,_=call('POST','/api/auth/register',{'username':name,'displayName':name,'region':region},expect=201)
 cred={'username':name,'secretKey':user['secretKey']}
 with open(state,'w') as f:json.dump(cred,f)
 os.chmod(state,0o600)
else:cred=json.load(open(state))
login,_=call('POST','/api/auth/login',cred)
result={}
for path in ('/api/me','/api/posting-window/current','/api/relationships/friends'):
 times=[]
 for i in range(20):
  _,elapsed=call('GET',path,token=login['token']);times.append(elapsed)
 times.sort();result[path]={'n':len(times),'p50_ms':times[9],'p95_ms':times[18],'p99_ms':times[19]}
if phase=='after':call('POST','/api/me/account-deletion',{},login['token'],expect=204)
print(json.dumps({'region':region,'phase':phase,'metrics':result},indent=2))
