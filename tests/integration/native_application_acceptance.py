# Real deployed-stack acceptance probe, not a mock test.
# Use ONLY disposable test accounts and configured test endpoints.
# Required: ALLOW_DISPOSABLE_TEST_WRITES=yes, TEST_OIDC_ISSUER, TEST_PUBLIC_URL,
# TEST_CREDENTIALS_FILE (JSON user/backend; two test users share synthetic password).
# Optional: TEST_CA_FILE, TEST_MCP_URL, TEST_USERNAME, TEST_USERNAME_B.
# Creates retained files/versions and explicitly approves a synthetic internal share.
# No tokens, secrets or authorization URLs are printed/saved.
import ssl,json,os,sys,secrets,urllib.request,urllib.parse,urllib.error,http.cookiejar,re,hashlib,base64,html
from pathlib import Path
root=Path(os.environ.get('NATIVE_TEST_STATE_DIR','.'))
realm=os.environ['TEST_OIDC_ISSUER'].rstrip('/'); base=realm.split('/realms/')[0]; public=os.environ['TEST_PUBLIC_URL'].rstrip('/')
assert os.environ.get('ALLOW_DISPOSABLE_TEST_WRITES')=='yes', 'Use an isolated test stack and explicitly allow test writes'
secrets_doc=json.loads(Path(os.environ['TEST_CREDENTIALS_FILE']).read_text())
class Redirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,req,fp,code,msg,headers,newurl):
  if newurl.startswith((public+'/auth/callback',public+'/v1/delegation/callback')):return None
  return super().redirect_request(req,fp,code,msg,headers,newurl)
jar=http.cookiejar.CookieJar();op=urllib.request.build_opener(urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=os.environ.get('TEST_CA_FILE'))),urllib.request.HTTPCookieProcessor(jar),Redirect())
def fetch(url,data=None):
 url=urllib.parse.urljoin(base,url)
 try:return op.open(urllib.request.Request(url,data=None if data is None else urllib.parse.urlencode(data).encode()),timeout=20)
 except urllib.error.HTTPError as e:return e
verifier=secrets.token_urlsafe(48);challenge=base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b'=').decode()
url=realm+'/protocol/openid-connect/auth?'+urllib.parse.urlencode({'client_id':'clouddrive-web','redirect_uri':public+'/auth/callback','response_type':'code','scope':'openid drive.read drive.write drive.delete drive.restore drive.convert drive.share.request drive.feedback','state':'local-selftest','code_challenge':challenge,'code_challenge_method':'S256'})
r=fetch(url);b=r.read().decode();act=re.search(r'<form[^>]+action="([^"]+)"',b);assert act,(r.status,b[:200])
r=fetch(html.unescape(act.group(1)),{'username':os.environ.get('TEST_USERNAME','test-user'),'password':secrets_doc['user'],'credentialId':''});b=r.read().decode()
if r.status==200:
 act=re.search(r'<form[^>]+action="([^"]+)"',b);assert act,b[:200]
 code=re.search(r'name="code"[^>]+value="([^"]+)"',b)
 r=fetch(html.unescape(act.group(1)),{'accept':'Yes',**({'code':html.unescape(code.group(1))} if code else {})});b=r.read().decode()
