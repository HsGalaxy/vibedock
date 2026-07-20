const state = {
  templates: [],
  environmentProfiles: [],
  workspaces: [],
  accounts: [],
  selectedTemplate: null,
  selectedEnvironmentProfile: null,
  filter: 'all',
  managementMode: false,
  selectedWorkspaceIDs: new Set(),
  pendingWorkspaceIDs: new Set(),
  batchBusy: false,
  controlAuthEnabled: true,
  accountAuth: new Map(),
  activeAuth: null,
  currentWorkspace: null,
  resourceStats: null,
  resourceStatsWorkspaceID: null,
  viewCache: new Map(),
  viewPlacements: {primary: null, secondary: null},
  viewLayoutFrame: 0,
  viewClock: 0,
  sidebarVisible: true,
  editingSessionID: null,
  editingAccountID: null,
  accountConfigAccountID: null,
  accountConfigPath: null,
  tabs: [],
  panes: { primary: null, secondary: null },
  focusedPane: 'primary',
  split: false,
  splitRatio: 0.5,
  dragDepth: 0,
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const toast = $('#toast');
const createDialog = $('#createDialog');
const sessionDialog = $('#sessionDialog');
const uploadDialog = $('#uploadDialog');
const previewDialog = $('#previewDialog');
const settingsDialog = $('#settingsDialog');
const transcriptDialog = $('#transcriptDialog');
const accountsDialog = $('#accountsDialog');
const accountDialog = $('#accountDialog');
const accountConfigDialog = $('#accountConfigDialog');
const environmentProfilesDialog = $('#environmentProfilesDialog');
const codexLoginDialog = $('#codexLoginDialog');
const claudeLoginDialog = $('#claudeLoginDialog');
const accountAuth = globalThis.VibeDockAccountAuth;
const AUTH_REQUEST_HEADER = 'X-VibeDock-Request';

function applyTheme(theme, persist = true) {
  const value = theme === 'light' ? 'light' : 'dark';
  document.documentElement.dataset.theme = value;
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', value === 'light' ? '#f6f8fa' : '#0d1117');
  $$('[data-theme-toggle]').forEach(button => {
    button.textContent = value === 'light' ? '☾' : '☼';
    button.title = value === 'light' ? '切换到暗色模式' : '切换到亮色模式';
    button.setAttribute('aria-label', button.title);
  });
  if (persist) {
    try { localStorage.setItem('vibedock-theme', value); } catch (_) { /* browser storage may be disabled */ }
  }
}

function toggleTheme() {
  applyTheme(document.documentElement.dataset.theme === 'light' ? 'dark' : 'light');
}

async function api(path, options = {}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), options.timeout ?? 30000);
  try {
    const response = await fetch(path, {...options, signal: options.signal || controller.signal});
    if (response.status === 204) return null;
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(body.detail ? `${body.error}: ${body.detail}` : body.error || `HTTP ${response.status}`);
      error.status = response.status;
      throw error;
    }
    return body;
  } catch (error) {
    if (error.name === 'AbortError') throw new Error('请求超时，请稍后重试');
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

function escapeHTML(value = '') {
  return String(value).replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]));
}

function showToast(message) {
  toast.textContent = message;
  toast.classList.remove('hidden');
  clearTimeout(showToast.timer);
  showToast.timer = setTimeout(() => toast.classList.add('hidden'), 2800);
}

function statusLabel(status) {
  return ({running:'运行中', exited:'已停止', stopped:'已停止', created:'待启动', creating:'创建中', missing:'容器缺失', error:'失败'})[status] || status || '未知';
}

function agentLabel(agent) {
  return ({codex:'Codex', claude:'Claude Code', pi:'Pi Agent', opencode:'OpenCode', shell:'Shell'})[agent] || agent;
}

function agentGlyph(agent) {
  return ({codex:'C', claude:'A', pi:'π', opencode:'OC', shell:'>_'})[agent] || '?';
}

function authTypeLabel(type) {
  return ({login:'网页登录 / 官方 CLI', api_key:'API Key', bearer_token:'Bearer Token', oauth_token:'Claude setup-token（非网页登录）'})[type] || type;
}

