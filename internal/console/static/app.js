/* Fluxgate 管理台 — 原生 ES2020 实现，不依赖任何外部库。
 * 所有网关数据都通过 textContent/DOM API 渲染，因此上游配置值永远不会被
 * 当作标记解析。 */

(function () {
  'use strict';

  var REFRESH_MS = 10000;

  /* The console may be mounted under a path prefix by a reverse proxy (for
   * example /gateway/console/), so management calls are resolved against the
   * directory the page was actually served from instead of the server root.
   * The last /console/ marker wins so a prefix that itself contains the
   * segment still resolves correctly. */
  var API_BASE = (function () {
    var path = window.location.pathname || '/console/';
    var marker = path.lastIndexOf('/console/');
    if (marker === -1) {
      return path.replace(/[^/]*$/, '');
    }
    return path.slice(0, marker + 1);
  })();

  function apiPath(suffix) {
    return API_BASE + suffix.replace(/^\//, '');
  }

  var state = {
    snapshot: null,
    username: '',
    channelNames: {},
    timer: null,
    countdown: 0,
    loading: false
  };

  var els = {};

  function $(id) { return document.getElementById(id); }

  function cacheElements() {
    els.authOverlay = $('auth-overlay');
    els.authForm = $('auth-form');
    els.authUsername = $('auth-username');
    els.authPassword = $('auth-password');
    els.authError = $('auth-error');
    els.authSubmit = $('auth-submit');
    els.app = $('app');

    els.status = $('topbar-status');
    els.statusLabel = els.status.querySelector('[data-status-label]');
    els.uptime = $('topbar-uptime');
    els.refresh = $('refresh-button');
    els.signout = $('signout-button');

    // Settings view and the password forms. Its container is the app, because
    // the settings page is a second view inside the signed-in shell rather than
    // a separate page load.
    els.dashboardView = document.querySelector('.container:not(#settings-view)');
    els.settingsButton = $('settings-button');
    els.settingsView = $('settings-view');
    els.settingsBack = $('settings-back');
    els.settingsAccount = $('settings-account');
    els.passwordForm = $('password-form');
    els.currentPassword = $('current-password');
    els.newPassword = $('new-password');
    els.confirmPassword = $('confirm-password');
    els.passwordFeedback = $('password-feedback');
    els.passwordSubmit = $('password-submit');
    els.passwordCancel = $('password-cancel');

    els.passwordRequired = $('password-required');
    els.requiredForm = $('required-password-form');
    els.requiredCurrent = $('required-current-password');
    els.requiredNew = $('required-new-password');
    els.requiredConfirm = $('required-confirm-password');
    els.requiredFeedback = $('required-password-feedback');
    els.requiredSubmit = $('required-password-submit');
    els.requiredSignout = $('required-signout');

    els.banner = $('error-banner');
    els.bannerTitle = els.banner.querySelector('[data-banner-title]');
    els.bannerDetail = els.banner.querySelector('[data-banner-detail]');
    els.bannerRetry = $('banner-retry');

    els.stats = {
      service: document.querySelector('[data-stat="service"]'),
      models: document.querySelector('[data-stat="models"]'),
      channels: document.querySelector('[data-stat="channels"]'),
      breakers: document.querySelector('[data-stat="breakers"]')
    };

    els.breakersBody = document.querySelector('[data-breakers-body]');
    els.breakersEmpty = document.querySelector('[data-breakers-empty]');
    els.breakersMeta = document.querySelector('[data-panel="breakers"] [data-meta]');

    els.channelsBody = document.querySelector('[data-channels-body]');
    els.channelsEmpty = document.querySelector('[data-channels-empty]');
    els.channelsMeta = document.querySelector('[data-panel="channels"] [data-meta]');

    els.modelsCloud = document.querySelector('[data-models-cloud]');
    els.modelsEmpty = document.querySelector('[data-models-empty]');
    els.modelsEmptyText = document.querySelector('[data-models-empty-text]');
    els.modelsMeta = document.querySelector('[data-panel="models"] [data-meta]');
    els.modelFilter = $('model-filter');

    els.nextRefresh = document.querySelector('[data-next-refresh]');
    els.footerGenerated = document.querySelector('[data-footer-generated]');
    els.footerLoaded = document.querySelector('[data-footer-loaded]');
  }

  /* ===== Formatting helpers ===== */

  /* 上游配置里的枚举值按中文显示；未知值原样保留，这样新值不会因为管理台
   * 还没有对应的翻译就消失。 */
  var ENUM_LABELS = {
    routing_strategy: {
      weighted: '加权轮询',
      round_robin: '轮询',
      stable_first: '固定优先'
    },
    breaker_mode: {
      cooldown: '通道冷却',
      disable: '通道禁用',
      key_cooldown: '密钥冷却',
      key_model_cooldown: '密钥+模型冷却'
    },
    proxy_source: {
      direct: '直连',
      key: '密钥代理',
      system: '系统代理',
      site: '站点代理'
    },
    scope: {
      channel: '通道',
      key: '密钥',
      key_model: '密钥+模型'
    }
  };

  function enumLabel(kind, value, fallback) {
    var raw = text(value, fallback);
    var labels = ENUM_LABELS[kind];
    if (labels && labels[raw]) { return labels[raw]; }
    return raw;
  }

  function formatDuration(ms) {
    if (typeof ms !== 'number' || !isFinite(ms) || ms < 0) { return '—'; }
    var totalSeconds = Math.floor(ms / 1000);
    var days = Math.floor(totalSeconds / 86400);
    var hours = Math.floor((totalSeconds % 86400) / 3600);
    var minutes = Math.floor((totalSeconds % 3600) / 60);
    var seconds = totalSeconds % 60;
    if (days > 0) { return days + ' 天 ' + hours + ' 小时'; }
    if (hours > 0) { return hours + ' 小时 ' + minutes + ' 分'; }
    if (minutes > 0) { return minutes + ' 分 ' + seconds + ' 秒'; }
    return seconds + ' 秒';
  }

  function formatTimestamp(value) {
    if (!value) { return '—'; }
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) { return String(value); }
    return parsed.toLocaleString();
  }

  function formatCountdown(seconds) {
    if (seconds <= 0) { return '刷新中…'; }
    return seconds + ' 秒后';
  }

  function relativeUntil(value) {
    if (!value) { return ''; }
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) { return ''; }
    var delta = parsed.getTime() - Date.now();
    if (delta <= 0) { return '已过期'; }
    return '恢复还需 ' + formatDuration(delta);
  }

  function text(value, fallback) {
    if (value === null || value === undefined || value === '') {
      return fallback === undefined ? '—' : fallback;
    }
    return String(value);
  }

  /* ===== DOM builders ===== */

  function element(tag, className, content) {
    var node = document.createElement(tag);
    if (className) { node.className = className; }
    if (content !== undefined) { node.textContent = content; }
    return node;
  }

  function cell(row, content, className) {
    var td = document.createElement('td');
    if (className) { td.className = className; }
    if (content === null || content === undefined) {
      td.textContent = '—';
    } else if (typeof content === 'string') {
      td.textContent = content;
    } else {
      td.appendChild(content);
    }
    return row.appendChild(td);
  }

  function badge(label, variant) {
    return element('span', 'badge badge-' + variant, label);
  }

  /* ===== Session handling =====
   * The session lives in an HTTP-only cookie set by the gateway, so this script
   * never holds a credential it could leak. It asks the gateway who it is
   * instead of remembering, which also means a session revoked elsewhere
   * (a password reset, an expiry) is noticed on the next request. */

  function showAuth(message) {
    stopAutoRefresh();
    els.app.hidden = true;
    els.passwordRequired.hidden = true;
    els.authOverlay.hidden = false;
    els.authError.hidden = !message;
    els.authError.textContent = message || '';
    els.authSubmit.disabled = false;
    els.authSubmit.querySelector('.btn-label').textContent = '登录';
    els.authPassword.value = '';
    els.authUsername.focus();
  }

  function showConsole() {
    els.authOverlay.hidden = true;
    els.app.hidden = false;
    showDashboard();
  }

  /* The forced-change screen: the account still has the built-in password, so
   * the gateway serves no data. Only the password form and sign-out are usable. */
  function showPasswordRequired() {
    stopAutoRefresh();
    els.authOverlay.hidden = true;
    els.app.hidden = false;
    els.passwordRequired.hidden = false;
    resetPasswordForm(els.requiredForm, els.requiredCurrent, els.requiredNew,
      els.requiredConfirm, els.requiredFeedback);
    els.requiredCurrent.focus();
  }

  function showDashboard() {
    els.passwordRequired.hidden = true;
    els.settingsView.hidden = true;
    els.dashboardView.hidden = false;
    startAutoRefresh();
  }

  function showSettings() {
    stopAutoRefresh();
    els.passwordRequired.hidden = true;
    els.dashboardView.hidden = true;
    els.settingsView.hidden = false;
    els.settingsAccount.textContent = state.username
      ? '当前登录账号：' + state.username + '。'
      : '已登录。';
    resetPasswordForm(els.passwordForm, els.currentPassword, els.newPassword,
      els.confirmPassword, els.passwordFeedback);
    els.currentPassword.focus();
  }

  function signOut() {
    stopAutoRefresh();
    state.snapshot = null;
    els.authUsername.value = '';
    els.authPassword.value = '';
    post('/management/logout').catch(function () { /* the cookie is cleared server-side */ })
      .then(function () { showAuth(''); });
  }

  /* ===== Data access ===== */

  function request(path) {
    return fetch(apiPath(path), {
      method: 'GET',
      headers: { 'Accept': 'application/json' },
      cache: 'no-store',
      credentials: 'same-origin'
    }).then(readResponse);
  }

  function post(path, payload) {
    return fetch(apiPath(path), {
      method: 'POST',
      headers: payload
        ? { 'Content-Type': 'application/json', 'Accept': 'application/json' }
        : { 'Accept': 'application/json' },
      body: payload ? JSON.stringify(payload) : undefined,
      cache: 'no-store',
      credentials: 'same-origin'
    }).then(readResponse);
  }

  function readResponse(response) {
    return response.json().catch(function () { return null; }).then(function (body) {
      return { status: response.status, ok: response.ok, body: body };
    });
  }

  /* 网关返回的错误码到中文提示的映射。网关的 code 字段是自动化依赖的契约，
   * 所以这里只做显示层翻译，不改动接口本身；未知错误码仍回退到网关的原文，
   * 新错误码不会因此被吞掉。 */
  var CODE_MESSAGES = {
    // 登录与会话
    invalid_credentials: '用户名或密码错误。',
    missing_credentials: '请输入用户名和密码。',
    too_many_attempts: '失败次数过多，请稍后重试。',
    unauthorized: '登录状态已失效，请重新登录。',
    console_auth_not_configured: '本网关未配置管理台登录。',
    management_auth_not_configured: '本网关未配置管理接口认证。',
    authentication_failed: '网关在处理登录时出错。',
    password_change_required: '请先修改默认密码，之后才能读取数据。',
    // 修改密码
    password_unchanged: '新密码不能与当前密码相同。',
    password_empty: '新密码不能为空，否则将无法再次登录。',
    invalid_password: '密码不符合要求（最长 4096 字节，且必须是有效的 UTF-8 文本）。',
    password_change_failed: '新密码保存失败。',
    invalid_json: '请求格式不正确。'
  };

  function errorMessage(body, fallback) {
    if (body && body.error) {
      if (typeof body.error === 'string') { return body.error; }
      if (body.error.code && CODE_MESSAGES[body.error.code]) {
        return CODE_MESSAGES[body.error.code];
      }
      if (body.error.message) { return body.error.message; }
      if (body.error.code) { return body.error.code; }
    }
    return fallback;
  }

  function refresh() {
    if (state.loading) { return; }
    state.loading = true;
    els.refresh.classList.add('spinning');

    Promise.all([
      request('/management/status'),
      request('/management/snapshot')
    ]).then(function (results) {
      var status = results[0];
      var snapshot = results[1];

      if (status.status === 401 || snapshot.status === 401) {
        showAuth('登录状态已过期，请重新登录后继续。');
        return;
      }
      // The gateway refuses data to an account that still holds the default
      // password; show the forced-change screen rather than an error banner.
      if (status.status === 403 || snapshot.status === 403) {
        showPasswordRequired();
        return;
      }
      if (status.status === 503 || snapshot.status === 503) {
        hideBanner();
        renderUnavailable(errorMessage(status.body, '本网关未配置管理台登录。'));
        return;
      }
      if (!status.ok || !snapshot.ok) {
        showBanner('网关请求失败',
          errorMessage(!status.ok ? status.body : snapshot.body, '网关返回了预期之外的响应。'));
        return;
      }

      hideBanner();
      state.snapshot = snapshot.body || {};
      render(status.body || {}, state.snapshot);
    }).catch(function (err) {
      showBanner('无法连接网关', String(err && err.message ? err.message : err));
    }).then(function () {
      state.loading = false;
      els.refresh.classList.remove('spinning');
      resetCountdown();
    });
  }

  function renderUnavailable(message) {
    setStatus('unknown', '不可用');
    setStat('service', '—', '管理接口已关闭', '');
    setStat('models', '—', '未知', '');
    setStat('channels', '—', '未知', '');
    setStat('breakers', '—', '未知', '');
    els.breakersBody.textContent = '';
    els.channelsBody.textContent = '';
    els.modelsCloud.textContent = '';
    els.channelsEmpty.hidden = false;
    els.breakersEmpty.hidden = false;
    els.modelsEmpty.hidden = false;
    els.breakersMeta.textContent = '';
    els.channelsMeta.textContent = '';
    els.modelsMeta.textContent = '';
    showBanner('管理接口不可用', message);
  }

  /* ===== Rendering ===== */

  function setStatus(kind, label) {
    els.status.className = 'status-pill status-' + kind;
    els.statusLabel.textContent = label;
  }

  function setStat(name, value, sub, variant) {
    var card = els.stats[name];
    if (!card) { return; }
    var valueNode = card.querySelector('[data-value]');
    var subNode = card.querySelector('[data-sub]');
    valueNode.textContent = value;
    valueNode.className = 'stat-value' + (variant ? ' is-' + variant : '');
    if (sub !== undefined) { subNode.textContent = sub; }
  }

  function render(status, snapshot) {
    var ready = status.ready === true;
    setStatus(ready ? 'ok' : 'bad', ready ? '就绪' : '未就绪');
    setStat('service', ready ? '在线' : '降级', status.service ? text(status.service) : '网关服务',
      ready ? 'ok' : 'bad');
    els.uptime.textContent = '已运行 ' + formatDuration(status.uptime_ms);
    els.uptime.title = '进程运行时长';

    var channels = Array.isArray(snapshot.channels) ? snapshot.channels : [];
    var breakers = Array.isArray(snapshot.breakers) ? snapshot.breakers : [];
    var models = Array.isArray(snapshot.models) ? snapshot.models : [];

    state.channelNames = {};
    channels.forEach(function (channel) {
      if (channel && channel.id) { state.channelNames[channel.id] = channel.name || channel.id; }
    });

    var enabled = channels.filter(function (channel) { return channel && channel.enabled; }).length;
    var openCount = breakers.filter(isBreakerOpen).length;

    var modelCount = typeof status.model_count === 'number' ? status.model_count : models.length;
    setStat('models', String(modelCount), '来自通道 ' + models.length + ' 个', '');
    setStat('channels', String(channels.length), '已启用 ' + enabled + ' 个', '');
    setStat('breakers', String(openCount), '熔断 ' + openCount + ' / 跟踪 ' + breakers.length,
      openCount > 0 ? 'bad' : 'ok');

    renderBreakers(breakers);
    renderChannels(channels);
    renderModels(models, els.modelFilter.value);

    els.footerGenerated.textContent = '快照时间：' + formatTimestamp(snapshot.generated_at);
    els.footerLoaded.textContent = '配置加载：' + formatTimestamp(snapshot.loaded_at);
  }

  function isBreakerOpen(entry) {
    if (!entry) { return false; }
    if (entry.disabled) { return true; }
    if (!entry.blocked_until) { return false; }
    var parsed = new Date(entry.blocked_until);
    return !isNaN(parsed.getTime()) && parsed.getTime() > Date.now();
  }

  function renderBreakers(breakers) {
    els.breakersBody.textContent = '';
    els.breakersMeta.textContent = '跟踪 ' + breakers.length + ' 项';

    if (breakers.length === 0) {
      els.breakersEmpty.hidden = false;
      document.querySelector('[data-breakers-table]').hidden = true;
      return;
    }
    document.querySelector('[data-breakers-table]').hidden = false;
    els.breakersEmpty.hidden = true;

    breakers.forEach(function (entry) {
      var row = document.createElement('tr');
      var open = isBreakerOpen(entry);

      cell(row, badge(enumLabel('scope', entry.scope, 'channel'), open ? 'warning' : 'muted'));

      var channelID = entry.channel_id || '';
      var channelName = state.channelNames[channelID] || channelID || '—';
      cell(row, element('span', 'cell-strong', channelName));

      cell(row, entry.key_id ? element('span', 'tag', entry.key_id) : '—');
      cell(row, entry.model ? element('span', 'tag', entry.model) : '—');
      cell(row, String(entry.consecutive_failures || 0), 'num');
      cell(row, String(entry.cooldown_level || 0), 'num');

      var stateCell = document.createElement('td');
      var label = '正常';
      var variant = 'success';
      if (entry.disabled) {
        label = '已禁用';
        variant = 'danger';
      } else if (open) {
        var remaining = relativeUntil(entry.blocked_until);
        label = remaining ? '熔断（' + remaining + '）' : '熔断';
        variant = 'danger';
      }
      stateCell.appendChild(badge(label, variant));
      row.appendChild(stateCell);

      els.breakersBody.appendChild(row);
    });
  }

  function renderChannels(channels) {
    els.channelsBody.textContent = '';
    els.channelsMeta.textContent = '已配置 ' + channels.length + ' 个';

    if (channels.length === 0) {
      els.channelsEmpty.hidden = false;
      document.querySelector('[data-channels-table]').hidden = true;
      return;
    }
    document.querySelector('[data-channels-table]').hidden = false;
    els.channelsEmpty.hidden = true;

    channels.forEach(function (channel) {
      var row = document.createElement('tr');

      var nameCell = document.createElement('td');
      nameCell.appendChild(element('span', 'cell-strong', text(channel.name, channel.id)));
      nameCell.appendChild(element('span', 'cell-id', '#' + text(channel.id)));
      row.appendChild(nameCell);

      cell(row, badge(channel.enabled ? '已启用' : '已禁用', channel.enabled ? 'success' : 'danger'));
      cell(row, String(channel.priority === undefined ? 0 : channel.priority), 'num');
      cell(row, String(channel.weight === undefined ? 0 : channel.weight), 'num');
      cell(row, element('span', 'tag', enumLabel('routing_strategy', channel.routing_strategy, 'weighted')));
      cell(row, element('span', 'tag', enumLabel('breaker_mode', channel.breaker_mode, 'cooldown')));

      var proxyVariant = channel.proxy_source === 'direct' ? 'muted' : 'info';
      cell(row, badge(enumLabel('proxy_source', channel.proxy_source, 'direct'), proxyVariant));

      var mappings = Array.isArray(channel.model_mappings) ? channel.model_mappings : [];
      var mappingCell = document.createElement('td');
      mappingCell.className = 'num';
      if (mappings.length === 0) {
        mappingCell.textContent = '0';
      } else {
        var summary = mappings.map(function (mapping) {
          return text(mapping.pattern) + ' → ' + text(mapping.target);
        }).join('\n');
        var tag = element('span', 'tag', String(mappings.length));
        tag.title = summary;
        mappingCell.appendChild(tag);
      }
      row.appendChild(mappingCell);

      els.channelsBody.appendChild(row);
    });
  }

  function renderModels(models, filter) {
    els.modelsCloud.textContent = '';
    els.modelsMeta.textContent = models.length + ' 个可路由';

    var needle = (filter || '').trim().toLowerCase();
    var visible = needle
      ? models.filter(function (model) { return String(model).toLowerCase().indexOf(needle) !== -1; })
      : models;

    els.modelsEmpty.hidden = visible.length !== 0;
    // With no filter the panel is empty because nothing is configured, which is
    // a different situation from a filter that matched nothing.
    if (els.modelsEmptyText) {
      els.modelsEmptyText.textContent = needle
        ? '没有符合筛选条件的模型'
        : '没有可路由的模型';
    }
    els.modelsMeta.textContent = needle
      ? visible.length + ' / ' + models.length + ' 个可路由'
      : models.length + ' 个可路由';

    var fragment = document.createDocumentFragment();
    visible.forEach(function (model) {
      fragment.appendChild(element('span', 'chip', String(model)));
    });
    els.modelsCloud.appendChild(fragment);
  }

  /* ===== Banners & refresh loop ===== */

  function showBanner(title, detail) {
    els.bannerTitle.textContent = title;
    els.bannerDetail.textContent = detail || '';
    els.banner.hidden = false;
  }

  function hideBanner() {
    els.banner.hidden = true;
  }

  function resetCountdown() {
    state.countdown = REFRESH_MS / 1000;
    els.nextRefresh.textContent = formatCountdown(state.countdown);
  }

  function startAutoRefresh() {
    stopAutoRefresh();
    resetCountdown();
    state.timer = window.setInterval(function () {
      state.countdown -= 1;
      if (state.countdown <= 0) {
        refresh();
        state.countdown = REFRESH_MS / 1000;
      }
      els.nextRefresh.textContent = formatCountdown(state.countdown);
    }, 1000);
  }

  function stopAutoRefresh() {
    if (state.timer !== null) {
      window.clearInterval(state.timer);
      state.timer = null;
    }
  }

  /* ===== Password change ===== */

  function resetPasswordForm(form, current, next, confirm, feedback) {
    form.reset();
    current.value = '';
    next.value = '';
    confirm.value = '';
    feedback.hidden = true;
    feedback.textContent = '';
    feedback.className = 'settings-feedback';
    var submit = form.querySelector('button[type="submit"]');
    if (submit) {
      submit.disabled = false;
      submit.querySelector('.btn-label').textContent =
        form.id === 'required-password-form' ? '设置密码' : '修改密码';
    }
  }

  function passwordFeedback(feedback, message, kind) {
    feedback.hidden = !message;
    feedback.textContent = message || '';
    feedback.className = 'settings-feedback' + (kind ? ' is-' + kind : '');
  }

  /* submitPasswordChange drives both the settings form and the forced-change
   * form; onDone runs only after the gateway accepted the new password. */
  function submitPasswordChange(form, feedback, onDone) {
    var current = form.querySelector('input[name="current_password"]');
    var next = form.querySelector('input[name="new_password"]');
    var confirm = form.querySelector('input[name="confirm_password"]');
    var submit = form.querySelector('button[type="submit"]');
    var label = submit.querySelector('.btn-label');
    var original = label.textContent;

    feedback.hidden = true;

    if (!current.value || !next.value) {
      passwordFeedback(feedback, '请输入当前密码和新密码。', 'error');
      return;
    }
    if (next.value !== confirm.value) {
      passwordFeedback(feedback, '两次输入的新密码不一致。', 'error');
      confirm.focus();
      return;
    }

    submit.disabled = true;
    label.textContent = '保存中…';

    post('/management/password', {
      current_password: current.value,
      new_password: next.value
    }).then(function (result) {
      if (result.ok) {
        resetPasswordForm(form, current, next, confirm, feedback);
        // Clear the values the browser may have kept before handing control back.
        form.reset();
        current.value = next.value = confirm.value = '';
        onDone();
        return;
      }
      submit.disabled = false;
      label.textContent = original;
      if (result.status === 401) {
        passwordFeedback(feedback, '当前密码不正确。', 'error');
        current.value = '';
        current.focus();
        return;
      }
      if (result.status === 400 && result.body && result.body.error) {
        passwordFeedback(feedback, errorMessage(result.body, '新密码未被接受。'), 'error');
        return;
      }
      if (result.status === 403) {
        // The session is authenticated but still gated: refresh so the forced
        // screen reappears rather than silently failing.
        passwordFeedback(feedback, '请重新登录后再修改密码。', 'error');
        return;
      }
      passwordFeedback(feedback, errorMessage(result.body, '网关返回了预期之外的响应。'), 'error');
    }).catch(function (err) {
      submit.disabled = false;
      label.textContent = original;
      passwordFeedback(feedback, '无法连接网关：' +
        String(err && err.message ? err.message : err), 'error');
    });
  }

  /* ===== Wiring ===== */

  function setSubmitting(submitting) {
    els.authSubmit.disabled = submitting;
    els.authSubmit.querySelector('.btn-label').textContent = submitting ? '登录中…' : '登录';
  }

  function signInError(message) {
    setSubmitting(false);
    els.authError.hidden = false;
    els.authError.textContent = message;
  }

  function bind() {
    els.authForm.addEventListener('submit', function (event) {
      event.preventDefault();
      var username = els.authUsername.value.trim();
      var password = els.authPassword.value;
      if (!username || !password) {
        els.authError.hidden = false;
        els.authError.textContent = '请输入用户名和密码。';
        return;
      }
      els.authError.hidden = true;
      setSubmitting(true);

      post('/management/login', { username: username, password: password }).then(function (result) {
        if (result.ok) {
          els.authPassword.value = '';
          state.username = (result.body && result.body.username) || username;
          // A default-credential account must set a real password before the
          // gateway will serve anything, so the console goes straight there.
          if (result.body && result.body.change_required) {
            showPasswordRequired();
            return;
          }
          showConsole();
          refresh();
          return;
        }
        if (result.status === 429) {
          signInError('失败次数过多，请等待几分钟后重试。');
          return;
        }
        if (result.status === 503) {
          signInError('本网关未配置管理台登录。');
          return;
        }
        if (result.status === 401) {
          signInError('用户名或密码错误。');
          return;
        }
        signInError(errorMessage(result.body, '网关返回了预期之外的响应。'));
      }).catch(function (err) {
        signInError('无法连接网关：' + String(err && err.message ? err.message : err));
      });
    });

    els.requiredForm.addEventListener('submit', function (event) {
      event.preventDefault();
      submitPasswordChange(els.requiredForm, els.requiredFeedback, function () {
        showConsole();
        refresh();
      });
    });
    els.requiredSignout.addEventListener('click', signOut);

    els.passwordForm.addEventListener('submit', function (event) {
      event.preventDefault();
      submitPasswordChange(els.passwordForm, els.passwordFeedback, function () {
        passwordFeedback(els.passwordFeedback, '密码已更新。', 'success');
        refresh();
      });
    });

    els.settingsButton.addEventListener('click', showSettings);
    els.settingsBack.addEventListener('click', function () {
      showDashboard();
      refresh();
    });
    els.passwordCancel.addEventListener('click', function () {
      showDashboard();
      refresh();
    });

    els.refresh.addEventListener('click', refresh);
    els.signout.addEventListener('click', signOut);
    els.bannerRetry.addEventListener('click', refresh);
    els.modelFilter.addEventListener('input', function () {
      renderModels(state.snapshot && state.snapshot.models ? state.snapshot.models : [], els.modelFilter.value);
    });

    document.addEventListener('visibilitychange', function () {
      // Only the live dashboard refreshes. Its container carries no hidden
      // attribute before sign-in, so asking whether the app and the dashboard
      // are both visible is what keeps a returning tab from firing an
      // unauthenticated request and reporting a session that never existed.
      if (!document.hidden && !els.app.hidden && !els.dashboardView.hidden) { refresh(); }
    });
  }

  /* boot asks the gateway who we are instead of trusting local storage: the
   * session cookie is HTTP-only, so only the server can answer that. */
  function boot() {
    cacheElements();
    bind();

    request('/management/session').then(function (result) {
      if (result.ok && result.body && result.body.authenticated) {
        state.username = result.body.username || '';
        if (result.body.change_required) {
          showPasswordRequired();
          return;
        }
        showConsole();
        refresh();
        return;
      }
      if (result.ok && result.body && result.body.configured === false) {
        showAuth('本网关未配置管理台登录。');
        return;
      }
      showAuth('');
    }).catch(function () {
      showAuth('无法连接网关。');
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