loc=r.headers.get('Location','');assert '/auth/callback?' in loc,(r.status,b[:300])
code=urllib.parse.parse_qs(urllib.parse.urlparse(loc).query)['code'][0]
r=fetch(realm+'/protocol/openid-connect/token',{'grant_type':'authorization_code','client_id':'clouddrive-web','redirect_uri':public+'/auth/callback','code':code,'code_verifier':verifier});d=json.load(r);assert 'access_token' in d,d
print('PKCE authorization-code exchange passed')
def claims(token):return json.loads(base64.urlsafe_b64decode(token.split('.')[1]+'=='))
c=claims(d['access_token']);print('token audience',c['aud'],'scope',c.get('scope'));assert 'agent-clouddrive' in c['aud']
r=fetch(realm+'/protocol/openid-connect/token',{'grant_type':'urn:ietf:params:oauth:grant-type:token-exchange','client_id':'agent-clouddrive','client_secret':secrets_doc['backend'],'subject_token':d['access_token'],'subject_token_type':'urn:ietf:params:oauth:token-type:access_token','requested_token_type':'urn:ietf:params:oauth:token-type:access_token','audience':'ocis','scope':'openid'});x=json.load(r)
print('token-exchange HTTP',r.status)
if 'access_token' not in x:print(x);sys.exit(1)
c=claims(x['access_token']);print('delegated token audience',c.get('aud'),'roles',c.get('roles'));assert 'ocis' in c.get('aud',[])
print('Keycloak reference exchange passed; oCIS acceptance not implied')
if True:
 import time,uuid
 api_base=public; access=d['access_token']
 def api(method,path,body=None,extra=None):
  headers={'Authorization':'Bearer '+access,'Origin':api_base};headers.update(extra or {})
  if isinstance(body,dict):headers['Content-Type']='application/json';body=json.dumps(body).encode()
  req=urllib.request.Request(api_base+path,data=body,method=method,headers=headers)
  try: rr=op.open(req,timeout=30)
  except urllib.error.HTTPError as e:rr=e
  raw=rr.read();result=json.loads(raw) if raw and 'json' in rr.headers.get('Content-Type','') else raw
  print('APP',method,path,rr.status,('[authorization URL redacted]' if isinstance(result,dict) and any(k in result for k in ['authorization_url','csrf_token','session_token']) else repr(result)[:1200]));return rr.status,result
 st,me=api('GET','/v1/me');assert st==200
 st,start=api('POST','/v1/delegation/start',{});assert st==200,start
 r=fetch(start['authorization_url']);b=r.read().decode()
 if r.status==200:
  act=re.search(r'<form[^>]+action="([^"]+)"',b);assert act,b[:200]
  code=re.search(r'name="code"[^>]+value="([^"]+)"',b)
  r=fetch(html.unescape(act.group(1)),{'accept':'Yes',**({'code':html.unescape(code.group(1))} if code else {})});b=r.read().decode()
 loc=r.headers.get('Location','');assert '/v1/delegation/callback?' in loc,(r.status,b[:300])
 # Deliberately perform the real application callback, with its bound cookie.
 rr=fetch(loc);rr.read()
 st,status=api('GET','/v1/delegation/status');assert st==200 and status['enabled'],status
 content='CloudDrive integration 中文 version one'.encode();name='native-'+uuid.uuid4().hex+'.txt'
 st,u=api('POST','/v1/uploads',{'name':name,'size_bytes':len(content),'content_type':'text/plain','idempotency_key':'upload-'+uuid.uuid4().hex});assert st==200 or st==201,u
 uid=u['id'];st,_=api('PATCH','/v1/tus/'+uid,content,{'Tus-Resumable':'1.0.0','Upload-Offset':'0','Content-Type':'application/offset+octet-stream'});assert st==204
 for attempt in range(15):
  st,fin=api('POST','/v1/uploads/'+uid+'/finalize',{})
  if st in [200,201] and fin.get('status') in ['committed','saved']:break
  time.sleep(1)
 else:raise AssertionError(fin)
 fid=fin.get('file_id') or u['file_id'];st,b=api('GET','/v1/files/'+fid+'/content');assert st==200 and b==content,(st,b)
 for attempt in range(30):
  st,result=api('POST','/v1/search',{'query':'CloudDrive','limit':20})
  if st==200 and any(item.get('file_id')==fid for item in result.get('items',[])):break
  time.sleep(1)
 else:raise AssertionError('converted content was not searchable')
 print('REAL NATIVE APP AUTH/UPLOAD/FIXED READ/OFFLINE CONVERSION/SEARCH PASSED')