function formatDate(value) {
  if (!value) return '—';
  return new Date(value).toLocaleString('zh-CN', {month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit'});
}

function workspaceRouteID() {
  return location.pathname.match(/^\/workspace\/([a-z0-9]{12})\/?$/)?.[1] || null;
}

function templateFor(ws) {
  const profile = state.environmentProfiles.find(item => item.id === ws.environment_profile_id);
  if (profile) return {name: profile.name, icon: profile.icon || '◇'};
  if (ws.environment_profile_name) return {name: ws.environment_profile_name, icon: '◇'};
  return state.templates.find(item => item.id === ws.template_id) || {name: ws.template_id, icon:'◆'};
}

function accountFor(id) {
  return state.accounts.find(account => account.id === id) || null;
}

function accountLabel(session) {
  if (session?.agent === 'pi' || session?.agent === 'opencode') {
    const config = session.direct_config || {};
    return [config.provider, config.model].filter(Boolean).join('/') || 'Session 参数';
  }
  if (!session?.account_id) return 'Workspace 默认';
  return accountFor(session.account_id)?.name || '账号已不存在';
}

function isDirectAgent(agent) { return agent === 'pi' || agent === 'opencode'; }

function authOptions(agent) {
  const common = [['login','网页登录 / 官方 CLI'],['api_key','API Key'],['bearer_token','Bearer Token']];
  if (agent === 'claude') common.push(['oauth_token','已有 Claude setup-token']);
  return common;
}

function accountLoginGuide(agent, accountID) {
  if (agent === 'shell' || isDirectAgent(agent)) return '';
  const account = accountFor(accountID);
  if (!accountID) return `<b>Workspace 默认认证</b><span>打开 Session 后直接使用 ${escapeHTML(agentLabel(agent))}。如需跨 Workspace 复用官方账号，请先在“账号与 Provider”中创建并登录原生账号。</span>`;
  if (!account) return `<b>账号配置不可用</b><span>所选账号已不存在，请重新选择或添加账号。</span>`;
  if (account.auth_type === 'login') return `<b>认证由账号中心管理</b><span>请在“账号与 Provider”中查看真实登录状态并完成官方登录。</span><button type="button" class="button ghost small" data-open-account-login="${escapeHTML(account.id)}">打开账号登录</button>`;
  if (account.auth_type === 'oauth_token') return `<b>使用已有 setup-token</b><span>VibeDock 只注入你提供的 Token，不创建或模拟 Claude OAuth。保存后启动或重启 Session 验证。</span>`;
  return `<b>托管 Provider 凭据</b><span>保存后会重启当前 Session，并通过控制面私有路径注入已加密的凭据。</span>`;
}

function renderSessionLoginGuide(agent = $('#sessionAgentInput')?.value, accountID = $('#sessionAccountInput')?.value) {
  const guide = $('#sessionLoginGuide');
  if (!guide) return;
  const markup = accountLoginGuide(agent, accountID);
  guide.classList.toggle('hidden', !markup);
  guide.innerHTML = markup;
  const loginButton = $('[data-open-account-login]', guide);
  if (loginButton) loginButton.onclick = () => openAccountManager({loginAccountID:loginButton.dataset.openAccountLogin});
}

function renderAccountOptions(agent, select, selected = '') {
  if (!select) return;
  if (agent === 'shell' || isDirectAgent(agent)) {
    select.innerHTML = `<option value="">${escapeHTML(agent === 'shell' ? 'Shell 不需要模型账号' : `${agentLabel(agent)} 使用 Session Provider`)}</option>`;
    select.value = '';
    select.disabled = true;
    if (select.id === 'sessionAccountInput') renderSessionLoginGuide(agent, '');
    return;
  }
  const matching = state.accounts.filter(account => account.agent === agent);
  select.disabled = false;
  select.innerHTML = `<option value="">Workspace 默认认证</option>${matching.map(account => `<option value="${escapeHTML(account.id)}">${escapeHTML(account.name)} · ${escapeHTML(authTypeLabel(account.auth_type))}</option>`).join('')}`;
  select.value = matching.some(account => account.id === selected) ? selected : '';
  if (select.id === 'sessionAccountInput') renderSessionLoginGuide(agent, select.value);
}

function updateDirectFields(prefix, agent, config = null) {
  const panel = $(`#${prefix}DirectFields`);
  const visible = isDirectAgent(agent);
  const permissionPanel = $(`#${prefix}PermissionFields`);
  if (permissionPanel) permissionPanel.classList.toggle('hidden', agent !== 'claude');
  if (panel) panel.classList.toggle('hidden', !visible);
  if (prefix === 'session') sessionDialog.classList.toggle('session-direct-modal', visible);
  const help = $(`#${prefix}AccountHelp`);
  if (help) help.textContent = visible ? `${agentLabel(agent)} 不使用统一账号；请配置当前 Session 的 Provider、模型和密钥。` : agent === 'shell' ? 'Shell 不需要模型账号。' : '选择托管账号；原生 CLI 登录由“账号与 Provider”统一管理。';
  const addAccountButton = $(`#${prefix}AddAccountButton`);
  if (addAccountButton) addAccountButton.classList.toggle('hidden', visible || agent === 'shell');
  if (prefix === 'session') renderSessionLoginGuide(agent, $(`#${prefix}AccountInput`)?.value || '');

  // Claude permission controls are independent of the Pi/OpenCode Provider panel.
  // Hydrate them before the direct-agent early return so editing a Claude session
  // never silently resets its saved permission policy.
  const permissionMode = $(`#${prefix}PermissionMode`);
  const dangerousSkip = $(`#${prefix}DangerousSkip`);
  const dangerousConfirmRow = $(`#${prefix}DangerousConfirmRow`);
  if (permissionMode) permissionMode.value = config?.permission_mode || 'manual';
  if (dangerousSkip) dangerousSkip.checked = Boolean(config?.dangerous_skip_permissions);
  if (dangerousConfirmRow) dangerousConfirmRow.classList.toggle('hidden', !dangerousSkip?.checked);

  if (!visible) return;
  const direct = config?.direct_config || config || {};
  $(`#${prefix}DirectAgentName`).textContent = agentLabel(agent);
  $(`#${prefix}DirectProvider`).value = direct.provider || '';
  $(`#${prefix}DirectBaseURL`).value = direct.base_url || '';
  $(`#${prefix}DirectModel`).value = direct.model || '';
  $(`#${prefix}DirectProtocol`).value = direct.protocol || 'openai-completions';
  $(`#${prefix}DirectThinkingRow`).classList.toggle('hidden', agent !== 'pi');
  $(`#${prefix}DirectThinking`).value = direct.thinking || '';
  $(`#${prefix}DirectAutoApprove`).checked = direct.permission_policy === 'auto_approve' || Boolean(direct.auto_approve);
  const secret = $(`#${prefix}DirectSecret`);
  secret.value = '';
  secret.placeholder = direct.has_secret ? `已保存 ${direct.secret_hint || 'API Key'}；留空保持不变` : '可选：API Key / Token';
  const clearRow = $(`#${prefix}DirectClearRow`);
  if (clearRow) clearRow.classList.toggle('hidden', !direct.has_secret);
  const clear = $(`#${prefix}DirectClearSecret`);
  if (clear) clear.checked = false;
}

function collectDirectConfig(prefix, agent) {
  if (!isDirectAgent(agent)) return null;
  return {
    provider: $(`#${prefix}DirectProvider`).value.trim(),
    base_url: $(`#${prefix}DirectBaseURL`).value.trim(),
    model: $(`#${prefix}DirectModel`).value.trim(),
    protocol: $(`#${prefix}DirectProtocol`).value,
    thinking: agent === 'pi' ? $(`#${prefix}DirectThinking`).value : '',
    permission_policy: $(`#${prefix}DirectAutoApprove`).checked ? 'auto_approve' : 'ask',
    auto_approve: $(`#${prefix}DirectAutoApprove`).checked,
    secret: $(`#${prefix}DirectSecret`).value.trim(),
    clear_secret: Boolean($(`#${prefix}DirectClearSecret`)?.checked),
  };
}

function launchProfileDefaults(agent, profile = {}) {
  return {
    version: 1,
    model: profile.model || '',
    codex_approval: agent === 'codex' ? (profile.codex_approval || '') : '',
    codex_sandbox: agent === 'codex' ? (profile.codex_sandbox || '') : '',
    codex_web_search: agent === 'codex' && Boolean(profile.codex_web_search),
    codex_config_overrides: agent === 'codex' ? (profile.codex_config_overrides || []) : [],
    claude_continue: agent === 'claude' && Boolean(profile.claude_continue),
    opencode_continue: agent === 'opencode' && Boolean(profile.opencode_continue),
    extra_args: profile.extra_args || [],
    config_mode: isDirectAgent(agent) ? (profile.config_mode || 'managed') : '',
    custom_config: isDirectAgent(agent) ? (profile.custom_config || '') : '',
  };
}

function applyLaunchProfile(agent, profile = null) {
  const value = launchProfileDefaults(agent, profile || {});
  $('#sessionLaunchModel').value = value.model;
  $('#sessionCodexApproval').value = value.codex_approval;
  $('#sessionCodexSandbox').value = value.codex_sandbox;
  $('#sessionCodexWebSearch').checked = value.codex_web_search;
  $('#sessionClaudeContinue').checked = value.claude_continue;
  $('#sessionOpenCodeContinue').checked = value.opencode_continue;
  $('#sessionCodexOverrides').value = value.codex_config_overrides.join('\n');
  $('#sessionExtraArgs').value = value.extra_args.join('\n');
  $('#sessionConfigMode').value = value.config_mode || 'managed';
  $('#sessionConfigMode').dataset.previous = value.config_mode || 'managed';
  $('#sessionCustomConfig').value = value.custom_config;
  updateLaunchProfileFields(agent);
}

function updateLaunchProfileFields(agent) {
  const isCodex = agent === 'codex';
  const isClaude = agent === 'claude';
  const isOpenCode = agent === 'opencode';
  $('#sessionCodexApprovalRow').classList.toggle('hidden', !isCodex);
  $('#sessionCodexSandboxRow').classList.toggle('hidden', !isCodex);
  $('#sessionCodexSearchRow').classList.toggle('hidden', !isCodex);
  $('#sessionCodexOverridesRow').classList.toggle('hidden', !isCodex);
  $('#sessionClaudeContinueRow').classList.toggle('hidden', !isClaude);
  $('#sessionOpenCodeContinueRow').classList.toggle('hidden', !isOpenCode);
  $('#sessionAgentConfigFields').classList.toggle('hidden', !isDirectAgent(agent));
  if (isDirectAgent(agent)) {
    const path = agent === 'pi' ? 'models.json' : 'opencode.json';
    $('#sessionConfigPath').textContent = path;
    const custom = $('#sessionConfigMode').value === 'custom';
    $('#sessionCustomConfig').readOnly = !custom;
    $('#sessionCustomConfig').classList.toggle('readonly', !custom);
  }
}

function collectLaunchProfile(agent) {
  const profile = {
    version: 1,
    model: $('#sessionLaunchModel').value.trim(),
    codex_approval: agent === 'codex' ? $('#sessionCodexApproval').value : '',
    codex_sandbox: agent === 'codex' ? $('#sessionCodexSandbox').value : '',
    codex_web_search: agent === 'codex' && $('#sessionCodexWebSearch').checked,
    codex_config_overrides: agent === 'codex' ? $('#sessionCodexOverrides').value.split(/\r?\n/).map(value => value.trim()).filter(Boolean) : [],
    claude_continue: agent === 'claude' && $('#sessionClaudeContinue').checked,
    opencode_continue: agent === 'opencode' && $('#sessionOpenCodeContinue').checked,
    extra_args: $('#sessionExtraArgs').value.split(/\r?\n/).map(value => value.trim()).filter(Boolean),
    config_mode: isDirectAgent(agent) ? $('#sessionConfigMode').value : '',
    custom_config: isDirectAgent(agent) && $('#sessionConfigMode').value === 'custom' ? $('#sessionCustomConfig').value : '',
  };
  return profile;
}

let launchPreviewTimer = 0;
async function refreshLaunchPreview() {
  const agent = $('#sessionAgentInput').value;
  const preview = $('#sessionLaunchPreview');
  if (!preview) return;
  preview.textContent = '正在生成…';
  try {
    const directConfig = collectDirectConfig('session', agent) || {};
    if (directConfig.secret) directConfig.secret = '';
    directConfig.clear_secret = false;
    const result = await api('/api/launch-preview', {
      method: 'POST', headers: {'Content-Type':'application/json'},
      body: JSON.stringify({
        agent,
        account_id: isDirectAgent(agent) ? '' : $('#sessionAccountInput').value,
        session_id: state.editingSessionID || '',
        permission_mode: agent === 'claude' ? $('#sessionPermissionMode').value : 'manual',
        dangerous_skip_permissions: agent === 'claude' && $('#sessionDangerousSkip').checked,
        direct_config: directConfig,
        launch_profile: collectLaunchProfile(agent),
      }), timeout: 15000,
    });
    preview.textContent = result.command?.preview || `${result.command?.executable || ''} ${(result.command?.arguments || []).join(' ')}`;
    const warnings = result.command?.warnings || [];
    const warningsBox = $('#sessionLaunchWarnings');
    warningsBox.classList.toggle('hidden', !warnings.length);
    warningsBox.textContent = warnings.join('；');
    if (result.config?.managed_template && isDirectAgent(agent) && $('#sessionConfigMode').value === 'managed') {
      $('#sessionCustomConfig').value = result.config.managed_template;
    }
  } catch (error) {
    preview.textContent = `无法生成预览：${error.message}`;
  }
}

function scheduleLaunchPreview() {
  clearTimeout(launchPreviewTimer);
  launchPreviewTimer = setTimeout(() => void refreshLaunchPreview(), 180);
}

/* Dashboard */
function setTemplate(id, environmentProfileID = '') {
  const changed = state.selectedTemplate !== id || state.selectedEnvironmentProfile !== (environmentProfileID || null);
  state.selectedTemplate = id;
  state.selectedEnvironmentProfile = environmentProfileID || null;
  $('#templateId').value = id;
  $('#environmentProfileId').value = environmentProfileID;
  $$('.template-option').forEach(button => button.classList.toggle('selected', button.dataset.id === id && (button.dataset.environmentProfile || '') === environmentProfileID));
  const profile = state.environmentProfiles.find(item => item.id === environmentProfileID);
  if (profile && changed) {
    $('#createForm [name="cpus"]').value = String(profile.cpus);
    $('#createForm [name="memory_mb"]').value = String(profile.memory_mb);
  }
}

function renderTemplates() {
  const builtins = state.templates.map(template => ({...template, environmentProfile:''}));
  const custom = state.environmentProfiles.map(profile => ({
    id:profile.base_template_id, name:profile.name, description:profile.description || '定制环境配置', icon:profile.icon || '◇', environmentProfile:profile.id,
  }));
  $('#templatePicker').innerHTML = [...builtins, ...custom].map(template => `
    <button type="button" class="template-option" data-id="${escapeHTML(template.id)}" data-environment-profile="${escapeHTML(template.environmentProfile)}">
      <strong>${escapeHTML(template.icon || '◆')} ${escapeHTML(template.name)}</strong>
      <span>${escapeHTML(template.description)}${template.environmentProfile ? ' · 定制' : ''}</span>
    </button>`).join('');
  $$('.template-option').forEach(button => button.onclick = () => setTemplate(button.dataset.id, button.dataset.environmentProfile));
  const selectedProfile = state.environmentProfiles.find(item => item.id === state.selectedEnvironmentProfile);
  if (selectedProfile) setTemplate(selectedProfile.base_template_id, selectedProfile.id);
  else if (state.templates.length) setTemplate(state.templates.some(t => t.id === state.selectedTemplate) ? state.selectedTemplate : state.templates[0].id);
}

function visibleWorkspaces() {
  return state.workspaces.filter(ws => state.filter === 'all' || (state.filter === 'running' ? ws.status === 'running' : ws.status !== 'running'));
}

function renderHomeOverview() {
  const running = state.workspaces.filter(ws => ws.status === 'running');
  const sessions = state.workspaces.flatMap(ws => (ws.sessions || []).map(session => ({workspace:ws, session})));
  const active = sessions.filter(item => item.session.status === 'running').length;
  const issues = state.workspaces.filter(ws => ['error', 'missing'].includes(ws.status)).length
    + sessions.filter(item => item.session.status === 'error').length;
  $('#overviewWorkspaceCount').textContent = String(state.workspaces.length);
  $('#overviewRunningCount').textContent = String(running.length);
  $('#overviewSessionCount').textContent = String(active);
  $('#overviewIssueCount').textContent = String(issues);
  $('#overviewIssueHint').textContent = issues ? '需要检查' : '状态正常';
  const recent = sessions.sort((a, b) => new Date(b.session.updated_at || b.session.created_at || 0) - new Date(a.session.updated_at || a.session.created_at || 0)).slice(0, 6);
  const list = $('#recentSessionList');
  if (!recent.length) {
    list.innerHTML = '<div class="operation-empty">还没有 Session。创建一个 Workspace 后，最近工作会显示在这里。</div>';
    return;
  }
  list.innerHTML = recent.map(({workspace, session}) => `<button class="operation-row" data-recent-workspace="${escapeHTML(workspace.id)}" data-recent-session="${escapeHTML(session.id)}">
    <span class="operation-agent ${escapeHTML(session.agent)}">${escapeHTML(agentGlyph(session.agent))}</span>
    <span class="operation-copy"><b>${escapeHTML(session.name || agentLabel(session.agent))}</b><small>${escapeHTML(workspace.name)} · ${escapeHTML(agentLabel(session.agent))} · ${escapeHTML(statusLabel(session.status))}</small></span>
    <time>${escapeHTML(formatDate(session.updated_at || session.created_at))}</time>
  </button>`).join('');
}

function renderWorkspaceBatchToolbar(visible = visibleWorkspaces()) {
  const toolbar = $('#workspaceBatchToolbar');
  toolbar.classList.toggle('hidden', !state.managementMode);
  const selected = state.selectedWorkspaceIDs.size;
  const actionable = [...state.selectedWorkspaceIDs].map(id => state.workspaces.find(ws => ws.id === id)).filter(Boolean);
  $('#workspaceSelectionSummary').textContent = selected ? `已选择 ${selected} 个 Workspace` : '未选择 Workspace';
  $('#batchStartButton').disabled = state.batchBusy || !actionable.some(ws => ws.status !== 'running');
  $('#batchStopButton').disabled = state.batchBusy || !actionable.some(ws => ws.status === 'running');
  $('#batchDeleteButton').disabled = state.batchBusy || !selected;
  const selectAll = $('#workspaceSelectAll');
  selectAll.disabled = state.batchBusy || !visible.length;
  const visibleSelected = visible.filter(ws => state.selectedWorkspaceIDs.has(ws.id)).length;
  selectAll.checked = visible.length > 0 && visibleSelected === visible.length;
  selectAll.indeterminate = visibleSelected > 0 && visibleSelected < visible.length;
  $('#manageWorkspacesButton').setAttribute('aria-pressed', String(state.managementMode));
  $('#manageWorkspacesButton').textContent = state.managementMode ? '完成管理' : '管理 Workspace';
}

function renderDashboard() {
  const visible = visibleWorkspaces();
  const runningCount = state.workspaces.filter(ws => ws.status === 'running').length;
  const errorCount = state.workspaces.filter(ws => ws.status === 'error' || ws.status === 'missing').length;
  $('#workspaceListSummary').textContent = `${state.workspaces.length} 个环境 · ${runningCount} 个运行中${errorCount ? ` · ${errorCount} 个需要处理` : ''}`;
  renderHomeOverview();
  state.selectedWorkspaceIDs = new Set([...state.selectedWorkspaceIDs].filter(id => state.workspaces.some(ws => ws.id === id)));
  const hasWorkspaces = state.workspaces.length > 0;
  $('#emptyState').classList.toggle('hidden', visible.length > 0);
  $('#workspaceGrid').classList.toggle('hidden', visible.length === 0);
  $('#workspaceGrid').innerHTML = visible.map(ws => {
    const template = templateFor(ws);
    const sessions = ws.sessions || [];
    const active = sessions.filter(item => item.status === 'running').length;
    const selected = state.selectedWorkspaceIDs.has(ws.id);
    const pending = state.pendingWorkspaceIDs.has(ws.id);
    return `<article class="workspace-card ${selected ? 'selected' : ''} ${pending ? 'pending' : ''}" data-workspace-card="${escapeHTML(ws.id)}" role="button" tabindex="0" aria-label="${escapeHTML(ws.name)}，${escapeHTML(statusLabel(ws.status))}，${sessions.length} 个 Session">
      <div class="workspace-card-top">
        ${state.managementMode ? `<label class="workspace-card-select"><input class="workspace-select" type="checkbox" data-workspace-select="${escapeHTML(ws.id)}" ${selected ? 'checked' : ''} ${pending ? 'disabled' : ''} aria-label="选择 ${escapeHTML(ws.name)}" />选择</label>` : `<span class="workspace-icon">${escapeHTML(template.icon || '◆')}</span>`}
        <span class="status-label ${escapeHTML(['running','error','missing'].includes(ws.status) ? ws.status : '')}">${escapeHTML(statusLabel(ws.status))}</span>
      </div>
      <h3>${escapeHTML(ws.name)}</h3>
      <span class="template-name">${escapeHTML(template.name)} · ${escapeHTML(ws.source || 'workspace')}</span>
      ${ws.error ? `<div class="card-error">${escapeHTML(ws.error)}</div>` : ''}
      <div class="workspace-card-meta"><span><b>${sessions.length}</b> sessions</span><span><b>${active}</b> active</span><span><b>${Number(ws.cpus) || 0}C / ${Math.round((Number(ws.memory_mb) || 0) / 1024)}G</b></span></div>
      <div class="workspace-card-actions">
        <button class="button" data-card-action="open" data-id="${escapeHTML(ws.id)}">继续工作</button>
        <button class="button ghost" data-card-action="${ws.status === 'running' ? 'stop' : 'start'}" data-id="${escapeHTML(ws.id)}" ${pending ? 'disabled' : ''}>${ws.status === 'running' ? '停止' : '启动'}</button>
      </div>
    </article>`;
  }).join('');
  renderWorkspaceBatchToolbar(visible);
  const filteredEmpty = hasWorkspaces && visible.length === 0;
  $('#emptyState h3').textContent = filteredEmpty ? '没有匹配的 Workspace' : '创建你的第一个工作环境';
  $('#emptyState p').textContent = filteredEmpty ? '换一个筛选条件，或清除当前筛选。' : '选择模板、导入项目，几秒后进入全屏工作台。';
  const emptyAction = $('#emptyStateAction');
  emptyAction.textContent = filteredEmpty ? '清除筛选' : '创建 Workspace';
  emptyAction.dataset.emptyAction = filteredEmpty ? 'clear-filter' : 'create';
}

function showDashboard(push = false) {
  persistLayout();
  destroyAllViews();
  state.currentWorkspace = null;
  state.resourceStats = null;
  state.resourceStatsWorkspaceID = null;
  $('#workspaceView').classList.add('hidden');
  $('#dashboardView').classList.remove('hidden');
  document.body.style.overflow = '';
  document.title = 'VibeDock';
  if (push && location.pathname !== '/') history.pushState({}, '', '/');
  renderDashboard();
}

function setManagementMode(enabled) {
  state.managementMode = Boolean(enabled);
  state.selectedWorkspaceIDs.clear();
  if (!enabled) $('#workspaceBatchResults').classList.add('hidden');
  renderDashboard();
}

function toggleWorkspaceSelection(id, selected = !state.selectedWorkspaceIDs.has(id)) {
  if (!state.managementMode || state.batchBusy || !state.workspaces.some(ws => ws.id === id)) return;
  if (selected) state.selectedWorkspaceIDs.add(id); else state.selectedWorkspaceIDs.delete(id);
  renderDashboard();
}

async function runWithConcurrency(items, limit, worker) {
  const results = new Array(items.length);
  let next = 0;
  async function consume() {
    while (next < items.length) {
      const index = next++;
      try { results[index] = {status:'fulfilled', value:await worker(items[index])}; }
      catch (reason) { results[index] = {status:'rejected', reason}; }
    }
  }
  await Promise.all(Array.from({length:Math.min(limit, items.length)}, consume));
  return results;
}

function renderBatchResults(action, targets, results) {
  const box = $('#workspaceBatchResults');
  const failures = results.map((result, index) => ({result, workspace:targets[index]})).filter(item => item.result.status === 'rejected');
  const success = results.length - failures.length;
  const labels = {start:'启动', stop:'停止', delete:'删除'};
  box.innerHTML = `<b>${escapeHTML(labels[action])}完成：${success} 个成功，${failures.length} 个失败。</b>${failures.length ? `<ul class="batch-result-failures">${failures.map(item => `<li>${escapeHTML(item.workspace.name)}：${escapeHTML(item.result.reason?.message || '操作失败')}</li>`).join('')}</ul>` : ''}`;
  box.classList.remove('hidden');
}

async function executeBatchAction(action, targets) {
  if (state.batchBusy || !targets.length) return;
  state.batchBusy = true;
  state.pendingWorkspaceIDs = new Set(targets.map(ws => ws.id));
  renderDashboard();
  const results = await runWithConcurrency(targets, 3, async ws => {
    try {
      if (action === 'delete') await api(`/api/workspaces/${ws.id}?purge=true`, {method:'DELETE'});
      else await api(`/api/workspaces/${ws.id}/${action}`, {method:'POST', timeout:70000});
      if (action === 'delete') localStorage.removeItem(`vibedock.layout.${ws.id}`);
      return ws.id;
    } catch (error) {
      if (action === 'delete' && error.status === 404) return ws.id;
      throw error;
    }
  });
  state.batchBusy = false;
  state.pendingWorkspaceIDs.clear();
  renderBatchResults(action, targets, results);
  await refreshAll({quiet:true});
  const succeeded = new Set(results.map((result, index) => result.status === 'fulfilled' ? targets[index].id : null).filter(Boolean));
  state.selectedWorkspaceIDs = new Set([...state.selectedWorkspaceIDs].filter(id => !succeeded.has(id)));
  renderDashboard();
}

function selectedTargets(action) {
  const selected = state.workspaces.filter(ws => state.selectedWorkspaceIDs.has(ws.id));
  if (action === 'start') return selected.filter(ws => ws.status !== 'running');
  if (action === 'stop') return selected.filter(ws => ws.status === 'running');
  return selected;
}

function requestBatchAction(action) {
  const targets = selectedTargets(action);
  if (!targets.length) return showToast('没有符合当前操作条件的 Workspace');
  if (action !== 'delete') return executeBatchAction(action, targets);
  $('#batchDeleteDescription').textContent = `将永久删除 ${targets.length} 个 Workspace 的项目卷、Home 卷和全部会话。此操作不可撤销。`;
  $('#batchDeleteList').innerHTML = `<ul>${targets.map(ws => `<li>${escapeHTML(ws.name)}</li>`).join('')}</ul>`;
  $('#batchDeleteDialog').showModal();
}

/* Workbench tabs and panes */
function layoutKey() { return state.currentWorkspace ? `vibedock.layout.${state.currentWorkspace.id}` : ''; }

function tabDescriptor(id) {
  if (!id || typeof id !== 'string') return null;
  const ws = state.currentWorkspace;
  if (!ws) return null;
  if (id === 'home') return {id, kind:'home', title:'Workspace', icon:'◆', url:''};
  if (id === 'code') return {id, kind:'code', title:'VS Code', icon:'⌘', url:`/w/${ws.id}/code/?folder=/workspace`};
  if (id.startsWith('session:')) {
    const sessionID = id.slice(8);
    const session = ws.sessions?.find(item => item.id === sessionID);
    if (!session) return null;
    return {id, kind:'session', payload:sessionID, session, title:session.name, icon:agentGlyph(session.agent), url:`/w/${ws.id}/terminal/${session.id}/`};
  }
  if (id.startsWith('port:')) {
    const port = Number(id.slice(5));
    if (!Number.isInteger(port)) return null;
    return {id, kind:'port', payload:port, title:`Preview :${port}`, icon:'◎', url:`/w/${ws.id}/port/${port}/`};
  }
  return null;
}

function persistLayout() {
  if (!state.currentWorkspace) return;
  localStorage.setItem(layoutKey(), JSON.stringify({
    tabs: state.tabs.map(tab => tab.id), panes: state.panes, focusedPane: state.focusedPane, split: state.split, splitRatio: state.splitRatio,
  }));
}

function restoreLayout() {
  const ws = state.currentWorkspace;
  const fallback = ws.status === 'running' && ws.sessions?.length ? `session:${ws.sessions[0].id}` : 'home';
  let saved = null;
  try { saved = JSON.parse(localStorage.getItem(layoutKey()) || 'null'); } catch (_) {}
  const ids = [...new Set([...(saved?.tabs || []), fallback, 'code'])].filter(id => tabDescriptor(id));
  state.tabs = ids.map(tabDescriptor);
  state.panes.primary = tabDescriptor(saved?.panes?.primary) ? saved.panes.primary : fallback;
  state.panes.secondary = tabDescriptor(saved?.panes?.secondary) ? saved.panes.secondary : (ids.find(id => id !== state.panes.primary) || 'code');
  state.focusedPane = saved?.focusedPane === 'secondary' ? 'secondary' : 'primary';
  state.split = Boolean(saved?.split);
  if (state.split && state.panes.secondary === state.panes.primary) state.panes.secondary = ids.find(id => id !== state.panes.primary) || null;
  if (state.split && !state.panes.secondary) state.split = false;
  state.splitRatio = Math.min(.75, Math.max(.25, Number(saved?.splitRatio) || .5));
}

function ensureTab(id) {
  let tab = state.tabs.find(item => item.id === id);
  if (!tab) {
    tab = tabDescriptor(id);
    if (tab) state.tabs.push(tab);
  } else {
    Object.assign(tab, tabDescriptor(id) || {});
  }
  return tab;
}

function activeTab(pane = state.focusedPane) { return state.tabs.find(tab => tab.id === state.panes[pane]) || null; }

function openTab(kind, payload = null, options = {}) {
  if (!state.currentWorkspace) return;
  let pane = options.pane || state.focusedPane;
  const id = kind === 'session' ? `session:${payload}` : kind === 'port' ? `port:${payload}` : kind;
  const tab = ensureTab(id);
  if (!tab) return;
  if (state.split) {
    const existingPane = ['primary','secondary'].find(candidate => state.panes[candidate] === id);
    if (existingPane) pane = existingPane;
  }
  state.panes[pane] = id;
  if (state.split) {
    const other = pane === 'primary' ? 'secondary' : 'primary';
    if (state.panes[other] === id) {
      state.panes[other] = state.tabs.find(candidate => candidate.id !== id)?.id || 'home';
    }
  }
  state.focusedPane = pane;
  renderWorkbench();
  persistLayout();
}

function closeTab(id) {
  if (id === 'home' && state.tabs.length === 1) return;
  const index = state.tabs.findIndex(tab => tab.id === id);
  if (index < 0) return;
  state.tabs.splice(index, 1);
  destroyTabViews(id);
  if (!state.tabs.length) state.tabs.push(tabDescriptor('home'));
  for (const pane of ['primary','secondary']) {
    if (state.panes[pane] === id) state.panes[pane] = state.tabs[Math.max(0, index - 1)]?.id || state.tabs[0].id;
  }
  renderWorkbench();
  persistLayout();
}

function renderTabStrip() {
  $('#tabStrip').innerHTML = state.tabs.map(tab => {
    const active = state.panes[state.focusedPane] === tab.id;
    return `<button class="tool-tab ${active ? 'active' : ''}" role="tab" aria-selected="${active}" tabindex="${active ? '0' : '-1'}" data-tab-id="${escapeHTML(tab.id)}" title="${escapeHTML(tab.title)}">
      <span class="tab-icon">${escapeHTML(tab.icon)}</span><span class="tab-title">${escapeHTML(tab.title)}</span><span class="tab-close" data-close-tab="${escapeHTML(tab.id)}" role="button" aria-label="关闭 ${escapeHTML(tab.title)} 标签">×</span>
    </button>`;
  }).join('');
}

function welcomeMarkup() {
  const ws = state.currentWorkspace;
  return `<div class="welcome-pane"><div class="welcome-inner">
    <div class="welcome-mark">VD</div><h2>${escapeHTML(ws.name)}</h2>
    <p>这里是你的云端开发桌面。Agent、编辑器、终端和运行中的网页都可以直接嵌在工作台里，也可以分屏并行使用。</p>
    <div class="quick-start-grid">
      <button class="quick-start" data-welcome="session"><span>◈</span><b>新建 Agent</b><small>Codex / Claude / Pi / OpenCode</small></button>
      <button class="quick-start" data-welcome="code"><span>⌘</span><b>打开 VS Code</b><small>编辑项目文件</small></button>
      <button class="quick-start" data-welcome="preview"><span>◎</span><b>打开 Preview</b><small>嵌入开发服务器</small></button>
    </div>
  </div></div>`;
}

const MAX_CACHED_VIEWS = 8;

function destroyView(view) {
  view?.host?.remove();
}

function destroyAllViews() {
  for (const view of state.viewCache.values()) destroyView(view);
  state.viewCache.clear();
  state.viewPlacements = {primary:null, secondary:null};
  cancelAnimationFrame(state.viewLayoutFrame);
  state.viewLayoutFrame = 0;
  state.viewClock = 0;
}

function viewKey(tabID) { return `${state.currentWorkspace?.id || 'none'}:${tabID}`; }

function destroyTabViews(tabID) {
  for (const [key, view] of state.viewCache) {
    if (view.tabID !== tabID) continue;
    destroyView(view);
    state.viewCache.delete(key);
  }
}

function evictViews() {
  const protectedKeys = new Set([viewKey(state.panes.primary), state.split ? viewKey(state.panes.secondary) : null].filter(Boolean));
  while (state.viewCache.size > MAX_CACHED_VIEWS) {
    const candidates = [...state.viewCache.values()].filter(view => !protectedKeys.has(view.key)).sort((a, b) => a.lastUsed - b.lastUsed);
    if (!candidates.length) break;
    const victim = candidates[0];
    destroyView(victim);
    state.viewCache.delete(victim.key);
    showToast('为保护浏览器资源，已卸载最久未使用的面板；Session 仍在运行。');
  }
}

function createView(tab) {
  const ws = state.currentWorkspace;
  const key = viewKey(tab.id);
  const host = document.createElement('div');
  host.className = 'pane-view';
  host.dataset.viewKey = key;
  host.hidden = true;
  if (!tab || tab.kind === 'home') {
    host.innerHTML = welcomeMarkup();
    bindWelcomeActions(host);
  } else if (ws.status !== 'running') {
    host.innerHTML = `<div class="stopped-pane"><strong>Workspace 当前未运行</strong><span>启动环境后，${escapeHTML(tab.title)} 会在这里恢复。</span><button class="button primary" data-start-workspace>启动 Workspace</button></div>`;
    $('[data-start-workspace]', host).onclick = workspacePower;
  } else {
    host.innerHTML = `<div class="pane-loading"><span class="spinner"></span><b>正在连接 ${escapeHTML(tab.title)}…</b></div><iframe class="pane-frame" title="${escapeHTML(tab.title)}" allow="clipboard-read; clipboard-write; fullscreen; autoplay" src="${escapeHTML(tab.url)}"></iframe>`;
    $('iframe', host).addEventListener('load', () => $('.pane-loading', host)?.classList.add('done'), {once:true});
  }
  $('#paneViewLayer').append(host);
  const view = {key, tabID:tab.id, host, lastUsed:++state.viewClock, status:ws.status};
  state.viewCache.set(key, view);
  return view;
}

function positionView(view, pane) {
  const layer = $('#paneViewLayer');
  const paneElement = $(`.work-pane[data-pane="${pane}"]`);
  const paneHead = $('.pane-head', paneElement);
  const layerRect = layer.getBoundingClientRect();
  const paneRect = paneElement.getBoundingClientRect();
  const headHeight = paneHead?.getBoundingClientRect().height || 0;
  view.host.style.left = `${Math.round(paneRect.left - layerRect.left)}px`;
  view.host.style.top = `${Math.round(paneRect.top - layerRect.top + headHeight)}px`;
  view.host.style.width = `${Math.max(0, Math.round(paneRect.width))}px`;
  view.host.style.height = `${Math.max(0, Math.round(paneRect.height - headHeight))}px`;
  view.host.dataset.pane = pane;
}

function layoutActiveViews() {
  state.viewLayoutFrame = 0;
  const activeKeys = new Set();
  for (const pane of ['primary','secondary']) {
    if (pane === 'secondary' && !state.split) continue;
    const tabID = state.panes[pane];
    if (!tabID) continue;
    const view = state.viewCache.get(viewKey(tabID));
    if (!view) continue;
    activeKeys.add(view.key);
    positionView(view, pane);
  }
  for (const view of state.viewCache.values()) view.host.hidden = !activeKeys.has(view.key);
}

function scheduleViewLayout() {
  cancelAnimationFrame(state.viewLayoutFrame);
  state.viewLayoutFrame = requestAnimationFrame(layoutActiveViews);
}

function renderPane(pane, force = false) {
  const tab = activeTab(pane);
  const ws = state.currentWorkspace;
  const signature = `${ws?.id || 'none'}:${tab?.id || 'empty'}:${ws?.status || 'none'}`;
  const paneElement = $(`.work-pane[data-pane="${pane}"]`);
  paneElement.classList.toggle('focused', state.focusedPane === pane);
  $(`#${pane}PaneLabel`).textContent = tab?.title || (pane === 'primary' ? '主工作区' : '辅助工作区');
  if (!ws) return;

  if (force && tab) {
    const old = state.viewCache.get(viewKey(tab.id));
    if (old) { destroyView(old); state.viewCache.delete(old.key); }
  }
  let view = tab ? state.viewCache.get(viewKey(tab.id)) : null;
  if (view && view.status !== ws.status) {
    destroyView(view);
    state.viewCache.delete(view.key);
    view = null;
  }
  if (!view && tab) view = createView(tab);
  state.viewPlacements[pane] = tab?.id || null;
  if (view) {
    view.lastUsed = ++state.viewClock;
    view.host.dataset.signature = signature;
  }
  evictViews();
  scheduleViewLayout();
}

function bindWelcomeActions(root) {
  $('[data-welcome="session"]', root).onclick = () => openSessionDialog();
  $('[data-welcome="code"]', root).onclick = () => openTab('code');
  $('[data-welcome="preview"]', root).onclick = () => previewDialog.showModal();
}

function renderSessionList() {
  const sessions = state.currentWorkspace?.sessions || [];
  $('#sessionEmpty').classList.toggle('hidden', sessions.length > 0);
  $('#sessionList').innerHTML = sessions.map(session => {
    const open = state.panes.primary === `session:${session.id}` || state.panes.secondary === `session:${session.id}`;
    const hasError = session.status === 'error' || Boolean(session.error);
    return `<div class="session-item ${open ? 'active' : ''} ${hasError ? 'has-error' : ''}" data-session-id="${session.id}" role="button" tabindex="0" aria-label="打开 ${escapeHTML(session.name)}，${escapeHTML(hasError ? `错误：${session.error}` : `${agentLabel(session.agent)}，${accountLabel(session)}`)}" ${session.error ? `title="${escapeHTML(session.error)}"` : ''}>
      <span class="session-agent ${escapeHTML(session.agent)}">${escapeHTML(agentGlyph(session.agent))}</span>
      <span class="session-copy"><b>${escapeHTML(session.name)}</b><small>${escapeHTML(hasError ? session.error : `${agentLabel(session.agent)} · ${accountLabel(session)}`)}</small></span>
      <i class="session-state ${escapeHTML(session.status)}"></i>
      <button class="session-gear" data-session-settings="${session.id}" title="会话设置">•••</button>
    </div>`;
  }).join('');
}

function formatBytes(bytes) {
  const value = Number(bytes);
  if (!Number.isFinite(value) || value < 0) return '—';
  const units = ['B','KB','MB','GB','TB'];
  let scaled = value; let unit = 0;
  while (scaled >= 1024 && unit < units.length - 1) { scaled /= 1024; unit++; }
  return `${scaled >= 10 || unit === 0 ? scaled.toFixed(0) : scaled.toFixed(1)} ${units[unit]}`;
}

function renderResourceMonitor() {
  const ws = state.currentWorkspace;
  const stats = state.resourceStatsWorkspaceID === ws?.id ? state.resourceStats : null;
  const sessions = ws?.sessions || [];
  const runningSessions = sessions.filter(session => session.status === 'running').length;
  $('#resourceSessions').textContent = `${runningSessions} / ${sessions.length} running`;
  const containerState = stats?.container_state || ws?.status || 'unknown';
  const stateElement = $('#resourceContainerState');
  stateElement.className = `resource-state ${['running','missing','error'].includes(containerState) ? containerState : containerState === 'exited' || containerState === 'stopped' ? 'stopped' : ''}`;
  stateElement.textContent = statusLabel(containerState);
  const cpuLimit = stats?.effective_limit?.cpu_cores ?? stats?.configured?.cpu_cores ?? ws?.cpus;
  $('#resourceCPULimit').textContent = `配置上限 ${Number.isFinite(Number(cpuLimit)) ? `${Number(cpuLimit)} vCPU` : '—'}`;
  $('#resourceCPU').textContent = stats?.live && Number.isFinite(Number(stats.usage?.cpu_percent)) ? `${Number(stats.usage.cpu_percent).toFixed(1)}%` : '—';
  const memoryUsed = Number(stats?.usage?.memory_bytes);
  const memoryLimit = Number(stats?.effective_limit?.memory_bytes ?? stats?.configured?.memory_bytes ?? (Number(ws?.memory_mb) * 1024 * 1024));
  const memoryPercent = stats?.live && memoryLimit > 0 ? Math.max(0, Math.min(100, memoryUsed / memoryLimit * 100)) : 0;
  $('#resourceMemory').textContent = stats?.live ? `${formatBytes(memoryUsed)} / ${formatBytes(memoryLimit)}` : `— / ${formatBytes(memoryLimit)}`;
  $('#resourceMemoryRemaining').textContent = stats?.live ? `剩余 ${formatBytes(Math.max(0, memoryLimit - memoryUsed))}` : stats?.unavailable_reason === 'container_missing' ? '容器缺失，无法采样' : stats?.unavailable_reason === 'stats_unavailable' ? '资源采样暂时不可用' : 'Workspace 未运行';
  const meter = $('#resourceMemoryMeter');
  meter.classList.toggle('warning', memoryPercent >= 70 && memoryPercent < 90);
  meter.classList.toggle('danger', memoryPercent >= 90);
  $('i', meter).style.width = `${memoryPercent}%`;
  meter.setAttribute('aria-valuenow', String(Math.round(memoryPercent)));
  meter.setAttribute('aria-valuetext', stats?.live ? `内存使用 ${formatBytes(memoryUsed)}，上限 ${formatBytes(memoryLimit)}，剩余 ${formatBytes(Math.max(0, memoryLimit - memoryUsed))}` : '当前没有实时内存样本');
  $('#resourceSampleStatus').textContent = stats?.live ? `实时采样 · ${formatDate(stats.sampled_at)}` : stats ? (stats.unavailable_reason === 'container_missing' ? '容器缺失' : stats.unavailable_reason === 'stats_unavailable' ? '采样服务暂时不可用' : '未运行，无实时样本') : '等待实时采样';
}

async function refreshResourceStats(workspaceID) {
  if (!workspaceID) return;
  try {
    const stats = await api(`/api/workspaces/${workspaceID}/stats`, {timeout:12000});
    if (state.currentWorkspace?.id !== workspaceID) return;
    state.resourceStats = stats;
    state.resourceStatsWorkspaceID = workspaceID;
  } catch (_) {
    if (state.currentWorkspace?.id !== workspaceID) return;
    state.resourceStats = {container_state:state.currentWorkspace.status, live:false, unavailable_reason:'stats_unavailable'};
    state.resourceStatsWorkspaceID = workspaceID;
  }
  renderResourceMonitor();
}

function renderWorkbench() {
  const ws = state.currentWorkspace;
  if (!ws) return;
  const template = templateFor(ws);
  document.title = `${ws.name} · VibeDock`;
  $('#workspaceIcon').textContent = template.icon || '◆';
  $('#workspaceName').textContent = ws.name;
  $('#workspaceSubtitle').textContent = `${template.name} · ${ws.source || ws.container_name}`;
  const status = $('#workspaceStatus');
  status.className = `status-badge ${ws.status}`;
  status.textContent = statusLabel(ws.status);
  $('#workspacePowerButton').textContent = ws.status === 'running' ? '◉' : '▶';
  $('#workspacePowerButton').title = ws.status === 'running' ? '停止 Workspace' : '启动 Workspace';
  renderResourceMonitor();
  $('#statusWorkspace').textContent = `${template.name} · ${statusLabel(ws.status)}`;
  const current = activeTab();
  $('#statusSession').textContent = current?.kind === 'session' ? `${agentLabel(current.session.agent)} · ${current.title}` : current?.title || 'Workspace';
  $('#paneGrid').classList.toggle('split', state.split);
  const splitPercent = Math.round(state.splitRatio * 100);
  $('#paneGrid').style.setProperty('--split-left', `${splitPercent}%`);
  $('#paneGrid').style.setProperty('--split-top', `${splitPercent}%`);
  $('#paneDivider').classList.toggle('hidden', !state.split);
  $('#paneDivider').setAttribute('aria-orientation', window.innerWidth <= 720 ? 'horizontal' : 'vertical');
  $('#paneDivider').setAttribute('aria-valuenow', String(splitPercent));
  $('.work-pane.secondary').classList.toggle('hidden', !state.split);
  $('#sendToOtherPaneButton').disabled = !state.split;
  $('#openExternalButton').disabled = !current?.url;
  $('#openExternalButton').title = current?.url ? '新窗口打开' : '当前页面无法在新窗口打开';
  renderSessionList();
  renderTabStrip();
  renderPane('primary');
  if (state.split) renderPane('secondary');
  else state.viewPlacements.secondary = null;
  scheduleViewLayout();
}

async function openWorkspace(id, push = true) {
  persistLayout();
  if (state.currentWorkspace?.id !== id) destroyAllViews();
  try {
    state.currentWorkspace = await api(`/api/workspaces/${id}`);
    state.resourceStats = null;
    state.resourceStatsWorkspaceID = id;
    state.sidebarVisible = true;
    $('#workspaceSidebar').classList.remove('mobile-hidden');
    $('#workspaceView').classList.remove('sidebar-collapsed');
    $('[data-activity="sessions"]')?.classList.add('active');
    restoreLayout();
    $('#dashboardView').classList.add('hidden');
    $('#workspaceView').classList.remove('hidden');
    document.body.style.overflow = 'hidden';
    if (push) history.pushState({}, '', `/workspace/${id}`);
    renderWorkbench();
    void refreshResourceStats(id);
  } catch (error) {
    showToast(error.message);
    showDashboard(true);
  }
}

function toggleSplit() {
  state.split = !state.split;
  if (state.split && (!tabDescriptor(state.panes.secondary) || state.panes.secondary === state.panes.primary)) {
    const alternative = state.tabs.find(tab => tab.id !== state.panes.primary) || ensureTab(state.panes.primary === 'code' ? 'home' : 'code');
    state.panes.secondary = alternative.id;
  }
  if (!state.split) state.focusedPane = 'primary';
  renderWorkbench();
  persistLayout();
}

function sendCurrentToOtherPane() {
  if (!state.split) return;
  const source = state.focusedPane;
  const other = source === 'primary' ? 'secondary' : 'primary';
  const sourceTab = state.panes[source];
  const otherTab = state.panes[other];
  if (!sourceTab || sourceTab === otherTab) return;
  state.panes[source] = otherTab;
  state.panes[other] = sourceTab;
  state.focusedPane = other;
  renderWorkbench();
  persistLayout();
}

function reloadFocusedPane() {
  renderPane(state.focusedPane, true);
}

function openFocusedExternal() {
  const tab = activeTab();
  if (tab?.url) window.open(tab.url, '_blank', 'noopener');
}

function toggleImmersive() {
  $('#workspaceView').classList.toggle('immersive');
}

/* Refresh and runtime actions */
let refreshPromise = null;
let pendingAccountSelection = null;
async function refreshAll({quiet = false} = {}) {
  if (refreshPromise) return refreshPromise;
  refreshPromise = (async () => {
  $('#refreshButton').disabled = true;
  try {
    [state.templates, state.environmentProfiles, state.workspaces, state.accounts] = await Promise.all([api('/api/templates'), api('/api/environment-profiles'), api('/api/workspaces'), api('/api/accounts')]);
    renderTemplates();
    if (environmentProfilesDialog.open) {
      const currentProfile = $('#environmentEditingID').value;
      renderEnvironmentProfileList(currentProfile);
    }
    if (accountsDialog.open) renderAccounts();
    if (state.currentWorkspace) {
      const updated = state.workspaces.find(ws => ws.id === state.currentWorkspace.id);
      if (updated) {
        if (updated.status !== state.currentWorkspace.status) destroyAllViews();
        state.currentWorkspace = updated;
        state.tabs = state.tabs.map(tab => tabDescriptor(tab.id)).filter(Boolean);
        if (!state.tabs.length) state.tabs = [tabDescriptor('home')];
        for (const pane of ['primary','secondary']) if (!tabDescriptor(state.panes[pane])) state.panes[pane] = state.tabs[0].id;
        renderWorkbench();
        void refreshResourceStats(updated.id);
      }
    } else renderDashboard();
  } catch (error) {
    if (!quiet) showToast(error.message);
  } finally {
    $('#refreshButton').disabled = false;
  }
  })();
  try { return await refreshPromise; }
  finally { refreshPromise = null; }
}

async function workspacePower() {
  const ws = state.currentWorkspace;
  if (!ws) return;
  const action = ws.status === 'running' ? 'stop' : 'start';
  $('#workspacePowerButton').disabled = true;
  try {
    state.currentWorkspace = await api(`/api/workspaces/${ws.id}/${action}`, {method:'POST', timeout:70000});
    destroyAllViews();
    showToast(action === 'start' ? 'Workspace 已启动' : 'Workspace 已停止');
    renderWorkbench();
    renderPane('primary', true);
    if (state.split) renderPane('secondary', true);
    await refreshAll({quiet:true});
  } catch (error) { showToast(error.message); }
  finally { $('#workspacePowerButton').disabled = false; }
}

async function dashboardAction(id, action) {
  if (action === 'open') return openWorkspace(id);
  try {
    await api(`/api/workspaces/${id}/${action}`, {method:'POST', timeout:70000});
    showToast(action === 'start' ? 'Workspace 已启动' : 'Workspace 已停止');
    await refreshAll({quiet:true});
  } catch (error) { showToast(error.message); }
}

/* Workspace creation and file import */
function openCreate(files = null) {
  $('#createForm').reset();
  $('#createError').classList.add('hidden');
  $('#fileHint').textContent = '也可以点击选择；Git 与上传文件可同时使用。';
  if (state.templates.length) setTemplate(state.templates[0].id, '');
  renderAccountOptions($('#createAgentInput').value, $('#createAccountInput'));
  updateDirectFields('create', $('#createAgentInput').value);
  createDialog.showModal();
  if (files?.length) {
    const transfer = new DataTransfer();
    [...files].forEach(file => transfer.items.add(file));
    $('#fileInput').files = transfer.files;
    $('#fileHint').textContent = `已选择 ${files.length} 个文件`;
  }
}

async function importFiles(files, options = {}) {
  const ws = state.currentWorkspace;
  const errorBox = options.errorBox || null;
  if (errorBox) errorBox.classList.add('hidden');
  if (!ws || ws.status !== 'running' || !files?.length) return false;
  const form = new FormData();
  [...files].forEach(file => form.append('files', file, file.webkitRelativePath || file.name));
  showToast(`正在导入 ${files.length} 个文件…`);
  try {
    await api(`/api/workspaces/${ws.id}/files`, {method:'POST', body:form, timeout:900000});
    showToast('文件已导入 /workspace');
    return true;
  } catch (error) {
    showToast(error.message);
    if (errorBox) {
      errorBox.textContent = error.message;
      errorBox.classList.remove('hidden');
    }
    return false;
  }
}

function bindDropzone(dropzone, input, hint) {
  ['dragenter','dragover'].forEach(type => dropzone.addEventListener(type, event => { event.preventDefault(); dropzone.classList.add('dragging'); }));
  ['dragleave','drop'].forEach(type => dropzone.addEventListener(type, event => { event.preventDefault(); dropzone.classList.remove('dragging'); }));
  dropzone.addEventListener('drop', event => {
    const transfer = new DataTransfer();
    for (const file of event.dataTransfer.files) transfer.items.add(file);
    input.files = transfer.files;
    hint.textContent = `已选择 ${transfer.files.length} 个文件`;
  });
  input.addEventListener('change', () => { hint.textContent = input.files.length ? `已选择 ${input.files.length} 个文件` : '请选择要导入的文件'; });
}

/* Sessions */
function openSessionDialog(sessionID = null, presetAgent = null) {
  const ws = state.currentWorkspace;
  if (!ws) return;
  state.editingSessionID = sessionID;
  const session = ws.sessions?.find(item => item.id === sessionID);
  $('#sessionForm').reset();
  $('#sessionError').classList.add('hidden');
  $('#sessionDialogEyebrow').textContent = session ? 'SESSION SETTINGS' : 'NEW SESSION';
  $('#sessionDialogTitle').textContent = session ? '编辑会话' : '创建会话';
  $('#saveSessionButton').textContent = session ? '保存修改' : '创建并打开';
  $('#deleteSessionButton').classList.toggle('hidden', !session);
  if (session) {
    $('#sessionNameInput').value = session.name;
    $('#sessionAgentInput').value = session.agent;
  } else if (presetAgent) $('#sessionAgentInput').value = presetAgent;
  renderAccountOptions($('#sessionAgentInput').value, $('#sessionAccountInput'), session?.account_id || '');
  updateDirectFields('session', $('#sessionAgentInput').value, session || null);
  applyLaunchProfile($('#sessionAgentInput').value, session?.launch_profile || null);
  sessionDialog.showModal();
  void refreshLaunchPreview();
}

async function saveSession(event) {
  event.preventDefault();
  const ws = state.currentWorkspace;
  const button = $('#saveSessionButton');
  const errorBox = $('#sessionError');
  const agent = $('#sessionAgentInput').value;
  const payload = {name:$('#sessionNameInput').value.trim(), agent, account_id:isDirectAgent(agent) ? '' : $('#sessionAccountInput').value, permission_mode:agent === 'claude' ? $('#sessionPermissionMode').value : 'manual', dangerous_skip_permissions:agent === 'claude' && $('#sessionDangerousSkip').checked, dangerous_confirmation:agent === 'claude' ? $('#sessionDangerousConfirmation').value.trim() : '', direct_config:collectDirectConfig('session', agent), launch_profile:collectLaunchProfile(agent)};
  button.disabled = true;
  errorBox.classList.add('hidden');
  try {
    let session;
    if (state.editingSessionID) {
      session = await api(`/api/workspaces/${ws.id}/sessions/${state.editingSessionID}`, {method:'PATCH', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)});
    } else {
      session = await api(`/api/workspaces/${ws.id}/sessions`, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)});
    }
    sessionDialog.close();
    state.currentWorkspace = await api(`/api/workspaces/${ws.id}`);
    if (session?.id) openTab('session', session.id);
    else renderWorkbench();
    if (session?.status === 'error' || session?.error) showToast(`会话已保存，但未就绪：${session.error || '请检查配置和最近输出'}`);
    else showToast(state.editingSessionID ? '会话已更新' : '会话已创建并打开');
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally { button.disabled = false; }
}

