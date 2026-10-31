#!/usr/bin/env python3
"""Live small-image compatibility checks with disposable accounts only."""
import hashlib,json,sys,time,urllib.request,urllib.error
base={'asia':'http://192.168.1.10','europe':'http://192.168.2.10','america':'http://192.168.3.10'}
image=open('/tmp/upload-probe-512.jpg','rb').read();accounts=[];passed=[]
def call(region,method,path,data=None,token=None,expect=200,ctype='application/json'):
 if isinstance(data,dict):data=json.dumps(data).encode()
 h={'Content-Type':ctype}
 if token:h['Authorization']='Bearer '+token
 req=urllib.request.Request(base[region]+path,data=data,headers=h,method=method)
 try:
  with urllib.request.urlopen(req,timeout=30) as r:code,body=r.status,r.read()
 except urllib.error.HTTPError as e:code,body=e.code,e.read()
 assert code==expect,(region,method,path,code,body[:150])
 return json.loads(body) if body and ctype=='application/json' and not path.startswith('/api/images/') else body

def upload(a,path,fields,expect=201):
 boundary='LocalUploadSecurityBoundary';body=b''
 for k,v in fields.items():body+=('--'+boundary+'\r\nContent-Disposition: form-data; name="'+k+'"\r\n\r\n'+v+'\r\n').encode()
 body+=('--'+boundary+'\r\nContent-Disposition: form-data; name="image"; filename="probe.jpg"\r\n\r\n').encode()+image+('\r\n--'+boundary+'--\r\n').encode()
 raw=call(a['region'],'POST',path,body,a['token'],expect,'multipart/form-data; boundary='+boundary)
 return json.loads(raw) if raw else None

def check_image(url,status=200):
 for region in base:
  body=call(region,'GET',url,expect=status)
  if status==200:assert body==image,(region,'image mismatch')
try:
 for region in base:
  name='upsecure_'+str(time.time_ns());u=call(region,'POST','/api/auth/register',{'username':name,'displayName':name,'region':region},expect=201)
  a={'region':region,'id':u['user']['id'],'token':call(region,'POST','/api/auth/login',{'username':name,'secretKey':u['secretKey']})['token']};accounts.append(a)
  p=upload(a,'/api/posts',{'visibility':'public','location_kind':'none'});a['post']=p
  check_image(p['imageUrl']);passed.append(region+': post and immediate image GET on all regions')
  for i in range(2):
   call(region,'PUT','/api/me/profile-image',image,a['token'],204,'image/jpeg')
   me=call(region,'GET','/api/me',token=a['token'])
   if i:check_image(old,403)
   old=me['profileImageUrl'];check_image(old)
  passed.append(region+': profile replacement immediately revokes old image')
  call(region,'PUT','/api/me/profile-image',b'not jpeg',a['token'],400,'image/jpeg')
  call(region,'PUT','/api/me/profile-image',image,None,401,'image/jpeg')
  passed.append(region+': malformed and unauthenticated uploads rejected')
 author=accounts[0]
 for a in accounts[1:]:
  target='/api/posts/'+author['post']['id']+'/realmojis'
  first=upload(a,target,{'reactionType':'like'});check_image(first['imageUrl'])
  second=upload(a,target,{'reactionType':'smile'});check_image(first['imageUrl'],403);check_image(second['imageUrl'])
  call('asia','PUT','/api/blocks/'+a['id'],{},author['token'],204)
  upload(a,target,{'reactionType':'heart'},403)
  passed.append(a['region']+': RealMoji replacement and block authorization')
 call('asia','DELETE','/api/posts/'+author['post']['id'],token=author['token'],expect=204)
 check_image(author['post']['imageUrl'],403)
 for a in accounts[1:]:upload(a,'/api/posts/'+author['post']['id']+'/realmojis',{'reactionType':'like'},403)
 passed.append('deleted post image and new RealMoji immediately denied')
finally:
 for a in accounts:
  call(a['region'],'POST','/api/me/account-deletion',{},a['token'],204)
  call(a['region'],'GET','/api/me',token=a['token'],expect=401)
print(json.dumps({'passed':passed,'errors':[]},indent=2))