if True:
 # HTTP MCP protocol smoke; not a Peri-client acceptance claim.
 def mcp(method,params,rid):
  q=urllib.request.Request(os.environ.get('TEST_MCP_URL',public+'/mcp'),data=json.dumps({'jsonrpc':'2.0','id':rid,'method':method,'params':params}).encode(),headers={'Authorization':'Bearer '+access,'Content-Type':'application/json','Accept':'application/json, text/event-stream',**({'MCP-Protocol-Version':init['protocolVersion']} if method!='initialize' else {})})
  rr=op.open(q,timeout=30);dd=json.load(rr);print('MCP',method,rr.status,'error' if dd.get('error') else 'result');assert 'error' not in dd,dd;return dd['result']
 init=mcp('initialize',{'protocolVersion':'2026-07-28','capabilities':{},'clientInfo':{'name':'native-selftest','version':'1.0'}},1)
 ts=mcp('tools/list',{},2);assert len(ts['tools'])>=21
 res=mcp('tools/call',{'name':'drive_stat','arguments':{'file_id':fid}},3);assert not res.get('isError'),res
 print('REAL MCP INITIALIZE/LIST/STAT PASSED',init['protocolVersion'])
 # Native overwrite serial admission, pinned originals and recycle lifecycle.
 st,meta=api('GET','/v1/files/'+fid);assert st==200
 v1=fin['version_id'];newdata=b'version two updated'
 request={'file_id':fid,'parent_id':'','name':name,'size_bytes':len(newdata),'content_type':'text/plain','idempotency_key':'overwrite-'+uuid.uuid4().hex,'expected_version_id':v1,'expected_metadata_revision':meta['metadata_revision']}
 st,up=api('POST','/v1/uploads',request);assert st==200,up
 st,conflict=api('POST','/v1/uploads',{**request,'idempotency_key':'competing-'+uuid.uuid4().hex});assert st==409,conflict
 st,_=api('PATCH','/v1/tus/'+up['id'],newdata,{'Tus-Resumable':'1.0.0','Upload-Offset':'0','Content-Type':'application/offset+octet-stream'});assert st==204
 st,newfin=api('POST','/v1/uploads/'+up['id']+'/finalize',{});assert st==200 and newfin['status']=='saved',newfin
 st,late=api('POST','/v1/uploads',{**request,'idempotency_key':'late-stale-'+uuid.uuid4().hex});assert st==409,late
 st,replay=api('POST','/v1/uploads',request);assert st==200 and replay['id']==up['id'],replay
 st,refinal=api('POST','/v1/uploads/'+up['id']+'/finalize',{});assert st==200 and refinal['version_id']==newfin['version_id'],refinal
 print('REAL STALE SECOND READER AND IDEMPOTENT FINALIZE PASSED')
 st,old=api('GET','/v1/files/'+fid+'/content?version_id='+v1);assert st==200 and old==content
 st,current=api('GET','/v1/files/'+fid+'/content');assert st==200 and current==newdata
 st,meta=api('GET','/v1/files/'+fid);assert st==200
 st,deleted=api('DELETE','/v1/files/'+fid,{'expected_metadata_revision':meta['metadata_revision'],'idempotency_key':'delete-'+uuid.uuid4().hex});assert st==200,deleted
 st,hidden=api('GET','/v1/files/'+fid+'/content');assert st in [403,404],hidden
 st,trs=api('GET','/v1/trash');assert st==200
 print('REAL NATIVE OVERWRITE CONFLICT/PINNED HISTORY/TRASH PASSED')

