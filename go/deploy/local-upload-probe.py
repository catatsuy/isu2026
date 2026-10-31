#!/usr/bin/env python3
"""Controlled before/after probe; run on an overseas host, creates then deletes accounts.
No initialize, window changes or benchmark commands. Requires an active posting window.
"""
import concurrent.futures, hashlib, json, math, os, sys, time, urllib.request, urllib.error
phase,region=sys.argv[1:3]
n=int(sys.argv[3]) if len(sys.argv)>3 else 20
base='http://127.0.0.1'
small=open('/tmp/upload-probe-512.jpg','rb').read()
large=open('/tmp/upload-probe-2048.jpg','rb').read()
metrics={};errors=[];accounts=[];replication=[]
def call(method,path,data=None,token=None,expect=200,ctype='application/json',measure=None):
 if isinstance(data,dict):data=json.dumps(data).encode()
 headers={'Content-Type':ctype}
 if token:headers['Authorization']='Bearer '+token
 req=urllib.request.Request(base+path,data=data,headers=headers,method=method)
 start=time.monotonic()
 try:
  with urllib.request.urlopen(req,timeout=60) as r:code,body=r.status,r.read()
 except urllib.error.HTTPError as e:code,body=e.code,e.read()
 elapsed=(time.monotonic()-start)*1000
 if measure:metrics.setdefault(measure,[]).append(elapsed)
 if code!=expect:errors.append([method,path,code,body[:120].decode(errors='replace')]);raise RuntimeError(errors[-1])
 return json.loads(body) if body and ctype=='application/json' else body

def account(i):
 name='up_'+str(time.time_ns())+'_'+str(i)
 u=call('POST','/api/auth/register',{'username':name,'displayName':name,'region':region},expect=201)
 t=call('POST','/api/auth/login',{'username':name,'secretKey':u['secretKey']})['token'];accounts.append(t)
 return t

def upload(path,token,image,fields,measure):
 boundary='LocalUploadProbeBoundary'
 content=b''
 for k,v in fields.items():content+=('--'+boundary+'\r\nContent-Disposition: form-data; name="'+k+'"\r\n\r\n'+v+'\r\n').encode()
 content+=('--'+boundary+'\r\nContent-Disposition: form-data; name="image"; filename="probe.jpg"\r\nContent-Type: image/jpeg\r\n\r\n').encode()+image+('\r\n--'+boundary+'--\r\n').encode()
 raw=call('POST',path,content,token,201,'multipart/form-data; boundary='+boundary,measure)
 return json.loads(raw)

def image_get(url,expected):
 # Through all regional nginx entrypoints, exercise signed GET and immediate bytes.
 for host in ('192.168.1.10','192.168.2.10','192.168.3.10'):
  start=time.monotonic()
  with urllib.request.urlopen('http://'+host+url,timeout=30) as r:got=r.read()
  metrics.setdefault('immediate_get_'+host,[]).append((time.monotonic()-start)*1000)
  assert got==expected,('image mismatch',host)

def copies(url):
 id=url.split('/')[3].split('?')[0];key=hashlib.sha256(id.encode()).hexdigest()
 # Local presence validates origin persistence. Full replica delay is sampled via
 # signed peer GET in a separate tool; this check makes no filesystem mutations.
 if phase=='after':assert os.path.isfile('/home/isucon/image-files/'+key[:2]+'/'+key)
try:
 author=account('author')
 window=call('GET','/api/posting-window/current',token=author)
 assert window['window'] is not None and window['canPost'],'active posting window required'
 target=upload('/api/posts',author,large,{'caption':'local upload probe','visibility':'public','location_kind':'none'},'post_ms')
 image_get(target['imageUrl'],large);copies(target['imageUrl'])
 for i in range(n):
  t=account(i)
  p=upload('/api/posts',t,large,{'caption':'local upload probe','visibility':'public','location_kind':'none'},'post_ms')
  image_get(p['imageUrl'],large);copies(p['imageUrl'])
  call('PUT','/api/me/profile-image',small,t,204,'image/jpeg','profile_ms')
  me=call('GET','/api/me',token=t);image_get(me['profileImageUrl'],small);copies(me['profileImageUrl'])
  for j in range(2):
   r=upload('/api/posts/'+target['id']+'/realmojis',t,small,{'reactionType':'like' if j==0 else 'smile'},'realmoji_ms')
   image_get(r['imageUrl'],small);copies(r['imageUrl'])
  call('POST','/api/me/account-deletion',{},t,204);accounts.remove(t)
finally:
 for t in accounts:
  try:call('POST','/api/me/account-deletion',{},t,204)
  except Exception as e:errors.append(str(e))
 def stats(v):
  v=sorted(v)
  return {'n':len(v),'p50_ms':round(v[math.ceil(len(v)*.50)-1],3),'p95_ms':round(v[math.ceil(len(v)*.95)-1],3),'p99_ms':round(v[math.ceil(len(v)*.99)-1],3)}
 print(json.dumps({'phase':phase,'region':region,'jpeg_bytes':{'post':len(large),'profile_realmoji':len(small)},'metrics':{k:stats(v) for k,v in metrics.items()},'errors':errors},indent=2))
