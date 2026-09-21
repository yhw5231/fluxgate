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

  /* 管理视图 = 概览之外的视图，它们读取的是可写的配置清单。 */
  var VIEWS = ['overview', 'channels', 'routes', 'upstream', 'keys', 'settings'];
  var MANAGEMENT_VIEWS = { channels: true, routes: true, upstream: true, keys: true };

  var state = {
    view: 'overview',
    snapshot: null,
    configuration: null,
    configurationError: null,
    username: '',
    channelNames: {},
    timer: null,
    countdown: 0,
    loading: false,
    editor: null,
    confirm: null
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
    els.nav = $('console-nav');
    els.viewPanels = document.querySelectorAll('[data-view-panel]');

    els.status = $('topbar-status');
    els.statusLabel = els.status.querySelector('[data-status-label]');
    els.uptime = $('topbar-uptime');
    els.refresh = $('refresh-button');
    els.signout = $('signout-button');

    // Settings view and the password forms. Its container is one of the app's
    // views, because the settings page is a sibling of the dashboard rather than
    // a separate page load.
    els.settingsView = $('settings-view');
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
    els.notice = $('notice');
    els.noticeText = els.notice.querySelector('[data-notice]');
    els.noticeDismiss = $('notice-dismiss');

    els.stats = {
      service: document.querySelector('[data-stat="service"]'),
      models: document.querySelector('[data-stat="models"]'),
      channels: document.querySelector('[data-stat="channels"]'),
      breakers: document.querySelector('[data-stat="breakers"]')
    };

    els.breakersBody = document.querySelector('[data-breakers-body]');
    els.breakersEmpty = document.querySelector('[data-breakers-empty]');
    els.breakersMeta = document.querySelector('[data-panel="breakers"] [data-meta]');
    els.breakersTable = document.querySelector('[data-breakers-table]');

    els.channelsBody = document.querySelector('[data-channels-body]');
    els.channelsEmpty = document.querySelector('[data-channels-empty]');
    els.channelsMeta = document.querySelector('[data-panel="channels"] [data-meta]');
    els.channelsTable = document.querySelector('[data-channels-table]');

    els.modelsCloud = document.querySelector('[data-models-cloud]');
    els.modelsEmpty = document.querySelector('[data-models-empty]');
    els.modelsEmptyText = document.querySelector('[data-models-empty-text]');
    els.modelsMeta = document.querySelector('[data-panel="models"] [data-meta]');
    els.modelFilter = $('model-filter');

    state.emptyText = {};
    document.querySelectorAll('[data-empty]').forEach(function (node) {
      var paragraph = node.querySelector('[data-empty-text]') || node;
      state.emptyText[node.getAttribute('data-empty')] = paragraph.textContent;
    });

    els.nextRefresh = document.querySelector('[data-next-refresh]');
    els.footerGenerated = document.querySelector('[data-footer-generated]');
    els.footerLoaded = document.querySelector('[data-footer-loaded]');

    els.editorOverlay = $('editor-overlay');
    els.editorTitle = $('editor-title');
    els.editorNote = $('editor-note');
    els.editorForm = $('editor-form');
    els.editorFields = $('editor-fields');
    els.editorFeedback = $('editor-feedback');
    els.editorSubmit = $('editor-submit');
    els.editorClose = $('editor-close');
    els.editorCancel = $('editor-cancel');

    els.confirmOverlay = $('confirm-overlay');
    els.confirmTitle = $('confirm-title');
    els.confirmText = $('confirm-text');
    els.confirmFeedback = $('confirm-feedback');
    els.confirmSubmit = $('confirm-submit');
    els.confirmCancel = $('confirm-cancel');

    els.secretOverlay = $('secret-overlay');
    els.secretText = $('secret-text');
    els.secretValue = $('secret-value');
    els.secretCopy = $('secret-copy');
    els.secretClose = $('secret-close');
    els.secretFeedback = $('secret-feedback');
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

  function pad(value) { return value < 10 ? '0' + value : String(value); }

  /* localInputValue renders a stored timestamp into the value an
   * input[type=datetime-local] expects, so editing shows the operator's own
   * clock rather than UTC. */
  function localInputValue(value) {
    if (!value) { return ''; }
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) { return ''; }
    return parsed.getFullYear() + '-' + pad(parsed.getMonth() + 1) + '-' + pad(parsed.getDate()) +
      'T' + pad(parsed.getHours()) + ':' + pad(parsed.getMinutes());
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

  function tag(label) {
    return element('span', 'tag', label);
  }

  function enabledBadge(enabled) {
    return badge(enabled ? '已启用' : '已禁用', enabled ? 'success' : 'danger');
  }

  /* ===== Resource metadata =====
   * The server owns which fields exist and how each one is validated; this table
   * only says how a field is labelled and which widget it gets. A field the
   * server returns without an entry here still renders, as a plain text input
   * named after the column, so a new column is usable before this file knows
   * about it. */

  var CHOICE_LABELS = {
    route_mode: { pattern: '按模型匹配', explicit_group: '显式分组' },
    routing_strategy: { weighted: '加权轮询', round_robin: '轮询', stable_first: '固定优先' },
    protocol: { http: 'HTTP', https: 'HTTPS', socks5: 'SOCKS5', socks5h: 'SOCKS5H' }
  };

  function choiceOptions(field) {
    var labels = CHOICE_LABELS[field.name] || {};
    return (field.choices || []).map(function (value) {
      return { value: value, label: labels[value] || value };
    });
  }

  var STATUS_OPTIONS = [
    { value: 'active', label: '启用' },
    { value: 'disabled', label: '停用' }
  ];

  /* FIELDS[resource][column] */
  var FIELDS = {
    sites: {
      name: { label: '名称', placeholder: '例如 OpenAI 官方' },
      url: { label: '站点地址', placeholder: 'https://api.example.com', help: '上游服务的根地址，必须包含 http:// 或 https://。' },
      platform: { label: '平台', placeholder: 'openai', help: '平台标识，同一个站点的账号共用它。' },
      status: { label: '状态', type: 'select', options: STATUS_OPTIONS, help: '只有 active 的站点才参与选路。' },
      global_weight: { label: '全局权重', type: 'number', step: 'any', placeholder: '1', help: '站点权重会乘到该站点每个通道的权重上。' },
      proxy_url: { label: '站点代理', placeholder: 'http://127.0.0.1:7890', help: '留空表示不使用站点级代理；填 system 表示使用系统代理。' },
      use_system_proxy: { label: '使用系统代理' },
      custom_headers: { label: '自定义请求头', type: 'json', placeholder: '{"X-Custom": "value"}', help: 'JSON 对象，会附加到发往该站点的请求上。' },
      forced_upstream_endpoint: { label: '强制上游端点', placeholder: 'https://api.example.com/v1', help: '填写后，该站点的请求一律发往这个地址。' }
    },
    accounts: {
      site_id: { label: '所属站点', type: 'reference', resource: 'sites' },
      access_token: { label: '访问令牌', help: '写入后不再显示原文，只显示末四位；留空表示保持不变。' },
      api_token: { label: 'API 令牌', help: '可选。填写后优先于访问令牌；留空表示保持不变，勾选清除可删除。' },
      status: { label: '状态', type: 'select', options: STATUS_OPTIONS },
      extra_config: {
        label: '扩展配置', type: 'json', placeholder: '{"proxyUrl": "http://127.0.0.1:7890"}',
        help: 'JSON 对象。proxyUrl 指定该账号使用的代理，useSystemProxy 为 true 时改用系统代理。'
      }
    },
    tokens: {
      account_id: { label: '所属账号', type: 'reference', resource: 'accounts' },
      token: { label: '令牌', help: '写入后不再显示原文，只显示末四位；留空表示保持不变。' },
      proxy_url: { label: '令牌代理', placeholder: 'socks5://127.0.0.1:1080', help: '该令牌专用代理，优先于账号和站点代理。' },
      use_system_proxy: { label: '使用系统代理' },
      enabled: { label: '启用' }
    },
    routes: {
      model_pattern: { label: '模型匹配', placeholder: 'gpt-4.1 或 gpt-* 或 re:^claude', help: '客户端请求的模型名。支持 * 和 ? 通配符，re: 前缀表示正则表达式。' },
      display_name: { label: '对外别名', placeholder: 'gpt-4.1', help: '客户端也可以用这个别名请求，优先级高于通配符匹配。' },
      route_mode: { label: '路由模式', type: 'select' },
      routing_strategy: { label: '路由策略', type: 'select' },
      enabled: { label: '启用' },
      model_mapping: { label: '模型映射', type: 'json', placeholder: '{"gpt-4.1": "gpt-4.1-2025-04-14"}', help: 'JSON 对象，把请求的模型名改写成上游的模型名。按写入顺序匹配。' },
      source_route_ids: { label: '成员路由', type: 'id_list', resource: 'routes', placeholder: '[1, 2]', help: '只有「显式分组」模式使用：JSON 数组，列出这个分组包含的路由 ID。' }
    },
    channels: {
      route_id: { label: '所属路由', type: 'reference', resource: 'routes' },
      account_id: { label: '上游账号', type: 'reference', resource: 'accounts' },
      token_id: { label: '账号令牌', type: 'reference', resource: 'tokens', dependsOn: 'account_id', help: '留空表示使用账号自身的访问令牌。' },
      source_model: { label: '源模型', placeholder: 'gpt-4.1', help: '该通道实际发给上游的模型名，留空时按路由决定。' },
      priority: { label: '优先级', type: 'number', step: '1', placeholder: '0', help: '数值越大越先被选中；优先级不同时不会互相轮询。' },
      weight: { label: '权重', type: 'number', step: '1', placeholder: '10', help: '同优先级之间按权重分配流量，必须大于 0。' },
      enabled: { label: '启用' }
    },
    keys: {
      name: { label: '名称', placeholder: '例如 内部服务' },
      key: { label: '密钥', help: '留空由网关自动生成；填写则使用你指定的值，且不能与已有密钥重复。' },
      enabled: { label: '启用' },
      expires_at: { label: '过期时间', type: 'datetime', help: '留空表示永不过期。' },
      max_cost: { label: '额度上限', type: 'number', step: 'any', help: '留空表示不限制。' },
      used_cost: { label: '已用额度', type: 'number', step: 'any', help: '由对账逻辑维护，可在这里重置。' },
      max_requests: { label: '请求数上限', type: 'number', step: '1', help: '留空表示不限制。' },
      used_requests: { label: '已用请求数', type: 'number', step: '1' },
      supported_models: { label: '排除模型', type: 'json', placeholder: '["gpt-4.1-mini", "re:^o1"]', help: 'JSON 数组，命中的模型会被拒绝并不出现在 /v1/models 中。' },
      allowed_route_ids: { label: '限定路由', type: 'id_list', resource: 'routes', placeholder: '[1, 2]', help: 'JSON 数组，只允许这些路由；留空表示不限制。' },
      excluded_site_ids: { label: '排除站点', type: 'id_list', resource: 'sites', placeholder: '[3]', help: 'JSON 数组，这些站点的通道不会被选中。' },
      site_weight_multipliers: { label: '站点权重系数', type: 'json', placeholder: '{"3": 2}', help: 'JSON 对象，键是站点 ID，值是大于 0 的倍数。' },
      excluded_credential_refs: {
        label: '排除凭据', type: 'json',
        placeholder: '[{"kind":"account_token","siteId":1,"accountId":2,"tokenId":3}]',
        help: 'JSON 数组，精确排除某个账号令牌对应的通道。'
      }
    },
    proxies: {
      name: { label: '名称', placeholder: '例如 本地出口' },
      protocol: { label: '协议', type: 'select' },
      url: { label: '代理地址', placeholder: 'http://127.0.0.1:7890', help: '支持的协议：http、https、socks5、socks5h。' },
      is_default: { label: '设为默认代理', help: '默认代理在所有层级都不匹配时生效。' },
      enabled: { label: '启用' }
    }
  };

  /* RESOURCES[resource] describes the management table of a resource. */
  var RESOURCES = {
    channels: {
      title: '通道',
      create: '添加通道',
      noun: '通道',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '所属路由', cell: function (row) { return routeName(row.route_id); } },
        { label: '上游账号', cell: function (row) { return accountName(row.account_id); } },
        { label: '账号令牌', cell: function (row) { return row.token_id ? tokenName(row.token_id) : tag('账号默认'); } },
        { label: '源模型', cell: function (row) { return text(row.source_model); } },
        { label: '优先级', className: 'num', cell: function (row) { return String(row.priority || 0); } },
        { label: '权重', className: 'num', cell: function (row) { return String(row.weight || 0); } },
        { label: '状态', cell: function (row) { return enabledBadge(row.enabled); } }
      ],
      actions: ['edit', 'delete'],
      describe: function (row) { return routeName(row.route_id) + ' → ' + accountName(row.account_id); }
    },
    routes: {
      title: '路由',
      create: '添加路由',
      noun: '路由',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '模型匹配', cell: function (row) { return element('span', 'cell-strong mono', text(row.model_pattern)); } },
        { label: '对外别名', cell: function (row) { return text(row.display_name); } },
        { label: '模式', cell: function (row) { return tag(enumLabel('route_mode', row.route_mode, 'pattern')); } },
        { label: '策略', cell: function (row) { return tag(enumLabel('routing_strategy', row.routing_strategy, 'weighted')); } },
        { label: '映射', className: 'num', cell: function (row) { return jsonSize(row.model_mapping, 'object'); } },
        { label: '成员路由', cell: function (row) { return routeIDList(row.source_route_ids); } },
        { label: '通道', className: 'num', cell: function (row) { return String(channelCount(row.id)); } },
        { label: '状态', cell: function (row) { return enabledBadge(row.enabled); } }
      ],
      actions: ['edit', 'delete'],
      describe: function (row) { return text(row.display_name, row.model_pattern); }
    },
    sites: {
      title: '站点',
      create: '添加站点',
      noun: '站点',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '名称', cell: function (row) { return element('span', 'cell-strong', text(row.name)); } },
        { label: '地址', cell: function (row) { return element('span', 'mono', text(row.url)); } },
        { label: '平台', cell: function (row) { return text(row.platform); } },
        { label: '状态', cell: function (row) { return statusBadge(row.status); } },
        { label: '全局权重', className: 'num', cell: function (row) { return text(row.global_weight, '1'); } },
        { label: '代理', cell: function (row) { return proxyCell(row.proxy_url, row.use_system_proxy); } }
      ],
      actions: ['edit', 'delete'],
      describe: function (row) { return text(row.name, '#' + row.id); }
    },
    accounts: {
      title: '账号',
      create: '添加账号',
      noun: '账号',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '站点', cell: function (row) { return siteName(row.site_id); } },
        { label: '访问令牌', cell: function (row) { return secretCell(row.access_token); } },
        { label: 'API 令牌', cell: function (row) { return secretCell(row.api_token); } },
        { label: '状态', cell: function (row) { return statusBadge(row.status); } },
        { label: '代理', cell: function (row) { return accountProxyCell(row.extra_config); } }
      ],
      actions: ['edit', 'delete'],
      describe: function (row) { return siteName(row.site_id) + ' #' + row.id; }
    },
    tokens: {
      title: '账号令牌',
      create: '添加令牌',
      noun: '令牌',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '账号', cell: function (row) { return accountName(row.account_id); } },
        { label: '令牌', cell: function (row) { return secretCell(row.token); } },
        { label: '代理', cell: function (row) { return proxyCell(row.proxy_url, row.use_system_proxy); } },
        { label: '状态', cell: function (row) { return enabledBadge(row.enabled); } }
      ],
      actions: ['edit', 'delete'],
      describe: function (row) { return accountName(row.account_id) + ' #' + row.id; }
    },
    proxies: {
      title: '代理',
      create: '添加代理',
      noun: '代理',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '名称', cell: function (row) { return element('span', 'cell-strong', text(row.name)); } },
        { label: '协议', cell: function (row) { return tag(enumLabel('protocol', row.protocol, 'http')); } },
        { label: '地址', cell: function (row) { return element('span', 'mono', text(row.url)); } },
        { label: '默认', cell: function (row) { return row.is_default ? badge('默认', 'info') : '—'; } },
        { label: '状态', cell: function (row) { return enabledBadge(row.enabled); } }
      ],
      actions: ['edit', 'delete'],
      describe: function (row) { return text(row.name, '#' + row.id); }
    },
    keys: {
      title: '客户端密钥',
      create: '添加密钥',
      noun: '密钥',
      columns: [
        { label: 'ID', className: 'num', cell: function (row) { return '#' + row.id; } },
        { label: '名称', cell: function (row) { return element('span', 'cell-strong', text(row.name)); } },
        { label: '密钥', cell: function (row) { return secretCell(row.key); } },
        { label: '状态', cell: function (row) { return enabledBadge(row.enabled); } },
        { label: '过期', cell: function (row) { return expiryCell(row.expires_at); } },
        { label: '用量', cell: function (row) { return usageCell(row); } },
        { label: '排除模型', className: 'num', cell: function (row) { return jsonSize(row.supported_models, 'array'); } }
      ],
      actions: ['edit', 'rotate', 'delete'],
      // The gateway generates the value when the create request omits it, so an
      // empty input is a valid create rather than a missing required field.
      optionalSecrets: ['key'],
      describe: function (row) { return text(row.name, '#' + row.id); }
    }
  };

  /* ===== Configuration lookups ===== */

  function resourceRows(name) {
    var resources = state.configuration && state.configuration.resources;
    var rows = resources && resources[name];
    return Array.isArray(rows) ? rows : [];
  }

  function rowByID(name, id) {
    var rows = resourceRows(name);
    for (var index = 0; index < rows.length; index++) {
      if (String(rows[index].id) === String(id)) { return rows[index]; }
    }
    return null;
  }

  /* A reference is shown as the name the operator knows, with the id kept in the
   * label so a missing name is still identifiable. */
  function referenceLabel(resource, id) {
    var row = rowByID(resource, id);
    if (!row) { return '#' + id + '（已删除）'; }
    switch (resource) {
      case 'sites': return text(row.name, '#' + row.id);
      case 'routes': return text(row.display_name, row.model_pattern) + ' (#' + row.id + ')';
      case 'accounts': return siteName(row.site_id) + ' (#' + row.id + ')';
      case 'tokens': return accountName(row.account_id) + ' · 令牌 #' + row.id;
      default: return '#' + row.id;
    }
  }

  function siteName(id) { return id ? referenceLabel('sites', id) : '—'; }
  function routeName(id) { return id ? referenceLabel('routes', id) : '—'; }
  function accountName(id) { return id ? referenceLabel('accounts', id) : '—'; }
  function tokenName(id) { return id ? referenceLabel('tokens', id) : '—'; }

  function channelCount(routeID) {
    return resourceRows('channels').filter(function (row) {
      return String(row.route_id) === String(routeID);
    }).length;
  }

  function statusBadge(status) {
    var value = text(status, 'active');
    if (value === 'active') { return badge('已启用', 'success'); }
    return badge(value, 'muted');
  }

  function secretCell(value) {
    if (!value) { return element('span', 'cell-muted', '未设置'); }
    return element('span', 'mono secret', value);
  }

  function proxyCell(proxyURL, useSystemProxy) {
    if (proxyURL) { return element('span', 'mono', proxyURL); }
    if (useSystemProxy) { return tag('系统代理'); }
    return element('span', 'cell-muted', '直连');
  }

  function accountProxyCell(extraConfig) {
    var parsed = parseJSON(extraConfig);
    if (!parsed || typeof parsed !== 'object') { return element('span', 'cell-muted', '直连'); }
    return proxyCell(parsed.proxyUrl, parsed.useSystemProxy === true);
  }

  function expiryCell(value) {
    if (!value) { return element('span', 'cell-muted', '永不过期'); }
    var parsed = new Date(value);
    if (!isNaN(parsed.getTime()) && parsed.getTime() <= Date.now()) {
      return badge('已过期 ' + formatTimestamp(value), 'danger');
    }
    return element('span', '', formatTimestamp(value));
  }

  function usageCell(row) {
    var requests = String(row.used_requests || 0) +
      (row.max_requests === null || row.max_requests === undefined ? ' / 不限' : ' / ' + row.max_requests);
    var cost = String(row.used_cost || 0) +
      (row.max_cost === null || row.max_cost === undefined ? ' / 不限' : ' / ' + row.max_cost);
    var wrapper = element('span', 'usage');
    wrapper.appendChild(element('span', 'tag', '请求 ' + requests));
    wrapper.appendChild(element('span', 'tag', '额度 ' + cost));
    return wrapper;
  }

  function jsonSize(value, kind) {
    var parsed = parseJSON(value);
    if (parsed === null || parsed === undefined) { return '0'; }
    if (kind === 'array' && Array.isArray(parsed)) { return String(parsed.length); }
    if (kind === 'object' && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return String(Object.keys(parsed).length);
    }
    return '0';
  }

  function routeIDList(value) {
    var parsed = parseJSON(value);
    if (!Array.isArray(parsed) || parsed.length === 0) { return element('span', 'cell-muted', '—'); }
    var wrapper = element('span', 'tag-list');
    parsed.forEach(function (id) {
      wrapper.appendChild(tag(routeName(id)));
    });
    return wrapper;
  }

  function parseJSON(value) {
    if (value === null || value === undefined || value === '') { return null; }
    if (typeof value !== 'string') { return value; }
    try {
      return JSON.parse(value);
    } catch (error) {
      return null;
    }
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
    showView('overview');
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

  function showView(name) {
    if (VIEWS.indexOf(name) === -1) { name = 'overview'; }
    state.view = name;
    els.passwordRequired.hidden = true;
    for (var index = 0; index < els.viewPanels.length; index++) {
      var panel = els.viewPanels[index];
      panel.hidden = panel.getAttribute('data-view-panel') !== name;
    }
    var buttons = els.nav.querySelectorAll('[data-view]');
    for (var buttonIndex = 0; buttonIndex < buttons.length; buttonIndex++) {
      var button = buttons[buttonIndex];
      var active = button.getAttribute('data-view') === name;
      button.classList.toggle('is-active', active);
      button.setAttribute('aria-current', active ? 'page' : 'false');
    }
    if (name === 'settings') {
      els.settingsAccount.textContent = state.username
        ? '当前登录账号：' + state.username + '。'
        : '已登录。';
      resetPasswordForm(els.passwordForm, els.currentPassword, els.newPassword,
        els.confirmPassword, els.passwordFeedback);
      els.currentPassword.focus();
    }
    // Only the overview is a live view: a management form must not be repainted
    // from under the operator while it is being filled in.
    if (name === 'overview') {
      startAutoRefresh();
    } else {
      stopAutoRefresh();
    }
    if (MANAGEMENT_VIEWS[name] && !state.configuration) { refresh(); }
  }

  function signOut() {
    stopAutoRefresh();
    state.snapshot = null;
    state.configuration = null;
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

  function send(method, path, payload) {
    return fetch(apiPath(path), {
      method: method,
      headers: payload
        ? { 'Content-Type': 'application/json', 'Accept': 'application/json' }
        : { 'Accept': 'application/json' },
      body: payload ? JSON.stringify(payload) : undefined,
      cache: 'no-store',
      credentials: 'same-origin'
    }).then(readResponse);
  }

  function post(path, payload) { return send('POST', path, payload); }
  function put(path, payload) { return send('PUT', path, payload); }
  function remove(path) { return send('DELETE', path); }

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
    invalid_json: '请求格式不正确。',
    // 配置管理
    configuration_not_supported: '本网关未启用配置管理，只能用配置文件维护。',
    configuration_not_found: '这条配置已经不存在，请刷新后重试。',
    configuration_write_failed: '网关保存失败，请查看网关日志。',
    configuration_conflict: '该值已被占用，请换一个再试。',
    configuration_referenced: '无法删除：仍有其他配置引用这条记录。',
    configuration_reload_failed: '变更已写入数据库，但网关重新加载配置失败，请重启网关后再试。',
    cross_origin_rejected: '请求来源不是管理台本身，已拒绝。',
    unknown_resource: '未知的配置类型。',
    request_too_large: '提交的内容过大。'
  };

  /* 字段级校验失败的中文说明。reason 是网关稳定给出的原因码，params 携带
   * 模板需要的数值；未知原因码回退到网关的英文原文。 */
  var REASON_MESSAGES = {
    required: function (field) { return '「' + field + '」必须填写。'; },
    too_long: function (field, params) { return '「' + field + '」最长 ' + params.limit + ' 字节。'; },
    not_allowed: function (field, params) {
      return '「' + field + '」只能是 ' + (params.choices || []).join(' / ') + '。';
    },
    must_be_positive: function (field) { return '「' + field + '」必须大于 0。'; },
    missing_reference: function (field, params) {
      return '「' + field + '」指向的 ' + params.table + ' 记录（ID ' + params.id + '）不存在。';
    },
    reference_mismatch: function (field, params) {
      return '「' + field + '」属于账号 ' + params.owner + '，与所选账号 ' + params.account + ' 不一致。';
    },
    unknown_field: function (field) { return '不认识的字段「' + field + '」。'; },
    referenced: function (field, params) {
      var noun = RESOURCE_NOUNS[params.resource] || params.resource;
      return '无法删除：仍有 ' + params.count + ' 条「' + noun + '」引用这条记录，请先删除它们或改指向别处。';
    }
  };

  /* 配置类型的中文名，用于「仍有 N 条××引用」这类提示。 */
  var RESOURCE_NOUNS = {
    sites: '站点', accounts: '账号', tokens: '账号令牌', routes: '路由',
    channels: '通道', keys: '客户端密钥', proxies: '代理'
  };

  function apiError(body) {
    return body && body.error && typeof body.error === 'object' ? body.error : null;
  }

  function errorMessage(body, fallback) {
    var error = apiError(body);
    if (error) {
      if (error.reason && REASON_MESSAGES[error.reason]) {
        var field = labelFor(error.field) || error.field || '';
        return REASON_MESSAGES[error.reason](field, error.params || {});
      }
      if (error.code && CODE_MESSAGES[error.code]) {
        var message = CODE_MESSAGES[error.code];
        // 删除被拒时网关会说明是哪一类记录挡住了它，原文是英文，这里补一句中文。
        if (error.code === 'configuration_referenced' && error.message) {
          return message + '（' + error.message + '）';
        }
        return message;
      }
      if (error.message) { return error.message; }
      if (error.code) { return error.code; }
    }
    return fallback;
  }

  /* labelFor returns the Chinese label of a column, whichever resource declares
   * it, so a field error can name the input the operator sees. */
  function labelFor(field) {
    if (!field) { return ''; }
    var resources = Object.keys(FIELDS);
    for (var index = 0; index < resources.length; index++) {
      var meta = FIELDS[resources[index]][field];
      if (meta && meta.label) { return meta.label; }
    }
    return '';
  }

  function fieldErrorField(body) {
    var error = apiError(body);
    return error && error.field ? error.field : '';
  }

  /* ===== Refresh loop ===== */

  function refresh() {
    if (state.loading) { return; }
    state.loading = true;
    els.refresh.classList.add('spinning');

    var wantsConfiguration = MANAGEMENT_VIEWS[state.view] || !state.configuration;
    var calls = [request('/management/status'), request('/management/snapshot')];
    if (wantsConfiguration) { calls.push(request('/management/configuration')); }

    Promise.all(calls).then(function (results) {
      var status = results[0];
      var snapshot = results[1];
      var configuration = results[2] || null;

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

      if (configuration) {
        if (configuration.status === 503) {
          // A gateway started without a write store can still be viewed.
          state.configuration = null;
          state.configurationError = errorMessage(configuration.body, '本网关未启用配置管理。');
        } else if (configuration.ok) {
          applyConfiguration(configuration.body);
        } else {
          state.configurationError = errorMessage(configuration.body, '读取配置失败。');
        }
      }

      hideBanner();
      state.snapshot = snapshot.body || {};
      render(status.body || {}, state.snapshot);
      renderManagement();
    }).catch(function (err) {
      showBanner('无法连接网关', String(err && err.message ? err.message : err));
    }).then(function () {
      state.loading = false;
      els.refresh.classList.remove('spinning');
      resetCountdown();
    });
  }

  /* applyConfiguration installs a configuration payload, whether it arrived from
   * a read or as the tail of a write response. */
  function applyConfiguration(configuration) {
    state.configuration = configuration || null;
    state.configurationError = null;
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
      els.breakersTable.hidden = true;
      return;
    }
    els.breakersTable.hidden = false;
    els.breakersEmpty.hidden = true;

    breakers.forEach(function (entry) {
      var row = document.createElement('tr');
      var open = isBreakerOpen(entry);

      cell(row, badge(enumLabel('scope', entry.scope, 'channel'), open ? 'warning' : 'muted'));

      var channelID = entry.channel_id || '';
      var channelName = state.channelNames[channelID] || channelID || '—';
      cell(row, element('span', 'cell-strong', channelName));

      cell(row, entry.key_id ? tag(entry.key_id) : '—');
      cell(row, entry.model ? tag(entry.model) : '—');
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
      els.channelsTable.hidden = true;
      return;
    }
    els.channelsTable.hidden = false;
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
      cell(row, tag(enumLabel('routing_strategy', channel.routing_strategy, 'weighted')));
      cell(row, tag(enumLabel('breaker_mode', channel.breaker_mode, 'cooldown')));

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
        var count = tag(String(mappings.length));
        count.title = summary;
        mappingCell.appendChild(count);
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

  /* ===== Management tables ===== */

  function renderManagement() {
    Object.keys(RESOURCES).forEach(function (resource) {
      var panel = document.querySelector('[data-management="' + resource + '"]');
      if (!panel) { return; }
      var table = panel.querySelector('[data-table]');
      var body = panel.querySelector('[data-body]');
      var emptyState = panel.querySelector('[data-empty]');
      var meta = panel.querySelector('[data-meta]');
      var head = table.querySelector('thead');
      var metadata = RESOURCES[resource];

      body.textContent = '';
      if (head) { head.textContent = ''; }

      if (state.configuration === null) {
        table.hidden = true;
        emptyState.hidden = false;
        setEmptyText(emptyState, resource, state.configurationError || '尚未读取配置。');
        meta.textContent = '';
        return;
      }

      var headerRow = document.createElement('tr');
      metadata.columns.forEach(function (column) {
        var th = document.createElement('th');
        th.textContent = column.label;
        if (column.className) { th.className = column.className; }
        headerRow.appendChild(th);
      });
      var actionHeader = document.createElement('th');
      actionHeader.className = 'actions';
      actionHeader.textContent = '操作';
      headerRow.appendChild(actionHeader);
      if (!head) {
        head = document.createElement('thead');
        table.insertBefore(head, table.firstChild);
      }
      head.appendChild(headerRow);

      var rows = resourceRows(resource);
      meta.textContent = '共 ' + rows.length + ' 条';

      if (rows.length === 0) {
        table.hidden = false;
        emptyState.hidden = false;
        setEmptyText(emptyState, resource, state.emptyText[resource] || '')
        return;
      }
      table.hidden = false;
      emptyState.hidden = true;
      setEmptyText(emptyState, resource, state.emptyText[resource] || '')

      rows.forEach(function (row) {
        var tr = document.createElement('tr');
        metadata.columns.forEach(function (column) {
          var content = column.cell(row);
          if (content && content.nodeType) {
            cell(tr, content, column.className);
          } else {
            cell(tr, content, column.className);
          }
        });
        tr.appendChild(actionCell(resource, row, metadata));
        body.appendChild(tr);
      });
    });
  }

  /* setEmptyText writes the notice line of an empty panel, so the declared text
   * comes back after an error or a transient state has been shown over it. */
  function setEmptyText(emptyState, resource, message) {
    var paragraph = emptyState.querySelector('[data-empty-text]');
    if (!paragraph) { paragraph = emptyState; }
    paragraph.textContent = message;
  }

  function actionCell(resource, row, metadata) {
    var td = document.createElement('td');
    td.className = 'actions';
    var group = element('div', 'row-actions');

    if (metadata.actions.indexOf('edit') !== -1) {
      var edit = element('button', 'btn btn-ghost btn-small', '编辑');
      edit.type = 'button';
      edit.addEventListener('click', function () { openEditor(resource, row); });
      group.appendChild(edit);
    }
    if (metadata.actions.indexOf('rotate') !== -1) {
      var rotate = element('button', 'btn btn-ghost btn-small', '轮换');
      rotate.type = 'button';
      rotate.addEventListener('click', function () {
        openConfirm({
          title: '轮换密钥',
          text: '将立即生成新的密钥，旧密钥随即失效。确认轮换「' + metadata.describe(row) + '」？',
          confirmLabel: '轮换',
          danger: false,
          run: function () { return rotateKey(row.id); }
        });
      });
      group.appendChild(rotate);
    }
    if (metadata.actions.indexOf('delete') !== -1) {
      var removeButton = element('button', 'btn btn-ghost btn-small btn-danger-text', '删除');
      removeButton.type = 'button';
      removeButton.addEventListener('click', function () {
        openConfirm({
          title: '删除' + metadata.noun,
          text: '确认删除' + metadata.noun + '「' + metadata.describe(row) + '」？此操作不可撤销。',
          confirmLabel: '删除',
          danger: true,
          run: function () { return deleteRow(resource, row.id); }
        });
      });
      group.appendChild(removeButton);
    }
    td.appendChild(group);
    return td;
  }

  /* ===== Editor ===== */

  /* openEditor renders a form for one resource. A null row creates, an existing
   * row edits: the same fields, but a stored secret starts empty because the
   * gateway only ever sends its mask. */
  function openEditor(resource, row) {
    var metadata = RESOURCES[resource];
    if (!metadata) { return; }
    var fields = state.configuration && state.configuration.fields
      ? state.configuration.fields[resource] : null;
    if (!Array.isArray(fields) || fields.length === 0) {
      showBanner('无法编辑', '网关没有返回「' + metadata.title + '」的字段定义。');
      return;
    }

    state.editor = { resource: resource, id: row ? row.id : 0, fields: [] };
    els.editorTitle.textContent = (row ? '编辑' : '添加') + metadata.noun;
    els.editorNote.hidden = !metadata.editorNote;
    els.editorNote.textContent = metadata.editorNote || '';
    els.editorFields.textContent = '';
    hideFeedback(els.editorFeedback);
    setSubmitting(els.editorSubmit, false, '保存');

    fields.forEach(function (field) {
      var built = buildField(resource, field, row);
      if (!built) { return; }
      els.editorFields.appendChild(built.wrapper);
      state.editor.fields.push(built);
    });

    els.editorOverlay.hidden = false;
    var firstInput = els.editorFields.querySelector('input:not([type=hidden]), select, textarea');
    if (firstInput) { firstInput.focus(); }
  }

  function closeEditor() {
    els.editorOverlay.hidden = true;
    els.editorForm.reset();
    els.editorFields.textContent = '';
    state.editor = null;
  }

  /* buildField turns one server field description into an input, a wrapper and a
   * reader that produces the JSON value to submit. */
  function buildField(resource, field, row) {
    var meta = (FIELDS[resource] && FIELDS[resource][field.name]) || {};
    var wrapper = element('div', 'field field-editor');
    wrapper.setAttribute('data-field', field.name);

    var inputID = 'editor-field-' + resource + '-' + field.name;
    var label = element('label', 'field-label', meta.label || field.name);
    label.setAttribute('for', inputID);
    wrapper.appendChild(label);

    var input = createInput(resource, field, meta, inputID, row);
    wrapper.appendChild(input.node);

    if (meta.help) {
      wrapper.appendChild(element('p', 'field-help', meta.help));
    }
    if (field.secret && row) {
      wrapper.appendChild(element('p', 'field-help', '当前值：' + (row[field.name] || '未设置') + '，留空表示保持不变。'));
    }

    var clearable = null;
    if (field.secret && field.clearable && row) {
      var clearLabel = element('label', 'checkline');
      clearable = document.createElement('input');
      clearable.type = 'checkbox';
      clearLabel.appendChild(clearable);
      clearLabel.appendChild(element('span', '', '清除该字段'));
      wrapper.appendChild(clearLabel);
    }

    return {
      name: field.name,
      kind: field.kind,
      secret: field.secret,
      required: field.required,
      input: input.node,
      wrapper: wrapper,
      read: function () {
        if (clearable && clearable.checked) { return null; }
        return input.read();
      }
    };
  }

  function createInput(resource, field, meta, inputID, row) {
    var value = row ? row[field.name] : undefined;

    if (field.secret) {
      var secretInput = document.createElement('input');
      secretInput.id = inputID;
      secretInput.className = 'field-input';
      secretInput.type = 'password';
      secretInput.autocomplete = 'new-password';
      secretInput.setAttribute('data-1p-ignore', 'true');
      secretInput.setAttribute('data-lpignore', 'true');
      secretInput.setAttribute('data-form-type', 'other');
      secretInput.spellcheck = false;
      secretInput.value = '';
      secretInput.placeholder = row && row[field.name] ? '留空保持不变' : text(meta.placeholder, '请输入');
      return {
        node: secretInput,
        read: function () { return secretInput.value.trim(); }
      };
    }

    if (meta.type === 'select' || (field.choices && field.choices.length > 0)) {
      var options = meta.options || choiceOptions(field);
      var select = document.createElement('select');
      select.id = inputID;
      select.className = 'field-input';
      if (!field.required) {
        var blank = document.createElement('option');
        blank.value = '';
        blank.textContent = '（未设置）';
        select.appendChild(blank);
      }
      options.forEach(function (option) {
        var entry = document.createElement('option');
        entry.value = option.value;
        entry.textContent = option.label;
        select.appendChild(entry);
      });
      var current = value === null || value === undefined ? '' : String(value);
      var known = options.some(function (option) { return String(option.value) === current; });
      if (!known && current !== '') {
        // A value the console does not know is preserved rather than silently
        // replaced, so opening the form cannot rewrite the configuration.
        var unknown = document.createElement('option');
        unknown.value = current;
        unknown.textContent = current + '（未识别）';
        select.appendChild(unknown);
      }
      select.value = current;
      return {
        node: select,
        read: function () {
          // "Not set" on an optional dropdown omits the field, so the column
          // default applies rather than an explicit NULL overwriting it.
          if (select.value === '' && !field.required) { return undefined; }
          return select.value;
        }
      };
    }

    if (meta.type === 'reference') {
      return createReferenceInput(resource, field, meta, inputID, row);
    }

    if (meta.type === 'id_list') {
      var listArea = document.createElement('textarea');
      listArea.id = inputID;
      listArea.className = 'field-input mono';
      listArea.rows = 2;
      listArea.placeholder = text(meta.placeholder, '');
      listArea.value = value === null || value === undefined ? '' : value;
      return {
        node: listArea,
        read: function () {
          var raw = listArea.value.trim();
          if (raw === '') { return null; }
          var parsed = parseJSON(raw);
          if (!Array.isArray(parsed)) { throw new FieldError('必须是 JSON 数组，例如 [1, 2]'); }
          return raw;
        }
      };
    }

    if (meta.type === 'json' || isJSONKind(field.kind)) {
      var area = document.createElement('textarea');
      area.id = inputID;
      area.className = 'field-input mono';
      area.rows = 3;
      area.placeholder = text(meta.placeholder, '');
      area.value = value === null || value === undefined ? '' : value;
      return {
        node: area,
        read: function () {
          var raw = area.value.trim();
          if (raw === '') { return null; }
          var parsed = parseJSON(raw);
          if (parsed === null || (typeof parsed !== 'object')) {
            throw new FieldError('必须是合法的 JSON，例如 ' + text(meta.placeholder, '{}'));
          }
          return raw;
        }
      };
    }

    if (meta.type === 'datetime' || field.kind === 'time') {
      var dateInput = document.createElement('input');
      dateInput.id = inputID;
      dateInput.className = 'field-input';
      dateInput.type = 'datetime-local';
      dateInput.value = localInputValue(value);
      return {
        node: dateInput,
        read: function () {
          if (!dateInput.value) { return ''; }
          var parsed = new Date(dateInput.value);
          if (isNaN(parsed.getTime())) { throw new FieldError('时间格式不正确'); }
          return parsed.toISOString();
        }
      };
    }

    if (field.kind === 'bool') {
      var toggle = document.createElement('input');
      toggle.id = inputID;
      toggle.type = 'checkbox';
      toggle.className = 'field-checkbox';
      // An untouched flag on a new row shows the schema default, so creating a
      // row does not switch on something the gateway would have left off.
      toggle.checked = row ? value === true : field.default === true;
      var toggleLine = element('label', 'checkline');
      toggleLine.appendChild(toggle);
      toggleLine.appendChild(element('span', '', meta.label || field.name));
      return {
        node: toggleLine,
        read: function () { return toggle.checked; }
      };
    }

    var input = document.createElement('input');
    input.id = inputID;
    input.className = 'field-input';
    input.type = (field.kind === 'int' || field.kind === 'real') || meta.type === 'number' ? 'number' : 'text';
    if (input.type === 'number') {
      input.step = meta.step || (field.kind === 'int' ? '1' : 'any');
    }
    input.placeholder = text(meta.placeholder, '');
    input.value = value === null || value === undefined ? '' : value;
    return {
      node: input,
      read: function () {
        var raw = input.value.trim();
        if (raw === '') { return ''; }
        if (input.type === 'number') {
          var number = Number(raw);
          if (!isFinite(number)) { throw new FieldError('必须是数字'); }
          return number;
        }
        return raw;
      }
    };
  }

  /* createReferenceInput offers the existing rows of another resource as a
   * dropdown, so an operator picks a route by name instead of typing its id. */
  function createReferenceInput(resource, field, meta, inputID, row) {
    var select = document.createElement('select');
    select.id = inputID;
    select.className = 'field-input';
    var blank = document.createElement('option');
    blank.value = '';
    blank.textContent = field.required ? '（请选择）' : '（未设置）';
    select.appendChild(blank);

    function fill() {
      var current = select.value;
      select.textContent = '';
      select.appendChild(blank);
      var target = meta.resource;
      resourceRows(target).forEach(function (candidate) {
        var option = document.createElement('option');
        option.value = String(candidate.id);
        option.textContent = referenceLabel(target, candidate.id);
        select.appendChild(option);
      });
      if (current) { select.value = current; }
    }
    fill();

    var value = row ? row[field.name] : '';
    select.value = value === null || value === undefined ? '' : String(value);

    // A token belongs to an account, so the list is narrowed to the account
    // chosen above; the server refuses a mismatch either way.
    if (meta.dependsOn) {
      var accountSelect = document.querySelector('[data-field="' + meta.dependsOn + '"] select');
      if (accountSelect) {
        accountSelect.addEventListener('change', function () {
          var accountID = accountSelect.value;
          Array.prototype.slice.call(select.querySelectorAll('option')).forEach(function (option) {
            if (!option.value) { return; }
            var token = rowByID('tokens', option.value);
            option.hidden = !!(accountID && token && String(token.account_id) !== String(accountID));
          });
          var selected = rowByID('tokens', select.value);
          if (accountID && selected && String(selected.account_id) !== String(accountID)) {
            select.value = '';
          }
        });
        accountSelect.dispatchEvent(new Event('change'));
      }
    }

    return {
      node: select,
      read: function () { return select.value === '' ? '' : Number(select.value); }
    };
  }

  function isJSONKind(kind) {
    return kind === 'json' || kind === 'json_array' || kind === 'json_object';
  }

  function FieldError(message) {
    this.message = message;
  }

  function submitEditor(event) {
    event.preventDefault();
    if (!state.editor) { return; }
    var editor = state.editor;
    var optionalSecrets = (RESOURCES[editor.resource] || {}).optionalSecrets || [];
    var values = {};

    clearInvalid(els.editorFields);
    for (var index = 0; index < editor.fields.length; index++) {
      var field = editor.fields[index];
      var value;
      try {
        value = field.read();
      } catch (error) {
        markInvalid(field.wrapper);
        showFeedback(els.editorFeedback, (labelFor(field.name) || field.name) + '：' + error.message, 'error');
        field.input.focus();
        return;
      }
      if (field.required && (value === '' || value === null || value === undefined)) {
        // An untouched secret keeps its stored value, and the gateway fills in a
        // secret it can mint, so neither is a missing required field.
        if (field.secret && (editor.id || optionalSecrets.indexOf(field.name) !== -1)) { continue; }
        markInvalid(field.wrapper);
        showFeedback(els.editorFeedback, '「' + (labelFor(field.name) || field.name) + '」必须填写。', 'error');
        field.input.focus();
        return;
      }
      if (value !== undefined) { values[field.name] = value; }
    }

    var path = '/management/configuration/' + editor.resource;
    var call = editor.id ? put(path + '/' + editor.id, values) : post(path, values);
    setSubmitting(els.editorSubmit, true, '保存中…');
    hideFeedback(els.editorFeedback);

    call.then(function (result) {
      setSubmitting(els.editorSubmit, false, '保存');
      if (result.ok) {
        if (result.body && result.body.configuration) { applyConfiguration(result.body.configuration); }
        closeEditor();
        renderManagement();
        var generated = result.body && result.body.generated;
        if (generated && generated.key) {
          openSecret(generated.key, '客户端密钥已创建。请立即复制保存，关闭后只能重新轮换。');
        } else {
          showNotice((editor.id ? '已保存' : '已创建') + '：' + describeResult(result.body, editor.resource));
        }
        refresh();
        return;
      }
      if (result.status === 401) {
        closeEditor();
        showAuth('登录状态已过期，请重新登录后继续。');
        return;
      }
      var field = fieldErrorField(result.body);
      if (field) {
        var wrapper = els.editorFields.querySelector('[data-field="' + field + '"]');
        if (wrapper) {
          markInvalid(wrapper);
          var focusable = wrapper.querySelector('input, select, textarea');
          if (focusable) { focusable.focus(); }
        }
      }
      showFeedback(els.editorFeedback, errorMessage(result.body, '网关拒绝了这次修改。'), 'error');
    }).catch(function (err) {
      setSubmitting(els.editorSubmit, false, '保存');
      showFeedback(els.editorFeedback, '无法连接网关：' + String(err && err.message ? err.message : err), 'error');
    });
  }

  function describeResult(body, resource) {
    var row = body && body.row;
    var metadata = RESOURCES[resource];
    if (row && metadata) { return metadata.describe(row); }
    return RESOURCE_NOUNS[resource] || resource;
  }

  function deleteRow(resource, id) {
    return remove('/management/configuration/' + resource + '/' + id).then(function (result) {
      if (result.ok) {
        if (result.body && result.body.configuration) { applyConfiguration(result.body.configuration); }
        renderManagement();
        showNotice('已删除' + describeCascade(result.body));
        refresh();
        return null;
      }
      if (result.status === 401) {
        showAuth('登录状态已过期，请重新登录后继续。');
        return null;
      }
      throw new Error(errorMessage(result.body, '删除失败。'));
    });
  }

  function describeCascade(body) {
    var cascaded = body && body.cascaded;
    if (!cascaded) { return '。'; }
    var parts = [];
    Object.keys(cascaded).forEach(function (resource) {
      if (cascaded[resource] > 0) {
        parts.push('，同时移除 ' + cascaded[resource] + ' 条' + (RESOURCE_NOUNS[resource] || resource));
      }
    });
    return parts.length ? parts.join('') + '。' : '。';
  }

  function rotateKey(id) {
    return post('/management/configuration/keys/' + id + '/rotate').then(function (result) {
      if (result.ok) {
        if (result.body && result.body.configuration) { applyConfiguration(result.body.configuration); }
        renderManagement();
        var generated = result.body && result.body.generated;
        if (generated && generated.key) {
          openSecret(generated.key, '密钥已轮换。请立即复制保存，旧密钥已经失效。');
        }
        refresh();
        return null;
      }
      if (result.status === 401) {
        showAuth('登录状态已过期，请重新登录后继续。');
        return null;
      }
      throw new Error(errorMessage(result.body, '轮换失败。'));
    });
  }

  /* ===== Confirm and secret dialogs ===== */

  function openConfirm(options) {
    state.confirm = options;
    els.confirmTitle.textContent = options.title;
    els.confirmText.textContent = options.text;
    els.confirmSubmit.querySelector('.btn-label').textContent = options.confirmLabel;
    els.confirmSubmit.className = 'btn ' + (options.danger ? 'btn-danger' : 'btn-primary');
    hideFeedback(els.confirmFeedback);
    els.confirmSubmit.disabled = false;
    els.confirmOverlay.hidden = false;
    els.confirmSubmit.focus();
  }

  function closeConfirm() {
    els.confirmOverlay.hidden = true;
    state.confirm = null;
  }

  function runConfirm() {
    if (!state.confirm) { return; }
    var options = state.confirm;
    els.confirmSubmit.disabled = true;
    options.run().then(function () {
      closeConfirm();
    }).catch(function (err) {
      els.confirmSubmit.disabled = false;
      showFeedback(els.confirmFeedback, String(err && err.message ? err.message : err), 'error');
    });
  }

  function openSecret(value, message) {
    els.secretText.textContent = message;
    els.secretValue.value = value;
    hideFeedback(els.secretFeedback);
    els.secretOverlay.hidden = false;
    els.secretValue.select();
  }

  function closeSecret() {
    els.secretValue.value = '';
    els.secretOverlay.hidden = true;
  }

  function copySecret() {
    var value = els.secretValue.value;
    if (!value) { return; }
    var done = function () { showFeedback(els.secretFeedback, '已复制到剪贴板。', 'success'); };
    var failed = function () {
      els.secretValue.select();
      showFeedback(els.secretFeedback, '浏览器不允许自动复制，请手动复制选中的内容。', 'error');
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(value).then(done).catch(failed);
      return;
    }
    try {
      els.secretValue.select();
      document.execCommand('copy');
      done();
    } catch (error) {
      failed();
    }
  }

  /* ===== Feedback helpers ===== */

  function showBanner(title, detail) {
    els.bannerTitle.textContent = title;
    els.bannerDetail.textContent = detail || '';
    els.banner.hidden = false;
  }

  function hideBanner() {
    els.banner.hidden = true;
  }

  function showNotice(message) {
    els.noticeText.textContent = message;
    els.notice.hidden = false;
    window.clearTimeout(showNotice.timer);
    showNotice.timer = window.setTimeout(function () { els.notice.hidden = true; }, 8000);
  }

  function showFeedback(node, message, kind) {
    node.hidden = !message;
    node.textContent = message || '';
    node.className = 'settings-feedback' + (kind ? ' is-' + kind : '');
  }

  function hideFeedback(node) { showFeedback(node, '', ''); }

  function setSubmitting(button, submitting, idleLabel) {
    button.disabled = submitting;
    button.querySelector('.btn-label').textContent = submitting ? '处理中…' : idleLabel;
  }

  function markInvalid(wrapper) {
    wrapper.classList.add('is-invalid');
  }

  function clearInvalid(container) {
    var marked = container.querySelectorAll('.is-invalid');
    for (var index = 0; index < marked.length; index++) {
      marked[index].classList.remove('is-invalid');
    }
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

  function setSignInSubmitting(submitting) {
    els.authSubmit.disabled = submitting;
    els.authSubmit.querySelector('.btn-label').textContent = submitting ? '登录中…' : '登录';
  }

  function signInError(message) {
    setSignInSubmitting(false);
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
      setSignInSubmitting(true);

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
    els.passwordCancel.addEventListener('click', function () {
      showView('overview');
      refresh();
    });

    els.nav.addEventListener('click', function (event) {
      var button = event.target.closest('[data-view]');
      if (!button) { return; }
      showView(button.getAttribute('data-view'));
      refresh();
    });

    document.querySelectorAll('[data-create]').forEach(function (button) {
      button.addEventListener('click', function () {
        openEditor(button.getAttribute('data-create'), null);
      });
    });

    els.refresh.addEventListener('click', refresh);
    els.signout.addEventListener('click', signOut);
    els.bannerRetry.addEventListener('click', refresh);
    els.modelFilter.addEventListener('input', function () {
      renderModels(state.snapshot && state.snapshot.models ? state.snapshot.models : [], els.modelFilter.value);
    });

    els.editorForm.addEventListener('submit', submitEditor);
    els.editorClose.addEventListener('click', closeEditor);
    els.editorCancel.addEventListener('click', closeEditor);
    els.confirmSubmit.addEventListener('click', runConfirm);
    els.confirmCancel.addEventListener('click', closeConfirm);
    els.secretCopy.addEventListener('click', copySecret);
    els.secretClose.addEventListener('click', closeSecret);
    els.noticeDismiss.addEventListener('click', function () { els.notice.hidden = true; });

    // Escape closes whichever dialog is on top, which is what a keyboard user
    // expects from a modal.
    document.addEventListener('keydown', function (event) {
      if (event.key !== 'Escape') { return; }
      if (!els.editorOverlay.hidden) { closeEditor(); return; }
      if (!els.confirmOverlay.hidden) { closeConfirm(); return; }
      if (!els.secretOverlay.hidden) { closeSecret(); }
    });

    document.addEventListener('visibilitychange', function () {
      // Only the live dashboard refreshes. Asking whether the app and the
      // overview are both visible is what keeps a returning tab from firing an
      // unauthenticated request and reporting a session that never existed.
      if (document.hidden || els.app.hidden) { return; }
      if (state.view === 'overview') { refresh(); }
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
