/* Fluxgate Console — vanilla ES2020, no external dependencies.
 * All gateway data is rendered through textContent/DOM APIs so upstream
 * configuration values can never be interpreted as markup. */

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
    els.modelsMeta = document.querySelector('[data-panel="models"] [data-meta]');
    els.modelFilter = $('model-filter');

    els.nextRefresh = document.querySelector('[data-next-refresh]');
    els.footerGenerated = document.querySelector('[data-footer-generated]');
    els.footerLoaded = document.querySelector('[data-footer-loaded]');
  }

  /* ===== Formatting helpers ===== */

  function formatDuration(ms) {
    if (typeof ms !== 'number' || !isFinite(ms) || ms < 0) { return '—'; }
    var totalSeconds = Math.floor(ms / 1000);
    var days = Math.floor(totalSeconds / 86400);
    var hours = Math.floor((totalSeconds % 86400) / 3600);
    var minutes = Math.floor((totalSeconds % 3600) / 60);
    var seconds = totalSeconds % 60;
    if (days > 0) { return days + 'd ' + hours + 'h'; }
    if (hours > 0) { return hours + 'h ' + minutes + 'm'; }
    if (minutes > 0) { return minutes + 'm ' + seconds + 's'; }
    return seconds + 's';
  }

  function formatTimestamp(value) {
    if (!value) { return '—'; }
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) { return String(value); }
    return parsed.toLocaleString();
  }

  function formatCountdown(seconds) {
    if (seconds <= 0) { return 'refreshing…'; }
    return 'in ' + seconds + 's';
  }

  function relativeUntil(value) {
    if (!value) { return ''; }
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) { return ''; }
    var delta = parsed.getTime() - Date.now();
    if (delta <= 0) { return 'expired'; }
    return 'resumes in ' + formatDuration(delta);
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
    els.authOverlay.hidden = false;
    els.authError.hidden = !message;
    els.authError.textContent = message || '';
    els.authSubmit.disabled = false;
    els.authSubmit.querySelector('.btn-label').textContent = 'Sign in';
    els.authPassword.value = '';
    els.authUsername.focus();
  }

  function showConsole() {
    els.authOverlay.hidden = true;
    els.app.hidden = false;
    startAutoRefresh();
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

  function errorMessage(body, fallback) {
    if (body && body.error) {
      if (typeof body.error === 'string') { return body.error; }
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
        showAuth('Your session expired. Sign in again to continue.');
        return;
      }
      if (status.status === 503 || snapshot.status === 503) {
        hideBanner();
        renderUnavailable(errorMessage(status.body, 'Console sign-in is not configured on this gateway.'));
        return;
      }
      if (!status.ok || !snapshot.ok) {
        showBanner('Gateway request failed',
          errorMessage(!status.ok ? status.body : snapshot.body, 'Unexpected response from the gateway.'));
        return;
      }

      hideBanner();
      state.snapshot = snapshot.body || {};
      render(status.body || {}, state.snapshot);
    }).catch(function (err) {
      showBanner('Cannot reach the gateway', String(err && err.message ? err.message : err));
    }).then(function () {
      state.loading = false;
      els.refresh.classList.remove('spinning');
      resetCountdown();
    });
  }

  function renderUnavailable(message) {
    setStatus('unknown', 'Unavailable');
    setStat('service', '—', 'management disabled', '');
    setStat('models', '—', 'unknown', '');
    setStat('channels', '—', 'unknown', '');
    setStat('breakers', '—', 'unknown', '');
    els.breakersBody.textContent = '';
    els.channelsBody.textContent = '';
    els.modelsCloud.textContent = '';
    els.channelsEmpty.hidden = false;
    els.breakersEmpty.hidden = false;
    els.modelsEmpty.hidden = false;
    els.breakersMeta.textContent = '';
    els.channelsMeta.textContent = '';
    els.modelsMeta.textContent = '';
    showBanner('Management endpoints unavailable', message);
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
    setStatus(ready ? 'ok' : 'bad', ready ? 'Ready' : 'Not ready');
    setStat('service', ready ? 'Online' : 'Degraded', status.service ? text(status.service) : 'gateway service',
      ready ? 'ok' : 'bad');
    els.uptime.textContent = 'up ' + formatDuration(status.uptime_ms);
    els.uptime.title = 'Process uptime';

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
    setStat('models', String(modelCount), models.length + ' from channels', '');
    setStat('channels', String(channels.length), enabled + ' enabled', '');
    setStat('breakers', String(openCount), 'open of ' + breakers.length + ' tracked',
      openCount > 0 ? 'bad' : 'ok');

    renderBreakers(breakers);
    renderChannels(channels);
    renderModels(models, els.modelFilter.value);

    els.footerGenerated.textContent = 'snapshot: ' + formatTimestamp(snapshot.generated_at);
    els.footerLoaded.textContent = 'config loaded: ' + formatTimestamp(snapshot.loaded_at);
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
    els.breakersMeta.textContent = breakers.length + ' tracked';

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

      cell(row, badge(text(entry.scope, 'channel'), open ? 'warning' : 'muted'));

      var channelID = entry.channel_id || '';
      var channelName = state.channelNames[channelID] || channelID || '—';
      cell(row, element('span', 'cell-strong', channelName));

      cell(row, entry.key_id ? element('span', 'tag', entry.key_id) : '—');
      cell(row, entry.model ? element('span', 'tag', entry.model) : '—');
      cell(row, String(entry.consecutive_failures || 0), 'num');
      cell(row, String(entry.cooldown_level || 0), 'num');

      var stateCell = document.createElement('td');
      var label = 'Closed';
      var variant = 'success';
      if (entry.disabled) {
        label = 'Disabled';
        variant = 'danger';
      } else if (open) {
        var remaining = relativeUntil(entry.blocked_until);
        label = remaining ? 'Open (' + remaining + ')' : 'Open';
        variant = 'danger';
      }
      stateCell.appendChild(badge(label, variant));
      row.appendChild(stateCell);

      els.breakersBody.appendChild(row);
    });
  }

  function renderChannels(channels) {
    els.channelsBody.textContent = '';
    els.channelsMeta.textContent = channels.length + ' configured';

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

      cell(row, badge(channel.enabled ? 'Enabled' : 'Disabled', channel.enabled ? 'success' : 'danger'));
      cell(row, String(channel.priority === undefined ? 0 : channel.priority), 'num');
      cell(row, String(channel.weight === undefined ? 0 : channel.weight), 'num');
      cell(row, element('span', 'tag', text(channel.routing_strategy, 'weighted')));
      cell(row, element('span', 'tag', text(channel.breaker_mode, 'cooldown')));

      var proxyVariant = channel.proxy_source === 'direct' ? 'muted' : 'info';
      cell(row, badge(text(channel.proxy_source, 'direct'), proxyVariant));

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
    els.modelsMeta.textContent = models.length + ' routable';

    var needle = (filter || '').trim().toLowerCase();
    var visible = needle
      ? models.filter(function (model) { return String(model).toLowerCase().indexOf(needle) !== -1; })
      : models;

    els.modelsEmpty.hidden = visible.length !== 0;
    els.modelsMeta.textContent = needle
      ? visible.length + ' / ' + models.length + ' routable'
      : models.length + ' routable';

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

  /* ===== Wiring ===== */

  function setSubmitting(submitting) {
    els.authSubmit.disabled = submitting;
    els.authSubmit.querySelector('.btn-label').textContent = submitting ? 'Signing in…' : 'Sign in';
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
        els.authError.textContent = 'Enter both the username and the password.';
        return;
      }
      els.authError.hidden = true;
      setSubmitting(true);

      post('/management/login', { username: username, password: password }).then(function (result) {
        if (result.ok) {
          els.authPassword.value = '';
          showConsole();
          refresh();
          return;
        }
        if (result.status === 429) {
          signInError('Too many failed attempts. Wait a few minutes and try again.');
          return;
        }
        if (result.status === 503) {
          signInError('No administrator account exists yet. Run "fluxgate admin create" on the gateway host.');
          return;
        }
        if (result.status === 401) {
          signInError('Invalid username or password.');
          return;
        }
        signInError(errorMessage(result.body, 'Unexpected gateway response.'));
      }).catch(function (err) {
        signInError('Cannot reach the gateway: ' + String(err && err.message ? err.message : err));
      });
    });

    els.refresh.addEventListener('click', refresh);
    els.signout.addEventListener('click', signOut);
    els.bannerRetry.addEventListener('click', refresh);
    els.modelFilter.addEventListener('input', function () {
      renderModels(state.snapshot && state.snapshot.models ? state.snapshot.models : [], els.modelFilter.value);
    });

    document.addEventListener('visibilitychange', function () {
      if (!document.hidden && !els.app.hidden) { refresh(); }
    });
  }

  /* boot asks the gateway who we are instead of trusting local storage: the
   * session cookie is HTTP-only, so only the server can answer that. */
  function boot() {
    cacheElements();
    bind();

    request('/management/session').then(function (result) {
      if (result.ok && result.body && result.body.authenticated) {
        showConsole();
        refresh();
        return;
      }
      if (result.ok && result.body && result.body.configured === false) {
        showAuth('No administrator account exists yet. Run "fluxgate admin create" on the gateway host.');
        return;
      }
      showAuth('');
    }).catch(function () {
      showAuth('Cannot reach the gateway.');
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