async function deleteCurrentSession() {
  const ws = state.currentWorkspace;
  const session = ws.sessions?.find(item => item.id === state.editingSessionID);
  if (!session || !confirm(`确定删除会话“${session.name}”？tmux 中未保存的状态会丢失。`)) return;
  try {
    await api(`/api/workspaces/${ws.id}/sessions/${session.id}`, {method:'DELETE'});
    sessionDialog.close();
    closeTab(`session:${session.id}`);
    state.currentWorkspace = await api(`/api/workspaces/${ws.id}`);
    renderWorkbench();
    showToast('会话已删除');
  } catch (error) { showToast(error.message); }
}

function focusedSession() {
  const tab = activeTab();
  return tab?.kind === 'session' ? tab.session : null;
}

async function restartFocusedSession() {
  const ws = state.currentWorkspace;
  const session = focusedSession();
  if (!ws || !session) return;
  try {
    await api(`/api/workspaces/${ws.id}/sessions/${session.id}/restart`, {method:'POST', timeout:70000});
    showToast('会话已重启');
    destroyTabViews(`session:${session.id}`);
    renderPane(state.focusedPane, true);
    await refreshAll({quiet:true});
  } catch (error) { showToast(error.message); }
}

async function showTranscript(sessionID = null) {
  const ws = state.currentWorkspace;
  const session = ws?.sessions?.find(item => item.id === (sessionID || focusedSession()?.id));
  if (!session) return;
  $('#transcriptTitle').textContent = session.name;
  $('#transcriptContent').textContent = 'Loading...';
  transcriptDialog.showModal();
  try {
    const result = await api(`/api/workspaces/${ws.id}/sessions/${session.id}/transcript?lines=1200`);
    $('#transcriptContent').textContent = result.text || '(没有输出)';
  } catch (error) { $('#transcriptContent').textContent = error.message; }
}