if True:
 tr=next(t for t in trs['items'] if t['root_file_id']==fid)
 st,rest=api('POST','/v1/trash/'+tr['id']+'/restore',{'expected_metadata_revision':tr['file']['metadata_revision'],'target_name':name,'idempotency_key':'restore-'+uuid.uuid4().hex});assert st==200,rest
 st,old=api('GET','/v1/files/'+fid+'/content?version_id='+v1);assert st==200 and old==content
 st,current=api('GET','/v1/files/'+fid+'/content');assert st==200 and current==newdata
 print('REAL NATIVE RESTORE PRESERVES BOTH VERSIONS PASSED')
 # Obtain independent B session and prove private-file denial.
 owner_op,owner_access=op,access
 op=urllib.request.build_opener(urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=os.environ.get('TEST_CA_FILE'))),urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),Redirect())
 verifier=secrets.token_urlsafe(48);challenge=base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b'=').decode()
 r=fetch(realm+'/protocol/openid-connect/auth?'+urllib.parse.urlencode({'client_id':'clouddrive-web','redirect_uri':public+'/auth/callback','response_type':'code','scope':'openid drive.read','state':'selftest-b','code_challenge':challenge,'code_challenge_method':'S256'}));b=r.read().decode();act=re.search(r'<form[^>]+action="([^"]+)"',b);assert act
 r=fetch(html.unescape(act.group(1)),{'username':os.environ.get('TEST_USERNAME_B','test-user-b'),'password':secrets_doc['user'],'credentialId':''});b=r.read().decode()
 if r.status==200:
  act=re.search(r'<form[^>]+action="([^"]+)"',b);assert act
  code=re.search(r'name="code"[^>]+value="([^"]+)"',b)
  r=fetch(html.unescape(act.group(1)),{'accept':'Yes',**({'code':html.unescape(code.group(1))} if code else {})});r.read()
 code=urllib.parse.parse_qs(urllib.parse.urlparse(r.headers['Location']).query)['code'][0]
 rr=fetch(realm+'/protocol/openid-connect/token',{'grant_type':'authorization_code','client_id':'clouddrive-web','redirect_uri':public+'/auth/callback','code':code,'code_verifier':verifier});bd=json.load(rr);access=bd['access_token'];bop=op
 st,bme=api('GET','/v1/me');assert st==200
 st,_=api('GET','/v1/files/'+fid);assert st==404
 st,_=api('GET','/v1/files/'+fid+'/content');assert st==404
 st,bs=api('POST','/v1/search',{'query':'CloudDrive'});assert st==200 and bs['items']==[]
 st,_=api('POST','/v1/directories',{'name':'forbidden','idempotency_key':uuid.uuid4().hex});assert st==403
 print('REAL NATIVE TWO-USER PRIVATE ISOLATION AND REDUCED SCOPE PASSED')
 # Approve a read/download share as A with actual UI-session CSRF; B REST can read but MCP cannot.
 op,access=owner_op,owner_access
 st,ses=api('POST','/v1/session',{});assert st==200
 st,sr=api('POST','/v1/share-requests',{'file_id':fid,'audience_type':'user','audience_id':bme['id'],'version_policy':'latest','rights':['read','download']});assert st==200
 st,_=api('POST','/v1/share-requests/'+sr['id']+'/approve',{});assert st==403
 st,grant=api('POST','/v1/share-requests/'+sr['id']+'/approve',{}, {'X-CSRF-Token':ses['csrf_token']});assert st==200,grant
 op,access=bop,bd['access_token']
 st,shared=api('GET','/v1/shared-with-me');assert st==200 and any(x.get('file_id')==fid for x in shared['items'])
 st,bc=api('GET','/v1/files/'+fid+'/content');assert st==200 and bc==newdata
 mr=mcp('tools/call',{'name':'drive_stat','arguments':{'file_id':fid}},4);assert mr.get('isError')
 print('REAL NATIVE CONFIRMED SHARING AND AGENT RECEIVED-SHARE EXCLUSION PASSED')
if True:
 # Current stateless MCP uses per-request context and server/discover, not legacy initialize.
 op,access=owner_op,owner_access
 def mcp_new(method,params,rid):
  params={**params,'_meta':{'io.modelcontextprotocol/protocolVersion':'2026-07-28','io.modelcontextprotocol/clientInfo':{'name':'native-selftest','version':'1.0'},'io.modelcontextprotocol/clientCapabilities':{}}}
  q=urllib.request.Request(os.environ.get('TEST_MCP_URL',public+'/mcp'),data=json.dumps({'jsonrpc':'2.0','id':rid,'method':method,'params':params}).encode(),headers={'Authorization':'Bearer '+access,'Content-Type':'application/json','Accept':'application/json, text/event-stream','MCP-Protocol-Version':'2026-07-28','Mcp-Method':method,**({'Mcp-Name':params['name']} if 'name' in params else {})})
  try:rr=op.open(q,timeout=30)
  except urllib.error.HTTPError as e:print('new MCP HTTP error',e.code,e.read().decode()[:300]);raise
  dd=json.load(rr);print('MCP0728',method,rr.status,'error' if dd.get('error') else 'result');assert 'error' not in dd,dd;return dd['result']
 disc=mcp_new('server/discover',{},10);assert '2026-07-28' in disc['supportedVersions'],disc
 tools=mcp_new('tools/list',{},11);assert len(tools['tools'])>=21
 rs=mcp_new('tools/call',{'name':'drive_stat','arguments':{'file_id':fid}},12);assert not rs.get('isError'),rs
 print('REAL MCP2026-07-28 DISCOVER/LIST/STAT PASSED')
