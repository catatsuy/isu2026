#!/usr/bin/env python3
import json,sys,time,urllib.request,urllib.error
phase,region=sys.argv[1:]
cred=json.load(open('/home/isucon/.replica-validation/read-path-'+region+'.json'))
def call(method,path,data=None,token=None,multipart=False,expect=(200,)):
 headers={'Content-Type':'application/json'}
 if token:headers['Authorization']='Bearer '+token
 if multipart:
  boundary='ContentReadProbe';headers['Content-Type']='multipart/form-data; boundary='+boundary
  body=b''
  for k,v in {'caption':'read path probe','visibility':'public','location_kind':'none'}.items():body+=('--'+boundary+'\r\nContent-Disposition: form-data; name="'+k+'"\r\n\r\n'+v+'\r\n').encode()
  body+=('--'+boundary+'\r\nContent-Disposition: form-data; name="image"; filename="probe.jpg"\r\nContent-Type: image/jpeg\r\n\r\n').encode()+open('/tmp/replica-test.jpg','rb').read()+('\r\n--'+boundary+'--\r\n').encode()
 else:body=json.dumps(data).encode() if data is not None else None
 req=urllib.request.Request('http://127.0.0.1:8080'+path,data=body,headers=headers,method=method)
 started=time.monotonic()
 try:
  with urllib.request.urlopen(req,timeout=20) as r:code,body=r.status,r.read()
 except urllib.error.HTTPError as e:code,body=e.code,e.read()
 assert code in expect,(path,code,body[:150])
 return json.loads(body) if body else None,(time.monotonic()-started)*1000,code
login,_,_=call('POST','/api/auth/login',cred);token=login['token']
window,_,_=call('GET','/api/posting-window/current',token=token)
post,_,code=call('GET','/api/posts/me/current',token=token,expect=(200,404))
if code==404:post,_,_=call('POST','/api/posts',token=token,multipart=True,expect=(201,))
result={}
for route in ('/api/feeds/public','/api/memories?month='+window['window']['localDate'][:7],'/api/posts/'+post['id']):
 times=[]
 for i in range(10):_,elapsed,_=call('GET',route,token=token);times.append(elapsed)
 times.sort();label='/api/posts/:id' if route=='/api/posts/'+post['id'] else route
 result[label]={'n':10,'p50_ms':times[4],'p95_ms':times[9],'p99_ms':times[9]}
print(json.dumps({'phase':phase,'region':region,'metrics':result},indent=2))
