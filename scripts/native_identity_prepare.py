"""Create reusable, private synthetic native-test state outside the repository."""
import importlib.util, json, os, secrets, shlex
from pathlib import Path
root = Path(os.environ['NATIVE_RUNTIME']).resolve()
repo = Path(os.environ['NATIVE_REPO_ROOT']).resolve()
if root == repo or repo in root.parents:
    raise SystemExit('Native runtime must be outside the source repository')
public = os.environ['NATIVE_PUBLIC_URL']
state = root / 'identity'
state.chmod(0o700)
manifest = state/'configuration.json'
expected = {'public_url': public, 'users':['owner','reader'], 'keycloak':'26.8.1', 'ocis':'8.2.2'}
if manifest.exists() and json.loads(manifest.read_text()) != expected:
    raise SystemExit('Native identity configuration differs from initialized state; use a fresh NATIVE_RUNTIME')
def write(path, value):
    path.write_text(value)
    path.chmod(0o600)
def envfile(name, values):
    write(state / name, ''.join(f'export {key}={shlex.quote(str(value))}\n' for key,value in values.items()))
cp = state / 'credentials.json'
if cp.exists():
    creds = json.loads(cp.read_text())
else:
    creds = {k: secrets.token_urlsafe(32) for k in ['user','backend','identity','background','ocis_admin']}
    write(cp, json.dumps(creds))
for key,value in creds.items(): write(state / (key + '.secret'), value)
write(state/'test-credentials.json',json.dumps({name:{'username':name,'password':creds['user']} for name in ['owner','reader']}))
write(state/'service-credentials.json',json.dumps({'client_id':'clouddrive-identity','client_secret':creds['identity'],'exchange_client_id':'agent-clouddrive','exchange_client_secret':creds['backend'],'background_client_id':'clouddrive-background','background_client_secret':creds['background']}))
if not (state/'delegation.key').exists():
    (state/'delegation.key').write_bytes(secrets.token_bytes(32))
    (state/'delegation.key').chmod(0o600)
spec=importlib.util.spec_from_file_location('realm_template',repo/'deploy/compose/realm_template.py')
mod=importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
realm=mod.realm(public, creds['backend'], creds['identity'], creds['background'])
for name in ['owner','reader']:
    realm['users'].append({'username':name,'enabled':True,'email':name+'@synthetic.invalid','emailVerified':True,'firstName':'Synthetic','lastName':name,'realmRoles':['offline_access'],'credentials':[{'type':'password','value':creds['user'],'temporary':False}]})
imp=root/'keycloak-26.8.1/data/import';imp.mkdir(parents=True,exist_ok=True)
write(imp/'clouddrive-realm.json',json.dumps(realm))
issuer='https://localhost:58443/realms/clouddrive'
ca=root/'tls/server.crt'
envfile('native.env',{
 'NATIVE_RUNTIME':root,'TEST_OIDC_ISSUER':issuer,'TEST_PUBLIC_URL':public,
 'TEST_MCP_URL':'http://127.0.0.1:58082/mcp','TEST_CA_FILE':ca,'SSL_CERT_FILE':ca,
 'TEST_CREDENTIALS_FILE':state/'test-credentials.json','TEST_NATIVE_CREDENTIALS_FILE':cp,'TEST_USERNAME':'owner','TEST_USERNAME_B':'reader',
 'ALLOW_DISPOSABLE_TEST_WRITES':'yes','ALLOW_DISPOSABLE_TEST_OIDC_CONSENT':'yes','NATIVE_TEST_STATE_DIR':state,
 'DRIVE_OIDC_ISSUER':issuer,'DRIVE_PUBLIC_URL':public,'DRIVE_ALLOWED_ORIGINS':public,
 'DRIVE_MCP_PUBLIC_URL':public+'/mcp','DRIVE_OCIS_INTERNAL_URL':'https://localhost:59201',
 'DRIVE_OAUTH_AUDIENCE':'agent-clouddrive',
 'DRIVE_CA_CERT_FILE':ca,'DRIVE_IDENTITY_SERVICE_CREDENTIAL_FILE':state/'service-credentials.json',
 'DRIVE_DELEGATION_KEY_FILE':state/'delegation.key',
 'DRIVE_BACKEND_CLIENT_SECRET_FILE':state/'backend.secret',
 'DRIVE_IDENTITY_CLIENT_SECRET_FILE':state/'identity.secret',
 'DRIVE_BACKGROUND_CLIENT_SECRET_FILE':state/'background.secret'})
envfile('ocis.env',{
 'OCIS_CONFIG_DIR':root/'ocis-config','OCIS_BASE_DATA_PATH':root/'ocis-data',
 'OCIS_URL':'https://localhost:59201','PROXY_HTTP_ADDR':'127.0.0.1:59201',
 'OCIS_LOG_LEVEL':'error','OCIS_LOG_COLOR':'false','OCIS_INSECURE':'false',
 'PROXY_TLS':'true','PROXY_TRANSPORT_TLS_CERT':ca,'PROXY_TRANSPORT_TLS_KEY':root/'tls/server.key',
 'SSL_CERT_FILE':ca,'PROXY_HTTPS_CACERT':ca,
 'OCIS_OIDC_ISSUER':issuer,'OCIS_EXCLUDE_RUN_SERVICES':'idp',
 'PROXY_OIDC_REWRITE_WELLKNOWN':'true','PROXY_AUTOPROVISION_ACCOUNTS':'true',
 'PROXY_AUTOPROVISION_CLAIM_USERNAME':'sub','PROXY_USER_OIDC_CLAIM':'sub','PROXY_USER_CS3_CLAIM':'username',
 'PROXY_ROLE_ASSIGNMENT_DRIVER':'oidc','GRAPH_LDAP_SERVER_WRITE_ENABLED':'true',
 'GRAPH_ASSIGN_DEFAULT_USER_ROLE':'false','GRAPH_USERNAME_MATCH':'none','WEB_OIDC_CLIENT_ID':'ocis',
 'OCIS_DISABLE_VERSIONING':'false','OCIS_SPACES_MAX_QUOTA':'0','STORAGE_USERS_DRIVER':'ocis',
 'STORAGE_USERS_OCIS_ROOT':root/'ocis-originals',
 'STORAGE_USERS_PURGE_TRASH_BIN_PERSONAL_DELETE_BEFORE':'0',
 'STORAGE_USERS_PURGE_TRASH_BIN_PROJECT_DELETE_BEFORE':'0',
 'IDM_ADMIN_PASSWORD':creds['ocis_admin']})
write(manifest, json.dumps(expected))
print('Prepared isolated synthetic realm and TLS-verified native configuration.')