/* Accounts */
const AUTH_STATUS_LIMIT = 3;
const CONTROL_AUTH_WARNING = '控制面认证未启用。仅应在可信网络中使用；任何能访问此控制面的人都可能启动登录或删除本地凭据。';
let authPollTimer = 0;
let authCountdownTimer = 0;

function accountReferenceCount(accountID) {
  return state.workspaces.reduce((total, ws) => total + (ws.sessions || []).filter(session => session.account_id === accountID).length, 0);
}
function accountEndpoint(account) { return account.base_url || (account.agent === 'codex' ? 'OpenAI 默认端点' : 'Anthropic 默认端点'); }
function nativeLoginAccount(account) { return Boolean(account && account.auth_type === 'login' && (account.agent === 'codex' || account.agent === 'claude')); }
function accountAuthRecord(accountID) { return state.accountAuth.get(accountID) || {status:'unknown', attemptID:'', attemptState:'', message:''}; }
function authAPIOptions(method = 'POST', body = {}) {
  return {method, headers:{'Content-Type':'application/json', [AUTH_REQUEST_HEADER]:'account-auth'}, body:JSON.stringify(body)};
}
function terminalPath(accountID, attemptID) {
  return `/auth/accounts/${encodeURIComponent(accountID)}/login-attempts/${encodeURIComponent(attemptID)}/terminal/`;
}
function updateControlAuthWarnings() {
  const disabled = !state.controlAuthEnabled;
  $('#accountAuthWarning')?.classList.toggle('hidden', !disabled);
  for (const id of ['codexAuthWarning','claudeAuthWarning']) {
    const warning = $(`#${id}`);
    if (!warning) continue;
    warning.textContent = disabled ? CONTROL_AUTH_WARNING : '';
    warning.classList.toggle('hidden', !disabled);
  }
}
function nativeAuthActionMarkup(account, record) {
  const active = record.status === 'login_in_progress' || accountAuth.isActiveAttempt(record.attemptState);
  const authenticated = record.status === 'authenticated';
  if (active) return `<button class="button primary" data-account-action="continue-login" data-id="${escapeHTML(account.id)}">继续登录</button><button class="button danger" data-account-action="cancel-login" data-id="${escapeHTML(account.id)}">取消</button>`;
  if (authenticated) return `<button class="button ghost" data-account-action="login" data-id="${escapeHTML(account.id)}">重新登录</button><button class="button danger" data-account-action="logout" data-id="${escapeHTML(account.id)}">退出登录</button>`;
  return `<button class="button primary" data-account-action="login" data-id="${escapeHTML(account.id)}">登录</button>`;
}
function renderAccounts() {
  const list = $('#accountList');
  const empty = $('#accountEmpty');
  empty.classList.toggle('hidden', state.accounts.length > 0);
  list.classList.toggle('hidden', state.accounts.length === 0);
  list.innerHTML = state.accounts.map(account => {
    const references = accountReferenceCount(account.id);
    const nativeLogin = nativeLoginAccount(account);
    const record = accountAuthRecord(account.id);
    const status = accountAuth.authStatusPresentation(record.status);
    const secretState = nativeLogin ? '由官方 CLI 管理' : account.has_secret ? `已保存 · ${escapeHTML(account.secret_hint || '••••')}` : '未配置';
    return `<article class="account-card ${nativeLogin ? 'native-login-card' : ''}" data-account-card="${escapeHTML(account.id)}">
      <div class="account-card-head"><span class="session-agent ${escapeHTML(account.agent)}">${escapeHTML(agentGlyph(account.agent))}</span><div><h4>${escapeHTML(account.name)}</h4><small>${escapeHTML(agentLabel(account.agent))} · ${escapeHTML(authTypeLabel(account.auth_type))}</small></div>${nativeLogin ? `<span class="native-auth-badge ${escapeHTML(status.tone)}">${escapeHTML(status.label)}</span>` : `<span class="account-usage">${references} sessions</span>`}</div>
      <div class="account-details"><div><span>Endpoint</span><b>${escapeHTML(accountEndpoint(account))}</b></div><div><span>Model</span><b>${escapeHTML(account.model || 'provider 默认')}</b></div><div><span>Credential</span><b>${secretState}</b></div>${nativeLogin ? `<div><span>官方登录状态</span><b>${escapeHTML(record.message || status.label)}</b></div>` : `<div><span>Last used</span><b>${escapeHTML(formatDate(account.last_used_at))}</b></div>`}</div>
      <div class="account-card-actions">${nativeLogin ? nativeAuthActionMarkup(account, record) : `<button class="button ghost" data-account-action="probe" data-id="${escapeHTML(account.id)}">测试</button>`}<button class="button ghost" data-account-action="config" data-id="${escapeHTML(account.id)}">配置文件</button><button class="button ghost" data-account-action="edit" data-id="${escapeHTML(account.id)}">编辑</button><button class="button danger" data-account-action="delete" data-id="${escapeHTML(account.id)}">删除</button></div>
    </article>`;
  }).join('');
  updateControlAuthWarnings();
}

