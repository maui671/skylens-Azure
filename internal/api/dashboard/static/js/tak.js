/**
 * SKYLENS — TAK Server Settings
 */

(function() {
  'use strict';

  // DOM refs
  var els = {
    status:          document.getElementById('tak-status'),
    address:         document.getElementById('tak-address'),
    useTLS:          document.getElementById('tak-use-tls'),
    enabled:         document.getElementById('tak-enabled'),
    rateLimit:       document.getElementById('tak-rate-limit'),
    rateLimitVal:    document.getElementById('tak-rate-limit-val'),
    staleTime:       document.getElementById('tak-stale-time'),
    staleTimeVal:    document.getElementById('tak-stale-time-val'),
    sendControllers: document.getElementById('tak-send-controllers'),
    certFile:        document.getElementById('tak-cert-file'),
    keyFile:         document.getElementById('tak-key-file'),
    caFile:          document.getElementById('tak-ca-file'),
    certInput:       document.getElementById('tak-cert-input'),
    keyInput:        document.getElementById('tak-key-input'),
    caInput:         document.getElementById('tak-ca-input'),
    lastError:       document.getElementById('tak-last-error'),
    saveBtn:         document.getElementById('tak-save-btn'),
    testBtn:         document.getElementById('tak-test-btn')
  };

  // Slider value display
  if (els.rateLimit) {
    els.rateLimit.addEventListener('input', function() {
      els.rateLimitVal.textContent = this.value + 's';
    });
  }
  if (els.staleTime) {
    els.staleTime.addEventListener('input', function() {
      els.staleTimeVal.textContent = this.value + 's';
    });
  }

  // Cert upload handlers
  if (els.certInput) els.certInput.addEventListener('change', function() { uploadCert('cert', this); });
  if (els.keyInput)  els.keyInput.addEventListener('change', function() { uploadCert('key', this); });
  if (els.caInput)   els.caInput.addEventListener('change', function() { uploadCert('ca', this); });

  // Clock
  setInterval(function() {
    var el = document.getElementById('hdr-clock');
    if (el) el.textContent = new Date().toLocaleTimeString('en-US', { hour12: false });
  }, 1000);

  // ─── STATUS CHECK ───
  function checkTAKStatus() {
    fetch('/api/tak/status')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        // Connection status
        if (!data.address) {
          els.status.textContent = 'Not configured';
          els.status.className = 'field-hint tak-status-off';
        } else if (data.connected) {
          els.status.textContent = 'Connected to ' + data.address;
          els.status.className = 'field-hint tak-status-ok';
        } else if (data.enabled) {
          els.status.textContent = 'Disconnected — reconnecting...';
          els.status.className = 'field-hint tak-status-err';
        } else {
          els.status.textContent = 'Disabled';
          els.status.className = 'field-hint tak-status-off';
        }

        // Populate fields
        if (els.address)         els.address.value = data.address || '';
        if (els.useTLS)          els.useTLS.checked = data.use_tls !== false;
        if (els.enabled)         els.enabled.checked = !!data.enabled;
        if (els.sendControllers) els.sendControllers.checked = !!data.send_controllers;

        if (els.rateLimit && data.rate_limit_sec) {
          els.rateLimit.value = data.rate_limit_sec;
          els.rateLimitVal.textContent = data.rate_limit_sec + 's';
        }
        if (els.staleTime && data.stale_seconds) {
          els.staleTime.value = data.stale_seconds;
          els.staleTimeVal.textContent = data.stale_seconds + 's';
        }

        // Cert file display
        setCertDisplay(els.certFile, data.cert_file);
        setCertDisplay(els.keyFile, data.key_file);
        setCertDisplay(els.caFile, data.ca_file);

        // Last error
        if (data.last_error) {
          els.lastError.textContent = data.last_error;
          els.lastError.style.display = '';
        } else {
          els.lastError.style.display = 'none';
        }
      })
      .catch(function(err) {
        els.status.textContent = 'Failed to load status';
        els.status.className = 'field-hint tak-status-err';
      });
  }

  function setCertDisplay(el, path) {
    if (!el) return;
    if (path) {
      // Show just the filename
      var name = path.split('/').pop();
      el.textContent = name;
      el.className = 'cert-file loaded';
    } else {
      el.textContent = 'Not uploaded';
      el.className = 'cert-file';
    }
  }

  // ─── SAVE ───
  window.saveTAK = function() {
    els.saveBtn.disabled = true;
    els.saveBtn.textContent = 'Saving...';

    var payload = {
      enabled:          els.enabled.checked,
      address:          els.address.value.trim(),
      use_tls:          els.useTLS.checked,
      rate_limit_sec:   parseInt(els.rateLimit.value, 10),
      stale_seconds:    parseInt(els.staleTime.value, 10),
      send_controllers: els.sendControllers.checked
    };

    fetch('/api/tak/status', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    })
    .then(function(r) { return r.json(); })
    .then(function(data) {
      if (data.ok) {
        showToast('TAK settings saved');
        checkTAKStatus();
      } else {
        showToast('Failed to save: ' + (data.error || 'Unknown error'), true);
      }
    })
    .catch(function(err) {
      showToast('Network error: ' + err.message, true);
    })
    .finally(function() {
      els.saveBtn.disabled = false;
      els.saveBtn.textContent = 'Save Settings';
    });
  };

  // ─── TEST CONNECTION ───
  window.testTAK = function() {
    els.testBtn.disabled = true;
    els.testBtn.textContent = 'Testing...';

    fetch('/api/tak/test', { method: 'POST' })
    .then(function(r) { return r.json(); })
    .then(function(data) {
      if (data.ok) {
        showToast(data.message || 'Connection successful');
      } else {
        showToast('Test failed: ' + (data.error || 'Unknown error'), true);
      }
    })
    .catch(function(err) {
      showToast('Network error: ' + err.message, true);
    })
    .finally(function() {
      els.testBtn.disabled = false;
      els.testBtn.textContent = 'Test Connection';
    });
  };

  // ─── UPLOAD CERT ───
  function uploadCert(type, input) {
    if (!input.files || !input.files[0]) return;

    var file = input.files[0];
    var form = new FormData();
    form.append('file', file);
    form.append('type', type);

    fetch('/api/tak/upload-cert', {
      method: 'POST',
      body: form
    })
    .then(function(r) { return r.json(); })
    .then(function(data) {
      if (data.ok) {
        showToast('Uploaded ' + data.filename);
        checkTAKStatus();
      } else {
        showToast('Upload failed: ' + (data.error || 'Unknown'), true);
      }
    })
    .catch(function(err) {
      showToast('Upload error: ' + err.message, true);
    });

    // Reset input so same file can be re-uploaded
    input.value = '';
  }

  // ─── TOAST ───
  function showToast(msg, isError) {
    var t = document.getElementById('toast');
    if (!t) return;
    t.textContent = msg;
    t.className = 'toast visible' + (isError ? ' error' : '');
    clearTimeout(t._timer);
    t._timer = setTimeout(function() {
      t.className = 'toast';
    }, 3000);
  }

  // Init
  checkTAKStatus();
  setInterval(checkTAKStatus, 10000);

})();
