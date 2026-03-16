/**
 * SKYLENS Authentication Client
 * Handles token management, session refresh, and protected requests
 */

const SkylensAuth = (function() {
    'use strict';

    // Configuration
    const CONFIG = {
        loginUrl: '/login',
        refreshEndpoint: '/api/auth/refresh',
        meEndpoint: '/api/auth/me',
        csrfEndpoint: '/api/auth/csrf',
        sessionWarningMinutes: 5,
        refreshCheckInterval: 60000,
    };

    // State
    let currentUser = null;
    let csrfToken = null;
    let refreshTimer = null;
    let warningShown = false;
    let authChecked = false;
    var _authPromise = null; // stored promise from requireAuth()

    // Preferences sync
    var PREFS_SYNC_KEYS = [
        'skylens_theme', 'skylens_poll_interval', 'skylens_compact_mode',
        'skylens_auto_refresh', 'skylens_spoof_threshold', 'skylens_auth_grace',
        'skylens_stale_timeout', 'skylens_map_center_lat', 'skylens_map_center_lng',
        'skylens_map_zoom', 'skylens_map_style', 'skylens_range_ring_radius',
        'skylens_trail_length', 'skylens_sound_alerts', 'skylens_show_controllers',
        'skylens_custom_ranges', 'skylens_range_rings_visible', 'skylens_audio', 'nz_settings', 'nz_zones'
    ];
    var _prefsSaveTimer = null;

    let _pageRevealed = false;

    /**
     * Hide page content until auth is confirmed
     */
    function hidePageContent() {
        // Add style to hide body content immediately
        if (!document.getElementById('auth-hide-style')) {
            const style = document.createElement('style');
            style.id = 'auth-hide-style';
            style.textContent = 'body { opacity: 0 !important; }';
            document.head.appendChild(style);
        }
        // Add loading overlay
        if (!document.getElementById('skylens-loading')) {
            const overlay = document.createElement('div');
            overlay.id = 'skylens-loading';
            overlay.innerHTML = '<div style="display:flex;flex-direction:column;align-items:center;gap:16px">' +
                '<div style="width:40px;height:40px;border:3px solid #2A3832;border-top-color:#4CAF50;border-radius:50%;animation:sl-spin .8s linear infinite"></div>' +
                '<div style="font-family:\'IBM Plex Sans\',sans-serif;font-size:13px;color:#6B9B74;letter-spacing:2px">SKYLENS</div></div>';
            overlay.style.cssText = 'position:fixed;top:0;left:0;right:0;bottom:0;background:#0A0E0D;display:flex;align-items:center;justify-content:center;z-index:99999;transition:opacity .3s ease';
            const ks = document.createElement('style');
            ks.textContent = '@keyframes sl-spin{to{transform:rotate(360deg)}}';
            document.head.appendChild(ks);
            document.body ? document.body.appendChild(overlay) :
                document.addEventListener('DOMContentLoaded', function() { document.body.appendChild(overlay); });
        }
    }

    /**
     * Show page content after auth + first data load
     * Called automatically after auth, or manually via SkylensAuth.revealPage()
     */
    function showPageContent() {
        if (_pageRevealed) return;
        _pageRevealed = true;
        // Small delay to let the first data fetch populate the DOM
        setTimeout(function() {
            const style = document.getElementById('auth-hide-style');
            if (style) style.remove();
            const overlay = document.getElementById('skylens-loading');
            if (overlay) {
                overlay.style.opacity = '0';
                setTimeout(function() { overlay.remove(); }, 300);
            }
        }, 400);
    }

    /**
     * Reveal page immediately (call from page JS after first data load)
     */
    function revealPage() {
        showPageContent();
    }

    /**
     * Initialize authentication (optional, doesn't redirect)
     */
    async function init() {
        try {
            const cached = localStorage.getItem('skylens_user');
            if (cached) {
                currentUser = JSON.parse(cached);
            }
        } catch (e) {}

        try {
            const response = await fetch(CONFIG.meEndpoint, {
                credentials: 'include'
            });

            if (!response.ok) {
                throw new Error('Not authenticated');
            }

            const data = await response.json();

            // Check if actually authenticated (not anonymous/disabled)
            if (!data.user || data.user.id === 0) {
                throw new Error('Not authenticated');
            }

            currentUser = data.user;
            localStorage.setItem('skylens_user', JSON.stringify(currentUser));
            await refreshCSRFToken();
            startSessionMonitor();
            authChecked = true;
            return currentUser;

        } catch (e) {
            currentUser = null;
            localStorage.removeItem('skylens_user');
            authChecked = true;
            return null;
        }
    }

    /**
     * Require authentication - redirects to login if not authenticated
     * BLOCKS page rendering until auth is confirmed
     */
    async function requireAuth() {
        if (!_authPromise) {
            _authPromise = _doRequireAuth();
        }
        return _authPromise;
    }

    async function _doRequireAuth() {
        // Hide content immediately to prevent flash
        hidePageContent();

        const user = await init();
        if (!user) {
            redirectToLogin();
            return false;
        }

        // Load per-account preferences from server into localStorage
        await loadPreferences();

        // Auth successful, show content
        showPageContent();
        return true;
    }

    /**
     * Wait for auth + preferences to be fully loaded.
     * Pages should await this before reading localStorage settings.
     */
    function whenReady() {
        return _authPromise || Promise.resolve(true);
    }

    /**
     * Redirect to login page
     */
    function redirectToLogin(message) {
        const currentPath = window.location.pathname;
        // Don't include login page as return URL
        if (currentPath === '/login' || currentPath === '/login.html') {
            window.location.href = CONFIG.loginUrl;
            return;
        }
        const returnUrl = encodeURIComponent(currentPath + window.location.search);
        let url = CONFIG.loginUrl + '?return=' + returnUrl;
        if (message) {
            url += '&message=' + encodeURIComponent(message);
        }
        window.location.href = url;
    }

    /**
     * Get current user
     */
    function getUser() {
        return currentUser;
    }

    /**
     * Check if user has a specific permission
     */
    function hasPermission(permission) {
        if (!currentUser || !currentUser.permissions) return false;
        return currentUser.permissions.includes(permission);
    }

    /**
     * Check if user has a specific role
     */
    function hasRole(role) {
        if (!currentUser) return false;
        return currentUser.role_name === role;
    }

    /**
     * Check if user is admin
     */
    function isAdmin() {
        return hasRole('admin');
    }

    /**
     * Check if user can access a TAP
     */
    function canAccessTap(tapId) {
        if (!currentUser) return false;
        if (currentUser.role_name === 'admin' || currentUser.role_name === 'operator') {
            return true;
        }
        if (currentUser.allowed_taps && currentUser.allowed_taps.includes(tapId)) {
            return true;
        }
        return false;
    }

    /**
     * Make an authenticated fetch request
     */
    async function authFetch(url, options = {}) {
        if (!csrfToken && ['POST', 'PUT', 'PATCH', 'DELETE'].includes(options.method)) {
            await refreshCSRFToken();
        }

        if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(options.method) && csrfToken) {
            options.headers = options.headers || {};
            options.headers['X-CSRF-Token'] = csrfToken;
        }

        options.credentials = 'include';

        const response = await fetch(url, options);

        if (response.status === 401) {
            const refreshed = await refreshSession();
            if (refreshed) {
                return fetch(url, options);
            } else {
                redirectToLogin('Session expired');
                throw new Error('Session expired');
            }
        }

        return response;
    }

    /**
     * Refresh the session token
     */
    async function refreshSession() {
        try {
            const response = await fetch(CONFIG.refreshEndpoint, {
                method: 'POST',
                credentials: 'include'
            });

            if (response.ok) {
                warningShown = false;
                await refreshCSRFToken();
                return true;
            }
            return false;
        } catch (e) {
            return false;
        }
    }

    /**
     * Refresh CSRF token
     */
    async function refreshCSRFToken() {
        try {
            const response = await fetch(CONFIG.csrfEndpoint, {
                credentials: 'include'
            });
            if (response.ok) {
                const data = await response.json();
                csrfToken = data.csrf_token;
            }
        } catch (e) {}
    }

    /**
     * Logout
     */
    async function logout() {
        try {
            await authFetch('/api/auth/logout', {
                method: 'POST'
            });
        } catch (e) {}

        currentUser = null;
        csrfToken = null;
        localStorage.removeItem('skylens_user');
        stopSessionMonitor();
        window.location.href = CONFIG.loginUrl;
    }

    /**
     * Start session monitoring
     */
    function startSessionMonitor() {
        if (refreshTimer) return;

        refreshTimer = setInterval(async () => {
            const refreshed = await refreshSession();
            if (!refreshed && !warningShown) {
                showSessionWarning();
            }
        }, CONFIG.refreshCheckInterval);
    }

    /**
     * Stop session monitoring
     */
    function stopSessionMonitor() {
        if (refreshTimer) {
            clearInterval(refreshTimer);
            refreshTimer = null;
        }
    }

    /**
     * Show session expiry warning
     */
    function showSessionWarning() {
        warningShown = true;

        let banner = document.getElementById('session-warning');
        if (!banner) {
            banner = document.createElement('div');
            banner.id = 'session-warning';
            banner.className = 'session-warning';
            banner.innerHTML = `
                <span class="session-warning-text">Your session will expire soon.</span>
                <button class="session-warning-btn" onclick="SkylensAuth.refreshSession().then(function(ok){if(ok)document.getElementById('session-warning').remove()})">Stay Signed In</button>
                <button class="session-warning-close" onclick="this.parentElement.remove()">&times;</button>
            `;
            document.body.appendChild(banner);

            if (!document.getElementById('session-warning-styles')) {
                const style = document.createElement('style');
                style.id = 'session-warning-styles';
                style.textContent = `
                    .session-warning {
                        position: fixed;
                        top: 0;
                        left: 0;
                        right: 0;
                        background: linear-gradient(135deg, #FF6D00 0%, #FF9100 100%);
                        color: #000;
                        padding: 12px 20px;
                        display: flex;
                        align-items: center;
                        justify-content: center;
                        gap: 16px;
                        font-size: 14px;
                        font-weight: 500;
                        z-index: 10000;
                        animation: slideDown 0.3s ease-out;
                    }
                    @keyframes slideDown {
                        from { transform: translateY(-100%); }
                        to { transform: translateY(0); }
                    }
                    .session-warning-btn {
                        background: #000;
                        color: #FF9100;
                        border: none;
                        padding: 6px 16px;
                        border-radius: 4px;
                        font-weight: 600;
                        cursor: pointer;
                    }
                    .session-warning-btn:hover {
                        background: #111;
                    }
                    .session-warning-close {
                        background: none;
                        border: none;
                        color: #000;
                        font-size: 20px;
                        cursor: pointer;
                        padding: 0 8px;
                        opacity: 0.7;
                    }
                    .session-warning-close:hover {
                        opacity: 1;
                    }
                `;
                document.head.appendChild(style);
            }
        }
    }

    /**
     * Render user menu in sidebar
     */
    function renderUserMenu(containerId) {
        const container = document.getElementById(containerId);
        if (!container || !currentUser) return;

        const initials = getInitials(currentUser.display_name || currentUser.username);

        container.innerHTML = `
            <div class="user-menu" onclick="SkylensAuth.toggleUserDropdown()">
                <div class="user-avatar">${initials}</div>
                <div class="user-info">
                    <div class="user-name">${escapeHtml(currentUser.username)}</div>
                    <div class="user-role">${escapeHtml(currentUser.role_name)}</div>
                </div>
                <span class="user-menu-chevron">&#9660;</span>
            </div>
            <div class="user-dropdown" id="user-dropdown" style="display:none">
                <a href="/profile" class="dropdown-item">
                    <span class="dropdown-icon">&#9881;</span> Profile
                </a>
                ${currentUser.role_name === 'admin' ? `
                <a href="/admin" class="dropdown-item">
                    <span class="dropdown-icon">&#128101;</span> User Management
                </a>
                ` : ''}
                <div class="dropdown-divider"></div>
                <button class="dropdown-item logout" onclick="SkylensAuth.logout()">
                    <span class="dropdown-icon">&#128682;</span> Sign Out
                </button>
            </div>
        `;

        if (!document.getElementById('user-menu-styles')) {
            const style = document.createElement('style');
            style.id = 'user-menu-styles';
            style.textContent = `
                .user-menu {
                    display: flex;
                    align-items: center;
                    gap: 10px;
                    padding: 10px 14px;
                    background: var(--bg1);
                    border-radius: 8px;
                    margin: 8px;
                    cursor: pointer;
                    transition: background 0.15s ease, transform 0.1s ease;
                }
                .user-menu:hover {
                    background: var(--bg3);
                }
                .user-menu:active {
                    transform: scale(0.98);
                }
                .user-avatar {
                    width: 32px;
                    height: 32px;
                    background: linear-gradient(135deg, var(--accent) 0%, var(--accent-hi) 100%);
                    border-radius: 6px;
                    display: flex;
                    align-items: center;
                    justify-content: center;
                    font-size: 12px;
                    font-weight: 700;
                    color: var(--bg0);
                    flex-shrink: 0;
                    text-transform: uppercase;
                    letter-spacing: 0.5px;
                }
                .user-info {
                    flex: 1;
                    min-width: 0;
                }
                .user-name {
                    font-size: 12px;
                    font-weight: 600;
                    color: var(--t0);
                    white-space: nowrap;
                    overflow: hidden;
                    text-overflow: ellipsis;
                    line-height: 1.2;
                }
                .user-role {
                    font-size: 10px;
                    color: var(--t3);
                    text-transform: uppercase;
                    letter-spacing: 0.5px;
                    font-weight: 500;
                }
                .user-menu-chevron {
                    color: var(--t3);
                    font-size: 8px;
                    transition: transform 0.2s ease;
                }
                .user-menu.open .user-menu-chevron {
                    transform: rotate(180deg);
                }
                .user-dropdown {
                    background: var(--bg1);
                    margin: 0 8px 8px;
                    border-radius: 8px;
                    overflow: hidden;
                    box-shadow: 0 4px 12px rgba(0,0,0,0.3);
                }
                .dropdown-item {
                    display: flex;
                    align-items: center;
                    gap: 10px;
                    padding: 10px 14px;
                    color: var(--t1);
                    text-decoration: none;
                    font-size: 12px;
                    border: none;
                    background: none;
                    width: 100%;
                    text-align: left;
                    cursor: pointer;
                    transition: background 0.1s ease, color 0.1s ease;
                }
                .dropdown-item:hover {
                    background: var(--bg3);
                    color: var(--t0);
                }
                .dropdown-item.logout {
                    color: var(--critical);
                }
                .dropdown-item.logout:hover {
                    background: rgba(255,82,82,0.1);
                }
                .dropdown-icon {
                    font-size: 13px;
                    width: 18px;
                    text-align: center;
                    opacity: 0.7;
                }
                .dropdown-divider {
                    height: 1px;
                    background: var(--border);
                    margin: 4px 8px;
                }
            `;
            document.head.appendChild(style);
        }
    }

    /**
     * Toggle user dropdown
     */
    function toggleUserDropdown() {
        const dropdown = document.getElementById('user-dropdown');
        const menu = document.querySelector('.user-menu');
        if (dropdown) {
            const isOpen = dropdown.style.display !== 'none';
            dropdown.style.display = isOpen ? 'none' : 'block';
            if (menu) menu.classList.toggle('open', !isOpen);
        }
    }

    /**
     * Get initials from name
     */
    function getInitials(name) {
        if (!name) return '?';
        const parts = name.split(/[\s-_]+/);
        if (parts.length >= 2) {
            return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
        }
        return name.slice(0, 2).toUpperCase();
    }

    /**
     * Escape HTML
     */
    function escapeHtml(text) {
        if (!text) return '';
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }

    /**
     * Load preferences from server into localStorage (server = source of truth)
     */
    async function loadPreferences() {
        try {
            var resp = await fetch('/api/user/preferences', { credentials: 'include' });
            if (!resp.ok) { console.warn('[PREFS] loadPreferences failed:', resp.status); return; }
            var prefs = await resp.json();
            var keys = Object.keys(prefs);
            console.log('[PREFS] Loaded', keys.length, 'keys from server');
            for (var i = 0; i < keys.length; i++) {
                var k = keys[i];
                var v = prefs[k];
                if (k === 'skylens_theme') {
                    localStorage.setItem(k, v);
                } else {
                    localStorage.setItem(k, typeof v === 'string' ? v : JSON.stringify(v));
                }
            }
        } catch (e) { console.error('[PREFS] loadPreferences error:', e); }
    }

    /**
     * Core save: collect all synced keys from localStorage and PUT to server
     */
    function _doSavePreferences() {
        var prefs = {};
        for (var i = 0; i < PREFS_SYNC_KEYS.length; i++) {
            var k = PREFS_SYNC_KEYS[i];
            var raw = localStorage.getItem(k);
            if (raw !== null) {
                if (k === 'skylens_theme') {
                    prefs[k] = raw;
                } else {
                    try { prefs[k] = JSON.parse(raw); } catch(e) { prefs[k] = raw; }
                }
            }
        }
        console.log('[PREFS] Saving', Object.keys(prefs).length, 'keys to server');
        authFetch('/api/user/preferences', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(prefs)
        }).then(function(r) {
            console.log('[PREFS] Save response:', r.status);
        }).catch(function(e) { console.error('[PREFS] Save failed:', e); });
    }

    /**
     * Save preferences (debounced 1s) — for rapid-fire settings changes
     */
    function savePreferences() {
        if (_prefsSaveTimer) clearTimeout(_prefsSaveTimer);
        _prefsSaveTimer = setTimeout(function() {
            _prefsSaveTimer = null;
            _doSavePreferences();
        }, 1000);
    }

    /**
     * Save preferences immediately — for explicit actions (add ring, save zone)
     */
    function savePreferencesNow() {
        if (_prefsSaveTimer) { clearTimeout(_prefsSaveTimer); _prefsSaveTimer = null; }
        _doSavePreferences();
    }

    // Flush any pending debounced save when navigating away
    if (typeof window !== 'undefined') {
        window.addEventListener('visibilitychange', function() {
            if (document.visibilityState === 'hidden' && _prefsSaveTimer) {
                clearTimeout(_prefsSaveTimer);
                _prefsSaveTimer = null;
                _doSavePreferences();
            }
        });
        window.addEventListener('beforeunload', function() {
            if (_prefsSaveTimer) {
                clearTimeout(_prefsSaveTimer);
                _prefsSaveTimer = null;
                _doSavePreferences();
            }
        });
    }

    // Public API
    return {
        init,
        requireAuth,
        getUser,
        hasPermission,
        hasRole,
        isAdmin,
        canAccessTap,
        fetch: authFetch,
        logout,
        refreshSession,
        renderUserMenu,
        toggleUserDropdown,
        redirectToLogin,
        revealPage,
        savePreferences,
        savePreferencesNow,
        whenReady
    };
})();

// Export for CommonJS environments
if (typeof module !== 'undefined' && module.exports) {
    module.exports = SkylensAuth;
}