async function loadAccountConfigFile(path = state.accountConfigPath) {
  const accountID = state.accountConfigAccountID;
  if (!accountID || !path) return;
  state.accountConfigPath = path;
  $('#accountConfigPath').textContent = path;
  $('#accountConfigState').textContent = '读取中…';
  $('#accountConfigError').classList.add('hidden');
  try {
    const document = await api(`/api/accounts/${accountID}/config/${encodeURIComponent(path)}`);
    $('#accountConfigContent').value = document.content || '';
    $('#accountConfigState').textContent = document.exists ? '已存在 · 可编辑' : '文件尚未创建';
  } catch (error) {
    $('#accountConfigState').textContent = '读取失败';
    $('#accountConfigError').textContent = error.message;
    $('#accountConfigError').classList.remove('hidden');
  }
}

async function openAccountConfig(accountID) {
  const account = accountFor(accountID);
  if (!account) return;
  state.accountConfigAccountID = accountID;
  state.accountConfigPath = null;
  $('#accountConfigTitle').textContent = `${account.name} · 配置文件`;
  $('#accountConfigContent').value = '';
  $('#accountConfigError').classList.add('hidden');
  accountConfigDialog.showModal();
  try {
    const result = await api(`/api/accounts/${accountID}/config`);
    const tree = $('#accountConfigTree');
    tree.innerHTML = (result.files || []).map(file => `<button type="button" data-config-path="${escapeHTML(file.path)}"><span>${escapeHTML(file.path)}</span><small>${file.exists ? `${file.size || 0} bytes` : '未创建'}</small></button>`).join('');
    const first = result.files?.[0]?.path;
    if (first) {
      tree.querySelector(`[data-config-path="${CSS.escape(first)}"]`)?.classList.add('selected');
      await loadAccountConfigFile(first);
    }
  } catch (error) {
    $('#accountConfigError').textContent = error.message;
    $('#accountConfigError').classList.remove('hidden');
  }
}

async function saveAccountConfig() {
  const accountID = state.accountConfigAccountID;
  const path = state.accountConfigPath;
  if (!accountID || !path) return;
  try {
    await api(`/api/accounts/${accountID}/config/${encodeURIComponent(path)}`, {method:'PUT', headers:{'Content-Type':'application/json', [AUTH_REQUEST_HEADER]:'account-auth'}, body:JSON.stringify({content:$('#accountConfigContent').value})});
    $('#accountConfigState').textContent = '已保存';
    showToast('账号配置文件已保存');
  } catch (error) {
    $('#accountConfigError').textContent = error.message;
    $('#accountConfigError').classList.remove('hidden');
  }
}

async function resetAccountConfig() {
  const accountID = state.accountConfigAccountID;
  const path = state.accountConfigPath;
  if (!accountID || !path || !confirm(`确定重置 ${path}？这会移除当前文件。`)) return;
  try {
    await api(`/api/accounts/${accountID}/config/${encodeURIComponent(path)}`, {method:'DELETE', headers:{[AUTH_REQUEST_HEADER]:'account-auth'}});
    await loadAccountConfigFile(path);
    showToast('账号配置文件已重置');
  } catch (error) {
    $('#accountConfigError').textContent = error.message;
    $('#accountConfigError').classList.remove('hidden');
  }
}
async function refreshNativeAuthStatuses() {
  const accounts = state.accounts.filter(nativeLoginAccount);
  if (!accounts.length) return;
  const results = await runWithConcurrency(accounts, AUTH_STATUS_LIMIT, async account => {
    const payload = await api(`/api/accounts/${account.id}/auth-status`, {timeout:15000});
    return accountAuth.normalizeAuthStatus(payload);
  });
  results.forEach((result, index) => {
    const account = accounts[index];
    if (result.status === 'fulfilled') state.accountAuth.set(account.id, result.value);
    else state.accountAuth.set(account.id, {status:'unavailable', attemptID:'', attemptState:'', message:result.reason?.message || '无法读取官方登录状态'});
  });
  if (accountsDialog.open) renderAccounts();
}
async function openAccountManager(options = {}) {
  renderAccounts();
  if (!accountsDialog.open) accountsDialog.showModal();
  await refreshNativeAuthStatuses();
  if (options.loginAccountID) {
    const account = accountFor(options.loginAccountID);
    if (nativeLoginAccount(account)) await continueOrStartLogin(account);
  }
}
function populateAuthOptions(agent, selected = '') {
  const options = authOptions(agent);
  const select = $('#accountAuthInput');
  select.innerHTML = options.map(([value,label]) => `<option value="${value}">${escapeHTML(label)}</option>`).join('');
  select.value = options.some(([value]) => value === selected) ? selected : 'login';
  const picker = $('#accountAuthPicker');
  picker.innerHTML = options.map(([value,label]) => {
    const login = value === 'login';
    const setupToken = value === 'oauth_token';
    const description = login
      ? (agent === 'codex' ? '使用 ChatGPT 与 OpenAI 官方设备页面' : '使用 Claude.ai 与 Claude Code 官方登录终端')
      : setupToken ? '粘贴已由 Claude 官方 CLI 生成的 setup-token，不是网页登录'
      : value === 'api_key' ? '加密保存 Provider API Key' : '加密保存 Bearer Token';
    return `<button type="button" class="auth-method-option ${select.value === value ? 'selected' : ''} ${login ? 'official-login' : ''}" role="radio" aria-checked="${select.value === value}" data-auth-method="${escapeHTML(value)}"><b>${escapeHTML(label)}</b><small>${escapeHTML(description)}</small></button>`;
  }).join('');
  updateAccountFormHints();
}
function selectAuthMethod(value) {
  const select = $('#accountAuthInput');
  if (![...select.options].some(option => option.value === value)) return;
  select.value = value;
  $$('.auth-method-option', $('#accountAuthPicker')).forEach(option => {
    const selected = option.dataset.authMethod === value;
    option.classList.toggle('selected', selected);
    option.setAttribute('aria-checked', String(selected));
  });
  updateAccountFormHints();
}
function updateAccountFormHints() {
  const agent = $('#accountAgentInput').value;
  const auth = $('#accountAuthInput').value;
  const isLogin = auth === 'login';
  $('#accountSecretField').classList.toggle('hidden', isLogin);
  $('#accountBaseURLInput').disabled = isLogin;
  if (isLogin) $('#accountBaseURLInput').value = '';
  $('#accountBaseURLHint').textContent = isLogin ? '网页登录 / 官方 CLI 使用 Provider 官方端点，不能自定义 Base URL。' : agent === 'codex' ? '留空使用 OpenAI 默认端点；自定义端点需要兼容 Responses API。' : '留空使用 Anthropic 官方 API。';
  $('#accountSecretLabel').textContent = ({api_key:agent === 'claude' ? 'Anthropic API Key' : 'API Key', bearer_token:'Bearer Token', oauth_token:'已有 Claude setup-token'})[auth] || 'API Key / Token';
  $('#accountSecretHint').textContent = auth === 'oauth_token' ? '请先使用 Claude 官方 CLI 生成 setup-token，再粘贴到这里。setup-token 是 Token 导入，不是 Claude.ai 网页登录。' : '密钥仅会发送到当前 VibeDock 控制面，并加密保存。';
  $('#accountAuthMethodHint').textContent = isLogin
    ? (agent === 'codex' ? '保存档案后将返回账号中心，并立即开始 ChatGPT / OpenAI 官方设备登录。' : '保存档案后将返回账号中心，并立即打开 Claude Code 官方登录终端。')
    : auth === 'oauth_token' ? 'setup-token 不会启动 Claude.ai 登录。要网页登录，请选择“网页登录 / 官方 CLI”。' : '这类 API 凭据由 VibeDock 控制面加密保存。';
}
function envMapToText(env = {}) { return Object.entries(env).sort(([a],[b]) => a.localeCompare(b)).map(([k,v]) => `${k}=${v}`).join('\n'); }
function parseEnvText(text) {
  const result = {};
  for (const [index, raw] of text.split(/\r?\n/).entries()) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const separator = line.indexOf('=');
    if (separator < 1) throw new Error(`额外环境变量第 ${index + 1} 行必须是 KEY=VALUE`);
    const key = line.slice(0, separator).trim();
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) throw new Error(`环境变量名 ${key} 不合法`);
    result[key] = line.slice(separator + 1);
  }
  return result;
}

function packageTextToList(text) {
  return text.split(/\r?\n/).map(item => item.trim()).filter(item => item && !item.startsWith('#'));
}

function environmentProfileFor(id) {
  return state.environmentProfiles.find(profile => profile.id === id) || null;
}

function ensureSelectValue(select, value, label) {
  const stringValue = String(value ?? '');
  if (stringValue && ![...select.options].some(option => option.value === stringValue)) {
    select.add(new Option(label || stringValue, stringValue));
  }
  select.value = stringValue;
}

function renderEnvironmentProfileList(selectedID = $('#environmentEditingID')?.value || '') {
  const list = $('#environmentProfileList');
  if (!state.environmentProfiles.length) {
    list.innerHTML = '<div class="environment-list-empty">还没有定制环境。新建后，它会出现在 Workspace 的环境模板选择器中。</div>';
    return;
  }
  list.innerHTML = state.environmentProfiles.map(profile => `<button type="button" class="environment-profile-item ${profile.id === selectedID ? 'selected' : ''}" data-environment-id="${escapeHTML(profile.id)}">
    <span>${escapeHTML(profile.icon || '◇')}</span><div><b>${escapeHTML(profile.name)}</b><small>${escapeHTML(profile.description || '定制环境配置')}</small></div>
  </button>`).join('');
}

function resetEnvironmentProfileEditor() {
  $('#environmentProfileForm').reset();
  $('#environmentEditingID').value = '';
  $('#environmentEditorTitle').textContent = '新建定制环境';
  $('#deleteEnvironmentProfile').classList.add('hidden');
  $('#environmentProfileError').classList.add('hidden');
  $('#environmentBaseTemplate').innerHTML = state.templates.map(template => `<option value="${escapeHTML(template.id)}">${escapeHTML(template.name)}</option>`).join('');
  $('#environmentIcon').value = '◇';
  $('#environmentCPUs').value = '2';
  $('#environmentMemory').value = '4096';
  renderEnvironmentProfileList('');
}

