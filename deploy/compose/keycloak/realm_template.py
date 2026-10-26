"""Generate Keycloak reference realm; no real users or user passwords included."""
import json

SCOPES = ["drive.read", "drive.write", "drive.delete", "drive.restore", "drive.convert", "drive.share.request", "drive.feedback"]


def mapper(name, protocol_mapper, **config):
    return {"name": name, "protocol": "openid-connect", "protocolMapper": protocol_mapper, "consentRequired": False, "config": config}


def audience(name, target):
    return {"name": name, "protocol": "openid-connect", "attributes": {"include.in.token.scope": "false"},
            "protocolMappers": [mapper(name, "oidc-audience-mapper", **{"included.client.audience": target, "id.token.claim": "false", "access.token.claim": "true"})]}


def realm(public_url="__PUBLIC_URL__", backend_secret="__BACKEND_CLIENT_SECRET__", identity_secret="__IDENTITY_CLIENT_SECRET__", background_secret="__BACKGROUND_CLIENT_SECRET__"):
    profile_mappers = [mapper("profile " + claim, "oidc-usermodel-property-mapper", **{"user.attribute": field, "claim.name": claim, "jsonType.label": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"}) for field, claim in (("username", "preferred_username"), ("firstName", "given_name"), ("lastName", "family_name"))]
    profile_mappers.append(mapper("full name", "oidc-full-name-mapper", **{"id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"}))
    standard = [
        {"name": "basic", "protocol": "openid-connect", "attributes": {"include.in.token.scope": "false"}, "protocolMappers": [mapper("subject", "oidc-sub-mapper", **{"access.token.claim": "true", "introspection.token.claim": "true"})]},
        {"name": "profile", "protocol": "openid-connect", "attributes": {"include.in.token.scope": "true"}, "protocolMappers": profile_mappers},
        {"name": "email", "protocol": "openid-connect", "attributes": {"include.in.token.scope": "true"}, "protocolMappers": [mapper("email", "oidc-usermodel-property-mapper", **{"user.attribute": "email", "claim.name": "email", "jsonType.label": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"}), mapper("email verified", "oidc-usermodel-property-mapper", **{"user.attribute": "emailVerified", "claim.name": "email_verified", "jsonType.label": "boolean", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"})]},
        {"name": "roles", "protocol": "openid-connect", "attributes": {"include.in.token.scope": "false"}, "protocolMappers": [mapper("client roles", "oidc-usermodel-client-role-mapper", **{"access.token.claim": "true", "claim.name": "resource_access.${client_id}.roles", "jsonType.label": "String", "multivalued": "true"}), mapper("realm roles", "oidc-usermodel-realm-role-mapper", **{"access.token.claim": "true", "claim.name": "realm_access.roles", "jsonType.label": "String", "multivalued": "true"}), mapper("audience resolve", "oidc-audience-resolve-mapper")]},
    ]
    scopes = [{"name": s, "protocol": "openid-connect", "attributes": {"include.in.token.scope": "true", "display.on.consent.screen": "true", "consent.screen.text": s}} for s in SCOPES]
    scopes.append({"name": "offline_access", "protocol": "openid-connect", "attributes": {"include.in.token.scope": "true", "display.on.consent.screen": "true", "consent.screen.text": "Allow background conversion to read your own files after you close the browser"}})
    scopes += standard
    scopes += [audience("audience-drive", "agent-clouddrive"), audience("audience-ocis", "ocis")]
    scopes[-2]["protocolMappers"].append(mapper("mcp resource", "oidc-audience-mapper", **{"included.custom.audience": public_url + "/mcp", "id.token.claim": "false", "access.token.claim": "true"}))
    scopes.append({"name": "groups", "protocol": "openid-connect", "protocolMappers": [mapper("groups", "oidc-group-membership-mapper", **{"full.path": "true", "claim.name": "groups", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"})]})
    scopes.append({"name": "ocis-roles", "protocol": "openid-connect", "protocolMappers": [mapper("ocis user role", "oidc-hardcoded-claim-mapper", **{"claim.name": "roles", "claim.value": '["ocisUser"]', "jsonType.label": "JSON", "id.token.claim": "false", "access.token.claim": "true", "userinfo.token.claim": "true"})]})
    clients = []
    for client_id in ("clouddrive-web", "peri"):
        clients.append({"clientId": client_id, "enabled": True, "protocol": "openid-connect", "publicClient": True, "standardFlowEnabled": True,
                        "directAccessGrantsEnabled": False, "implicitFlowEnabled": False, "serviceAccountsEnabled": False, "consentRequired": True,
                        "fullScopeAllowed": False, "redirectUris": [public_url + "/auth/callback"] if client_id == "clouddrive-web" else [],
                        "webOrigins": [public_url] if client_id == "clouddrive-web" else [],
                        "attributes": {"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": public_url + "/*"},
                        "defaultClientScopes": ["basic", "profile", "email", "roles", "groups", "audience-drive"], "optionalClientScopes": SCOPES})
    clients += [
        {"clientId": "agent-clouddrive", "enabled": True, "protocol": "openid-connect", "publicClient": False,
         "secret": backend_secret, "standardFlowEnabled": False, "directAccessGrantsEnabled": False, "serviceAccountsEnabled": False, "consentRequired": False,
         "fullScopeAllowed": False, "attributes": {"standard.token.exchange.enabled": "true"},
         "defaultClientScopes": ["basic", "profile", "email", "groups", "ocis-roles", "audience-ocis"], "optionalClientScopes": SCOPES},
        {"clientId": "clouddrive-background", "enabled": True, "protocol": "openid-connect", "publicClient": False,
         "secret": background_secret, "standardFlowEnabled": True, "directAccessGrantsEnabled": False, "serviceAccountsEnabled": False,
         "redirectUris": [public_url + "/v1/delegation/callback"], "webOrigins": [public_url], "consentRequired": True,
         "fullScopeAllowed": False, "attributes": {"pkce.code.challenge.method": "S256"},
         "defaultClientScopes": ["basic", "profile", "email", "groups", "audience-drive"], "optionalClientScopes": SCOPES + ["offline_access"]},
        {"clientId": "ocis", "enabled": True, "protocol": "openid-connect", "publicClient": False,
         "standardFlowEnabled": False, "directAccessGrantsEnabled": False, "serviceAccountsEnabled": False, "fullScopeAllowed": False,
         "defaultClientScopes": ["basic", "profile", "email", "groups", "ocis-roles"]},
        {"clientId": "clouddrive-identity", "enabled": True, "protocol": "openid-connect", "publicClient": False,
         "secret": identity_secret, "standardFlowEnabled": False, "directAccessGrantsEnabled": False, "serviceAccountsEnabled": True,
         "fullScopeAllowed": False, "defaultClientScopes": ["basic", "roles"]},
    ]
    return {"realm": "clouddrive", "enabled": True, "displayName": "Agent CloudDrive", "sslRequired": "external", "registrationAllowed": False,
            "loginWithEmailAllowed": True, "duplicateEmailsAllowed": False, "resetPasswordAllowed": True,
            "bruteForceProtected": True, "accessTokenLifespan": 300, "ssoSessionIdleTimeout": 1800,
            "roles": {"realm": [{"name": "offline_access", "description": "Offline access for explicit user-consented background processing"}, {"name": "default-roles-clouddrive", "composite": True, "composites": {"realm": ["offline_access"]}}]},
            "defaultRole": {"name": "default-roles-clouddrive", "composite": True, "clientRole": False},
            "eventsEnabled": True, "eventsListeners": [], "enabledEventTypes": ["UPDATE_PROFILE"],
            "adminEventsEnabled": True, "adminEventsDetailsEnabled": True, "clientScopes": scopes, "clients": clients,
            "scopeMappings": [{"clientScope": "offline_access", "roles": ["offline_access"]}],
            "clientScopeMappings": {"realm-management": [{"client": "clouddrive-identity", "roles": ["view-users", "query-users", "view-events"]}]},
            "users": [{"username": "service-account-clouddrive-identity", "enabled": True, "serviceAccountClientId": "clouddrive-identity",
                       "clientRoles": {"realm-management": ["view-users", "query-users", "view-events"]}}]}


if __name__ == "__main__":
    print(json.dumps(realm(), indent=2))
