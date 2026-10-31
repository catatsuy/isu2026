#!/usr/bin/env python3
"""Disposable end-to-end checks, run on isu01 over existing routed Private IPs."""
import json,sys,time,urllib.request,urllib.error
base={'asia':'http://127.0.0.1:8080','europe':'http://192.168.2.10','america':'http://192.168.3.10'}
checks=[]
def call(region,method,path,data=None,token=None,expect=200,multipart=False):
    headers={'Content-Type':'application/json'}
    if token:headers['Authorization']='Bearer '+token
    if multipart:
        boundary='ReplicaProbeBoundary'
        headers['Content-Type']='multipart/form-data; boundary='+boundary
        content=b''
        for k,v in {'caption':'replica validation','visibility':'public','location_kind':'none'}.items():
            content+=('--'+boundary+'\r\nContent-Disposition: form-data; name="'+k+'"\r\n\r\n'+v+'\r\n').encode()
        content+=('--'+boundary+'\r\nContent-Disposition: form-data; name="image"; filename="probe.jpg"\r\nContent-Type: image/jpeg\r\n\r\n').encode()+open('/tmp/replica-test.jpg','rb').read()+('\r\n--'+boundary+'--\r\n').encode()
    else:content=json.dumps(data).encode() if data is not None else None
    req=urllib.request.Request(base[region]+path,data=content,headers=headers,method=method)
    try:
        with urllib.request.urlopen(req,timeout=30) as r:code,body=r.status,r.read()
    except urllib.error.HTTPError as e:code,body=e.code,e.read()
    assert code==expect,(region,method,path,code,body[:300])
    return json.loads(body) if body else None

def account(region,suffix):
    name='replcheck_'+str(int(time.time()))+'_'+suffix
    user=call('asia','POST','/api/auth/register',{'username':name,'displayName':name,'region':region},expect=201)
    session=call('asia','POST','/api/auth/login',{'username':name,'secretKey':user['secretKey']})
    call(region,'GET','/api/me',token=session['token'])
    return {'id':user['user']['id'],'token':session['token'],'region':region,'name':name,'secret':user['secretKey']}

admin=call('asia','POST','/api/admin/auth/login',{'username':'admin','password':'password1234'})['token']
a=account('europe','a');b=account('america','b')
checks.append('Primary register/login immediately visible on both regional replicas')
call('europe','PATCH','/api/me',{'bio':'private_replica_probe'},a['token'])
assert call('america','GET','/api/users/'+a['id'],token=b['token'])['bio']==''
checks.append('Primary permissions hide private bio before friendship')
for user in (a,b):
    current=call(user['region'],'GET','/api/posting-window/current',token=user['token'])
    assert current['window'] is not None and current['canPost'],current
pa=call('europe','POST','/api/posts',token=a['token'],expect=201,multipart=True)
pb=call('america','POST','/api/posts',token=b['token'],expect=201,multipart=True)
call('america','GET','/api/posts/'+pa['id'],token=b['token'])
checks.append('Post COMMIT immediately visible from other region')
call('europe','PATCH','/api/posts/'+pa['id'],{'visibility':'friends'},a['token'])
call('america','GET','/api/posts/'+pa['id'],token=b['token'],expect=404)
checks.append('Visibility change immediately enforced in other region')
call('europe','POST','/api/friend-requests',{'recipientId':b['id']},a['token'],expect=201)
call('america','POST','/api/friend-requests',{'recipientId':a['id']},b['token'],expect=201)
call('america','GET','/api/posts/'+pa['id'],token=b['token'])
assert call('america','GET','/api/users/'+a['id'],token=b['token'])['bio']=='private_replica_probe'
comment=call('america','POST','/api/posts/'+pa['id']+'/comments',{'body':'replica comment probe'},b['token'],expect=201)
comments=call('america','GET','/api/posts/'+pa['id']+'/comments',token=b['token'])
assert any(item['id']==comment['id'] for item in comments['items'])
call('america','DELETE','/api/comments/'+comment['id'],token=b['token'],expect=204)
comments=call('america','GET','/api/posts/'+pa['id']+'/comments',token=b['token'])
assert all(item['id']!=comment['id'] for item in comments['items'])
checks.append('Deleted comments are removed from local payload by current Primary state')
call('europe','DELETE','/api/friendships/'+b['id'],token=a['token'],expect=204)
call('america','GET','/api/posts/'+pa['id'],token=b['token'],expect=404)
assert call('america','GET','/api/users/'+a['id'],token=b['token'])['bio']==''
checks.append('Unfriend immediately enforced for posts and private bio in other region')
call('europe','PATCH','/api/posts/'+pa['id'],{'visibility':'public'},a['token'])
call('america','GET','/api/posts/'+pa['id'],token=b['token'])
call('europe','PUT','/api/blocks/'+b['id'],{},a['token'],expect=204)
call('america','GET','/api/posts/'+pa['id'],token=b['token'],expect=404)
call('america','GET','/api/users/'+a['id'],token=b['token'],expect=404)
search=call('america','GET','/api/users?q='+a['name'],token=b['token'])
assert all(item['id']!=a['id'] for item in search['items'])
checks.append('Block immediately enforced for posts, profiles and local search payloads')
# Login has no region middleware: invalidate the US token through asia's app.
replacement=call('asia','POST','/api/auth/login',{'username':b['name'],'secretKey':b['secret']})
call('america','GET','/api/me',token=b['token'],expect=401)
b['token']=replacement['token']
checks.append('Session replacement on Primary immediately invalidates replica authentication')
call('america','POST','/api/me/account-deletion',{},b['token'],expect=204)
call('america','GET','/api/me',token=b['token'],expect=401)
checks.append('Account deletion immediately invalidates replica authentication')
# Keep a disposable active session to verify reboot persistence and reset invalidation.
import os
os.makedirs('/home/isucon/.replica-validation',mode=0o700,exist_ok=True)
with open('/home/isucon/.replica-validation/consistency-state.json','w') as f:json.dump({'region':a['region'],'token':a['token'],'id':a['id']},f)
import os
os.chmod('/home/isucon/.replica-validation/consistency-state.json',0o600)
call('europe','POST','/api/me/account-deletion',{},a['token'],expect=204)
checks.append('All disposable validation accounts deactivated after tests')
print(json.dumps({'passed':checks},indent=2))