function editEnvironmentProfile(id) {
  const profile = environmentProfileFor(id);
  if (!profile) return resetEnvironmentProfileEditor();
  $('#environmentEditingID').value = profile.id;
  $('#environmentEditorTitle').textContent = profile.name;
  $('#environmentName').value = profile.name;
  $('#environmentDescription').value = profile.description || '';
  ensureSelectValue($('#environmentIcon'), profile.icon || '◇', profile.icon || '◇');
  ensureSelectValue($('#environmentBaseTemplate'), profile.base_template_id, profile.base_template_id);
  ensureSelectValue($('#environmentCPUs'), profile.cpus, `${profile.cpus} vCPU`);
  ensureSelectValue($('#environmentMemory'), profile.memory_mb, `${profile.memory_mb} MB`);
  $('#environmentApt').value = (profile.apt_packages || []).join('\n');
  $('#environmentNpm').value = (profile.npm_modules || []).join('\n');
  $('#environmentPython').value = (profile.python_packages || []).join('\n');
  $('#environmentVariables').value = envMapToText(profile.environment);
  $('#environmentInitScript').value = profile.init_script || '';
  $('#deleteEnvironmentProfile').classList.remove('hidden');
  $('#environmentProfileError').classList.add('hidden');
  renderEnvironmentProfileList(profile.id);
}

function openEnvironmentProfiles(selectedID = '') {
  resetEnvironmentProfileEditor();
  const selected = environmentProfileFor(selectedID) || state.environmentProfiles[0];
  if (selected) editEnvironmentProfile(selected.id);
  environmentProfilesDialog.showModal();
}

