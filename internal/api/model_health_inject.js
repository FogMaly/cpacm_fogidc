(function () {
  'use strict';

  if (window.__CPAPI_MODEL_HEALTH_LIVE_FIX_INIT__) {
    return;
  }
  window.__CPAPI_MODEL_HEALTH_LIVE_FIX_INIT__ = true;

  var MODEL_HEALTH_ENDPOINT = '/model-health.json';
  var PROVIDER_HEALTH_ENDPOINT = '/v0/management/provider-health';
  var PROVIDER_BALANCES_ENDPOINT = '/v0/management/provider-balances';
  var POLL_MS = 15000;
  var PROVIDER_PROBE_STYLE_ID = 'cpapi-provider-probe-style';
  var PROVIDER_BALANCE_ROW_ATTR = 'data-cpapi-provider-balance-row';
  var SECURE_STORAGE_PREFIX = 'enc::v1::';
  var SECURE_STORAGE_NAMESPACE = 'cli-proxy-api-webui::secure-storage';
  var KEY_CANDIDATES = [
    'management_key',
    'managementKey',
    'openclaw_management_key',
    'cpapi.management_key'
  ];
  var STATUS_TEXT = { green: '健康', red: '不健康', unknown: '未知' };
  var STATUS_STYLE = {
    green: 'background:#dcfce7;color:#166534;border:1px solid #86efac;',
    red: 'background:#fee2e2;color:#991b1b;border:1px solid #fca5a5;',
    unknown: 'background:#f3f4f6;color:#4b5563;border:1px solid #d1d5db;'
  };
  var MODEL_TAG_STYLE = {
    green: { bg: '#dcfce7', border: '#86efac', text: '#166534' },
    red: { bg: '#fee2e2', border: '#fca5a5', text: '#991b1b' },
    unknown: { bg: '#f3f4f6', border: '#d1d5db', text: '#4b5563' }
  };
  var blockedPrefixTokens = {
    optional: true,
    when: true,
    set: true,
    call: true,
    calls: true,
    model: true,
    models: true,
    target: true,
    targets: true,
    entry: true,
    entries: true,
    this: true,
    prefix: true,
    channel: true
  };

  var latestHealthData = null;
  var latestProviderHealthData = null;
  var latestProviderBalanceData = null;
  var latestIndex = null;
  var knownPrefixes = [];
  var prefixCache = new WeakMap();
  var authSnifferInstalled = false;
  var providerBalanceReloadTimer = 0;
  var repaintBurstTimer = 0;
  var repaintBurstRemaining = 0;
  var initialHealthBootstrapped = false;

  function normText(value) {
    return String(value || '').toLowerCase().replace(/\s+/g, ' ').trim();
  }

  function escapeHtml(value) {
    return String(value == null ? '' : value)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/\"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function escapeRegExp(value) {
    return String(value || '').replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }

  function safeStorageGet(store, key) {
    try {
      if (!store || typeof store.getItem !== 'function') {
        return '';
      }
      var value = store.getItem(key);
      return typeof value === 'string' ? value.trim() : '';
    } catch (err) {
      return '';
    }
  }

  function decodeBase64(value) {
    try {
      return window.atob(String(value || ''));
    } catch (err) {
      return '';
    }
  }

  function bytesFromString(value) {
    var text = String(value || '');
    if (typeof TextEncoder !== 'undefined') {
      return new TextEncoder().encode(text);
    }
    var out = new Uint8Array(text.length);
    for (var i = 0; i < text.length; i += 1) {
      out[i] = text.charCodeAt(i) & 255;
    }
    return out;
  }

  function stringFromBytes(bytes) {
    if (typeof TextDecoder !== 'undefined') {
      return new TextDecoder().decode(bytes);
    }
    var text = '';
    for (var i = 0; i < bytes.length; i += 1) {
      text += String.fromCharCode(bytes[i]);
    }
    try {
      return decodeURIComponent(escape(text));
    } catch (err) {
      return text;
    }
  }

  function xorBytes(input, key) {
    var out = new Uint8Array(input.length);
    if (!key.length) {
      return out;
    }
    for (var i = 0; i < input.length; i += 1) {
      out[i] = input[i] ^ key[i % key.length];
    }
    return out;
  }

  function secureStorageKeyBytes() {
    try {
      return bytesFromString(SECURE_STORAGE_NAMESPACE + '|' + window.location.host + '|' + navigator.userAgent);
    } catch (err) {
      return bytesFromString(SECURE_STORAGE_NAMESPACE);
    }
  }

  function decryptSecureStorageValue(value) {
    var raw = String(value || '').trim();
    if (!raw || raw.indexOf(SECURE_STORAGE_PREFIX) !== 0) {
      return raw;
    }
    var encoded = raw.slice(SECURE_STORAGE_PREFIX.length);
    var binary = decodeBase64(encoded);
    if (!binary) {
      return '';
    }
    var input = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i += 1) {
      input[i] = binary.charCodeAt(i);
    }
    try {
      return stringFromBytes(xorBytes(input, secureStorageKeyBytes()));
    } catch (err) {
      return '';
    }
  }

  function safeStorageGetDecoded(store, key) {
    return decryptSecureStorageValue(safeStorageGet(store, key));
  }

  function parseJSONValue(raw) {
    if (!raw) {
      return null;
    }
    try {
      return JSON.parse(raw);
    } catch (err) {
      return null;
    }
  }

  function rememberManagementKey(value) {
    var normalized = String(value || '').trim().replace(/^Bearer\s+/i, '').trim();
    if (!normalized) {
      return '';
    }
    var changed = window.__CPAPI_MODEL_HEALTH_MGMT_KEY__ !== normalized;
    window.__CPAPI_MODEL_HEALTH_MGMT_KEY__ = normalized;
    if (changed) {
      scheduleAuthenticatedRefresh();
    }
    return normalized;
  }

  function parsePersistedAuthState() {
    var raw =
      safeStorageGetDecoded(window.localStorage, 'cli-proxy-auth') ||
      safeStorageGetDecoded(window.sessionStorage, 'cli-proxy-auth');
    if (!raw) {
      return {};
    }
    var parsed = parseJSONValue(raw);
    if (parsed && typeof parsed === 'object') {
      if (parsed.state && typeof parsed.state === 'object') {
        return parsed.state;
      }
      return parsed;
    }
    return {};
  }

  function headerValue(headers, key) {
    if (!headers) {
      return '';
    }
    if (typeof Headers !== 'undefined' && headers instanceof Headers) {
      return String(headers.get(key) || '').trim();
    }
    if (Array.isArray(headers)) {
      for (var i = 0; i < headers.length; i += 1) {
        var pair = headers[i];
        if (!pair || pair.length < 2) {
          continue;
        }
        if (normText(pair[0]) === normText(key)) {
          return String(pair[1] || '').trim();
        }
      }
      return '';
    }
    if (typeof headers === 'object') {
      var direct = headers[key];
      if (direct == null) {
        direct = headers[String(key || '').toLowerCase()];
      }
      return String(direct || '').trim();
    }
    return '';
  }

  function captureManagementKeyFromHeaders(headers) {
    rememberManagementKey(
      headerValue(headers, 'X-Management-Key') ||
      headerValue(headers, 'x-management-key') ||
      headerValue(headers, 'Authorization') ||
      headerValue(headers, 'authorization')
    );
  }

  function installAuthSniffer() {
    if (authSnifferInstalled) {
      return;
    }
    authSnifferInstalled = true;

    if (typeof window.fetch === 'function' && !window.__CPAPI_MODEL_HEALTH_FETCH_PATCHED__) {
      window.__CPAPI_MODEL_HEALTH_FETCH_PATCHED__ = true;
      var originalFetch = window.fetch.bind(window);
      window.fetch = function (input, init) {
        try {
          captureManagementKeyFromHeaders(init && init.headers);
        } catch (err) {}
        return originalFetch(input, init);
      };
    }

    if (typeof XMLHttpRequest !== 'undefined' && !XMLHttpRequest.prototype.__CPAPI_MODEL_HEALTH_PATCHED__) {
      XMLHttpRequest.prototype.__CPAPI_MODEL_HEALTH_PATCHED__ = true;
      var originalSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
      XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
        try {
          if (normText(name) === 'authorization' || normText(name) === 'x-management-key') {
            rememberManagementKey(value);
          }
        } catch (err) {}
        return originalSetRequestHeader.apply(this, arguments);
      };
    }
  }

  function getManagementKey() {
    if (window.__CPAPI_MODEL_HEALTH_MGMT_KEY__) {
      return window.__CPAPI_MODEL_HEALTH_MGMT_KEY__;
    }

    var query = new URLSearchParams(window.location.search || '');
    var state = parsePersistedAuthState();
    var candidates = [
      window.__CPAPI_REAUTH_MGMT_KEY__ || '',
      query.get('managementKey') || '',
      query.get('management_key') || '',
      safeStorageGetDecoded(window.localStorage, 'managementKey'),
      safeStorageGetDecoded(window.sessionStorage, 'managementKey'),
      safeStorageGetDecoded(window.localStorage, 'management_key'),
      safeStorageGetDecoded(window.sessionStorage, 'management_key'),
      state.managementKey || '',
      state.management_key || ''
    ];

    for (var i = 0; i < KEY_CANDIDATES.length; i += 1) {
      candidates.push(safeStorageGetDecoded(window.localStorage, KEY_CANDIDATES[i]));
      candidates.push(safeStorageGetDecoded(window.sessionStorage, KEY_CANDIDATES[i]));
    }

    for (var j = 0; j < candidates.length; j += 1) {
      var value = rememberManagementKey(candidates[j]);
      if (value) {
        return value;
      }
    }

    try {
      for (var idx = 0; idx < localStorage.length; idx += 1) {
        var key = localStorage.key(idx);
        if (!key) {
          continue;
        }
        var raw = decryptSecureStorageValue(localStorage.getItem(key));
        if (!raw || raw.length < 2) {
          continue;
        }
        if ((raw[0] !== '{' && raw[0] !== '[') || raw.length > 1024 * 1024) {
          continue;
        }
        var parsed = parseJSONValue(raw);
        if (!parsed || typeof parsed !== 'object') {
          continue;
        }
          var nested = [
            parsed.managementKey,
            parsed.management_key,
            parsed['x-management-key'],
            parsed.state && parsed.state.managementKey,
            parsed.state && parsed.state.management_key
          ];
          for (var ni = 0; ni < nested.length; ni += 1) {
            var found = rememberManagementKey(nested[ni]);
            if (found) {
              return found;
            }
          }
      }
    } catch (err) {}

    return '';
  }

  function isVisible(el) {
    if (!el || !el.getBoundingClientRect) {
      return false;
    }
    var rect = el.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) {
      return false;
    }
    var style = window.getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
  }

  function statusKey(status) {
    var value = normText(status);
    if (value === 'green' || value === 'ok' || value === 'healthy' || value === 'available' || value === 'pass') {
      return 'green';
    }
    if (value === 'red' || value === 'error' || value === 'failed' || value === 'unavailable' || value === 'down') {
      return 'red';
    }
    if (
      value === 'yellow' ||
      value === 'warn' ||
      value === 'warning' ||
      value === 'degraded' ||
      value === 'unstable' ||
      value === 'partial' ||
      value === 'unsupported'
    ) {
      return 'green';
    }
    return 'unknown';
  }

  function statusRank(status) {
    var key = statusKey(status);
    if (key === 'red') {
      return 3;
    }
    if (key === 'green') {
      return 1;
    }
    return 0;
  }

  function pickWorseStatus(a, b) {
    if (!a) {
      return statusKey(b);
    }
    if (!b) {
      return statusKey(a);
    }
    return statusRank(a) >= statusRank(b) ? statusKey(a) : statusKey(b);
  }

  function statusBadge(status) {
    var key = statusKey(status);
    var text = STATUS_TEXT[key] || STATUS_TEXT.unknown;
    var style = STATUS_STYLE[key] || STATUS_STYLE.unknown;
    return '<span style="display:inline-flex;align-items:center;justify-content:center;padding:2px 8px;border-radius:999px;font-size:11px;font-weight:700;' + style + '">' + text + '</span>';
  }

  function modelOrderValue(status) {
    var key = statusKey(status);
    if (key === 'red') {
      return 0;
    }
    if (key === 'green') {
      return 2;
    }
    return 3;
  }

  function containsModelToken(text, token) {
    if (!text || !token) {
      return false;
    }
    if (text === token) {
      return true;
    }
    if (text.indexOf(token) === -1) {
      return false;
    }
    if (token.length > 4) {
      return true;
    }
    var re = new RegExp('(^|[^a-z0-9_.-])' + escapeRegExp(token) + '([^a-z0-9_.-]|$)');
    return re.test(text);
  }

  function compactText(value, maxLen) {
    var text = String(value || '').replace(/\s+/g, ' ').trim();
    if (!text) {
      return '';
    }
    if (!maxLen || text.length <= maxLen) {
      return text;
    }
    return text.slice(0, maxLen - 1) + '…';
  }

  function updateKnownPrefixes(prefixes) {
    knownPrefixes = Array.from(prefixes || [])
      .map(function (value) { return normText(value); })
      .filter(Boolean)
      .sort(function (a, b) { return b.length - a.length; });
  }

  function findKnownPrefix(text) {
    var source = normText(text);
    if (!source) {
      return '';
    }
    for (var i = 0; i < knownPrefixes.length; i += 1) {
      var prefix = knownPrefixes[i];
      var re = new RegExp('(^|[^a-z0-9._-])' + escapeRegExp(prefix) + '($|[^a-z0-9._-])', 'i');
      if (re.test(source)) {
        return prefix;
      }
    }
    return '';
  }

  function normalizePrefixValue(value) {
    var source = String(value || '').trim();
    if (!source) {
      return '';
    }
    source = source.replace(/^prefix\s*[:：]?\s*/i, '').trim();
    source = source.replace(/^[（(][^()（）]{1,32}[)）]\s*[:：]?\s*/i, '').trim();
    var tokenMatch = source.match(/^([a-z0-9._-]{1,64})(?:\s|$)/i);
    if (!tokenMatch || !tokenMatch[1]) {
      return '';
    }
    var token = normText(tokenMatch[1]);
    if (!token || blockedPrefixTokens[token]) {
      return '';
    }
    return token;
  }

  function extractPrefix(text) {
    var source = String(text || '');
    var match = source.match(/(?:^|\n|\s)(?:前缀|前綴|prefix|渠道|channel)\s*[:：]?\s*([a-z0-9._-]+)/i);
    if (match && match[1]) {
      return normText(match[1]);
    }
    return findKnownPrefix(source);
  }

  function extractPrefixFromControls(root) {
    if (!root || typeof root.querySelectorAll !== 'function') {
      return '';
    }
    var fields = root.querySelectorAll(
      'input[name*="prefix" i],textarea[name*="prefix" i],' +
      'input[id*="prefix" i],textarea[id*="prefix" i],' +
      'input[class*="prefix" i],textarea[class*="prefix" i],' +
      'input[name*="channel" i],textarea[name*="channel" i],' +
      'input[id*="channel" i],textarea[id*="channel" i],' +
      'input[class*="channel" i],textarea[class*="channel" i]'
    );
    for (var i = 0; i < fields.length; i += 1) {
      var node = fields[i];
      var value = normalizePrefixValue((node && node.value) || (node && node.getAttribute && node.getAttribute('value')) || '');
      if (value) {
        return value;
      }
    }
    return '';
  }

  function extractPrefixFromLabeledRows(root) {
    if (!root || typeof root.querySelectorAll !== 'function') {
      return '';
    }
    var labels = root.querySelectorAll('[class*="fieldLabel"],label,th,dt,strong');
    for (var i = 0; i < labels.length; i += 1) {
      var labelNode = labels[i];
      var labelText = normText(labelNode && labelNode.textContent);
      if (
        labelText.indexOf('prefix') === -1 &&
        labelText.indexOf('前缀') === -1 &&
        labelText.indexOf('前綴') === -1 &&
        labelText.indexOf('渠道') === -1 &&
        labelText.indexOf('channel') === -1
      ) {
        continue;
      }
      var probes = [];
      if (labelNode.nextElementSibling) {
        probes.push(labelNode.nextElementSibling);
      }
      if (labelNode.parentElement) {
        probes.push(labelNode.parentElement);
      }
      for (var p = 0; p < probes.length; p += 1) {
        var probe = probes[p];
        if (!probe) {
          continue;
        }
        var direct = normalizePrefixValue(probe.value || probe.textContent || '');
        if (direct) {
          return direct;
        }
        var known = findKnownPrefix(probe.textContent || '');
        if (known) {
          return known;
        }
      }
    }
    return '';
  }

  function pathPrefix() {
    var hash = String(window.location.hash || '');
    var path = hash.replace(/^#/, '') || window.location.pathname || '';
    var match =
      path.match(/\/ai-providers\/([^/?#]+)/i) ||
      path.match(/\/ai_providers\/([^/?#]+)/i) ||
      path.match(/\/providers\/([^/?#]+)/i);
    return match && match[1] ? normText(match[1]) : '';
  }

  function findPrefixFromNode(node) {
    var fromPath = pathPrefix();
    if (fromPath) {
      return fromPath;
    }
    var cur = node;
    for (var depth = 0; cur && cur !== document.body && depth < 24; depth += 1) {
      if (prefixCache.has(cur)) {
        var cached = prefixCache.get(cur);
        if (cached) {
          return cached;
        }
      }
      var fromControls = extractPrefixFromControls(cur);
      if (fromControls) {
        prefixCache.set(cur, fromControls);
        return fromControls;
      }
      var fromRows = extractPrefixFromLabeledRows(cur);
      if (fromRows) {
        prefixCache.set(cur, fromRows);
        return fromRows;
      }
      var found = extractPrefix(cur.innerText || cur.textContent || '');
      prefixCache.set(cur, found || '');
      if (found) {
        return found;
      }
      cur = cur.parentElement;
    }
    return '';
  }

  function normalizedModelTokens(item, prefix) {
    var tokens = [];
    function push(value) {
      var text = normText(value);
      if (text && tokens.indexOf(text) === -1) {
        tokens.push(text);
      }
    }

    var model = String((item && item.model) || '').trim();
    var upstream = String((item && item.upstream_model) || '').trim();

    push(model);
    push(upstream);

    if (model && model.indexOf('/') > 0) {
      push(model.split('/').slice(1).join('/'));
    }
    if (prefix && upstream) {
      push(prefix + '/' + upstream);
    }
    return tokens;
  }

  function buildModelStatusIndex(models) {
    var plain = Object.create(null);
    var byPrefix = Object.create(null);
    var prefixes = new Set();

    for (var i = 0; i < models.length; i += 1) {
      var item = models[i] || {};
      var prefix = normText(item.provider_prefix || '');
      var status = statusKey(item.status);
      var tokens = normalizedModelTokens(item, prefix);

      if (prefix) {
        prefixes.add(prefix);
        if (!byPrefix[prefix]) {
          byPrefix[prefix] = Object.create(null);
        }
      }

      for (var t = 0; t < tokens.length; t += 1) {
        var token = tokens[t];
        plain[token] = pickWorseStatus(plain[token], status);
        if (prefix) {
          byPrefix[prefix][token] = pickWorseStatus(byPrefix[prefix][token], status);
        }
      }
    }

    var plainKeys = Object.keys(plain).sort(function (a, b) { return b.length - a.length; });
    var byPrefixKeys = Object.create(null);
    Object.keys(byPrefix).forEach(function (prefix) {
      byPrefixKeys[prefix] = Object.keys(byPrefix[prefix]).sort(function (a, b) { return b.length - a.length; });
    });

    updateKnownPrefixes(prefixes);

    return {
      plain: plain,
      plainKeys: plainKeys,
      byPrefix: byPrefix,
      byPrefixKeys: byPrefixKeys
    };
  }

  function resetModelTagColors() {
    var nodes = document.querySelectorAll('[data-cpapi-health]');
    for (var i = 0; i < nodes.length; i += 1) {
      var host = nodes[i];
      host.style.borderColor = '';
      host.style.backgroundColor = '';
      host.style.boxShadow = '';
      host.removeAttribute('data-cpapi-health');
      host.removeAttribute('title');
    }
  }

  function paintModelTag(node, status) {
    var host = node;
    if (!host.matches(
      "[class*='SystemPage-module__modelTag___']," +
      "[class*='SystemPage-module__modelItem___']," +
      "[class*='AiProvidersPage-module__modelTag___']," +
      "[class*='AiProvidersPage-module__modelDiscoveryRow___']," +
      "[class*='AiProvidersPage-module__modelDiscoveryRowSelected___']"
    )) {
      host = node.closest(
        "[class*='SystemPage-module__modelTag___']," +
        "[class*='SystemPage-module__modelItem___']," +
        "[class*='AiProvidersPage-module__modelTag___']," +
        "[class*='AiProvidersPage-module__modelDiscoveryRow___']," +
        "[class*='AiProvidersPage-module__modelDiscoveryRowSelected___']"
      );
    }
    if (!host) {
      return;
    }
    var key = statusKey(status);
    var theme = MODEL_TAG_STYLE[key] || MODEL_TAG_STYLE.unknown;
    var isPill =
      host.matches("[class*='SystemPage-module__modelTag___']") ||
      host.matches("[class*='AiProvidersPage-module__modelTag___']");
    if (isPill) {
      host.style.borderColor = theme.border;
      host.style.backgroundColor = theme.bg;
      host.style.boxShadow = 'inset 0 0 0 1px ' + theme.border;
    } else {
      host.style.borderColor = theme.border;
      host.style.backgroundColor = '';
      host.style.boxShadow = 'inset 3px 0 0 0 ' + theme.border;
    }
    host.setAttribute('data-cpapi-health', key);
    host.setAttribute('title', '健康状态: ' + (STATUS_TEXT[key] || STATUS_TEXT.unknown));

  }

  function resolveStatusForNode(index, node) {
    if (!index || !node) {
      return '';
    }
    var text = normText(node.textContent || '');
    if (!text) {
      return '';
    }

    var prefix = findPrefixFromNode(node) || findKnownPrefix(text);
    var scopedKeys = prefix && index.byPrefixKeys[prefix] ? index.byPrefixKeys[prefix] : null;
    var scopedMap = prefix && index.byPrefix[prefix] ? index.byPrefix[prefix] : null;

    if (scopedKeys && scopedMap) {
      for (var i = 0; i < scopedKeys.length; i += 1) {
        var scopedKey = scopedKeys[i];
        if (containsModelToken(text, scopedKey)) {
          return scopedMap[scopedKey];
        }
      }
    }

    for (var j = 0; j < index.plainKeys.length; j += 1) {
      var key = index.plainKeys[j];
      if (containsModelToken(text, key)) {
        return index.plain[key];
      }
    }

    return '';
  }

  function applyChannelModelColors(data) {
    if (!data || !Array.isArray(data.models) || data.models.length === 0) {
      latestIndex = null;
      resetModelTagColors();
      return;
    }

    latestIndex = buildModelStatusIndex(data.models);
    resetModelTagColors();

    var nodes = document.querySelectorAll(
      "[class*='SystemPage-module__modelTag___']," +
      "[class*='SystemPage-module__modelItem___']," +
      "[class*='AiProvidersPage-module__modelTag___']," +
      "[class*='AiProvidersPage-module__modelDiscoveryRow___']," +
      "[class*='AiProvidersPage-module__modelDiscoveryRowSelected___']"
    );

    for (var i = 0; i < nodes.length; i += 1) {
      var node = nodes[i];
      var status = resolveStatusForNode(latestIndex, node);
      if (status) {
        paintModelTag(node, status);
      }
    }
  }

  function normalizePayload(payload) {
    if (!payload || typeof payload !== 'object') {
      return null;
    }

    if (Array.isArray(payload.models)) {
      var directSummary = payload.summary || {};
      return {
        updated_at: String(payload.updated_at || '').trim(),
        summary: {
          total: Number(directSummary.total || payload.models.length || 0),
          green: Number(directSummary.green || 0) + Number(directSummary.yellow || 0),
          yellow: 0,
          red: Number(directSummary.red || 0)
        },
        models: payload.models.slice()
      };
    }

    if (!Array.isArray(payload.entries)) {
      return null;
    }

    var models = [];
    var summary = { total: 0, green: 0, yellow: 0, red: 0 };
    var updatedAt = String(payload.updated_at || '').trim();

    for (var i = 0; i < payload.entries.length; i += 1) {
      var entry = payload.entries[i] || {};
      var prefix = String(entry.provider_prefix || entry.prefix || '').trim();
      var entryUpdatedAt = String(entry.updated_at || updatedAt || '').trim();
      var list = Array.isArray(entry.models) ? entry.models : [];

      for (var j = 0; j < list.length; j += 1) {
        var item = Object.assign({}, list[j] || {});
        item.provider_prefix = prefix;
        if (!item.tested_at && entryUpdatedAt) {
          item.tested_at = entryUpdatedAt;
        }
        models.push(item);
        summary.total += 1;
        var status = statusKey(item.status);
        if (status === 'green') {
          summary.green += 1;
        } else if (status === 'red') {
          summary.red += 1;
        }
      }
    }

    return {
      updated_at: updatedAt,
      summary: summary,
      models: models
    };
  }

  function render(data) {
    var normalized = normalizePayload(data);
    if (!normalized || !Array.isArray(normalized.models) || normalized.models.length === 0) {
      latestHealthData = null;
      latestIndex = null;
      resetModelTagColors();
      return;
    }
    latestHealthData = normalized;
    applyChannelModelColors(normalized);
    startRepaintBurst(2, 140);
  }

  function load() {
    var managementKey = getManagementKey();
    if (!managementKey) {
      resetModelTagColors();
      return;
    }

    fetch(MODEL_HEALTH_ENDPOINT, {
      method: 'GET',
      headers: {
        'X-Management-Key': managementKey
      },
      cache: 'no-store'
    })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error('HTTP ' + resp.status);
        }
        return resp.json();
      })
      .then(render)
      .catch(function (err) {
        window.__CPAPI_MODEL_HEALTH_LAST_ERROR__ = String(err && err.message || err || '');
      });
  }

  function remountAndRepaint() {
    prefixCache = new WeakMap();
    removeLegacyProviderProbePanel();
    if (latestHealthData) {
      applyChannelModelColors(latestHealthData);
    }
    applyProviderBalanceColors(latestProviderBalanceData);
  }

  function startRepaintBurst(count, delay) {
    var rounds = Number(count) || 0;
    if (rounds <= 0) {
      return;
    }
    if (repaintBurstRemaining < rounds) {
      repaintBurstRemaining = rounds;
    }
    if (repaintBurstTimer) {
      return;
    }
    function run() {
      repaintBurstTimer = 0;
      if (repaintBurstRemaining <= 0) {
        return;
      }
      repaintBurstRemaining -= 1;
      remountAndRepaint();
      if (repaintBurstRemaining > 0) {
        repaintBurstTimer = setTimeout(run, typeof delay === 'number' ? delay : 140);
      }
    }
    repaintBurstTimer = setTimeout(run, 0);
  }

  function removeLegacyProviderProbePanel() {
    var legacyIds = ['cpapi-provider-probe-panel-v1'];
    for (var i = 0; i < legacyIds.length; i += 1) {
      var node = document.getElementById(legacyIds[i]);
      if (node && node.parentNode) {
        node.parentNode.removeChild(node);
      }
    }
    var maybeLegacy = document.querySelectorAll('[class*="cpapi-provider-probe-"]');
    for (var j = 0; j < maybeLegacy.length; j += 1) {
      var el = maybeLegacy[j];
      if (el && el.parentNode) {
        el.parentNode.removeChild(el);
      }
    }
  }

  function currentPagePath() {
    var hash = String(window.location.hash || '').replace(/^#/, '').trim();
    return hash || window.location.pathname || '/';
  }

  function isAIProvidersPage() {
    var path = currentPagePath();
    if (/\/ai-providers(?:\/|$)/i.test(path) || /\/ai_providers(?:\/|$)/i.test(path)) {
      return true;
    }
    return !!document.querySelector(
      "[class*='AiProvidersPage-module__container___']," +
      "[class*='AiProvidersPage-module__content___']," +
      "[class*='AiProvidersPage-module__pageTitle___']"
    );
  }

  function ensureProviderProbeStyles() {
    if (document.getElementById(PROVIDER_PROBE_STYLE_ID)) {
      return;
    }
    var style = document.createElement('style');
    style.id = PROVIDER_PROBE_STYLE_ID;
    style.textContent = [
      '[data-cpapi-provider-balance-text="green"]{color:#166534!important;}',
      '[data-cpapi-provider-balance-text="red"]{color:#991b1b!important;}',
      '[data-cpapi-provider-balance-text="unknown"]{color:#4b5563!important;}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '].cpapi-provider-balance-row{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-top:6px;font-size:12px;line-height:1.4;}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-balance-meta{display:flex;align-items:center;gap:8px;min-width:0;}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-balance-label{flex:0 0 auto;font-size:12px;font-weight:600;color:var(--text-secondary,#6b7280);}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-health-badge{display:inline-flex;align-items:center;justify-content:center;padding:1px 8px;border-radius:999px;font-size:11px;font-weight:700;white-space:nowrap;}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-health-score{flex:0 0 auto;font-size:11px;font-weight:600;color:var(--text-tertiary,#6b7280);white-space:nowrap;}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-balance-main{flex:1 1 auto;min-width:0;display:flex;align-items:center;justify-content:flex-end;gap:8px;}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-balance-value{flex:0 1 auto;min-width:0;text-align:right;font-size:12px;font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;color:var(--text-primary,#111827);}',
      '[' + PROVIDER_BALANCE_ROW_ATTR + '] .cpapi-provider-balance-extra{flex:0 0 auto;font-size:11px;color:var(--text-tertiary,#6b7280);white-space:nowrap;}'
    ].join('');
    document.head.appendChild(style);
  }

  function providerRouteHint() {
    var path = currentPagePath();
    var match =
      path.match(/\/ai-providers\/([^/?#]+)/i) ||
      path.match(/\/ai_providers\/([^/?#]+)/i);
    return match && match[1] ? normText(match[1]) : '';
  }

  function providerHintMatchesItem(hint, item) {
    if (!hint || !item) {
      return true;
    }
    var section = normText(item.section || '');
    var providerKey = normText(item.provider_key || '');
    var displayName = normText(item.display_name || '');

    if (hint === providerKey || hint === section) {
      return true;
    }
    if (displayName && displayName.indexOf(hint) !== -1) {
      return true;
    }
    if (hint === 'openai' || hint === 'compat' || hint === 'openai-compatibility') {
      return section === 'openai-compatibility' || providerKey === 'compat';
    }
    if (hint === 'gemini') {
      return section === 'gemini-api-key' || providerKey === 'gemini';
    }
    if (hint === 'claude') {
      return section === 'claude-api-key' || providerKey === 'claude';
    }
    if (hint === 'codex') {
      return section === 'codex-api-key' || providerKey === 'codex';
    }
    if (hint === 'vertex') {
      return section === 'vertex-api-key' || providerKey === 'vertex';
    }
    return false;
  }

  function sortProviderProbeItems(items) {
    return items.slice().sort(function (a, b) {
      var byStatus = statusRank(b && b.status) - statusRank(a && a.status);
      if (byStatus !== 0) {
        return byStatus;
      }
      return normText((a && a.display_name) || '').localeCompare(normText((b && b.display_name) || ''));
    });
  }

  function clearProviderBalanceColors() {
    var hosts = document.querySelectorAll('[data-cpapi-provider-balance]');
    for (var i = 0; i < hosts.length; i += 1) {
      var host = hosts[i];
      host.style.borderColor = '';
      host.style.backgroundColor = '';
      host.style.boxShadow = '';
      host.removeAttribute('data-cpapi-provider-balance');
    }
    var texts = document.querySelectorAll('[data-cpapi-provider-balance-text]');
    for (var j = 0; j < texts.length; j += 1) {
      var node = texts[j];
      node.style.color = '';
      node.removeAttribute('data-cpapi-provider-balance-text');
    }
    var rows = document.querySelectorAll('[' + PROVIDER_BALANCE_ROW_ATTR + ']');
    for (var k = 0; k < rows.length; k += 1) {
      var row = rows[k];
      if (row && row.parentNode) {
        row.parentNode.removeChild(row);
      }
    }
  }

  function providerBalanceTokens(item) {
    var tokens = [];
    function push(value) {
      var text = normText(value);
      if (text && tokens.indexOf(text) === -1) {
        tokens.push(text);
      }
    }
    if (!item) {
      return tokens;
    }
    push(item.display_name);
    push(item.provider_key);
    push(item.section);
    push(item.prefix);
    push(providerShortHost(item.base_url));

    var displayName = normText(item.display_name || '');
    if (displayName.indexOf('openrouter') !== -1) {
      push('openrouter');
    }
    if (displayName.indexOf('anthropic') !== -1) {
      push('anthropic');
    }
    if (displayName.indexOf('sub2api') !== -1) {
      push('sub2api');
    }
    if (displayName.indexOf('yunyi') !== -1) {
      push('yunyi');
    }
    if (displayName.indexOf('nvidia') !== -1) {
      push('nvidia');
    }
    return tokens;
  }

  function providerShortHost(value) {
    var raw = String(value || '').trim();
    if (!raw) {
      return '';
    }
    var match = raw.match(/^https?:\/\/([^/]+)/i);
    if (match && match[1]) {
      return normText(match[1]);
    }
    return normText(raw);
  }

  function aggregateProviderBalanceStatus(items) {
    var status = '';
    for (var i = 0; i < items.length; i += 1) {
      status = pickWorseStatus(status, items[i] && items[i].status);
    }
    return status || 'unknown';
  }

  function providerRouteIndex() {
    var path = currentPagePath();
    var match =
      path.match(/\/ai-providers\/[^/?#]+\/(\d+)/i) ||
      path.match(/\/ai_providers\/[^/?#]+\/(\d+)/i);
    if (!match || !match[1]) {
      return -1;
    }
    var value = parseInt(match[1], 10);
    return isFinite(value) ? value : -1;
  }

  function providerCardTitleNode(card) {
    if (!card || typeof card.querySelector !== 'function') {
      return null;
    }
    return card.querySelector("[class*='AiProvidersPage-module__cardTitle___']");
  }

  function providerDescriptorFromText(text) {
    var value = normText(text);
    if (!value) {
      return null;
    }
    if (value.indexOf('openai') !== -1) {
      return { hint: 'openai', section: 'openai-compatibility', providerKey: 'compat' };
    }
    if (value.indexOf('gemini') !== -1) {
      return { hint: 'gemini', section: 'gemini-api-key', providerKey: 'gemini' };
    }
    if (value.indexOf('claude') !== -1) {
      return { hint: 'claude', section: 'claude-api-key', providerKey: 'claude' };
    }
    if (value.indexOf('codex') !== -1) {
      return { hint: 'codex', section: 'codex-api-key', providerKey: 'codex' };
    }
    if (value.indexOf('vertex') !== -1) {
      return { hint: 'vertex', section: 'vertex-api-key', providerKey: 'vertex' };
    }
    if (value.indexOf('nvidia') !== -1) {
      return { hint: 'nvidia', section: 'nvidia-api-key', providerKey: 'nvidia' };
    }
    return null;
  }

  function providerCardDescriptor(card) {
    var title = providerCardTitleNode(card);
    return providerDescriptorFromText(title && title.textContent);
  }

  function providerItemsForDescriptor(items, descriptor) {
    if (!items || !items.length || !descriptor) {
      return [];
    }
    return items.filter(function (item) {
      if (!item) {
        return false;
      }
      if (descriptor.section && normText(item.section || '') === descriptor.section) {
        return true;
      }
      if (descriptor.providerKey && normText(item.provider_key || '') === descriptor.providerKey) {
        return true;
      }
      return providerHintMatchesItem(descriptor.hint, item);
    });
  }

  function sortProviderBalanceItemsByIndex(items) {
    return items.slice().sort(function (a, b) {
      var left = Number(a && a.index);
      var right = Number(b && b.index);
      if (!isFinite(left) && !isFinite(right)) {
        return normText((a && a.display_name) || '').localeCompare(normText((b && b.display_name) || ''));
      }
      if (!isFinite(left)) {
        return 1;
      }
      if (!isFinite(right)) {
        return -1;
      }
      return left - right;
    });
  }

  function closestProviderCard(node) {
    if (!node || typeof node.closest !== 'function') {
      return null;
    }
    var card = node.closest('.card');
    if (!card) {
      return null;
    }
    return providerCardTitleNode(card) ? card : null;
  }

  function providerItemsForOverviewCard(card, items) {
    return providerItemsForDescriptor(items, providerCardDescriptor(card));
  }

  function isProviderDetailCard(node) {
    return !!(node && node.querySelector && node.querySelector("[class*='AiProvidersPage-module__fieldRow___']"));
  }

  function providerItemsForOverviewRow(row, items) {
    if (!row || !items || !items.length) {
      return [];
    }
    var card = closestProviderCard(row);
    if (!card) {
      return [];
    }
    var cardItems = sortProviderBalanceItemsByIndex(providerItemsForOverviewCard(card, items));
    if (!cardItems.length) {
      return [];
    }
    var rows = card.querySelectorAll('.item-row');
    for (var i = 0; i < rows.length; i += 1) {
      if (rows[i] !== row) {
        continue;
      }
      return cardItems[i] ? [cardItems[i]] : cardItems;
    }
    return cardItems;
  }

  function matchedProviderBalanceItemsForNode(node, items, routeHint) {
    if (!node || !items || !items.length) {
      return [];
    }

    if (node.matches && node.matches('.card') && isProviderDetailCard(node) && routeHint) {
      var detailItems = items.filter(function (item) { return providerHintMatchesItem(routeHint, item); });
      if (detailItems.length) {
        var detailIndex = providerRouteIndex();
        if (detailIndex >= 0) {
          var sortedDetail = sortProviderBalanceItemsByIndex(detailItems);
          if (sortedDetail[detailIndex]) {
            return [sortedDetail[detailIndex]];
          }
        }
        return detailItems;
      }
    }

    if (node.matches && node.matches('.item-row')) {
      var overviewRowItems = providerItemsForOverviewRow(node, items);
      if (overviewRowItems.length) {
        return overviewRowItems;
      }
    }

    if (node.matches && node.matches('.card')) {
      var overviewCardItems = providerItemsForOverviewCard(node, items);
      if (overviewCardItems.length) {
        return overviewCardItems;
      }
    }

    if (routeHint) {
      var hinted = items.filter(function (item) { return providerHintMatchesItem(routeHint, item); });
      if (hinted.length) {
        var routeIndex = providerRouteIndex();
        if (routeIndex >= 0) {
          var sortedHinted = sortProviderBalanceItemsByIndex(hinted);
          if (sortedHinted[routeIndex]) {
            return [sortedHinted[routeIndex]];
          }
        }
        return hinted;
      }
    }

    var text = normText(node.textContent || '');
    if (!text) {
      return [];
    }

    var matched = [];
    for (var i = 0; i < items.length; i += 1) {
      var tokens = providerBalanceTokens(items[i]);
      for (var t = 0; t < tokens.length; t += 1) {
        if (containsModelToken(text, tokens[t])) {
          matched.push(items[i]);
          break;
        }
      }
    }
    return matched;
  }

  function providerBalanceRowLabel(lead) {
    var kind = normText(lead && lead.kind);
    if (kind === 'usage_30d') {
      return '近30天';
    }
    if (kind === 'remaining_balance') {
      return '余额';
    }
    return '额度';
  }

  function providerBalanceCurrencySymbol(currency) {
    var raw = String(currency || '').trim().toUpperCase();
    if (raw === 'USD') {
      return '$';
    }
    if (raw === 'CNY') {
      return '¥';
    }
    if (raw === 'EUR') {
      return '€';
    }
    return '';
  }

  function providerBalanceNumber(value) {
    var num = Number(value);
    return isFinite(num) ? num : null;
  }

  function providerBalanceValuePrefix(item) {
    var label = String(item && item.label || '');
    var detail = String(item && item.detail || '');
    if (label.indexOf('钱包') >= 0 || detail.indexOf('账户钱包') >= 0) {
      return '钱包';
    }
    if (label.indexOf('订阅') >= 0 || detail.indexOf('订阅') >= 0) {
      return '今日';
    }
    if (label.indexOf('当日') >= 0 || detail.indexOf('今日') >= 0 || detail.indexOf('后重置') >= 0) {
      return '今日';
    }
    return '余额';
  }

  function providerBalanceDetailParts(item) {
    var detail = String(item && item.detail || '');
    if (!detail) {
      return [];
    }
    return detail.split('|').map(function (part) {
      return String(part || '').trim();
    }).filter(Boolean);
  }

  function providerBalanceFilteredDetail(item) {
    return providerBalanceDetailParts(item).filter(function (part) {
      return !/^来源[:：]/.test(part);
    });
  }

  function providerBalanceSecondaryText(item) {
    var parts = providerBalanceFilteredDetail(item);
    for (var i = 0; i < parts.length; i += 1) {
      if (/^订阅到期\s+/i.test(parts[i])) {
        return parts[i];
      }
    }
    return '';
  }

  function providerBalancePrimaryText(item) {
    if (!item) {
      return '';
    }
    var kind = normText(item.kind);
    var amount = providerBalanceNumber(item.amount);
    var symbol = providerBalanceCurrencySymbol(item.currency);
    if (kind === 'usage_30d' && amount !== null) {
      return '30天 ' + symbol + amount.toFixed(2);
    }
    if (kind === 'remaining_balance' && amount !== null) {
      return providerBalanceValuePrefix(item) + ' ' + symbol + amount.toFixed(2);
    }
    return compactText(item.label || item.detail || item.display_name || '', 48);
  }

  function providerHealthItems(payload) {
    return payload && Array.isArray(payload.items) ? payload.items.slice() : [];
  }

  function providerHealthItemStatus(item) {
    if (!item) {
      return '';
    }
    return item.overall_status || (item.health && item.health.status) || item.status || '';
  }

  function providerHealthScore(item) {
    if (!item) {
      return null;
    }
    var direct = Number(item.score);
    if (isFinite(direct)) {
      return direct;
    }
    var nested = Number(item.health && item.health.score);
    return isFinite(nested) ? nested : null;
  }

  function formatProviderHealthScore(score) {
    var value = Number(score);
    if (!isFinite(value)) {
      return '';
    }
    var rounded = Math.round(value * 10) / 10;
    if (Math.abs(rounded - Math.round(rounded)) < 0.05) {
      return String(Math.round(rounded));
    }
    return rounded.toFixed(1);
  }

  function providerHealthMatchScore(healthItem, balanceItem) {
    if (!healthItem || !balanceItem) {
      return 0;
    }
    var score = 0;
    var healthPrefix = normText(healthItem.prefix || '');
    var balancePrefix = normText(balanceItem.prefix || '');
    if (healthPrefix && balancePrefix && healthPrefix === balancePrefix) {
      score += 8;
    }
    var healthBaseURL = providerShortHost(healthItem.base_url);
    var balanceBaseURL = providerShortHost(balanceItem.base_url);
    if (healthBaseURL && balanceBaseURL && healthBaseURL === balanceBaseURL) {
      score += 6;
    }
    var healthDisplayName = normText(healthItem.display_name || '');
    var balanceDisplayName = normText(balanceItem.display_name || '');
    if (healthDisplayName && balanceDisplayName && healthDisplayName === balanceDisplayName) {
      score += 4;
    }
    if (healthDisplayName && balanceDisplayName && (healthDisplayName.indexOf(balanceDisplayName) >= 0 || balanceDisplayName.indexOf(healthDisplayName) >= 0)) {
      score += 1;
    }
    return score;
  }

  function matchedProviderHealthItem(balanceItem) {
    var items = providerHealthItems(latestProviderHealthData);
    if (!balanceItem || !items.length) {
      return null;
    }
    var best = null;
    var bestScore = 0;
    for (var i = 0; i < items.length; i += 1) {
      var score = providerHealthMatchScore(items[i], balanceItem);
      if (score > bestScore) {
        best = items[i];
        bestScore = score;
      }
    }
    return bestScore > 0 ? best : null;
  }

  function providerHealthText(status) {
    var key = statusKey(status);
    return key === 'green' ? '健康' : key === 'red' ? '不健康' : '未知';
  }

  function upsertProviderBalanceRow(host, status, items) {
    if (!host || !status || !items || !items.length) {
      return;
    }
    var container = null;
    var anchor = null;
    if (host.matches && host.matches('.item-row')) {
      container = host.querySelector('.item-meta') || host;
      anchor = container.querySelector('.item-title');
    } else if (host.matches && host.matches('.card') && isProviderDetailCard(host)) {
      var detailRows = host.querySelectorAll("[class*='AiProvidersPage-module__fieldRow___']");
      anchor = detailRows.length ? detailRows[detailRows.length - 1] : null;
      container = anchor && anchor.parentNode ? anchor.parentNode : host;
    }
    if (!container) {
      return;
    }
    var lead = sortProviderProbeItems(items)[0] || {};
    var healthItem = matchedProviderHealthItem(lead);
    var visualStatus = providerHealthItemStatus(healthItem) || status;
    var key = statusKey(visualStatus);
    var scoreText = formatProviderHealthScore(providerHealthScore(healthItem));
    var row = container.querySelector('[' + PROVIDER_BALANCE_ROW_ATTR + ']');
    if (!row) {
      row = document.createElement('div');
      row.setAttribute(PROVIDER_BALANCE_ROW_ATTR, '1');
      if (anchor && anchor.parentNode === container && anchor.nextSibling) {
        container.insertBefore(row, anchor.nextSibling);
      } else {
        container.appendChild(row);
      }
    }
    row.className = 'cpapi-provider-balance-row';
    row.innerHTML = '';
    var meta = document.createElement('span');
    meta.className = 'cpapi-provider-balance-meta';
    var label = document.createElement('span');
    label.className = 'cpapi-provider-balance-label';
    label.textContent = providerBalanceRowLabel(lead) + ':';
    var badge = document.createElement('span');
    badge.className = 'cpapi-provider-health-badge';
    badge.innerHTML = statusBadge(visualStatus);
    var healthScore = document.createElement('span');
    healthScore.className = 'cpapi-provider-health-score';
    healthScore.textContent = scoreText ? ('健康值 ' + scoreText) : '';
    var valueWrap = document.createElement('span');
    valueWrap.className = 'cpapi-provider-balance-main';
    var value = document.createElement('span');
    value.className = 'cpapi-provider-balance-value';
    value.textContent = providerBalancePrimaryText(lead);
    value.title = [
      providerHealthText(visualStatus),
      scoreText ? ('健康值 ' + scoreText) : '',
      lead.display_name,
      lead.label,
      providerBalanceFilteredDetail(lead).join(' | ')
    ].filter(Boolean).join(' · ');
    value.setAttribute('data-cpapi-provider-balance-text', key);
    var extra = document.createElement('span');
    extra.className = 'cpapi-provider-balance-extra';
    extra.textContent = providerBalanceSecondaryText(lead);
    meta.appendChild(label);
    meta.appendChild(badge);
    if (healthScore.textContent) {
      meta.appendChild(healthScore);
    }
    row.appendChild(meta);
    valueWrap.appendChild(value);
    if (extra.textContent) {
      valueWrap.appendChild(extra);
    }
    row.appendChild(valueWrap);
  }

  function paintProviderBalanceHost(host, status, items) {
    if (!host || !status) {
      return;
    }
    var key = statusKey(status);
    host.setAttribute('data-cpapi-provider-balance', key);
    upsertProviderBalanceRow(host, key, items);
  }

  function scheduleAuthenticatedRefresh() {
    if (providerBalanceReloadTimer) {
      clearTimeout(providerBalanceReloadTimer);
    }
    providerBalanceReloadTimer = setTimeout(function () {
      providerBalanceReloadTimer = 0;
      startRepaintBurst(3, 140);
      load();
      loadProviderHealth();
      loadProviderBalances();
    }, 120);
  }

  function applyProviderBalanceColors(payload) {
    clearProviderBalanceColors();
    if (!isAIProvidersPage()) {
      return;
    }

    var items = payload && Array.isArray(payload.items) ? payload.items.slice() : [];
    if (!items.length) {
      return;
    }

    ensureProviderProbeStyles();
    var hint = providerRouteHint();
    var filtered = hint ? items.filter(function (item) { return providerHintMatchesItem(hint, item); }) : items.slice();
    if (!filtered.length) {
      filtered = items.slice();
    }

    var hosts = document.querySelectorAll(
      '.card,' +
      '.item-row'
    );

    var painted = false;
    for (var i = 0; i < hosts.length; i += 1) {
      var host = hosts[i];
      if (!isVisible(host)) {
        continue;
      }
      var matched = matchedProviderBalanceItemsForNode(host, filtered, hint);
      var status = matched.length ? aggregateProviderBalanceStatus(matched) : '';
      if (!status && hint) {
        status = aggregateProviderBalanceStatus(filtered);
        matched = filtered;
      }
      if (!status) {
        continue;
      }
      paintProviderBalanceHost(host, status, matched);
      painted = true;
    }

    if (!painted && hint) {
      var container = document.querySelector(
        "[class*='AiProvidersPage-module__content___']," +
        "[class*='AiProvidersPage-module__container___']"
      );
      if (container) {
        paintProviderBalanceHost(container, aggregateProviderBalanceStatus(filtered), filtered);
      }
    }
  }

  function loadProviderBalances() {
    var managementKey = getManagementKey();
    if (!managementKey) {
      latestProviderBalanceData = null;
      clearProviderBalanceColors();
      return;
    }

    fetch(PROVIDER_BALANCES_ENDPOINT, {
      method: 'GET',
      headers: {
        'X-Management-Key': managementKey
      },
      cache: 'no-store'
    })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error('HTTP ' + resp.status);
        }
        return resp.json();
      })
      .then(function (payload) {
        latestProviderBalanceData = payload;
        applyProviderBalanceColors(payload);
        startRepaintBurst(2, 140);
      })
      .catch(function (err) {
        window.__CPAPI_PROVIDER_BALANCE_LAST_ERROR__ = String(err && err.message || err || '');
      });
  }

  function loadProviderHealth() {
    var managementKey = getManagementKey();
    if (!managementKey) {
      latestProviderHealthData = null;
      return;
    }

    fetch(PROVIDER_HEALTH_ENDPOINT, {
      method: 'GET',
      headers: {
        'X-Management-Key': managementKey
      },
      cache: 'no-store'
    })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error('HTTP ' + resp.status);
        }
        return resp.json();
      })
      .then(function (payload) {
        latestProviderHealthData = payload;
        applyProviderBalanceColors(latestProviderBalanceData);
        startRepaintBurst(2, 140);
      })
      .catch(function (err) {
        window.__CPAPI_PROVIDER_HEALTH_LAST_ERROR__ = String(err && err.message || err || '');
      });
  }

  installAuthSniffer();
  if (window.__CPAPI_INITIAL_MODEL_HEALTH__ && !initialHealthBootstrapped) {
    initialHealthBootstrapped = true;
    render(window.__CPAPI_INITIAL_MODEL_HEALTH__);
  }
  remountAndRepaint();
  startRepaintBurst(18, 16);
  window.addEventListener('hashchange', function () { startRepaintBurst(18, 16); });
  window.addEventListener('popstate', function () { startRepaintBurst(18, 16); });
  load();
  loadProviderHealth();
  loadProviderBalances();
  setInterval(load, POLL_MS);
  setInterval(loadProviderHealth, POLL_MS);
  setInterval(loadProviderBalances, POLL_MS);
})();
