# Keycloak native M0 validation — 2026-10-23

Official Keycloak26.8.1 on Java21 was started locally using an isolated disposable realm and synthetic user. Real HTTPS was used with the known local test certificate; TLS verification was not disabled. Tests used the repository's realm_template.py after fixing import omissions.

## Findings and passed protocol steps

1. First import exposed missing built-in profile/email/roles client scopes: declaring custom clientScopes did not implicitly supply these. The template was corrected to declare actual claim mappers and identity-client role scope mappings. A fresh realm imported the corrected template.
2. Actual login + user-consent form + authorization-code exchange with S256 PKCE succeeded.
3. Access token audience contained agent-clouddrive and the configured resource /mcp; requested drive.read/drive.write scopes were present.
4. Standard confidential-backend token exchange with the original user's subject token succeeded HTTP200. The resulting token had audience ocis and roles [ocisUser]. No administrator impersonation was used.

This is real identity-provider protocol evidence, not complete G01/G02 acceptance. It does not yet prove external-oCIS user provisioning/mapping, worker refresh delegation, full account-disable propagation or actual Peri client integration. Secrets, raw tokens, login session URLs and local server private keys are deliberately excluded from the deliverable.

Source distribution: https://www.keycloak.org/downloads and https://github.com/keycloak/keycloak/releases/tag/26.8.1 .

## Additional actual integration corrections

- oCIS external-IdP personal-space discovery succeeded after requesting `openid` explicitly during token exchange. Without it, token exchange returned200 but oCIS userinfo returned403 and file access401. The adapter now includes openid.
- The background offline consent flow must use a separate confidential client. Marking the shared exchange client consentRequired caused ordinary interactive exchange to fail before background consent. A dedicated clouddrive-background client restored interactive access.
- A disposable imported test user explicitly granted offline_access obtained an Offline refresh token, refreshed it successfully, and exchanged the renewed access token for oCIS audience. This does not by itself prove normal administrative user provisioning inherits the expected default role; template uses an explicit default-role composite and scope mapping.
- Native full application authentication exposed another real template problem: access JWTs without the basic client-scope sub mapper lacked a subject. The API correctly rejected them rather than accepting identity from an unverified source. The realm template requires an explicit basic scope and subject mapper.

No tokens or test passwords are included in this evidence record.
