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

  /* 管理视图 = 概览之外的视图，它们读取的是可写的配置清单。设置页也读取配置，
   * 因为运行策略和配置一起返回。 */
  var VIEWS = ['overview', 'upstream', 'routes', 'keys', 'settings'];
  var MANAGEMENT_VIEWS = { upstream: true, routes: true, keys: true, settings: true };

  /* The settings view groups its panels by function and shows one group at a
   * time, so no single page carries the account, the runtime policy and the
   * proxies at once. */
  var SETTINGS_VIEWS = ['account', 'policy', 'proxies'];

  var state = {
    view: 'overview',
    settingsView: 'account',
    snapshot: null,
    configuration: null,
    configurationError: null,
    username: '',
    channelNames: {},
    timer: null,
    countdown: 0,
    loading: false,
    editor: null,
    confirm: null,
    policySignature: null
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
    els.settingsNav = $('settings-nav');
    els.settingsPanels = document.querySelectorAll('[data-settings-panel]');
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

    els.policyPanels = $('policy-panels');
    els.breakersResetAll = $('breakers-reset-all');
    els.routingBody = document.querySelector('[data-body="routing"]');
    els.routingEmpty = document.querySelector('[data-empty="routing"]');
    els.routingMeta = document.querySelector('[data-panel="routing"] [data-meta]');
    els.routingTable = document.querySelector('[data-table="routing"]');

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
    key_mode: { available_first: '可用优先', round_robin: '轮询' },
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

  var KEY_MODE_OPTIONS = [
    { value: 'available_first', label: '可用优先' },
    { value: 'round_robin', label: '轮询' }
  ];

  function keyModeLabel(value) {
    return value === 'available_first' ? '可用优先' : '轮询';
  }

  /* 一条线路（一个上游密钥）的密钥模式，来自它所在路由的策略：可用优先的
   * 上游让第一个密钥保持最高优先级，轮询的上游让所有密钥同权。 */
  function channelKeyModeLabel(strategy) {
    if (strategy === 'stable_first') { return '可用优先'; }
    if (strategy === 'round_robin') { return '轮询'; }
    return '加权';
  }

  /* FIELDS[resource][column]
   *
   * 「上游」的表单是自建的（密钥要一行一个、模型要能拉取和勾选），这里仍然保留
   * 它的字段标签，因为字段级报错要用它们指出是哪一个输入。 */
  var FIELDS = {
    upstreams: {
      name: { label: '名称', placeholder: '例如 OpenAI 官方' },
      url: {
        label: 'API 地址', placeholder: 'https://api.example.com', normalize: normalizeAPIAddress,
        help: '上游服务的根地址。只填到域名时会自动补上 /v1，结尾多余的 / 会去掉；上游挂在子路径下时请把子路径写上（例如 https://example.com/openai）。'
      },
      global_weight: { label: '权重', type: 'number', step: 'any', placeholder: '1', help: '权重越大分到的请求越多；同一个模型的多个上游之间按权重分配。' },
      custom_headers: { label: '请求头', type: 'json', placeholder: '{"X-Custom": "value"}', help: 'JSON 对象，会附加到发往这个上游的每个请求上。' },
      proxy_url: { label: '代理', placeholder: 'http://127.0.0.1:7890', help: '留空表示直连；填 system 表示使用系统代理。获取模型和转发请求都走这个出口。' },
      status: { label: '状态', type: 'select', options: STATUS_OPTIONS, help: '停用后该上游的线路不再参与选路。' },
      keys: { label: '密钥', help: '一行一个，可以整段粘贴。留空表示保持已保存的密钥不变。' },
      key_mode: { label: '密钥模式', type: 'select', options: KEY_MODE_OPTIONS, help: '可用优先：先用第一个密钥，失败或熔断后再用下一个。轮询：按权重在所有密钥之间分配。' },
      models: { label: '模型', help: '勾选这个上游提供的模型，保存后网关自动建立路由。' },
      model_mapping: { label: '模型映射', help: '把客户端请求的模型名换成上游认识的模型名。' }
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
      supported_models: {
        label: '排除模型', type: 'choice_list', filterPlaceholder: '筛选模型…',
        help: '勾选要拒绝的模型：命中的模型会被拒绝，也不会出现在 /v1/models 中。也支持手填一条模式（例如 re:^o1）。',
        manual: { placeholder: '手动添加模型名或 re: 模式', action: '添加' },
        candidates: modelCandidates, describe: function (value) { return String(value); },
        emptyText: '网关还没有可排除的模型：先在上游里勾选模型。'
      },
      allowed_route_ids: {
        label: '限定路由', type: 'choice_list', filterPlaceholder: '筛选路由…',
        help: '勾选后只允许这些路由；一个都不勾表示不限制。',
        candidates: routeCandidates, describe: function (value) { return referenceLabel('routes', value); },
        emptyText: '网关里还没有路由：先在上游里勾选模型，路由会自动建立。'
      },
      excluded_site_ids: {
        label: '排除上游', type: 'choice_list', filterPlaceholder: '筛选上游…',
        help: '勾选后这些上游的线路不会被选中。',
        candidates: upstreamCandidates, describe: function (value) { return referenceLabel('sites', value); },
        emptyText: '还没有上游。'
      },
      site_weight_multipliers: { label: '上游权重系数', type: 'json', placeholder: '{"3": 2}', help: 'JSON 对象，键是上游 ID，值是大于 0 的倍数。' },
      excluded_credential_refs: {
        label: '排除凭据', type: 'json',
        placeholder: '[{"kind":"account_token","siteId":1,"accountId":2,"tokenId":3}]',
        help: 'JSON 数组，精确排除某个上游密钥对应的线路。'
      }
    },
    proxies: {
      name: { label: '名称', placeholder: '例如 本地出口' },
      protocol: { label: '协议', type: 'select' },
      url: { label: '代理地址', placeholder: 'http://127.0.0.1:7890', help: '支持的协议：http、https、socks5、socks5h。' },
      is_default: { label: '设为默认代理', help: '没有其他代理设置生效时使用它。' },
      enabled: { label: '启用' }
    }
  };

  /* RESOURCES[resource] describes the management table of a resource. */
  var RESOURCES = {
    upstreams: {
      title: '上游',
      create: '添加上游',
      noun: '上游',
      columns: [
        { label: '名称', cell: function (row) { return element('span', 'cell-strong', text(row.name)); } },
        { label: 'API 地址', cell: function (row) { return element('span', 'mono', text(row.url)); } },
        { label: '权重', className: 'num', cell: function (row) { return text(row.global_weight, '1'); } },
        { label: '密钥', cell: function (row) { return upstreamKeysCell(row); } },
        { label: '模型', cell: function (row) { return upstreamModelsCell(row); } },
        { label: '状态', cell: function (row) { return statusBadge(row.status); } }
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
    }
  };

  function upstreamKeyList(row) {
    var keys = parseJSON(row ? row.keys : null);
    return Array.isArray(keys) ? keys : [];
  }

  function upstreamKeyMode(row) {
    return row.key_mode === 'available_first' ? 'available_first' : 'round_robin';
  }

  function upstreamKeysCell(row) {
    var wrapper = element('span', 'tag-list');
    var keys = upstreamKeyList(row);
    wrapper.appendChild(tag(keys.length + ' 个'));
    wrapper.appendChild(tag(keyModeLabel(upstreamKeyMode(row))));
    return wrapper;
  }

  function upstreamModelsCell(row) {
    var models = parseJSON(row.models);
    if (!Array.isArray(models) || models.length === 0) {
      return element('span', 'cell-muted', '未选择');
    }
    var count = tag(String(models.length));
    count.title = models.join('\n');
    return count;
  }

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

  /* 排除模型、限定路由、排除上游这三项都是从一个列表里挑，所以候选来自网关已经
   * 读到的配置，而不是让 operator 手写 JSON。 */

  function upstreamCandidates() {
    return resourceRows('upstreams').map(function (row) {
      var models = parseJSON(row.models);
      var count = Array.isArray(models) ? models.length : 0;
      return {
        value: row.id,
        label: text(row.name, '#' + row.id),
        detail: '#' + row.id + ' · ' + count + ' 个模型'
      };
    });
  }

  function routeCandidates() {
    return resourceRows('routes').map(function (row) {
      var detail = '#' + row.id;
      if (row.model_pattern) { detail += ' · ' + row.model_pattern; }
      return {
        value: row.id,
        label: text(row.display_name, text(row.model_pattern, '#' + row.id)),
        detail: detail
      };
    });
  }

  /* 可排除的模型：网关当前在路由的模型，加上上游里勾选的模型和路由上的匹配模式
   * （路由模式可能是 re: 形式，按模式整体排除也是合理的用法）。 */
  function modelCandidates() {
    var seen = {};
    var candidates = [];
    function add(value) {
      var name = String(value === null || value === undefined ? '' : value).trim();
      if (name === '' || seen[name]) { return; }
      seen[name] = true;
      candidates.push({ value: name, label: name });
    }
    var routed = state.configuration && state.configuration.models;
    if (Array.isArray(routed)) { routed.forEach(add); }
    resourceRows('routes').forEach(function (row) { add(row.model_pattern); });
    resourceRows('upstreams').forEach(function (row) {
      var models = parseJSON(row.models);
      if (Array.isArray(models)) { models.forEach(add); }
    });
    candidates.sort(function (left, right) {
      return left.value < right.value ? -1 : (left.value > right.value ? 1 : 0);
    });
    return candidates;
  }

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

  /* ===== API address =====
   *
   * 一个上游的地址有好几种写法：https://host、https://host/、https://host/v1、
   * https://host/v1/，还有从文档里整段复制来的 https://host/v1/chat/completions。
   * 网关探测模型列表时能分辨这些写法，这里再把它们归成一个规范形式存下来，operator
   * 看到的和探测、转发用的是同一个地址。 */

  var ENDPOINT_SUFFIX = /\/(chat\/completions|completions|responses|messages|models)$/i;

  function normalizeAPIAddress(value) {
    var raw = String(value === null || value === undefined ? '' : value).trim();
    if (raw === '') { return ''; }
    // 粘贴完整接口地址时退回到版本根，例如 /v1/chat/completions 变成 /v1。
    var trimmed = raw.replace(/\/+$/, '').replace(ENDPOINT_SUFFIX, '').replace(/\/+$/, '');
    var parsed = parseAddress(trimmed);
    if (!parsed || parsed.search || parsed.hash) { return trimmed; }
    // 只填到域名时补上 /v1；写了别的路径就按写的来，上游可能挂在子路径下。
    if (parsed.pathname === '' || parsed.pathname === '/') { return trimmed + '/v1'; }
    return trimmed;
  }

  function parseAddress(value) {
    try {
      return new URL(value);
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
      showSettingsView(state.settingsView);
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

  /* showSettingsView switches the group of settings on screen. The password
   * form is not reset here: an operator who steps over to the proxies and comes
   * back finds what they had typed, and only entering the settings view afresh
   * starts the form over. */
  function showSettingsView(name) {
    if (SETTINGS_VIEWS.indexOf(name) === -1) { name = 'account'; }
    state.settingsView = name;
    for (var index = 0; index < els.settingsPanels.length; index++) {
      var panel = els.settingsPanels[index];
      panel.hidden = panel.getAttribute('data-settings-panel') !== name;
    }
    var buttons = els.settingsNav.querySelectorAll('[data-settings-view]');
    for (var buttonIndex = 0; buttonIndex < buttons.length; buttonIndex++) {
      var button = buttons[buttonIndex];
      var active = button.getAttribute('data-settings-view') === name;
      button.classList.toggle('is-active', active);
      button.setAttribute('aria-current', active ? 'page' : 'false');
    }
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
    request_too_large: '提交的内容过大。',
    // 运行策略与上游探测
    upstream_probe_failed: '获取模型失败，请检查 API 地址和密钥。',
    upstream_unreachable: '无法连接该上游。',
    breakers_not_supported: '本网关没有可恢复的熔断记录。'
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
    },
    // 探测上游模型列表时的原因码，用于把失败说清楚。
    invalid_request: function () { return '请先填写 API 地址，并至少填一个密钥。'; },
    invalid_proxy: function (field, params) {
      return '代理地址不可用：' + text(params.detail, '请检查「代理」这一栏或设置页里的默认代理。');
    },
    no_model_endpoint: function (field, params) {
      var tried = Array.isArray(params.endpoints) && params.endpoints.length
        ? '（已尝试 ' + params.endpoints.join('、') + '）' : '';
      return '该上游没有可用的模型列表接口' + tried + '，请检查 API 地址，或手动添加模型名。';
    },
    credential_rejected: function (field, params) { return '上游拒绝了该密钥（HTTP ' + params.status + '），请检查密钥是否正确。'; },
    upstream_status: function (field, params) { return '上游返回了 HTTP ' + params.status + '。'; },
    unreadable_response: function () { return '上游返回的不是模型列表，请手动添加模型名。'; },
    unreachable: function (field, params) {
      var where = [];
      if (params.endpoint) { where.push('尝试地址 ' + params.endpoint); }
      var outbound = proxyPhrase(params);
      if (outbound) { where.push('出口 ' + outbound); }
      var detail = params.detail ? ' 网关的报错：' + params.detail : '';
      return '无法连接该上游' + (where.length ? '（' + where.join('，') + '）' : '') +
        '，请检查地址、代理和网络。' + detail;
    }
  };

  /* proxyPhrase names the outbound the gateway used, from the stable source code
   * the probe reports. */
  function proxyPhrase(params) {
    switch (params.proxy_source) {
      case 'system': return '系统代理';
      case 'direct': return '直连';
      case 'key':
      case 'site':
      case 'default':
        return params.proxy_url ? '代理 ' + params.proxy_url : '代理';
      default:
        return params.proxy_url ? '代理 ' + params.proxy_url : '';
    }
  }

  /* 配置类型的中文名，用于「仍有 N 条××引用」这类提示。 */
  var RESOURCE_NOUNS = {
    upstreams: '上游', sites: '上游', accounts: '上游凭据', tokens: '上游密钥', routes: '路由',
    channels: '线路', keys: '客户端密钥', proxies: '代理'
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
        // 探测类错误的具体原因（凭据被拒、返回了非 JSON 等）来自网关，附在后面。
        if (error.message) { return message + '（' + error.message + '）'; }
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
      renderRouting();
      renderPolicy();
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

  function renderChannels(channels) {
    els.channelsBody.textContent = '';
    els.channelsMeta.textContent = '已配置 ' + channels.length + ' 条线路';

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
      cell(row, tag(channelKeyModeLabel(channel.routing_strategy)));

      var proxyVariant = channel.proxy_source === 'direct' ? 'muted' : 'info';
      cell(row, badge(enumLabel('proxy_source', channel.proxy_source, 'direct'), proxyVariant));

      els.channelsBody.appendChild(row);
    });
  }

  /* renderBreakers lists the recorded circuits. A tripped circuit can be cleared
   * from here, which is the manual half of recovery: a channel cooled down for
   * another fifteen minutes returns to service as soon as an operator who has
   * fixed the upstream says so. */
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

      var actionCell = document.createElement('td');
      actionCell.className = 'actions';
      if (open || entry.consecutive_failures > 0) {
        var restore = busyButton('btn btn-ghost btn-small', '恢复');
        restore.addEventListener('click', function () { resetBreaker(entry, restore); });
        actionCell.appendChild(restore);
      }
      row.appendChild(actionCell);

      els.breakersBody.appendChild(row);
    });
  }

  /* resetBreaker clears one recorded circuit, by the identifiers the breaker
   * filed it under. */
  function resetBreaker(entry, button) {
    setSubmitting(button, true, '恢复');
    post('/management/breakers/reset', {
      scope: entry.scope || 'channel',
      channel_id: entry.channel_id || '',
      key_id: entry.key_id || '',
      model: entry.model || ''
    }).then(function (result) {
      setSubmitting(button, false, '恢复');
      if (result.ok) {
        showNotice('已恢复该线路，下一个请求会重新尝试它。');
        refresh();
        return;
      }
      if (result.status === 401) { showAuth('登录状态已过期，请重新登录后继续。'); return; }
      showBanner('恢复失败', errorMessage(result.body, '网关拒绝了这次恢复。'));
    }).catch(function (err) {
      setSubmitting(button, false, '恢复');
      showBanner('无法连接网关', String(err && err.message ? err.message : err));
    });
  }

  function resetAllBreakers() {
    setSubmitting(els.breakersResetAll, true, '全部恢复');
    post('/management/breakers/reset', { scope: 'all' }).then(function (result) {
      setSubmitting(els.breakersResetAll, false, '全部恢复');
      if (result.ok) {
        var count = result.body && typeof result.body.reset === 'number' ? result.body.reset : 0;
        showNotice(count > 0 ? '已恢复 ' + count + ' 条熔断记录。' : '没有需要恢复的熔断记录。');
        refresh();
        return;
      }
      if (result.status === 401) { showAuth('登录状态已过期，请重新登录后继续。'); return; }
      showBanner('恢复失败', errorMessage(result.body, '网关拒绝了这次恢复。'));
    }).catch(function (err) {
      setSubmitting(els.breakersResetAll, false, '全部恢复');
      showBanner('无法连接网关', String(err && err.message ? err.message : err));
    });
  }

  /* renderRouting shows the routing table the gateway derived from the upstreams:
   * one row per model, with the upstreams that serve it and the name each of them
   * knows it by. It is read-only because it is not configured — it is what the
   * model selection on the upstream page produces. */
  function renderRouting() {
    if (!els.routingBody) { return; }
    els.routingBody.textContent = '';
    var rows = routingRows();
    els.routingMeta.textContent = rows.length + ' 个模型';

    var empty = els.routingEmpty;
    if (state.configuration === null) {
      els.routingTable.hidden = true;
      empty.hidden = false;
      setEmptyText(empty, 'routing', state.configurationError || '尚未读取配置。');
      return;
    }
    if (rows.length === 0) {
      els.routingTable.hidden = false;
      empty.hidden = false;
      setEmptyText(empty, 'routing', '还没有可路由的模型。到「上游」页添加一个上游并选中模型。');
      return;
    }
    els.routingTable.hidden = false;
    empty.hidden = true;
    setEmptyText(empty, 'routing', '还没有可路由的模型。到「上游」页添加一个上游并选中模型。');

    var head = els.routingTable.querySelector('thead');
    if (head) { head.textContent = ''; } else {
      head = document.createElement('thead');
      els.routingTable.insertBefore(head, els.routingTable.firstChild);
    }
    var headerRow = document.createElement('tr');
    ['模型', '上游', '上游模型名', '匹配方式', '线路数', '状态'].forEach(function (label) {
      var th = document.createElement('th');
      th.textContent = label;
      headerRow.appendChild(th);
    });
    head.appendChild(headerRow);

    rows.forEach(function (entry) {
      var row = document.createElement('tr');
      cell(row, element('span', 'cell-strong mono', entry.model));
      cell(row, entry.upstreams.length ? entry.upstreams.join('、') : '—');
      cell(row, entry.targets.length ? entry.targets.join('、') : '与左侧一致');
      cell(row, tag(entry.mode));
      cell(row, String(entry.lines), 'num');
      cell(row, enabledBadge(entry.enabled));
      els.routingBody.appendChild(row);
    });
  }

  function routingRows() {
    var routes = resourceRows('routes');
    var channels = resourceRows('channels');
    var rows = [];
    routes.forEach(function (route) {
      var model = String(route.display_name || '').trim() || String(route.model_pattern || '').trim();
      if (!model) { return; }
      var upstreams = [];
      var targets = [];
      var lines = 0;
      channels.forEach(function (channel) {
        if (String(channel.route_id) !== String(route.id)) { return; }
        lines += 1;
        var owner = upstreamNameOfChannel(channel);
        if (owner && upstreams.indexOf(owner) === -1) { upstreams.push(owner); }
        var target = String(channel.source_model || '').trim();
        if (target && target !== model && targets.indexOf(target) === -1) { targets.push(target); }
      });
      rows.push({
        model: model,
        upstreams: upstreams,
        targets: targets,
        lines: lines,
        enabled: route.enabled !== false,
        mode: routingMode(route)
      });
    });
    rows.sort(function (left, right) { return left.model < right.model ? -1 : (left.model > right.model ? 1 : 0); });
    return rows;
  }

  function upstreamNameOfChannel(channel) {
    var account = rowByID('accounts', channel.account_id);
    if (!account) { return ''; }
    return siteName(account.site_id);
  }

  function routingMode(route) {
    if (route.route_mode === 'explicit_group') { return '分组'; }
    var pattern = String(route.model_pattern || '');
    if (String(route.display_name || '').trim()) { return '别名'; }
    if (pattern.indexOf('re:') === 0) { return '正则'; }
    if (pattern.indexOf('*') !== -1 || pattern.indexOf('?') !== -1) { return '通配'; }
    return '精确';
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
    // The upstream form is the one form the generic renderer cannot build: its
    // keys are a list that is pasted in one go, and its models are picked from
    // what the upstream actually serves.
    if (resource === 'upstreams') {
      openUpstreamEditor(row);
      return;
    }
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

  /* ===== Upstream editor =====
   *
   * The upstream form keeps the same contract as the generic one — the gateway
   * says which fields exist, this file says how each is labelled and which
   * widget it gets — but two fields need more than an input:
   *
   *   keys    one key per line, so a batch copies in at once. The gateway shows
   *           stored keys masked; submitting a mask keeps the key it stands for.
   *   models  picked from what the upstream answers, each with the name that
   *           upstream knows it by. Saving them is what creates the routes.
   */
  function openUpstreamEditor(row) {
    var fields = state.configuration && state.configuration.fields
      ? state.configuration.fields.upstreams : null;
    if (!Array.isArray(fields) || fields.length === 0) {
      showBanner('无法编辑', '网关没有返回「上游」的字段定义。');
      return;
    }

    state.editor = { resource: 'upstreams', id: row ? row.id : 0, fields: [] };
    els.editorTitle.textContent = (row ? '编辑' : '添加') + '上游';
    els.editorNote.hidden = true;
    els.editorFields.textContent = '';
    hideFeedback(els.editorFeedback);
    setSubmitting(els.editorSubmit, false, '保存');

    fields.forEach(function (field) {
      // The mapping is edited per model inside the picker, not as raw JSON, and
      // the platform identifier the schema stores is not read by the gateway, so
      // neither belongs in a form an operator fills in by hand.
      if (field.name === 'model_mapping' || field.name === 'platform') { return; }
      if (field.name === 'models') {
        var picker = createModelPicker(row);
        els.editorFields.appendChild(picker.wrapper);
        state.editor.fields = state.editor.fields.concat(picker.fields);
        return;
      }
      if (field.name === 'keys') {
        var keys = createKeysField(field, row);
        els.editorFields.appendChild(keys.wrapper);
        state.editor.fields.push(keys);
        return;
      }
      var built = buildField('upstreams', field, row);
      if (!built) { return; }
      els.editorFields.appendChild(built.wrapper);
      state.editor.fields.push(built);
    });

    els.editorOverlay.hidden = false;
    var firstInput = els.editorFields.querySelector('input:not([type=hidden]), select, textarea');
    if (firstInput) { firstInput.focus(); }
  }

  /* createKeysField is the batch key input: one key per line. */
  function createKeysField(field, row) {
    var wrapper = element('div', 'field field-editor');
    wrapper.setAttribute('data-field', 'keys');
    var inputID = 'editor-field-upstreams-keys';
    var label = element('label', 'field-label', (FIELDS.upstreams.keys || {}).label || '密钥');
    label.setAttribute('for', inputID);
    wrapper.appendChild(label);

    var stored = upstreamKeyList(row);
    var area = document.createElement('textarea');
    area.id = inputID;
    area.className = 'field-input mono';
    area.rows = 4;
    area.spellcheck = false;
    area.placeholder = '每行一个密钥，可以一次粘贴多个';
    area.value = stored.join('\n');
    wrapper.appendChild(area);

    wrapper.appendChild(element('p', 'field-help', (FIELDS.upstreams.keys || {}).help || ''));
    if (stored.length > 0) {
      wrapper.appendChild(element('p', 'field-help',
        '已保存 ' + stored.length + ' 个密钥，上面显示的是末四位；按行序排列，留空表示保持不变。'));
    }

    return {
      name: 'keys', kind: 'json_array', required: false, input: area, wrapper: wrapper,
      read: function () {
        var lines = area.value.split('\n').map(function (line) { return line.trim(); })
          .filter(function (line) { return line !== ''; });
        if (lines.length === 0 && stored.length === 0) {
          throw new FieldError('至少填写一个密钥');
        }
        return lines;
      }
    };
  }

  /* createModelPicker builds the model section: fetch, filter, select, and name
   * the model the way the upstream knows it. */
  function createModelPicker(row) {
    var wrapper = element('div', 'field field-editor');
    wrapper.setAttribute('data-field', 'models');
    var heading = element('span', 'field-label', (FIELDS.upstreams.models || {}).label || '模型');
    wrapper.appendChild(heading);
    wrapper.appendChild(element('p', 'field-help',
      '勾选这个上游提供的模型；保存后网关会自动为它们建立路由。「上游模型名」填上游认识的写法，留空表示与模型名相同。'));

    var rows = [];
    var known = {};
    var filter = '';
    var storedMapping = parseJSON(row ? row.model_mapping : null);
    var storedModels = parseJSON(row ? row.models : null);
    if (Array.isArray(storedModels)) {
      storedModels.forEach(function (name) {
        addModel(String(name), true, mappingTarget(storedMapping, name));
      });
    }

    var toolbar = element('div', 'picker-toolbar');
    var fetchButton = busyButton('btn btn-ghost btn-small', '获取模型');
    fetchButton.addEventListener('click', function () { fetchModels(fetchButton); });
    var filterInput = document.createElement('input');
    filterInput.type = 'search';
    filterInput.className = 'field-input filter-input';
    filterInput.placeholder = '筛选模型…';
    filterInput.setAttribute('aria-label', '筛选模型');
    filterInput.addEventListener('input', function () {
      filter = filterInput.value.trim().toLowerCase();
      paint();
    });
    var status = element('span', 'picker-status', '已选 ' + rows.length + ' 个');
    toolbar.appendChild(fetchButton);
    toolbar.appendChild(filterInput);
    toolbar.appendChild(status);
    wrapper.appendChild(toolbar);

    var list = element('div', 'model-list');
    wrapper.appendChild(list);

    var manual = element('div', 'picker-manual');
    var manualInput = document.createElement('input');
    manualInput.type = 'text';
    manualInput.className = 'field-input';
    manualInput.placeholder = '手动添加一个模型名';
    var manualButton = busyButton('btn btn-ghost btn-small', '添加');
    manualButton.addEventListener('click', function () {
      var name = manualInput.value.trim();
      if (!name) { return; }
      addModel(name, true, '');
      manualInput.value = '';
      filter = '';
      filterInput.value = '';
      paint();
    });
    manualInput.addEventListener('keydown', function (event) {
      if (event.key === 'Enter') { event.preventDefault(); manualButton.click(); }
    });
    manual.appendChild(manualInput);
    manual.appendChild(manualButton);
    wrapper.appendChild(manual);

    paint();

    function addModel(name, checked, target) {
      name = String(name || '').trim();
      if (!name) { return; }
      if (known[name]) {
        if (checked) { known[name].checked = true; }
        if (target && !known[name].target) { known[name].target = target; }
        return;
      }
      var entry = { name: name, checked: checked === true, target: target || '' };
      known[name] = entry;
      rows.push(entry);
      rows.sort(function (left, right) { return left.name < right.name ? -1 : (left.name > right.name ? 1 : 0); });
    }

    function visibleRows() {
      if (!filter) { return rows; }
      return rows.filter(function (entry) { return entry.name.toLowerCase().indexOf(filter) !== -1; });
    }

    function paint() {
      list.textContent = '';
      var visible = visibleRows();
      if (visible.length === 0) {
        list.appendChild(element('p', 'field-help', rows.length === 0
          ? '还没有模型。填写 API 地址和密钥后点「获取模型」，也可以手动添加。'
          : '没有符合筛选条件的模型。'));
      }
      visible.forEach(function (entry) {
        var line = element('label', 'model-line');
        var box = document.createElement('input');
        box.type = 'checkbox';
        box.checked = entry.checked;
        box.addEventListener('change', function () {
          entry.checked = box.checked;
          updateStatus();
        });
        line.appendChild(box);
        line.appendChild(element('span', 'model-name mono', entry.name));
        var target = document.createElement('input');
        target.type = 'text';
        target.className = 'field-input model-target';
        target.placeholder = '上游模型名（可选）';
        target.value = entry.target;
        target.addEventListener('input', function () { entry.target = target.value.trim(); });
        line.appendChild(target);
        list.appendChild(line);
      });
      updateStatus();
    }

    function updateStatus() {
      var selected = rows.filter(function (entry) { return entry.checked; }).length;
      status.textContent = rows.length === 0
        ? ''
        : '已选 ' + selected + ' / ' + rows.length + ' 个';
    }

    function fetchModels(button) {
      var url = normalizeAPIAddress(editorFieldValue('url'));
      if (!url) {
        showFeedback(els.editorFeedback, '请先填写 API 地址。', 'error');
        return;
      }
      var headers = {};
      var rawHeaders = editorFieldValue('custom_headers');
      if (rawHeaders) {
        var parsedHeaders = parseJSON(rawHeaders);
        if (parsedHeaders && typeof parsedHeaders === 'object' && !Array.isArray(parsedHeaders)) {
          headers = parsedHeaders;
        }
      }
      setSubmitting(button, true, '获取模型');
      status.textContent = '正在向上游查询…';
      hideFeedback(els.editorFeedback);

      // 探测带上表单里的代理，走出去的路径和真实请求一致；密钥留空时网关用已保存
      // 的那个，代理留空时用默认代理。
      post('/management/upstreams/models', {
        id: state.editor ? state.editor.id : 0,
        url: url,
        key: firstKeyLine(),
        proxy_url: editorFieldValue('proxy_url'),
        headers: headers
      }).then(function (result) {
        setSubmitting(button, false, '获取模型');
        if (result.ok) {
          var models = (result.body && result.body.models) || [];
          models.forEach(function (name) { addModel(name, false, ''); });
          var endpoint = result.body && result.body.endpoint;
          // 选择计数由 picker 自己维护，探测结果放在反馈区：说清楚是从哪个地址
          // 拿到的，地址写错时一眼就能看出来。
          paint();
          showFeedback(els.editorFeedback,
            '上游返回 ' + models.length + ' 个模型' + (endpoint ? '（来自 ' + endpoint + '）' : '') + '，勾选要使用的模型。',
            'success');
          return;
        }
        status.textContent = '';
        if (result.status === 401) { showAuth('登录状态已过期，请重新登录后继续。'); return; }
        showFeedback(els.editorFeedback, errorMessage(result.body, '获取模型失败。'), 'error');
      }).catch(function (err) {
        setSubmitting(button, false, '获取模型');
        status.textContent = '';
        showFeedback(els.editorFeedback, '无法连接网关：' + String(err && err.message ? err.message : err), 'error');
      });
    }

    /* firstKeyLine is what the probe presents: the key the operator just typed,
     * or the mask of a stored one, which the gateway resolves itself. */
    function firstKeyLine() {
      var raw = editorFieldValue('keys');
      var lines = String(raw || '').split('\n');
      for (var index = 0; index < lines.length; index++) {
        if (lines[index].trim() !== '') { return lines[index].trim(); }
      }
      return '';
    }

    return {
      wrapper: wrapper,
      fields: [
        {
          name: 'models', kind: 'json_array', required: false, input: list, wrapper: wrapper,
          read: function () {
            return rows.filter(function (entry) { return entry.checked; })
              .map(function (entry) { return entry.name; });
          }
        },
        {
          name: 'model_mapping', kind: 'json_object', required: false, input: list, wrapper: wrapper,
          read: function () {
            var mapping = {};
            var count = 0;
            rows.forEach(function (entry) {
              if (!entry.checked || !entry.target || entry.target === entry.name) { return; }
              mapping[entry.name] = entry.target;
              count += 1;
            });
            return count === 0 ? null : mapping;
          }
        }
      ]
    };
  }

  function mappingTarget(mapping, name) {
    if (!mapping || typeof mapping !== 'object') { return ''; }
    var target = mapping[name];
    return typeof target === 'string' ? target.trim() : '';
  }

  /* editorFieldValue reads the current value of a rendered editor input, which is
   * how the model picker sees the address and the keys typed above it. */
  function editorFieldValue(name) {
    var wrapper = els.editorFields.querySelector('[data-field="' + name + '"]');
    if (!wrapper) { return ''; }
    var input = wrapper.querySelector('input, select, textarea');
    return input ? input.value : '';
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
      secretInput.setAttribute('data-bwignore', 'true');
      secretInput.setAttribute('data-form-type', 'other');
      secretInput.spellcheck = false;
      secretInput.value = '';
      secretInput.placeholder = row && row[field.name] ? '留空保持不变' : text(meta.placeholder, '请输入');
      // 浏览器和密码管理器会把本站保存的登录密码填进任何一个密码框，而这里填进去
      // 的会被当成上游/客户端密钥存下来。所以字段先保持只读，聚焦后才可输入，并且
      // 在没有任何输入的情况下出现的内容一律清掉：提交的就是 operator 亲手填的。
      keepUnfilled(secretInput);
      return {
        node: secretInput,
        read: function () { return secretInput.value.trim(); }
      };
    }

    if (meta.type === 'choice_list') {
      return createChoiceList(field, meta, row);
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
      // A create starts on the value the gateway would store for an omitted
      // field, so the form shows what is about to be saved instead of "not set".
      if (current === '' && !row && field.default_value !== undefined && field.default_value !== null) {
        current = String(field.default_value);
      }
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
    input.autocomplete = 'off';
    if (input.type === 'number') {
      input.step = meta.step || (field.kind === 'int' ? '1' : 'any');
    }
    input.placeholder = text(meta.placeholder, '');
    input.value = value === null || value === undefined ? '' : value;
    if (meta.normalize) {
      // The address is shown in the form the gateway stores it in, so the
      // operator sees what will be saved before saving it.
      input.addEventListener('blur', function () {
        var normalized = meta.normalize(input.value);
        if (normalized !== '' && normalized !== input.value) { input.value = normalized; }
      });
    }
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
        return meta.normalize ? meta.normalize(raw) : raw;
      }
    };
  }

  /* keepUnfilled keeps a credential field empty until the operator types in it.
   *
   * A browser or a password manager fills a password input as soon as the dialog
   * is rendered — with the console's own login password, since that is the
   * credential saved for this origin — and a value nobody typed would be stored
   * as an upstream or client key. The field is read-only until it is focused,
   * which is what makes both leave it alone, and anything that still appears
   * without a keystroke is cleared. */
  function keepUnfilled(input) {
    input.readOnly = true;
    input.addEventListener('focus', function () { input.readOnly = false; });
    input.addEventListener('blur', function () {
      if (input.value === '') { input.readOnly = true; }
    });
    window.setTimeout(function () {
      if (document.activeElement !== input) { input.value = ''; }
    }, 200);
  }

  /* createChoiceList is the multi-select field: a restriction is picked from the
   * candidates the console already holds instead of being typed as JSON. */
  function createChoiceList(field, meta, row) {
    var entries = [];
    var index = {};
    var filter = '';

    // A candidate keeps the type it arrived with — a route id stays a number,
    // because the gateway reads these lists as ids — while the string form is
    // what identifies an entry for selecting and filtering.
    function add(entry) {
      var key = entry.value === null || entry.value === undefined ? '' : String(entry.value).trim();
      if (key === '' || index[key]) { return; }
      index[key] = true;
      entries.push({
        value: entry.value,
        key: key,
        label: text(entry.label, key),
        detail: text(entry.detail, ''),
        checked: entry.checked === true
      });
    }

    var stored = parseJSON(row ? row[field.name] : null);
    var selected = Array.isArray(stored) ? stored : [];
    var wanted = {};
    selected.forEach(function (value) { wanted[String(value)] = true; });

    var candidates = typeof meta.candidates === 'function' ? meta.candidates() : [];
    candidates.forEach(function (candidate) {
      add({
        value: candidate.value,
        label: candidate.label,
        detail: candidate.detail,
        checked: wanted[String(candidate.value)] === true
      });
    });
    // A stored value with no candidate — a route that was deleted, a model that
    // was renamed — stays selected, so opening the form and saving cannot drop a
    // restriction the operator never touched.
    selected.forEach(function (value) {
      add({
        value: value,
        label: meta.describe ? meta.describe(value) : String(value),
        detail: '不在当前列表中',
        checked: true
      });
    });

    var wrapper = element('div', 'choice-picker');
    var toolbar = element('div', 'picker-toolbar');
    var filterInput = document.createElement('input');
    filterInput.type = 'search';
    filterInput.className = 'field-input filter-input';
    filterInput.placeholder = text(meta.filterPlaceholder, '筛选…');
    filterInput.setAttribute('aria-label', text(meta.filterPlaceholder, '筛选'));
    filterInput.addEventListener('input', function () {
      filter = filterInput.value.trim().toLowerCase();
      paint();
    });
    var clearButton = busyButton('btn btn-ghost btn-small', '清空');
    clearButton.addEventListener('click', function () {
      entries.forEach(function (entry) { entry.checked = false; });
      paint();
    });
    var status = element('span', 'picker-status', '');
    toolbar.appendChild(filterInput);
    toolbar.appendChild(clearButton);
    toolbar.appendChild(status);
    wrapper.appendChild(toolbar);

    var list = element('div', 'choice-list');
    wrapper.appendChild(list);

    if (meta.manual) {
      var manual = element('div', 'picker-manual');
      var manualInput = document.createElement('input');
      manualInput.type = 'text';
      manualInput.className = 'field-input mono';
      manualInput.placeholder = text(meta.manual.placeholder, '');
      var manualButton = busyButton('btn btn-ghost btn-small', text(meta.manual.action, '添加'));
      manualButton.addEventListener('click', function () {
        var value = manualInput.value.trim();
        if (value === '') { return; }
        add({ value: value, label: value, checked: true });
        manualInput.value = '';
        filter = '';
        filterInput.value = '';
        paint();
        list.scrollTop = list.scrollHeight;
      });
      manualInput.addEventListener('keydown', function (event) {
        if (event.key === 'Enter') { event.preventDefault(); manualButton.click(); }
      });
      manual.appendChild(manualInput);
      manual.appendChild(manualButton);
      wrapper.appendChild(manual);
    }

    paint();

    function visible() {
      if (!filter) { return entries; }
      return entries.filter(function (entry) {
        return entry.label.toLowerCase().indexOf(filter) !== -1 ||
          entry.key.toLowerCase().indexOf(filter) !== -1;
      });
    }

    function paint() {
      list.textContent = '';
      var shown = visible();
      if (shown.length === 0) {
        list.appendChild(element('p', 'field-help', entries.length === 0
          ? text(meta.emptyText, '没有可选的条目。')
          : '没有符合筛选条件的条目。'));
      }
      shown.forEach(function (entry) {
        var line = element('label', 'choice-line');
        var box = document.createElement('input');
        box.type = 'checkbox';
        box.checked = entry.checked;
        box.addEventListener('change', function () {
          entry.checked = box.checked;
          paintStatus();
        });
        line.appendChild(box);
        line.appendChild(element('span', 'choice-name mono', entry.label));
        line.appendChild(element('span', 'choice-detail', entry.detail));
        line.title = entry.key;
        list.appendChild(line);
      });
      paintStatus();
    }

    function paintStatus() {
      var count = entries.filter(function (entry) { return entry.checked; }).length;
      status.textContent = entries.length === 0 ? '' : '已选 ' + count + ' / ' + entries.length + ' 个';
    }

    return {
      node: wrapper,
      read: function () {
        var values = entries.filter(function (entry) { return entry.checked; })
          .map(function (entry) { return entry.value; });
        return values.length === 0 ? null : JSON.stringify(values);
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

  /* ===== Runtime policy =====
   *
   * The settings page edits the policy the gateway applies while it runs:
   * whether a failed request may move to another line, how it is retried, and
   * how long a failing line is held out of rotation. The gateway owns the field
   * list, so a policy value added there shows up here without a change to this
   * file; only the labels and the wording of each section live here. */
  var POLICY_SECTION_ORDER = ['failover', 'retry', 'breaker'];

  var POLICY_SECTIONS = {
    failover: {
      title: '故障转移',
      help: '一条线路失败后，请求是否换到别的线路继续。关闭后只在同一条线路上重试，便于观察某个上游本身的失败。'
    },
    retry: {
      title: '错误重试',
      help: '在把失败交给客户端之前，网关自己重试多少次、等多久。只有列表里的状态码会被重试，其余错误立即返回。'
    },
    breaker: {
      title: '故障冷却与恢复',
      help: '连续失败达到阈值后，这条线路被暂时移出选路（熔断），冷却时间每次按倍数增长，到期自动恢复；作用域选「通道禁用」的线路不会自动恢复，需要在概览页手动恢复。'
    }
  };

  var POLICY_FIELDS = {
    'gateway.failover.enabled': { label: '启用故障转移' },
    'gateway.failover.cross_upstream': { label: '允许跨上游转移', help: '关闭后只在同一个上游的不同密钥之间切换，不会换到其它上游。' },
    'gateway.retry.max_attempts': { label: '单次请求最多尝试', unit: 'count' },
    'gateway.retry.max_attempts_per_channel': { label: '单条线路最多尝试', unit: 'count' },
    'gateway.retry.base_backoff_ms': { label: '首次重试等待', unit: 'ms' },
    'gateway.retry.max_backoff_ms': { label: '重试等待上限', unit: 'ms' },
    'gateway.retry.statuses': { label: '重试的状态码', help: '用逗号分隔，例如 408,429,500,502,503,504。' },
    'gateway.breaker.mode': { label: '熔断作用域', help: '决定一次熔断影响多大范围：整条线路、一个密钥、或一个密钥加一个模型。' },
    'gateway.breaker.threshold': { label: '连续失败多少次熔断', unit: 'count' },
    'gateway.breaker.base_cooldown_seconds': { label: '首次冷却时间', unit: 's' },
    'gateway.breaker.max_cooldown_seconds': { label: '冷却时间上限', unit: 's' },
    'gateway.breaker.cooldown_multiplier': { label: '冷却倍数', unit: 'x', help: '每多熔断一次，冷却时间乘以此倍数，直到上限。' }
  };

  var POLICY_UNITS = { count: '次', ms: '毫秒', s: '秒', x: '倍' };

  function policyUnit(unit) {
    return POLICY_UNITS[unit] || unit;
  }

  function renderPolicy() {
    if (!els.policyPanels) { return; }
    var configuration = state.configuration;
    var signature = JSON.stringify(configuration
      ? { policy: configuration.policy, fields: configuration.policy_fields }
      : null);
    // The form is rebuilt only when the values behind it changed, so a refresh
    // cannot wipe out what the operator is typing.
    if (signature === state.policySignature) { return; }

    els.policyPanels.textContent = '';
    if (!configuration || !Array.isArray(configuration.policy_fields)) {
      state.policySignature = signature;
      return;
    }
    var bySection = {};
    configuration.policy_fields.forEach(function (field) {
      var section = field.section || 'retry';
      if (!bySection[section]) { bySection[section] = []; }
      bySection[section].push(field);
    });
    POLICY_SECTION_ORDER.forEach(function (name) {
      var fields = bySection[name];
      if (!fields || fields.length === 0) { return; }
      els.policyPanels.appendChild(policySection(name, fields, configuration.policy || {}));
    });
    state.policySignature = signature;
  }

  function policySection(name, fields, values) {
    var meta = POLICY_SECTIONS[name] || { title: name };
    var section = element('section', 'panel');
    section.setAttribute('data-policy-section', name);

    var header = element('header', 'panel-header');
    var title = element('div', 'panel-title');
    var tick = element('span', 'title-tick', '');
    tick.setAttribute('aria-hidden', 'true');
    title.appendChild(tick);
    var heading = document.createElement('h2');
    heading.textContent = meta.title;
    title.appendChild(heading);
    header.appendChild(title);
    section.appendChild(header);

    var body = element('div', 'settings-body');
    if (meta.help) { body.appendChild(element('p', 'panel-help', meta.help)); }

    var form = element('form', 'settings-form');
    form.setAttribute('action', 'about:blank');
    form.setAttribute('autocomplete', 'off');
    var inputs = fields.map(function (field) {
      var widget = policyField(field, values[field.key]);
      form.appendChild(widget.wrapper);
      return { field: field, widget: widget };
    });

    var feedback = element('p', 'settings-feedback');
    feedback.setAttribute('role', 'alert');
    feedback.hidden = true;
    form.appendChild(feedback);

    var actions = element('div', 'settings-actions');
    var save = element('button', 'btn btn-primary', '');
    save.type = 'submit';
    save.appendChild(element('span', 'btn-label', '保存'));
    var restore = element('button', 'btn btn-ghost', '');
    restore.type = 'button';
    restore.appendChild(element('span', 'btn-label', '恢复默认'));
    restore.addEventListener('click', function () {
      submitPolicy(inputs, feedback, restore, '恢复默认', true);
    });
    actions.appendChild(save);
    actions.appendChild(restore);
    form.appendChild(actions);
    form.appendChild(element('p', 'settings-hint',
      '清空某一项表示恢复该进程默认值（由部署时的环境变量决定）。'));
    form.addEventListener('submit', function (event) {
      event.preventDefault();
      submitPolicy(inputs, feedback, save, '保存', false);
    });

    body.appendChild(form);
    section.appendChild(body);
    return section;
  }

  /* policyField renders one policy value. Clearing an input submits null, which
   * is what removes the override rather than storing an empty value. */
  function policyField(field, value) {
    var meta = POLICY_FIELDS[field.key] || {};
    var labelText = (meta.label || field.key);
    if (field.kind !== 'bool' && meta.unit) { labelText += '（' + policyUnit(meta.unit) + '）'; }
    var inputID = 'policy-' + String(field.key).replace(/[.]/g, '-');
    var wrapper = element('div', 'field field-editor');
    wrapper.setAttribute('data-field', field.key);

    function fieldLabel() {
      var label = element('label', 'field-label', labelText);
      label.setAttribute('for', inputID);
      return label;
    }

    var read;
    if (field.kind === 'bool') {
      var toggle = document.createElement('input');
      toggle.type = 'checkbox';
      toggle.className = 'field-checkbox';
      toggle.id = inputID;
      toggle.checked = value === true;
      var line = element('label', 'checkline');
      line.appendChild(toggle);
      line.appendChild(element('span', '', labelText));
      wrapper.appendChild(line);
      read = function () { return toggle.checked; };
    } else if (field.kind === 'choice') {
      var select = document.createElement('select');
      select.id = inputID;
      select.className = 'field-input';
      (field.choices || []).forEach(function (choice) {
        var option = document.createElement('option');
        option.value = choice;
        option.textContent = enumLabel('breaker_mode', choice, choice);
        select.appendChild(option);
      });
      var current = value === undefined || value === null ? '' : String(value);
      select.value = current;
      wrapper.appendChild(fieldLabel());
      wrapper.appendChild(select);
      read = function () { return select.value; };
    } else if (field.kind === 'int_list') {
      var list = document.createElement('input');
      list.type = 'text';
      list.id = inputID;
      list.className = 'field-input mono';
      list.placeholder = '408,429,500,502,503,504';
      list.value = Array.isArray(value) ? value.join(',') : '';
      wrapper.appendChild(fieldLabel());
      wrapper.appendChild(list);
      read = function () {
        var raw = list.value.trim();
        if (raw === '') { return null; }
        var codes = [];
        raw.split(',').forEach(function (part) {
          part = part.trim();
          if (part === '') { return; }
          var code = Number(part);
          if (!isFinite(code) || code !== Math.floor(code)) {
            throw new FieldError('状态码必须是整数：' + part);
          }
          codes.push(code);
        });
        return codes.length === 0 ? null : codes;
      };
    } else {
      var number = document.createElement('input');
      number.type = 'number';
      number.id = inputID;
      number.className = 'field-input';
      number.step = field.kind === 'float' ? 'any' : '1';
      if (field.min !== undefined && field.min !== null) { number.min = String(field.min); }
      if (field.max !== undefined && field.max !== null) { number.max = String(field.max); }
      number.value = value === undefined || value === null ? '' : String(value);
      wrapper.appendChild(fieldLabel());
      wrapper.appendChild(number);
      read = function () {
        var raw = number.value.trim();
        if (raw === '') { return null; }
        var parsed = Number(raw);
        if (!isFinite(parsed)) { throw new FieldError('必须是数字'); }
        return parsed;
      };
    }
    if (meta.help) { wrapper.appendChild(element('p', 'field-help', meta.help)); }
    return { wrapper: wrapper, read: read };
  }

  /* submitPolicy writes the values of one section. A restore sends null for
   * every field, which puts the process defaults back. */
  function submitPolicy(inputs, feedback, button, idleLabel, restore) {
    var settings = {};
    for (var index = 0; index < inputs.length; index++) {
      var entry = inputs[index];
      var value;
      try {
        value = restore ? null : entry.widget.read();
      } catch (error) {
        showFeedback(feedback,
          ((POLICY_FIELDS[entry.field.key] || {}).label || entry.field.key) + '：' + error.message, 'error');
        return;
      }
      settings[entry.field.key] = value;
    }

    setSubmitting(button, true, idleLabel);
    hideFeedback(feedback);
    put('/management/policy', { settings: settings }).then(function (result) {
      setSubmitting(button, false, idleLabel);
      if (result.ok) {
        if (result.body && result.body.configuration) { applyConfiguration(result.body.configuration); }
        renderPolicy();
        renderManagement();
        showNotice(restore ? '已恢复默认设置。' : '运行策略已保存，立即生效。');
        return;
      }
      if (result.status === 401) { showAuth('登录状态已过期，请重新登录后继续。'); return; }
      showFeedback(feedback, errorMessage(result.body, '网关拒绝了这次修改。'), 'error');
    }).catch(function (err) {
      setSubmitting(button, false, idleLabel);
      showFeedback(feedback, '无法连接网关：' + String(err && err.message ? err.message : err), 'error');
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
    var label = button.querySelector('.btn-label');
    if (label) { label.textContent = submitting ? '处理中…' : idleLabel; }
  }

  /* busyButton builds a button whose label can be swapped while a request is in
   * flight, which is what tells the operator the click was received. */
  function busyButton(className, label) {
    var button = element('button', className);
    button.type = 'button';
    button.appendChild(element('span', 'btn-label', label));
    return button;
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

    els.settingsNav.addEventListener('click', function (event) {
      var button = event.target.closest('[data-settings-view]');
      if (!button) { return; }
      showSettingsView(button.getAttribute('data-settings-view'));
    });

    document.querySelectorAll('[data-create]').forEach(function (button) {
      button.addEventListener('click', function () {
        openEditor(button.getAttribute('data-create'), null);
      });
    });

    els.refresh.addEventListener('click', refresh);
    els.signout.addEventListener('click', signOut);
    els.bannerRetry.addEventListener('click', refresh);
    if (els.breakersResetAll) {
      els.breakersResetAll.addEventListener('click', resetAllBreakers);
    }
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
