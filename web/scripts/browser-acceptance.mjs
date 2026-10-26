#!/usr/bin/env node
// Live DOM + real Keycloak acceptance. --check-only never opens a socket or browser.
import { randomUUID, createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import { AcceptanceError, requireCondition as check, failureCode, loadConfig, preflight, newSafeContext, login, observeSession, apiResponse, checkedJSON, createEvidence, createEvidenceWriter, runCase, parseArgs, validateAnonymousLink, anonymousResourceKind, verifyAnonymousHTML } from './browser-acceptance-lib.mjs';

export const CASE_IDS = [
  'real_keycloak_owner_login', 'real_keycloak_recipient_login', 'own_file_list_dom',
  'read_only_scope_ui_and_backend', 'offline_delegation_prerequisite', 'upload_first_version_dom',
  'private_file_isolation_backend', 'raster_preview_dom', 'overwrite_and_pinned_history_dom_and_backend',
  'trash_and_restore_dom', 'share_explicit_confirmation_dom', 'received_share_read_only_dom_and_backend',
  'share_revocation_dom_and_backend', 'anonymous_pdf_link_preview_dom_and_resource_isolation',
  'anonymous_pdf_link_revocation_dom_and_backend', 'fixture_soft_delete_dom',
];
// Deterministic, synthetic, valid single-page PDFs. No external file or user data is uploaded.
export function fixturePDF(marker) {
  check(/^[A-Za-z0-9 -]+$/.test(marker), 'invalid_synthetic_fixture_marker');
  const text = `BT /F1 18 Tf 50 740 Td (${marker}) Tj ET\n`;
  const objects = ['<< /Type /Catalog /Pages 2 0 R >>', '<< /Type /Pages /Kids [3 0 R] /Count 1 >>', '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>', '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>', `<< /Length ${Buffer.byteLength(text)} >>\nstream\n${text}endstream`];
  let pdf = '%PDF-1.4\n', offsets = [0];
  for (let i = 0; i < objects.length; i++) { offsets.push(Buffer.byteLength(pdf)); pdf += `${i + 1} 0 obj\n${objects[i]}\nendobj\n`; }
  const xref = Buffer.byteLength(pdf);
  pdf += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n` + offsets.slice(1).map(offset => `${String(offset).padStart(10, '0')} 00000 n \n`).join('');
  pdf += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(pdf);
}
const sha256 = buffer => createHash('sha256').update(buffer).digest('hex');
async function refreshList(page, config, endpoint = '/v1/files') {
  const response = apiResponse(page, config, endpoint);
  await page.getByRole('button', { name: '刷新', exact: true }).click();
  return checkedJSON(await response);
}
async function section(page, label) {
  await page.getByRole('menuitem', { name: label, exact: true }).click();
  await page.getByRole('heading', { name: label, exact: true }).waitFor();
}
async function allListed(page, config, endpoint) {
  let batch = await refreshList(page, config, endpoint);
  const items = [...(batch.items ?? [])], seen = new Set();
  // Follow response cursors and the actual UI, not a timing-sensitive visibility snapshot.
  while (batch.next_cursor) {
    check(!seen.has(batch.next_cursor), 'listing_cursor_cycle'); seen.add(batch.next_cursor);
    const response = apiResponse(page, config, endpoint);
    await page.getByRole('button', { name: '加载更多', exact: true }).click();
    batch = await checkedJSON(await response); items.push(...(batch.items ?? []));
  }
  return items;
}
async function ownFixture(page, config, name) {
  const deadline = Date.now() + config.timeoutMs;
  while (Date.now() < deadline) {
    const row = (await allListed(page, config, '/v1/files')).find(item => item.name === name);
    if (row?.id && row.current_version_id) { await page.getByRole('button', { name, exact: true }).waitFor(); return row; }
    await page.waitForTimeout(500);
  }
  throw new AcceptanceError('saved_fixture_missing_from_real_list');
}
async function openDetails(page, name) {
  await page.getByRole('button', { name, exact: true }).click();
  const drawer = page.getByRole('dialog', { name, exact: true });
  await drawer.waitFor(); return drawer;
}
async function closeDetails(page, name) {
  const drawer = page.getByRole('dialog', { name, exact: true });
  // Ant Design exposes a stable accessible Close button in the drawer header.
  await drawer.getByRole('button', { name: 'Close', exact: true }).click();
  await drawer.waitFor({ state: 'hidden' });
}
async function fileMenu(page, name, action) {
  await page.getByRole('button', { name: `${name} 的操作`, exact: true }).click();
  await page.getByRole('menuitem', { name: action, exact: true }).click();
}
async function upload(page, config, name, bytes, overwrite = false) {
  if (overwrite) await fileMenu(page, name, '上传新版本');
  else await page.getByRole('button', { name: '上传文件', exact: true }).click();
  const modal = page.getByRole('dialog', { name: overwrite ? '上传新版本' : '上传文件', exact: true });
  await modal.locator('input[type="file"]').first().setInputFiles({ name, mimeType: 'application/pdf', buffer: bytes });
  // Uppy owns this hidden-input/button pair; no application hook or state injection.
  await modal.locator('.uppy-StatusBar-actionBtn--upload').click();
  await modal.getByText('原件已保存，转换另行处理', { exact: true }).waitFor({ timeout: config.timeoutMs });
  await modal.getByRole('button', { name: 'Close', exact: true }).click();
  await page.getByRole('button', { name: '关闭面板', exact: true }).click();
  await modal.waitFor({ state: 'hidden' });
  return ownFixture(page, config, name);
}
async function verifySource(session, fileId, version, expected) {
  // Native showSaveFilePicker cannot be driven by Playwright's input filechooser.
  // Do not replace it. Verify real source bytes with the authentic browser session,
  // and explicitly leave native save-picker UX outside this automated coverage.
  const response = await session.request('GET', `/v1/files/${encodeURIComponent(fileId)}/content?version_id=${encodeURIComponent(version)}`);
  check(response.ok() && sha256(await response.body()) === sha256(expected), 'authenticated_source_bytes_or_pinned_version_mismatch');
}
async function softDelete(page, config, name) {
  await fileMenu(page, name, '移入回收站');
  const modal = page.getByRole('dialog', { name: `将“${name}”移入回收站？`, exact: true });
  await modal.getByRole('button', { name: '移入回收站', exact: true }).click();
  await modal.waitFor({ state: 'hidden' });
  const deadline = Date.now() + config.timeoutMs;
  while (Date.now() < deadline) {
    const rows = await allListed(page, config, '/v1/files');
    if (!rows.some(row => row.name === name)) return;
    await page.waitForTimeout(500);
  }
  throw new AcceptanceError('soft_delete_did_not_leave_own_list');
}
async function previewDOM(page, config, drawer, fileId, versionId) {
  const result = apiResponse(page, config, `/v1/preview/${encodeURIComponent(fileId)}`);
  result.catch(() => {});
  await drawer.getByRole('tab', { name: '原件预览', exact: true }).click();
  const manifest = await checkedJSON(await result, 'real_preview_service_did_not_return_success');
  check(manifest.version_id === versionId && manifest.pages?.length > 0, 'preview_version_or_page_count_mismatch');
  for (const pageInfo of manifest.pages) {
    const url = new URL(pageInfo.url, config.publicURL);
    check(url.origin === config.publicURL && url.pathname.startsWith(`/v1/preview/${fileId}/pages/`) && url.searchParams.get('version_id') === versionId, 'uncontrolled_preview_asset');
  }
  const image = drawer.getByRole('img', { name: /^第 \d+ 页$/ }).first();
  await image.waitFor();
  await image.evaluate(async element => { if (!element.complete) await new Promise((resolve, reject) => { element.addEventListener('load', resolve, { once: true }); element.addEventListener('error', reject, { once: true }); }); });
  check(await image.evaluate(element => element.complete && element.naturalWidth > 0 && element.naturalHeight > 0), 'preview_image_not_decoded');
}
export async function executeBrowserCases(browser, config, evidence) {
  const ownerContext = await newSafeContext(browser), recipientContext = await newSafeContext(browser);
  const owner = await ownerContext.newPage(), recipient = await recipientContext.newPage();
  const ownerSession = observeSession(owner, config), recipientSession = observeSession(recipient, config);
  let ownerMe, recipientMe, file, firstVersion, latestVersion, shareId;
  let anonymousContext, anonymousPage, anonymousLink, anonymousShareId, priorAnonymousImageURL;
  const name = `browser-acceptance-${randomUUID()}.pdf`;
  const first = fixturePDF('CloudDrive acceptance version one'), second = fixturePDF('CloudDrive acceptance version two');
  let pageErrors = 0; owner.on('pageerror', () => pageErrors++); recipient.on('pageerror', () => pageErrors++);
  const test = (id, action) => runCase(evidence, id, async () => { const before = pageErrors; await action(); check(pageErrors === before, 'unexpected_application_javascript_error'); });
  try {
    await test('real_keycloak_owner_login', async () => { ownerMe = await login(owner, config, config.owner); });
    await test('real_keycloak_recipient_login', async () => { recipientMe = await login(recipient, config, config.recipient, { readOnly: true }); check(ownerMe.id !== recipientMe.id, 'fixture_accounts_resolved_to_same_identity'); });
    await test('own_file_list_dom', async () => { const listed = await refreshList(owner, config); check(Array.isArray(listed.items), 'own_list_response_missing_items'); await owner.getByRole('table').waitFor(); });
    await test('read_only_scope_ui_and_backend', async () => {
      check(await recipient.getByRole('button', { name: '上传文件', exact: true }).isDisabled(), 'read_only_upload_not_disabled');
      check(await recipient.getByRole('button', { name: '新建文件夹', exact: true }).isDisabled(), 'read_only_create_not_disabled');
      const forbiddenName = `forbidden-${randomUUID()}`;
      const denied = await recipientSession.request('POST', '/v1/directories', { name: forbiddenName, idempotency_key: randomUUID() });
      const body = await denied.json();
      if (denied.ok()) evidence.unexpected_mutations = [{ kind: 'directory', name: forbiddenName, ...(typeof body.id === 'string' && /^[a-f0-9-]{36}$/.test(body.id) ? { resource_id: body.id } : {}) }];
      check(denied.status() === 403 && (body.error?.code ?? body.code) === 'INSUFFICIENT_SCOPE', 'backend_did_not_enforce_reduced_scope');
    });
    await test('offline_delegation_prerequisite', async () => {
      const status = await checkedJSON(await ownerSession.request('GET', '/v1/delegation/status'));
      check(status.enabled === true, 'owner_offline_delegation_must_be_prepared_interactively');
    });
    await test('upload_first_version_dom', async () => {
      evidence.retained_fixtures.push({ name, lifecycle: 'outcome_unknown', synthetic_only: true, permanent_deletion: false });
      file = await upload(owner, config, name, first); firstVersion = file.current_version_id;
      Object.assign(evidence.retained_fixtures[0], { file_id: file.id, lifecycle: 'active' });
    });
    await test('private_file_isolation_backend', async () => {
      for (const suffix of ['', '/content', '/versions']) { const denied = await recipientSession.request('GET', `/v1/files/${encodeURIComponent(file.id)}${suffix}`); check([403, 404].includes(denied.status()), 'recipient_could_read_private_owner_file'); }
      check(!(await allListed(recipient, config, '/v1/files')).some(row => row.id === file.id), 'private_file_in_recipient_own_list');
    });
    await test('raster_preview_dom', async () => { const drawer = await openDetails(owner, name); await previewDOM(owner, config, drawer, file.id, firstVersion); await closeDetails(owner, name); });
    await test('overwrite_and_pinned_history_dom_and_backend', async () => {
      const updated = await upload(owner, config, name, second, true); latestVersion = updated.current_version_id;
      check(updated.id === file.id && latestVersion !== firstVersion, 'overwrite_did_not_preserve_file_and_create_version');
      const versionsResponse = apiResponse(owner, config, `/v1/files/${encodeURIComponent(file.id)}/versions`);
      const drawer = await openDetails(owner, name), versions = await checkedJSON(await versionsResponse);
      check(versions.items?.some(v => (v.version_id ?? v.id) === firstVersion) && versions.items?.some(v => (v.version_id ?? v.id) === latestVersion), 'both_source_versions_not_retained');
      await drawer.getByRole('tab', { name: '历史版本', exact: true }).click();
      await drawer.getByRole('button', { name: /^v1 ·/ }).click();
      await drawer.getByRole('button', { name: /^v1 ·.*当前选择$/ }).waitFor(); await verifySource(ownerSession, file.id, firstVersion, first);
      await drawer.getByRole('button', { name: /^v2 ·/ }).click();
      await drawer.getByRole('button', { name: /^v2 ·.*当前选择$/ }).waitFor(); await verifySource(ownerSession, file.id, latestVersion, second);
      await closeDetails(owner, name);
    });
    await test('trash_and_restore_dom', async () => {
      await softDelete(owner, config, name); evidence.retained_fixtures[0].lifecycle = 'trash';
      await section(owner, '回收站');
      const trash = await allListed(owner, config, '/v1/trash');
      check(trash.some(row => (row.file?.id ?? row.id) === file.id), 'fixture_not_in_trash');
      const row = owner.getByRole('row').filter({ has: owner.getByText(name, { exact: true }) });
      await row.getByRole('button', { name: '恢复', exact: true }).click();
      const modal = owner.getByRole('dialog', { name: '恢复文件', exact: true });
      await modal.getByLabel('名称', { exact: true }).fill(name);
      await modal.getByRole('button', { name: '确认', exact: true }).click(); await modal.waitFor({ state: 'hidden' });
      await section(owner, '我的文件');
      const restored = await ownFixture(owner, config, name);
      check(restored.id === file.id && restored.current_version_id === latestVersion, 'restore_changed_file_or_latest_version');
      evidence.retained_fixtures[0].lifecycle = 'active';
      const drawer = await openDetails(owner, name); await drawer.getByRole('tab', { name: '历史版本', exact: true }).click();
      await drawer.getByRole('button', { name: /^v1 ·/ }).click(); await drawer.getByRole('button', { name: /^v1 ·.*当前选择$/ }).waitFor(); await verifySource(ownerSession, file.id, firstVersion, first); await closeDetails(owner, name);
    });
    await test('share_explicit_confirmation_dom', async () => {
      await fileMenu(owner, name, '请求分享');
      const request = owner.getByRole('dialog', { name: '创建分享待确认请求', exact: true });
      await request.getByLabel('系统用户 ID', { exact: true }).fill(recipientMe.id);
      await request.getByLabel('有效天数', { exact: true }).fill('1');
      await request.getByRole('button', { name: '创建待确认请求', exact: true }).click();
      const approval = owner.getByRole('dialog', { name: '确认开放文件访问', exact: true }); await approval.waitFor();
      await approval.getByText(`用户：${recipientMe.id}`, { exact: true }).waitFor();
      check(await approval.getByRole('button', { name: '确认并开放访问', exact: true }).isDisabled(), 'share_confirmation_checkbox_not_required');
      const denied = await recipientSession.request('GET', `/v1/files/${encodeURIComponent(file.id)}/content`);
      check([403, 404].includes(denied.status()), 'pending_request_already_granted_access');
      await approval.getByText(`用户：${recipientMe.id}`, { exact: true }).waitFor();
      await approval.getByRole('checkbox', { name: '我已核对上述信息，同意按这些固定条件开放访问', exact: true }).check();
      const response = owner.waitForResponse(r => new URL(r.url()).origin === config.publicURL && /^\/v1\/share-requests\/[^/]+\/approve$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST');
      await approval.getByRole('button', { name: '确认并开放访问', exact: true }).click();
      const granted = await checkedJSON(await response); shareId = granted.id; check(shareId, 'confirmed_share_missing_id');
      evidence.retained_fixtures[0].share_id = shareId;
      await approval.waitFor({ state: 'hidden' });
    });
    await test('received_share_read_only_dom_and_backend', async () => {
      await section(recipient, '分享给我');
      const shared = await allListed(recipient, config, '/v1/shared-with-me');
      check(shared.some(row => (row.file?.id ?? row.file_id ?? row.id) === file.id), 'confirmed_share_missing_for_recipient');
      const drawer = await openDetails(recipient, name);
      check(await drawer.getByRole('tab', { name: '历史版本', exact: true }).count() === 0 && await drawer.getByRole('tab', { name: '转换正文', exact: true }).count() === 0, 'recipient_received_owner_only_tabs');
      check(await drawer.getByRole('button', { name: /重新转换|请求分享/ }).count() === 0, 'received_share_exposed_mutating_controls');
      // Shared tabs initially select preview but the component intentionally requires a load click.
      const load = drawer.getByRole('button', { name: '加载受控预览', exact: true });
      if (await load.isVisible()) await load.click();
      await drawer.getByRole('img', { name: /^第 \d+ 页$/ }).first().waitFor();
      check(await drawer.getByRole('button', { name: '下载原件', exact: true }).isEnabled(), 'shared_original_download_control_not_enabled');
      const result = await recipientSession.request('GET', `/v1/files/${encodeURIComponent(file.id)}/content`);
      check(result.ok() && sha256(await result.body()) === sha256(second), 'recipient_download_was_not_latest_original');
      const denied = await recipientSession.request('GET', `/v1/files/${encodeURIComponent(file.id)}/content?version_id=${encodeURIComponent(firstVersion)}`);
      check([403, 404].includes(denied.status()), 'latest_share_leaked_historical_source');
      await closeDetails(recipient, name);
    });
    await test('share_revocation_dom_and_backend', async () => {
      await section(owner, '分享管理'); await owner.getByRole('tab', { name: '已有分享', exact: true }).click();
      const row = owner.getByRole('row').filter({ hasText: shareId });
      // API projections may display file ID rather than file name, so select the exact generated file.
      const fileRow = await row.count() ? row : owner.getByRole('row').filter({ hasText: name }).or(owner.getByRole('row').filter({ hasText: file.id }));
      check(await fileRow.count() === 1, 'cannot_uniquely_select_created_share');
      await fileRow.getByRole('button', { name: '撤销', exact: true }).click();
      await owner.getByRole('button', { name: '撤销分享', exact: true }).click();
      await owner.getByText('分享已撤销', { exact: true }).last().waitFor();
      const denied = await recipientSession.request('GET', `/v1/files/${encodeURIComponent(file.id)}/content`);
      check([403, 404].includes(denied.status()), 'revoked_share_still_allows_download');
      const listed = await allListed(recipient, config, '/v1/shared-with-me');
      check(!listed.some(row => (row.file?.id ?? row.file_id ?? row.id) === file.id), 'revoked_share_still_listed');
      evidence.retained_fixtures[0].share_revoked = true;
    });
    await test('anonymous_pdf_link_preview_dom_and_resource_isolation', async () => {
      await section(owner, '我的文件'); await ownFixture(owner, config, name);
      await fileMenu(owner, name, '请求分享');
      const request = owner.getByRole('dialog', { name: '创建分享待确认请求', exact: true });
      await request.getByLabel('分享给', { exact: true }).click();
      await owner.getByRole('option', { name: '未登录链接访问者', exact: true }).click();
      await request.getByText('仅受控原件预览，不提供原件下载', { exact: true }).waitFor();
      await request.getByLabel('有效天数', { exact: true }).fill('1');
      await request.getByRole('button', { name: '创建待确认请求', exact: true }).click();
      const approval = owner.getByRole('dialog', { name: '确认开放文件访问', exact: true });
      await approval.getByText('持有链接的未登录访问者', { exact: true }).waitFor();
      check(await approval.getByRole('button', { name: '确认并开放访问', exact: true }).isDisabled(), 'anonymous_share_confirmation_checkbox_not_required');
      await approval.getByRole('checkbox', { name: '我已核对上述信息，同意按这些固定条件开放访问', exact: true }).check();
      const grantedResponse = owner.waitForResponse(r => new URL(r.url()).origin === config.publicURL && /^\/v1\/share-requests\/[^/]+\/approve$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST');
      await approval.getByRole('button', { name: '确认并开放访问', exact: true }).click();
      const grant = await checkedJSON(await grantedResponse);
      check(typeof grant.id === 'string' && /^[a-f0-9-]{36}$/.test(grant.id), 'anonymous_grant_missing_valid_id');
      anonymousShareId = grant.id;
      // This capability URL is deliberately held only in memory, never evidence/logs/clipboard.
      anonymousLink = validateAnonymousLink(grant.public_url ?? grant.url, config);
      evidence.retained_fixtures[0].anonymous_share_id = anonymousShareId;
      const grantedDialog = owner.getByRole('dialog', { name: '分享已开放', exact: true });
      await grantedDialog.getByText(anonymousLink.href, { exact: true }).waitFor();
      await grantedDialog.getByRole('button', { name: '已保存链接', exact: true }).click();
      anonymousContext = await newSafeContext(browser); anonymousPage = await anonymousContext.newPage();
      anonymousPage.setDefaultTimeout(config.timeoutMs);
      let unexpectedRequest = false, decodedPages = 0, rasterRequests = 0;
      anonymousPage.on('request', request => {
        const kind = anonymousResourceKind(request.url(), config, anonymousLink.pathname);
        if (kind === 'unexpected') unexpectedRequest = true;
        if (kind === 'raster_page') rasterRequests++;
      });
      const document = await anonymousPage.goto(anonymousLink.href, { waitUntil: 'domcontentloaded' });
      check(document?.status() === 200 && document.headers()['content-type']?.includes('text/html'), 'unsigned_pdf_link_did_not_return_preview_html');
      verifyAnonymousHTML(await anonymousPage.content());
      check(await anonymousPage.locator('a[href], button, form, iframe, embed, object, script').count() === 0, 'unsigned_preview_exposed_actions_or_active_content');
      const images = anonymousPage.locator('img');
      await images.first().waitFor();
      for (const img of await images.all()) {
        const imageURL = await img.getAttribute('src');
        check(imageURL && anonymousResourceKind(imageURL, config, anonymousLink.pathname) === 'raster_page', 'unsigned_preview_image_not_controlled');
        const absolute = new URL(imageURL, config.publicURL);
        check(absolute.searchParams.get('version_id') === latestVersion, 'unsigned_preview_did_not_use_current_source');
        priorAnonymousImageURL ??= absolute.href;
        const raster = await anonymousContext.request.get(absolute.href, { maxRedirects: 0, timeout: config.timeoutMs });
        const rasterBytes = await raster.body();
        check(raster.status() === 200 && /^image\/png(?:;|$)/.test(raster.headers()['content-type'] ?? '') && rasterBytes.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])), 'unsigned_preview_resource_is_not_png');
        await img.evaluate(element => element.decode());
        check(await img.evaluate(element => element.complete && element.naturalWidth > 0 && element.naturalHeight > 0), 'unsigned_preview_raster_did_not_decode');
        decodedPages++;
      }
      check(!unexpectedRequest && decodedPages > 0 && rasterRequests >= decodedPages, 'unsigned_preview_requested_uncontrolled_resource');
      const originalDenied = await anonymousContext.request.get(`${config.publicURL}/v1/files/${encodeURIComponent(file.id)}/content`, { maxRedirects: 0, timeout: config.timeoutMs });
      const internalPreviewDenied = await anonymousContext.request.get(`${config.publicURL}/v1/preview/${encodeURIComponent(file.id)}`, { maxRedirects: 0, timeout: config.timeoutMs });
      check([401, 403, 404].includes(originalDenied.status()) && [401, 403, 404].includes(internalPreviewDenied.status()), 'unsigned_link_context_can_access_authenticated_original_or_preview');
      evidence.cases.find(c => c.id === 'anonymous_pdf_link_preview_dom_and_resource_isolation').facts = { decoded_pdf_pages: decodedPages, original_http_status: originalDenied.status(), authenticated_preview_http_status: internalPreviewDenied.status(), resource_chain_confined_to_public_rasters: true };
    });
    await test('anonymous_pdf_link_revocation_dom_and_backend', async () => {
      await section(owner, '分享管理'); await owner.getByRole('tab', { name: '已有分享', exact: true }).click();
      // Ant Table's stable rowKey is the verified generated grant ID, not its secret link.
      const row = owner.locator(`tr[data-row-key="${anonymousShareId}"]`);
      await row.getByRole('button', { name: '撤销', exact: true }).click();
      await owner.getByRole('button', { name: '撤销分享', exact: true }).click();
      await owner.getByText('分享已撤销', { exact: true }).last().waitFor();
      // An already-decoded image cannot be recalled; prove a new request to its exact old URL is denied.
      const revokedImage = await anonymousContext.request.get(priorAnonymousImageURL, { maxRedirects: 0, timeout: config.timeoutMs, headers: { 'Cache-Control': 'no-cache' } });
      check([403, 404].includes(revokedImage.status()), 'revoked_anonymous_page_resource_still_accessible');
      const revokedDocument = await anonymousPage.reload({ waitUntil: 'domcontentloaded' });
      check([403, 404].includes(revokedDocument?.status()), 'revoked_anonymous_html_still_accessible');
      evidence.retained_fixtures[0].anonymous_share_revoked = true;
      evidence.cases.find(c => c.id === 'anonymous_pdf_link_revocation_dom_and_backend').facts = { previously_issued_image_http_status: revokedImage.status(), public_document_http_status: revokedDocument.status() };
      await anonymousContext.close(); anonymousContext = undefined;
      anonymousLink = undefined; priorAnonymousImageURL = undefined;
    });
    await test('fixture_soft_delete_dom', async () => {
      await section(owner, '我的文件'); await ownFixture(owner, config, name); await softDelete(owner, config, name);
      evidence.retained_fixtures[0].lifecycle = 'trash';
    });
  } finally {
    ownerSession.dispose(); recipientSession.dispose();
    await anonymousContext?.close().catch(() => {}); anonymousLink = undefined; priorAnonymousImageURL = undefined;
    // No logout click: logging out one context might invalidate a different deliberate test session.
    await ownerContext.close().catch(() => {}); await recipientContext.close().catch(() => {});
  }
}
export async function main(args = process.argv.slice(2), env = process.env) {
  let options = { mode: 'check-only', output: 'test-results/browser-acceptance.json' }, evidence = createEvidence(CASE_IDS), browser, persist;
  let exitCode = 2;
  try {
    options = parseArgs(args); evidence.mode = options.mode;
    persist = await createEvidenceWriter(options.output, { protectedPaths: [env.TEST_PASSWORD_FILE, env.TEST_PASSWORD_FILE_B, env.TEST_CREDENTIALS_FILE] });
    const config = await loadConfig(env);
    evidence.preflight = await preflight(config, { network: options.mode !== 'check-only' });
    if (options.mode !== 'run') { exitCode = 3; } else {
      const { chromium } = await import('playwright-core');
      // No --no-sandbox, ignore-certificate-errors, CDP attach, or alternate launch fallback.
      browser = await chromium.launch({ executablePath: config.chromiumPath, headless: true, chromiumSandbox: true });
      evidence.browser_started = true; evidence.execution = 'executed';
      await executeBrowserCases(browser, config, evidence);
      check(evidence.cases.every(item => item.status === 'pass'), 'declared_case_not_executed');
      evidence.status = 'pass'; exitCode = 0;
    }
  } catch (error) {
    evidence.failure_code = failureCode(error);
    evidence.status = evidence.execution === 'executed' ? 'fail' : 'not_run';
    exitCode = evidence.execution === 'executed' || options.mode === 'run' ? 1 : 2;
  } finally {
    await browser?.close().catch(() => {});
    evidence.finished_at = new Date().toISOString(); evidence.exit_code = exitCode;
    let saved = false;
    try { if (persist) { await persist(evidence); saved = true; } }
    catch { exitCode = 1; }
    console.log(`Browser acceptance: ${evidence.status}; ${evidence.cases.filter(c => c.status === 'pass').length}/${evidence.cases.length} executed cases passed. Evidence ${saved ? 'written' : 'not created; use a fresh output path, never a credential path'}. No acceptance matrix updated.`);
  }
  return exitCode;
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) process.exitCode = await main();