async function saveEnvironmentProfile(event) {
  event.preventDefault();
  const id = $('#environmentEditingID').value;
  const button = $('#saveEnvironmentProfile');
  const errorBox = $('#environmentProfileError');
  button.disabled = true;
  errorBox.classList.add('hidden');
  try {
    const payload = {
      name:$('#environmentName').value.trim(), description:$('#environmentDescription').value.trim(), icon:$('#environmentIcon').value,
      base_template_id:$('#environmentBaseTemplate').value, cpus:Number($('#environmentCPUs').value), memory_mb:Number($('#environmentMemory').value),
      apt_packages:packageTextToList($('#environmentApt').value), npm_modules:packageTextToList($('#environmentNpm').value),
      python_packages:packageTextToList($('#environmentPython').value), environment:parseEnvText($('#environmentVariables').value),
      init_script:$('#environmentInitScript').value,
    };
    const saved = await api(id ? `/api/environment-profiles/${id}` : '/api/environment-profiles', {
      method:id ? 'PATCH' : 'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload),
    });
    state.environmentProfiles = await api('/api/environment-profiles');
    state.selectedEnvironmentProfile = saved.id;
    renderTemplates();
    editEnvironmentProfile(saved.id);
    showToast(id ? '定制环境已更新' : '定制环境已创建');
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally {
    button.disabled = false;
  }
}

async function deleteEnvironmentProfile() {
  const id = $('#environmentEditingID').value;
  const profile = environmentProfileFor(id);
  if (!profile || !confirm(`删除定制环境“${profile.name}”？已有 Workspace 不会被删除。`)) return;
  try {
    await api(`/api/environment-profiles/${id}`, {method:'DELETE'});
    state.environmentProfiles = await api('/api/environment-profiles');
    if (state.selectedEnvironmentProfile === id) state.selectedEnvironmentProfile = null;
    renderTemplates();
    resetEnvironmentProfileEditor();
    if (state.environmentProfiles[0]) editEnvironmentProfile(state.environmentProfiles[0].id);
    showToast('定制环境已删除，已有 Workspace 保持不变');
  } catch (error) {
    const errorBox = $('#environmentProfileError');
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  }
}

function openAccountEditor(accountID = null, options = {}) {
  state.editingAccountID = accountID;
  if (!accountID && (options.returnTo || options.startLogin)) pendingAccountSelection = {returnTo:options.returnTo || 'accounts-login', agent:options.agent || 'codex', startLogin:Boolean(options.startLogin)};
  const account = accountFor(accountID);
  $('#accountForm').reset();
  $('#accountError').classList.add('hidden');
  $('#accountDialogEyebrow').textContent = account ? 'EDIT ACCOUNT' : 'NEW ACCOUNT';
  $('#accountDialogTitle').textContent = account ? '编辑账号配置' : '添加账号配置';
  $('#saveAccountButton').textContent = account ? '保存修改' : '保存账号';
  $('#clearSecretRow').classList.toggle('hidden', !account?.has_secret);
  $('#clearSecretInput').checked = false;
  $('#accountAgentInput').disabled = Boolean(account && accountReferenceCount(account.id) > 0);
  if (account) {
    $('#accountNameInput').value = account.name; $('#accountAgentInput').value = account.agent; populateAuthOptions(account.agent, account.auth_type);
    $('#accountBaseURLInput').value = account.base_url || ''; $('#accountModelInput').value = account.model || ''; $('#accountEnvInput').value = envMapToText(account.extra_env);
    $('#accountSecretInput').placeholder = account.has_secret ? `已保存 ${account.secret_hint || '••••'}，留空保持不变` : '粘贴密钥';
  } else {
    $('#accountAgentInput').value = pendingAccountSelection?.agent === 'claude' ? 'claude' : 'codex';
    populateAuthOptions($('#accountAgentInput').value,'login');
  }
  updateAccountFormHints();
  accountDialog.showModal();
}
async function saveAccount(event) {
  event.preventDefault();
  const editingAccountID = state.editingAccountID;
  const button = $('#saveAccountButton'); const errorBox = $('#accountError'); button.disabled = true; errorBox.classList.add('hidden');
  try {
    const payload = {name:$('#accountNameInput').value.trim(), agent:$('#accountAgentInput').value, auth_type:$('#accountAuthInput').value, base_url:$('#accountBaseURLInput').disabled ? '' : $('#accountBaseURLInput').value.trim(), model:$('#accountModelInput').value.trim(), secret:$('#accountSecretInput').value.trim(), clear_secret:$('#clearSecretInput').checked, extra_env:parseEnvText($('#accountEnvInput').value)};
    const path = editingAccountID ? `/api/accounts/${editingAccountID}` : '/api/accounts';
    const saved = await api(path, {method:editingAccountID ? 'PATCH' : 'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)});
    accountDialog.close();
    state.accounts = await api('/api/accounts');
    renderAccounts();
    const pending = pendingAccountSelection;
    pendingAccountSelection = null;
    if (pending?.returnTo === 'session' && sessionDialog.open) {
      renderAccountOptions(payload.agent, $('#sessionAccountInput'), saved?.id || '');
      renderSessionLoginGuide(payload.agent, saved?.id || '');
    }
    if (pending?.returnTo === 'create' && createDialog.open) renderAccountOptions(payload.agent, $('#createAccountInput'), saved?.id || '');
    if (!editingAccountID && payload.auth_type === 'login') {
      showToast('原生登录账号已创建，正在返回账号中心。');
      await openAccountManager();
      if (saved?.id && (pending?.startLogin || pending?.returnTo === 'accounts-login')) await startAccountLogin(saved.id);
    } else showToast(editingAccountID ? '账号配置已更新' : '账号配置已创建');
  } catch (error) { errorBox.textContent = error.message; errorBox.classList.remove('hidden'); }
  finally { button.disabled = false; }
}
async function probeAccount(id) {
  const account = accountFor(id);
  if (!account) return;
  try {
    const result = await api(`/api/accounts/${id}/probe`, {method:'POST'});
    const messages = {
      ready:'连接成功，凭据可用',
      auth_failed:`Provider 可访问，但凭据被拒绝${result.http_status ? `（HTTP ${result.http_status}）` : ''}`,
      reachable:`Provider 已响应${result.http_status ? `（HTTP ${result.http_status}）` : ''}，但未确认凭据可用`,
      interactive:'需要在 Agent 终端内完成官方 CLI 登录',
      session_required:'请启动或重启绑定此账号的 Session 完成验证',
    };
    showToast(result.message || messages[result.status] || (result.ok ? '连接测试成功' : '连接测试未通过'));
  } catch (error) { showToast(error.message); }
}
async function deleteAccount(id) { const account = accountFor(id); if (!account || !confirm(`确定删除账号“${account.name}”？原生登录账号的本地 CLI 凭据也会被删除。`)) return; try { await api(`/api/accounts/${id}`, {method:'DELETE'}); state.accountAuth.delete(id); state.accounts = await api('/api/accounts'); renderAccounts(); showToast('账号配置已删除'); } catch (error) { showToast(error.message); } }

function selectNativeLoginAccount(agent) {
  const matches = state.accounts.filter(account => account.agent === agent && account.auth_type === 'login');
  if (!matches.length) return null;
  const inProgress = matches.find(account => accountAuthRecord(account.id).status === 'login_in_progress');
  const unauthenticated = matches.find(account => ['unauthenticated','unknown','unavailable'].includes(accountAuthRecord(account.id).status));
  return inProgress || unauthenticated || matches[0];
}
async function startTopLevelLogin(agent) {
  const account = selectNativeLoginAccount(agent);
  if (account) return continueOrStartLogin(account);
  openAccountEditor(null, {agent, startLogin:true, returnTo:'accounts-login'});
}
function currentAuthDialog() {
  return state.activeAuth?.provider === 'claude' ? claudeLoginDialog : codexLoginDialog;
}
function clearAuthTimers() {
  clearTimeout(authPollTimer);
  clearInterval(authCountdownTimer);
  authPollTimer = 0;
  authCountdownTimer = 0;
}
function setSensitiveText(element, value = '') {
  if (element) element.textContent = value;
}
function clearSensitiveAttemptUI(provider) {
  if (provider === 'codex') {
    setSensitiveText($('#codexDeviceCode'), '—');
    $('#openCodexOfficialPage').dataset.url = '';
    $('#openCodexOfficialPage').disabled = true;
    $('#copyCodexCode').disabled = true;
  }
  if (provider === 'claude') {
    const frame = $('#claudeLoginTerminal');
    frame.removeAttribute('src');
    frame.hidden = true;
    $('#claudeTerminalPlaceholder').classList.remove('hidden');
  }
}
function renderAuthAttempt(attempt) {
  if (!state.activeAuth || state.activeAuth.accountID !== attempt.accountID || state.activeAuth.attemptID !== attempt.id) return;
  state.activeAuth.attempt = attempt;
  const provider = state.activeAuth.provider;
  const statusElement = $(`#${provider}LoginStatus`);
  const errorElement = $(`#${provider}LoginError`);
  const cancelButton = $(`#cancel${provider === 'codex' ? 'Codex' : 'Claude'}Login`);
  const retryButton = $(`#retry${provider === 'codex' ? 'Codex' : 'Claude'}Login`);
  const terminal = accountAuth.isTerminalAttempt(attempt.state);
  state.accountAuth.set(attempt.accountID, {status:terminal ? (attempt.state === 'completed' ? 'authenticated' : 'unauthenticated') : 'login_in_progress', attemptID:terminal ? '' : attempt.id, attemptState:attempt.state, message:attempt.message || accountAuth.attemptStatePresentation(attempt.state), attempt:terminal ? null : attempt});
  statusElement.textContent = attempt.message || accountAuth.attemptStatePresentation(attempt.state);
  const errorMessage = attempt.errorMessage || (attempt.state === 'failed' ? '官方登录未完成，请重试。' : '');
  errorElement.textContent = errorMessage;
  errorElement.classList.toggle('hidden', !errorMessage);
  cancelButton.classList.toggle('hidden', terminal);
  retryButton.classList.toggle('hidden', !terminal || attempt.state === 'completed');
  if (provider === 'codex') {
    const officialURL = accountAuth.safeOfficialURL(attempt.verificationURL);
    setSensitiveText($('#codexDeviceCode'), attempt.userCode || (terminal ? '—' : '等待官方 CLI…'));
    $('#copyCodexCode').disabled = !attempt.userCode || terminal;
    $('#openCodexOfficialPage').dataset.url = officialURL;
    $('#openCodexOfficialPage').disabled = !officialURL || terminal;
  } else if (!terminal) {
    const frame = $('#claudeLoginTerminal');
    if (!frame.src && attempt.id) frame.src = terminalPath(attempt.accountID, attempt.id);
    frame.hidden = false;
    $('#claudeTerminalPlaceholder').classList.add('hidden');
  }
  if (terminal) {
    clearSensitiveAttemptUI(provider);
    clearTimeout(authPollTimer);
    renderAccounts();
    void refreshNativeAuthStatuses();
  }
}
function updateAuthCountdown() {
  const active = state.activeAuth;
  if (!active) return;
  const remaining = accountAuth.secondsRemaining(active.attempt?.expiresAt);
  $(`#${active.provider}LoginCountdown`).textContent = accountAuth.formatCountdown(remaining);
}
function scheduleAuthPoll(delay) {
  clearTimeout(authPollTimer);
  const active = state.activeAuth;
  if (!active || !accountAuth.shouldPoll({dialogOpen:currentAuthDialog()?.open, documentHidden:document.hidden, state:active.attempt?.state})) return;
  authPollTimer = setTimeout(pollActiveAuthAttempt, delay);
}
async function pollActiveAuthAttempt() {
  const active = state.activeAuth;
  if (!active || document.hidden || !currentAuthDialog()?.open || !accountAuth.isActiveAttempt(active.attempt?.state)) return;
  try {
    const payload = await api(`/api/accounts/${active.accountID}/login-attempts/${active.attemptID}`, {timeout:15000});
    if (state.activeAuth !== active) return;
    const attempt = accountAuth.normalizeAttempt(payload, active);
    renderAuthAttempt(attempt);
    scheduleAuthPoll(attempt.pollAfterMs);
  } catch (error) {
    if (state.activeAuth !== active) return;
    const errorElement = $(`#${active.provider}LoginError`);
    errorElement.textContent = error.message;
    errorElement.classList.remove('hidden');
    scheduleAuthPoll(3000);
  }
}
function openAuthDialog(account, attempt) {
  clearAuthTimers();
  const normalized = accountAuth.normalizeAttempt(attempt, {accountID:account.id, provider:account.agent});
  if (!normalized.id) throw new Error('登录服务没有返回 attempt_id');
  state.activeAuth = {accountID:account.id, attemptID:normalized.id, provider:account.agent, account, attempt:normalized};
  const providerName = account.agent === 'codex' ? 'Codex' : 'Claude';
  $(`#${account.agent}LoginAccountName`).textContent = account.name;
  $(`#${account.agent}LoginError`).textContent = '';
  $(`#${account.agent}LoginError`).classList.add('hidden');
  updateControlAuthWarnings();
  renderAuthAttempt(normalized);
  const dialog = account.agent === 'codex' ? codexLoginDialog : claudeLoginDialog;
  if (!dialog.open) dialog.showModal();
  renderAuthAttempt(normalized);
  updateAuthCountdown();
  authCountdownTimer = setInterval(updateAuthCountdown, 1000);
  scheduleAuthPoll(normalized.pollAfterMs);
}
async function startAccountLogin(accountID) {
  const account = accountFor(accountID);
  if (!nativeLoginAccount(account)) return showToast('只有 Codex / Claude 原生登录账号可以使用官方登录');
  try {
    const payload = await api(`/api/accounts/${account.id}/login-attempts`, authAPIOptions('POST', {}));
    const attempt = accountAuth.normalizeAttempt(payload, {accountID:account.id, provider:account.agent});
    state.accountAuth.set(account.id, {status:'login_in_progress', attemptID:attempt.id, attemptState:attempt.state, message:attempt.message});
    renderAccounts();
    openAuthDialog(account, attempt);
  } catch (error) { showToast(error.message); }
}
async function continueOrStartLogin(account) {
  const record = accountAuthRecord(account.id);
  const activeAttempt = record.attempt || null;
  const attemptID = record.attemptID || activeAttempt?.id || '';
  if (attemptID && (record.status === 'login_in_progress' || accountAuth.isActiveAttempt(record.attemptState || activeAttempt?.state))) {
    try {
      const payload = activeAttempt || await api(`/api/accounts/${account.id}/login-attempts/${attemptID}`, {timeout:15000});
      return openAuthDialog(account, accountAuth.normalizeAttempt(payload, {id:attemptID, accountID:account.id, provider:account.agent}));
    } catch (error) {
      if (error.status !== 404 && error.status !== 410) return showToast(error.message);
    }
  }
  return startAccountLogin(account.id);
}
async function cancelAccountLogin(accountID, attemptID = '') {
  const account = accountFor(accountID);
  const record = accountAuthRecord(accountID);
  const id = attemptID || record.attemptID || record.attempt?.id;
  if (!account || !id) return;
  try {
    await api(`/api/accounts/${accountID}/login-attempts/${id}`, authAPIOptions('DELETE', {}));
    state.accountAuth.set(accountID, {status:'unauthenticated', attemptID:'', attemptState:'cancelled', message:'登录已取消'});
    if (state.activeAuth?.attemptID === id) renderAuthAttempt(accountAuth.normalizeAttempt({id, account_id:accountID, provider:account.agent, state:'cancelled', message:'登录已取消'}));
    renderAccounts();
  } catch (error) { showToast(error.message); }
}
async function logoutAccount(accountID) {
  const account = accountFor(accountID);
  if (!nativeLoginAccount(account) || !confirm(`退出“${account.name}”的官方 CLI 登录？这会删除 VibeDock 中的本地登录凭据；Provider 远端会话是否已撤销取决于官方 CLI。`)) return;
  try {
    const result = await api(`/api/accounts/${accountID}/logout`, authAPIOptions('POST', {}));
    state.accountAuth.set(accountID, {status:'unauthenticated', attemptID:'', attemptState:'', message:result?.provider_logout === 'unconfirmed' ? '本地凭据已删除；远端撤销未确认' : '已退出本地官方 CLI 登录'});
    renderAccounts();
    showToast(result?.provider_logout === 'unconfirmed' ? '本地凭据已删除；Provider 远端撤销未确认' : '已退出登录');
  } catch (error) { showToast(error.message); }
}
function cleanupClosedAuthDialog(provider) {
  clearAuthTimers();
  clearSensitiveAttemptUI(provider);
  if (state.activeAuth?.provider === provider) state.activeAuth = null;
}
function closeAuthDialog(provider) {
  const dialog = provider === 'claude' ? claudeLoginDialog : codexLoginDialog;
  if (dialog.open) dialog.close();
  cleanupClosedAuthDialog(provider);
}
async function retryActiveAuth(provider) {
  const accountID = state.activeAuth?.provider === provider ? state.activeAuth.accountID : '';
  if (!accountID) return;
  closeAuthDialog(provider);
  await startAccountLogin(accountID);
}

/* Settings */
function showSettings() {
  const ws = state.currentWorkspace; const template = templateFor(ws);
  $('#settingsContent').innerHTML = `<div class="dialog-header"><div><div class="eyebrow">WORKSPACE SETTINGS</div><h3>${escapeHTML(ws.name)}</h3><p>管理运行状态、访问凭据与持久化数据。</p></div><button class="icon-button" data-close-settings>×</button></div>
    <div class="settings-grid"><div class="settings-row"><span>Template</span><b>${escapeHTML(template.name)}</b></div><div class="settings-row"><span>Image</span><code>${escapeHTML(ws.image)}</code></div><div class="settings-row"><span>Container</span><code>${escapeHTML(ws.container_name)}</code></div><div class="settings-row"><span>Resources</span><b>${ws.cpus} vCPU · ${ws.memory_mb} MB</b></div><div class="settings-row"><span>Persistent data</span><b>/workspace + /home/coder</b></div></div>
    <div class="password-box"><b>✓ 已由 VibeDock 控制面授权</b><span>VS Code 无需再次输入 Workspace 密码。</span></div>
    <div class="dialog-actions"><button class="button danger" data-delete-workspace>删除环境与数据</button><span class="dialog-spacer"></span><button class="button ghost" data-close-settings>关闭</button></div>`;
  $$('[data-close-settings]', settingsDialog).forEach(button => button.onclick = () => settingsDialog.close());
  $('[data-delete-workspace]', settingsDialog).onclick = deleteWorkspace;
  settingsDialog.showModal();
}
async function deleteWorkspace() {
  const ws = state.currentWorkspace;
  if (!ws || !confirm(`确定删除“${ws.name}”？项目卷、Home 卷和全部会话都会被删除。`)) return;
  try { await api(`/api/workspaces/${ws.id}?purge=true`, {method:'DELETE'}); localStorage.removeItem(layoutKey()); settingsDialog.close(); showToast('Workspace 已删除'); await refreshAll({quiet:true}); showDashboard(true); } catch (error) { showToast(error.message); }
}

/* Command palette */
const commands = [
  {id:'session', icon:'◈', title:'新建 Agent Session', detail:'Codex、Claude、Pi、OpenCode 或 Shell'},
  {id:'code', icon:'⌘', title:'打开 VS Code', detail:'在当前面板内嵌编辑器'},
  {id:'preview', icon:'◎', title:'打开 Preview', detail:'嵌入容器内开发服务'},
  {id:'split', icon:'◫', title:'切换分屏', detail:'并行显示两个工具'},
  {id:'reload', icon:'↻', title:'重新加载当前工具', detail:'刷新当前 iframe'},
  {id:'transcript', icon:'≡', title:'查看 Agent 最近输出', detail:'读取当前 Session 的 tmux 输出'},
  {id:'restart', icon:'⟳', title:'重启当前 Agent', detail:'重新启动当前 Session'},
  {id:'upload', icon:'⇧', title:'导入文件', detail:'上传 ZIP 或文件到 /workspace'},
  {id:'accounts', icon:'♙', title:'账号与 API', detail:'管理 Codex / Claude 凭据'},
  {id:'settings', icon:'⚙', title:'Workspace 设置', detail:'环境信息、授权与删除'},
];
function ensureCommandDialog() {
  if ($('#commandDialog')) return;
  const dialog = document.createElement('dialog');
  dialog.id = 'commandDialog'; dialog.className = 'modal command-modal';
  dialog.innerHTML = `<div class="command-shell"><input id="commandInput" placeholder="输入命令、工具或 Session 名称…" autocomplete="off"><div id="commandList" class="command-list"></div></div>`;
  document.body.append(dialog);
  $('#commandInput').addEventListener('input', renderCommands);
  $('#commandInput').addEventListener('keydown', event => {
    const items = $$('.command-item', $('#commandList'));
    const active = items.findIndex(item => item.classList.contains('active'));
    if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && items.length) {
      event.preventDefault();
      const next = (active + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
      items.forEach((item, index) => item.classList.toggle('active', index === next));
      items[next]?.scrollIntoView({block:'nearest'});
      return;
    }
    if (event.key === 'Enter') items[active >= 0 ? active : 0]?.click();
  });
  $('#commandList').addEventListener('click', event => { const item = event.target.closest('[data-command]'); if (item) runCommand(item.dataset.command); });
}
function openCommandPalette() { ensureCommandDialog(); $('#commandInput').value = ''; renderCommands(); $('#commandDialog').showModal(); setTimeout(() => $('#commandInput').focus(), 0); }
function renderCommands() {
  const query = ($('#commandInput')?.value || '').toLowerCase();
  const sessionCommands = (state.currentWorkspace?.sessions || []).map(session => ({id:`open-session:${session.id}`, icon:agentGlyph(session.agent), title:session.name, detail:`打开 ${agentLabel(session.agent)} Session`}));
  const items = [...commands, ...sessionCommands].filter(item => `${item.title} ${item.detail}`.toLowerCase().includes(query));
  $('#commandList').innerHTML = items.map((item,index) => `<button class="command-item ${index === 0 ? 'active' : ''}" data-command="${escapeHTML(item.id)}"><span>${escapeHTML(item.icon)}</span><div><b>${escapeHTML(item.title)}</b><small>${escapeHTML(item.detail)}</small></div></button>`).join('');
}
function runCommand(id) {
  $('#commandDialog')?.close();
  if (id.startsWith('open-session:')) return openTab('session', id.slice(13));
  if (id === 'session') openSessionDialog();
  if (id === 'code') openTab('code');
  if (id === 'preview') previewDialog.showModal();
  if (id === 'split') toggleSplit();
  if (id === 'reload') reloadFocusedPane();
  if (id === 'transcript') showTranscript();
  if (id === 'restart') restartFocusedSession();
  if (id === 'upload') uploadDialog.showModal();
  if (id === 'accounts') openAccountManager();
  if (id === 'settings') showSettings();
}

/* Forms */
$('#createForm').addEventListener('submit', async event => {
  event.preventDefault();
  const button = $('#createButton'); const errorBox = $('#createError'); button.disabled = true; button.textContent = '正在创建…'; errorBox.classList.add('hidden');
  try {
    const form = new FormData(event.currentTarget); const direct = collectDirectConfig('create', $('#createAgentInput').value);
    if (direct) { form.set('direct_provider', direct.provider); form.set('direct_base_url', direct.base_url); form.set('direct_model', direct.model); form.set('direct_protocol', direct.protocol); form.set('direct_thinking', direct.thinking); form.set('direct_permission_policy', direct.permission_policy); form.set('direct_auto_approve', String(direct.auto_approve)); form.set('direct_secret', direct.secret); }
    form.delete('files'); for (const file of $('#fileInput').files) form.append('files', file, file.webkitRelativePath || file.name);
    const ws = await api('/api/workspaces', {method:'POST', body:form, timeout:900000});
    createDialog.close();
    if (ws?.error || ws?.sessions?.some(session => session.status === 'error' || session.error)) showToast(`Workspace 已创建，但部分组件未就绪：${ws.error || ws.sessions.find(session => session.error)?.error || '请检查 Session 配置'}`);
    else showToast('Workspace 创建成功');
    await refreshAll({quiet:true});
    await openWorkspace(ws.id);
  } catch (error) { errorBox.textContent = error.message; errorBox.classList.remove('hidden'); }
  finally { button.disabled = false; button.textContent = '创建并启动'; }
});
$('#uploadForm').addEventListener('submit', async event => {
  event.preventDefault();
  const input = $('#workspaceFileInput');
  if (!input.files.length) return;
  const files = [...input.files];
  const button = $('#uploadButton');
  const errorBox = $('#uploadError');
  button.disabled = true;
  errorBox.classList.add('hidden');
  const imported = await importFiles(files, {errorBox});
  button.disabled = false;
  if (imported) {
    uploadDialog.close();
    input.value = '';
  }
});
$('#previewForm').addEventListener('submit', event => { event.preventDefault(); const port = Number($('#previewPortInput').value); if (!Number.isInteger(port) || port < 1 || port > 65535) return; previewDialog.close(); openTab('port', port); });
$('#sessionDangerousSkip').onchange = () => { $('#sessionDangerousConfirmRow').classList.toggle('hidden', !$('#sessionDangerousSkip').checked); scheduleLaunchPreview(); };
$('#sessionForm').addEventListener('submit', saveSession);
$('#accountForm').addEventListener('submit', saveAccount);
$('#environmentProfileForm').addEventListener('submit', saveEnvironmentProfile);
$('#saveAccountConfig').onclick = () => void saveAccountConfig();
$('#reloadAccountConfig').onclick = () => void loadAccountConfigFile();
$('#resetAccountConfig').onclick = () => void resetAccountConfig();

/* Event wiring */
const openCreateButton = $('#openCreate');
if (openCreateButton) openCreateButton.onclick = () => openCreate();
$('#quickImportButton').onclick = () => { openCreate(); $('#fileInput').click(); };
$('#homeImportButton').onclick = () => { openCreate(); $('#fileInput').click(); };
$('#openAccounts').onclick = openAccountManager;
$('#openEnvironmentProfiles').onclick = () => openEnvironmentProfiles();
$('#createManageEnvironments').onclick = () => openEnvironmentProfiles(state.selectedEnvironmentProfile || '');
$('#newEnvironmentProfile').onclick = resetEnvironmentProfileEditor;
$('#deleteEnvironmentProfile').onclick = deleteEnvironmentProfile;
$('#environmentProfileList').addEventListener('click', event => {
  const item = event.target.closest('[data-environment-id]');
  if (item) editEnvironmentProfile(item.dataset.environmentId);
});
$$('[data-open-environments]').forEach(button => button.onclick = () => openEnvironmentProfiles());
$$('[data-open-accounts]').forEach(button => button.onclick = openAccountManager);
$$('[data-theme-toggle]').forEach(button => button.onclick = toggleTheme);
$$('[data-home-filter]').forEach(button => button.onclick = () => {
  state.filter = button.dataset.homeFilter;
  $$('[data-filter]').forEach(item => item.classList.toggle('active', item.dataset.filter === state.filter));
  renderDashboard();
  $('.recent-heading')?.scrollIntoView({behavior:'smooth', block:'start'});
});
$('#recentSessionList').addEventListener('click', async event => {
  const item = event.target.closest('[data-recent-workspace]');
  if (!item) return;
  await openWorkspace(item.dataset.recentWorkspace);
  openTab('session', item.dataset.recentSession);
});
$('#refreshButton').onclick = () => refreshAll();
$('#manageWorkspacesButton').onclick = () => setManagementMode(!state.managementMode);
$('#exitManageButton').onclick = () => setManagementMode(false);
$('#workspaceSelectAll').onchange = event => {
  for (const ws of visibleWorkspaces()) {
    if (event.target.checked) state.selectedWorkspaceIDs.add(ws.id); else state.selectedWorkspaceIDs.delete(ws.id);
  }
  renderDashboard();
};
$('#batchStartButton').onclick = () => requestBatchAction('start');
$('#batchStopButton').onclick = () => requestBatchAction('stop');
$('#batchDeleteButton').onclick = () => requestBatchAction('delete');
$('#batchDeleteForm').addEventListener('submit', event => { event.preventDefault(); $('#batchDeleteDialog').close(); executeBatchAction('delete', selectedTargets('delete')); });
$('#newSessionButton').onclick = () => openSessionDialog();
$('#workspacePowerButton').onclick = workspacePower;
$('#workspaceSettingsButton').onclick = showSettings;
$('#uploadFilesButton').onclick = () => uploadDialog.showModal();
$('#previewButton').onclick = () => previewDialog.showModal();
$('#commandButton').onclick = openCommandPalette;
$('#reloadPaneButton').onclick = reloadFocusedPane;
$('#openExternalButton').onclick = openFocusedExternal;
$('#sendToOtherPaneButton').onclick = sendCurrentToOtherPane;
$('#splitButton').onclick = toggleSplit;
$('#fullscreenButton').onclick = toggleImmersive;
$('#deleteSessionButton').onclick = deleteCurrentSession;
$('#newAccountButton').onclick = () => { pendingAccountSelection = null; openAccountEditor(); };
$('#emptyAddAccount').onclick = () => { pendingAccountSelection = null; openAccountEditor(); };
$('#loginCodexTop').onclick = () => startTopLevelLogin('codex');
$('#loginClaudeTop').onclick = () => startTopLevelLogin('claude');
$('#accountAuthPicker').addEventListener('click', event => { const option = event.target.closest('[data-auth-method]'); if (option) selectAuthMethod(option.dataset.authMethod); });
$('#closeCodexLogin').onclick = () => closeAuthDialog('codex');
$('#doneCodexLogin').onclick = () => closeAuthDialog('codex');
$('#closeClaudeLogin').onclick = () => closeAuthDialog('claude');
$('#doneClaudeLogin').onclick = () => closeAuthDialog('claude');
codexLoginDialog.addEventListener('close', () => cleanupClosedAuthDialog('codex'));
claudeLoginDialog.addEventListener('close', () => cleanupClosedAuthDialog('claude'));
$('#cancelCodexLogin').onclick = () => state.activeAuth?.provider === 'codex' && cancelAccountLogin(state.activeAuth.accountID, state.activeAuth.attemptID);
$('#cancelClaudeLogin').onclick = () => state.activeAuth?.provider === 'claude' && cancelAccountLogin(state.activeAuth.accountID, state.activeAuth.attemptID);
$('#retryCodexLogin').onclick = () => retryActiveAuth('codex');
$('#retryClaudeLogin').onclick = () => retryActiveAuth('claude');
$('#copyCodexCode').onclick = async () => {
  const code = state.activeAuth?.provider === 'codex' ? state.activeAuth.attempt?.userCode : '';
  if (!code) return;
  try { await navigator.clipboard.writeText(code); showToast('设备代码已复制'); }
  catch (_) { showToast('无法自动复制，请选中代码后手动复制'); }
};
$('#openCodexOfficialPage').onclick = event => {
  const url = accountAuth.safeOfficialURL(event.currentTarget.dataset.url);
  if (url) window.open(url, '_blank', 'noopener,noreferrer');
};
$('#createAddAccountButton').onclick = () => openAccountEditor(null, {returnTo:'create', agent:$('#createAgentInput').value});
$('#sessionAddAccountButton').onclick = () => openAccountEditor(null, {returnTo:'session', agent:$('#sessionAgentInput').value});
$('#accountAgentInput').onchange = () => populateAuthOptions($('#accountAgentInput').value, $('#accountAuthInput').value);
$('#accountAuthInput').onchange = () => selectAuthMethod($('#accountAuthInput').value);
$('#createAgentInput').onchange = () => { const agent = $('#createAgentInput').value; renderAccountOptions(agent, $('#createAccountInput')); updateDirectFields('create', agent); };
$('#sessionAgentInput').onchange = () => { const agent = $('#sessionAgentInput').value; renderAccountOptions(agent, $('#sessionAccountInput')); updateDirectFields('session', agent); applyLaunchProfile(agent); scheduleLaunchPreview(); };
$('#sessionAccountInput').onchange = () => { renderSessionLoginGuide($('#sessionAgentInput').value, $('#sessionAccountInput').value); scheduleLaunchPreview(); };
$('#sessionConfigMode').onchange = event => {
  const previous = event.currentTarget.dataset.previous || 'managed';
  if (previous === 'custom' && event.currentTarget.value === 'managed' && !confirm('切换到 Managed 后，下次启动会用表单重新生成配置文件。确定继续？')) {
    event.currentTarget.value = 'custom';
    return;
  }
  event.currentTarget.dataset.previous = event.currentTarget.value;
  updateLaunchProfileFields($('#sessionAgentInput').value);
  scheduleLaunchPreview();
};
$('#refreshLaunchPreview').onclick = () => void refreshLaunchPreview();
for (const id of ['sessionPermissionMode','sessionDirectProvider','sessionDirectModel','sessionDirectBaseURL','sessionDirectProtocol','sessionDirectThinking','sessionDirectAutoApprove','sessionLaunchModel','sessionCodexApproval','sessionCodexSandbox','sessionCodexWebSearch','sessionClaudeContinue','sessionOpenCodeContinue','sessionCodexOverrides','sessionExtraArgs','sessionCustomConfig']) {
  const element = $(`#${id}`);
  if (!element) continue;
  element.addEventListener(element.matches('select,input[type="checkbox"]') ? 'change' : 'input', scheduleLaunchPreview);
}

$$('[data-create]').forEach(button => button.onclick = () => openCreate());
$('#emptyStateAction').onclick = event => {
  if (event.currentTarget.dataset.emptyAction === 'clear-filter') {
    state.filter = 'all';
    state.selectedWorkspaceIDs.clear();
    $$('[data-filter]').forEach(item => item.classList.toggle('active', item.dataset.filter === 'all'));
    renderDashboard();
    return;
  }
  openCreate();
};
$$('[data-home]').forEach(button => button.onclick = () => showDashboard(true));
$$('[data-close]').forEach(button => button.onclick = () => document.getElementById(button.dataset.close).close());
$$('[data-filter]').forEach(button => button.onclick = () => { state.filter = button.dataset.filter; state.selectedWorkspaceIDs.clear(); $$('[data-filter]').forEach(item => item.classList.toggle('active', item === button)); renderDashboard(); });
$$('[data-new-session]').forEach(button => button.onclick = () => openSessionDialog());

$('#workspaceGrid').addEventListener('change', event => {
  const checkbox = event.target.closest('[data-workspace-select]');
  if (checkbox) toggleWorkspaceSelection(checkbox.dataset.workspaceSelect, checkbox.checked);
});
$('#workspaceGrid').addEventListener('click', event => {
  if (event.target.closest('[data-workspace-select]')) return event.stopPropagation();
  const action = event.target.closest('[data-card-action]');
  if (action) { event.stopPropagation(); return dashboardAction(action.dataset.id, action.dataset.cardAction); }
  const card = event.target.closest('[data-workspace-card]');
  if (!card) return;
  if (state.managementMode) return toggleWorkspaceSelection(card.dataset.workspaceCard);
  openWorkspace(card.dataset.workspaceCard);
});
$('#workspaceGrid').addEventListener('keydown', event => {
  const card = event.target.closest('[data-workspace-card]');
  if (!card || event.target.closest('button, input, select, textarea')) return;
  if (event.key !== 'Enter' && event.key !== ' ') return;
  event.preventDefault();
  if (state.managementMode) toggleWorkspaceSelection(card.dataset.workspaceCard);
  else openWorkspace(card.dataset.workspaceCard);
});
$('#sessionList').addEventListener('keydown', event => {
  const item = event.target.closest('[data-session-id]');
  if (!item || event.target.closest('button, input, select, textarea')) return;
  if (event.key !== 'Enter' && event.key !== ' ') return;
  event.preventDefault();
  openTab('session', item.dataset.sessionId);
});
$('.activity-bar').addEventListener('click', event => {
  const item = event.target.closest('[data-activity]');
  if (!item) return;
  const action = item.dataset.activity;
  if (action === 'sessions') {
    const collapsed = $('#workspaceView').classList.toggle('sidebar-collapsed');
    state.sidebarVisible = !collapsed;
    $('#workspaceSidebar').classList.toggle('mobile-hidden', collapsed);
    $('[data-activity="sessions"]').classList.toggle('active', !collapsed);
    scheduleViewLayout();
    return;
  }
  if (action === 'code') openTab('code');
  if (action === 'preview') previewDialog.showModal();
  if (action === 'terminal') openSessionDialog(null, 'shell');
  if (action === 'files') uploadDialog.showModal();
  if (action === 'accounts') openAccountManager();
  if (action === 'settings') showSettings();
});
$('#sessionList').addEventListener('click', event => {
  const settings = event.target.closest('[data-session-settings]'); if (settings) { event.stopPropagation(); return openSessionDialog(settings.dataset.sessionSettings); }
  const item = event.target.closest('[data-session-id]'); if (item) openTab('session', item.dataset.sessionId);
});
$('#tabStrip').addEventListener('keydown', event => {
  const tabs = $$('.tool-tab', $('#tabStrip'));
  const current = event.target.closest('[role="tab"]');
  if (!current || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
  event.preventDefault();
  const index = tabs.indexOf(current);
  const nextIndex = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
  tabs[nextIndex]?.focus();
  const tabID = tabs[nextIndex]?.dataset.tabId;
  if (tabID) openTab(tabID.startsWith('session:') ? 'session' : tabID.startsWith('port:') ? 'port' : tabID, tabID.includes(':') ? tabID.split(':')[1] : null);
});
$('#tabStrip').addEventListener('click', event => {
  const close = event.target.closest('[data-close-tab]'); if (close) { event.stopPropagation(); return closeTab(close.dataset.closeTab); }
  const tab = event.target.closest('[data-tab-id]'); if (tab) openTab(tab.dataset.tabId.startsWith('session:') ? 'session' : tab.dataset.tabId.startsWith('port:') ? 'port' : tab.dataset.tabId, tab.dataset.tabId.includes(':') ? tab.dataset.tabId.split(':')[1] : null, {pane:event.shiftKey && state.split ? (state.focusedPane === 'primary' ? 'secondary' : 'primary') : state.focusedPane});
});
$$('.work-pane').forEach(pane => pane.addEventListener('pointerdown', () => { state.focusedPane = pane.dataset.pane; renderWorkbench(); persistLayout(); }));
$('#accountList').addEventListener('click', event => {
  const action = event.target.closest('[data-account-action]');
  if (!action) return;
  const account = accountFor(action.dataset.id);
  if (action.dataset.accountAction === 'config') void openAccountConfig(action.dataset.id);
  if (action.dataset.accountAction === 'edit') openAccountEditor(action.dataset.id);
  if (action.dataset.accountAction === 'probe') probeAccount(action.dataset.id);
  if (action.dataset.accountAction === 'delete') deleteAccount(action.dataset.id);
  if (action.dataset.accountAction === 'login') startAccountLogin(action.dataset.id);
  if (action.dataset.accountAction === 'continue-login' && account) continueOrStartLogin(account);
  if (action.dataset.accountAction === 'cancel-login') cancelAccountLogin(action.dataset.id);
  if (action.dataset.accountAction === 'logout') logoutAccount(action.dataset.id);
});
$('#accountConfigTree').addEventListener('click', event => {
  const button = event.target.closest('[data-config-path]');
  if (!button) return;
  $$('.config-tree button').forEach(item => item.classList.toggle('selected', item === button));
  void loadAccountConfigFile(button.dataset.configPath);
});

let resizingSplit = false;
$('#paneDivider').addEventListener('pointerdown', event => {
  resizingSplit = true;
  $('#paneDivider').setPointerCapture?.(event.pointerId);
  event.preventDefault();
});
window.addEventListener('pointermove', event => {
  if (!resizingSplit || !state.split) return;
  const rect = $('#paneGrid').getBoundingClientRect();
  const ratio = window.innerWidth <= 720
    ? (event.clientY - rect.top) / rect.height
    : (event.clientX - rect.left) / rect.width;
  state.splitRatio = Math.min(.75, Math.max(.25, ratio));
  const splitPercent = Math.round(state.splitRatio * 100);
  $('#paneGrid').style.setProperty('--split-left', `${splitPercent}%`);
  $('#paneGrid').style.setProperty('--split-top', `${splitPercent}%`);
  $('#paneDivider').setAttribute('aria-valuenow', String(splitPercent));
  scheduleViewLayout();
});
$('#paneDivider').addEventListener('keydown', event => {
  if (!state.split || !['ArrowLeft','ArrowRight','ArrowUp','ArrowDown'].includes(event.key)) return;
  event.preventDefault();
  const increase = event.key === 'ArrowRight' || event.key === 'ArrowDown';
  state.splitRatio = Math.min(.75, Math.max(.25, state.splitRatio + (increase ? .05 : -.05)));
  renderWorkbench();
  persistLayout();
});
window.addEventListener('pointerup', () => {
  if (!resizingSplit) return;
  resizingSplit = false;
  persistLayout();
});

window.addEventListener('keydown', event => {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); if (state.currentWorkspace) openCommandPalette(); }
  if ((event.metaKey || event.ctrlKey) && event.key === '\\') { event.preventDefault(); if (state.currentWorkspace) toggleSplit(); }
  if (event.key === 'Escape' && $('#workspaceView').classList.contains('immersive')) toggleImmersive();
});
window.addEventListener('popstate', () => { const id = workspaceRouteID(); if (id) openWorkspace(id, false); else showDashboard(false); });
window.addEventListener('resize', () => {
  if (!state.currentWorkspace) return;
  $('#paneDivider').setAttribute('aria-orientation', window.innerWidth <= 720 ? 'horizontal' : 'vertical');
  scheduleViewLayout();
});

/* Workbench-wide drag import */
document.addEventListener('dragenter', event => {
  if (!event.dataTransfer?.types?.includes('Files')) return;
  event.preventDefault(); state.dragDepth++;
  if (state.currentWorkspace) $('#dropOverlay').classList.remove('hidden');
});
document.addEventListener('dragover', event => { if (event.dataTransfer?.types?.includes('Files')) event.preventDefault(); });
document.addEventListener('dragleave', event => { if (!event.dataTransfer?.types?.includes('Files')) return; state.dragDepth = Math.max(0, state.dragDepth - 1); if (!state.dragDepth) $('#dropOverlay').classList.add('hidden'); });
document.addEventListener('drop', event => {
  if (event.target.closest?.('.dropzone')) return;
  if (!event.dataTransfer?.files?.length) return;
  event.preventDefault(); state.dragDepth = 0; $('#dropOverlay').classList.add('hidden');
  if (state.currentWorkspace) importFiles(event.dataTransfer.files); else openCreate(event.dataTransfer.files);
});

bindDropzone($('#dropzone'), $('#fileInput'), $('#fileHint'));
bindDropzone($('#uploadDialog .dropzone'), $('#workspaceFileInput'), $('#workspaceFileHint'));

async function boot() {
  applyTheme(document.documentElement.dataset.theme, false);
  ensureCommandDialog();
  const healthBadge = $('#healthBadge');
  try {
    const health = await api('/api/health');
    const healthy = health.ok === true;
    state.controlAuthEnabled = health.control_plane_auth?.enabled !== false;
    updateControlAuthWarnings();
    healthBadge.classList.toggle('error', !healthy);
    healthBadge.lastChild.textContent = healthy ? ' 控制面正常' : ' 控制面异常';
    healthBadge.title = healthy ? 'Control plane 正常' : 'Control plane 异常';
  } catch (_) {
    healthBadge.classList.add('error');
    healthBadge.lastChild.textContent = ' 控制面不可用';
    healthBadge.title = '无法连接 Control plane';
  }
  await refreshAll({quiet:false});
  const id = workspaceRouteID();
  if (id) await openWorkspace(id, false); else showDashboard(false);
  setInterval(() => { if (!document.hidden) refreshAll({quiet:true}); }, 15000);
}
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    clearTimeout(authPollTimer);
    return;
  }
  refreshAll({quiet:true});
  if (state.activeAuth && currentAuthDialog()?.open && accountAuth.isActiveAttempt(state.activeAuth.attempt?.state)) void pollActiveAuthAttempt();
});

boot();
