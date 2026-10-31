#!/usr/bin/env python3
"""Read-only signed Primary API probe; run on an overseas app host."""
import hashlib,hmac,json,secrets,time,urllib.request,sys
region,operation=sys.argv[1:]
assert region in ('europe','america') and operation in ('ready','diagnostics','replica_health')
token=open('/home/isucon/.image-file-token','rb').read().strip()
key=hmac.new(token,b'isureal-primary-api-v1',hashlib.sha256).digest()
request={'op':operation}
if operation=='replica_health':request['replica_name']='isu02' if region=='europe' else 'isu03'
body=json.dumps(request,separators=(',',':')).encode();stamp=str(int(time.time()));nonce=secrets.token_hex(16)
uri='/api/internal/primary'
parts=['request-v1','POST',uri,region,stamp,nonce,'','','application/json',hashlib.sha256(body).hexdigest()]
signature=hmac.new(key,('\n'.join(parts)+'\n').encode(),hashlib.sha256).hexdigest()
headers={'Content-Type':'application/json','X-Isureal-Primary-Region':region,'X-Isureal-Primary-Time':stamp,'X-Isureal-Primary-Nonce':nonce,'X-Isureal-Primary-Signature':signature}
req=urllib.request.Request('http://192.168.1.10'+uri,data=body,headers=headers,method='POST')
started=time.monotonic()
with urllib.request.urlopen(req,timeout=10) as response:
 data=response.read();parts=['response-v1',nonce,str(response.status),hashlib.sha256(data).hexdigest()]
 expected=hmac.new(key,('\n'.join(parts)+'\n').encode(),hashlib.sha256).hexdigest()
 assert hmac.compare_digest(response.headers['X-Isureal-Primary-Response'],expected)
 print(json.dumps({'operation':operation,'elapsed_ms':(time.monotonic()-started)*1000,'result':json.loads(data)},indent=2))
