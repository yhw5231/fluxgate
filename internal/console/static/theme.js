/* Fluxgate 管理台 — 日间/夜间配色。
 *
 * 这段由页面头部的 <script> 同步执行，也就是在样式表生效、浏览器画出第一帧
 * 之前，先把 html 上的 data-theme 定下来。放到 app.js 里就晚了：跟随系统的
 * 机器会先看到另一套配色，再翻过来。
 *
 * 没有存过选择时跟随系统，并且系统在会话中途变色（到点切夜间）也跟着；操作者
 * 按过切换按钮之后，存下来的选择优先，不再跟随。顶栏那一枚按钮也在这里绑定，
 * app.js 因此不必知道主题的存在。 */
(function () {
  'use strict';

  var STORAGE_KEY = 'fluxgate.theme';
  var root = document.documentElement;
  var query = window.matchMedia ? window.matchMedia('(prefers-color-scheme: light)') : null;

  function systemTheme() {
    return query && query.matches ? 'light' : 'dark';
  }

  function storedTheme() {
    try {
      var value = window.localStorage.getItem(STORAGE_KEY);
      if (value === 'light' || value === 'dark') { return value; }
    } catch (err) {
      /* 隐私模式下 localStorage 取不到，跟随系统即可。 */
    }
    return null;
  }

  function apply(theme) {
    root.setAttribute('data-theme', theme);

    // 首帧调用时 body 还没解析出来，这一枚按钮是后面 bind() 再补的。
    var button = document.getElementById('theme-button');
    if (!button) { return; }
    var toLight = theme === 'dark';
    button.setAttribute('aria-pressed', toLight ? 'false' : 'true');
    button.title = toLight ? '切换到日间模式' : '切换到夜间模式';
    button.setAttribute('aria-label', button.title);
  }

  function toggle() {
    var next = (storedTheme() || systemTheme()) === 'dark' ? 'light' : 'dark';
    try {
      window.localStorage.setItem(STORAGE_KEY, next);
    } catch (err) {
      /* 存不下就只在本次会话里生效。 */
    }
    apply(next);
  }

  apply(storedTheme() || systemTheme());

  // 跟着系统走的时候，系统变了就跟着变。
  if (query) {
    var followSystem = function () {
      if (!storedTheme()) { apply(systemTheme()); }
    };
    if (query.addEventListener) { query.addEventListener('change', followSystem); }
    else if (query.addListener) { query.addListener(followSystem); }
  }

  function bind() {
    var button = document.getElementById('theme-button');
    if (button) { button.addEventListener('click', toggle); }
    apply(storedTheme() || systemTheme());
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bind);
  } else {
    bind();
  }
})();
