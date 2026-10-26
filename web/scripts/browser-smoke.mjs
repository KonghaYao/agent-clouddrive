// Real Chromium smoke test. No --no-sandbox, token injection, or fake authenticated state.
// This test checks the actual compiled login UI and the outbound PKCE request.
import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { createServer } from 'vite';
import { chromium } from 'playwright-core';
const server = await createServer({ server: { host: '127.0.0.1', port: 5173, strictPort: true } });
let browser;
try {
  await server.listen();
  browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true, chromiumSandbox: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const origin = 'http://127.0.0.1:5173';
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/config.json', route => route.fulfill({ contentType: 'application/json', body: JSON.stringify({ oidc_issuer: `${origin}/test-identity`, oidc_client_id: 'clouddrive-web', oidc_audience: 'agent-clouddrive' }) }));
  await page.route('**/test-identity/.well-known/openid-configuration', route => route.fulfill({ contentType: 'application/json', body: JSON.stringify({ issuer: `${origin}/test-identity`, authorization_endpoint: `${origin}/test-identity/authorize`, token_endpoint: `${origin}/test-identity/token`, jwks_uri: `${origin}/test-identity/keys`, response_types_supported: ['code'], subject_types_supported: ['public'], id_token_signing_alg_values_supported: ['RS256'], code_challenge_methods_supported: ['S256'] }) }));
  await page.route('**/test-identity/authorize?**', route => route.fulfill({ contentType: 'text/html', body: '<title>PKCE test authorization endpoint</title><p>Authorization request received. This smoke test does not issue identity tokens.</p>' }));
  await page.goto(origin);
  await page.getByRole('heading', { name: '连接你的个人空间' }).waitFor();
  assert.equal(await page.getByRole('checkbox').count(), 7);
  for (const checkbox of await page.getByRole('checkbox').all()) assert.equal(await checkbox.isChecked(), true);
  await page.getByRole('button', { name: '取消全选' }).click();
  assert.equal(await page.getByRole('button', { name: '继续安全登录' }).isDisabled(), true);
  await page.getByRole('button', { name: '全部选择' }).click();
  await page.getByRole('checkbox').nth(2).uncheck();
  await mkdir('test-results', { recursive: true });
  await page.screenshot({ path: 'test-results/login-desktop.png', fullPage: true });
  await page.getByRole('button', { name: '继续安全登录' }).click();
  await page.waitForURL('**/test-identity/authorize?**');
  const request = new URL(page.url());
  assert.equal(request.searchParams.get('response_type'), 'code');
  assert.equal(request.searchParams.get('code_challenge_method'), 'S256');
  assert.ok(request.searchParams.get('code_challenge'));
  assert.ok(request.searchParams.get('state'));
  assert.equal(request.searchParams.get('scope').includes('drive.delete'), false);
  assert.equal(request.searchParams.get('redirect_uri'), `${origin}/auth/callback`);
  await page.goto(`${origin}/auth/callback?error=access_denied&state=${encodeURIComponent(request.searchParams.get('state'))}`);
  await page.getByText('身份验证未完成。请检查身份服务配置后重新登录。').waitFor();
  assert.equal(new URL(page.url()).search, '');
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: 'test-results/login-mobile.png', fullPage: true });
  assert.deepEqual(errors, []);
  console.log('PASS: browser scope selection, PKCE redirect, interrupted login, mobile layout smoke. Does not verify real IdP/oCIS or 10GiB transfer.');
} finally { await browser?.close(); await server.close(); }
