# Diagnostic probe for an isolated disposable own-user test account.
# Creates test files/versions and MOVE trash entries; never run on production.
# Demonstrates native endpoint semantics, not business acceptance.
import urllib.request,ssl,json,re,base64,urllib.error
from pathlib import Path
import os
root=Path(os.environ.get('OCIS_PROBE_OUTPUT','.'))
ctx=ssl.create_default_context(cafile=os.environ.get('OCIS_CA_FILE'))
p=os.environ['OCIS_TEST_PASSWORD']
auth='Basic '+base64.b64encode((os.environ['OCIS_TEST_USER']+':'+p).encode()).decode()
base=os.environ['OCIS_TEST_URL'].rstrip('/')
def req(method,url,data=None,headers=None):
 h={'Authorization':auth}; h.update(headers or {})
 try:r=urllib.request.urlopen(urllib.request.Request(url,data=data,method=method,headers=h),context=ctx,timeout=20)
 except urllib.error.HTTPError as e:r=e
 b=r.read();print(method,url.split('/data/')[0].replace(base,''),r.status,{k:v for k,v in r.headers.items() if k.lower() in ['etag','oc-fileid','upload-offset','upload-length']},b[:3000].decode(errors='replace'));return r.status,dict(r.headers),b
_,_,b=req('GET',base+'/graph/v1.0/me/drives')
space=next(x for x in json.loads(b)['value'] if x['driveType']=='personal'); dav=space['root']['webDavUrl']
import time,uuid,xml.etree.ElementTree as ET
name='m0-'+uuid.uuid4().hex+'.txt'
_,h,_=req('POST',dav,b'',{'Tus-Resumable':'1.0.0','Upload-Length':'13','Upload-Metadata':'filename '+base64.b64encode(name.encode()).decode(),'If-None-Match':'*'})
loc=h.get('Location');assert loc
req('PATCH',loc,b'first version',{'Tus-Resumable':'1.0.0','Upload-Offset':'0','Content-Type':'application/offset+octet-stream'})
time.sleep(2)
_,h,b=req('PROPFIND',dav+'/'+name,b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>',{'Depth':'0','Content-Type':'application/xml'})
e=ET.fromstring(b); rid=e.find('.//{http://owncloud.org/ns}fileid');print('resource',None if rid is None else rid.text)
rid=rid.text
etag=e.find('.//{DAV:}getetag').text
stable=base+'/dav/spaces/'+rid
req('GET',stable)
req('PROPFIND',base+'/dav/meta/'+rid+'/v',b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>',{'Depth':'1','Content-Type':'application/xml'})
_,h,_=req('POST',dav,b'',{'Tus-Resumable':'1.0.0','Upload-Length':'14','Upload-Metadata':'filename '+base64.b64encode(name.encode()).decode(),'If-Match':etag})
loc=h.get('Location');assert loc
req('PATCH',loc,b'second version',{'Tus-Resumable':'1.0.0','Upload-Offset':'0','Content-Type':'application/offset+octet-stream'})
time.sleep(2)
_,_,b=req('PROPFIND',base+'/dav/meta/'+rid+'/v',b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>',{'Depth':'1','Content-Type':'application/xml'})
(root/'ocis-revisions.xml').write_bytes(b)
for href in ET.fromstring(b).findall('.//{DAV:}href'):
 if '/v/' in href.text:req('GET',base+href.text)
req('MKCOL',dav+'/.clouddrive-trash',b'')
_,_,b=req('PROPFIND',stable,b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>',{'Depth':'0','Content-Type':'application/xml'})
newetag=ET.fromstring(b).find('.//{DAV:}getetag').text
req('MOVE',stable,b'',{'Destination':dav+'/.clouddrive-trash/'+name,'Overwrite':'F','If-Match':newetag})
req('GET',stable)
req('PROPFIND',base+'/dav/meta/'+rid+'/v',b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>',{'Depth':'1','Content-Type':'application/xml'})
# Fixed-byte reads and delayed final-commit conflict against the moved resource.
req('GET',stable,headers={'If-Match':etag})
req('GET',stable,headers={'If-Match':newetag})
for href in ET.fromstring(b).findall('.//{DAV:}href'):
 if '/v/' in href.text: req('GET',base+href.text)
trashdav=dav+'/.clouddrive-trash'
def begin(content):
 st,hh,_=req('POST',trashdav,b'',{'Tus-Resumable':'1.0.0','Upload-Length':str(len(content)),'Upload-Metadata':'filename '+base64.b64encode(name.encode()).decode(),'If-Match':newetag});return hh.get('Location')
a=begin(b'writer A');bb=begin(b'writer B')
if a: req('PATCH',a,b'writer A',{'Tus-Resumable':'1.0.0','Upload-Offset':'0','Content-Type':'application/offset+octet-stream'})
time.sleep(2)
if bb: req('PATCH',bb,b'writer B',{'Tus-Resumable':'1.0.0','Upload-Offset':'0','Content-Type':'application/offset+octet-stream'})
time.sleep(2)
req('GET',stable)
