/* ═══════════════════════════════════════════════════════
   SKYLENS MOBILE — drawer nav, FAB, gestures
   Loaded on all pages. Only activates at <=900px.
   ═══════════════════════════════════════════════════════ */
(function () {
    'use strict';

    var BREAKPOINT = 900;
    var sidebar = null;
    var overlay = null;
    var hamburger = null;
    var isOpen = false;

    // ── Detect which sidebar variant exists ──
    function getSidebar() {
        return document.getElementById('sidebar') || document.querySelector('.sidebar');
    }

    // ── Create overlay element ──
    function createOverlay() {
        var el = document.createElement('div');
        el.className = 'mobile-overlay';
        el.addEventListener('click', closeDrawer);
        document.body.appendChild(el);
        return el;
    }

    // ── Create hamburger button ──
    function createHamburger() {
        var btn = document.createElement('button');
        btn.className = 'mobile-hamburger';
        btn.setAttribute('aria-label', 'Open navigation');
        btn.innerHTML =
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">' +
            '<line x1="3" y1="6" x2="21" y2="6"/>' +
            '<line x1="3" y1="12" x2="21" y2="12"/>' +
            '<line x1="3" y1="18" x2="21" y2="18"/>' +
            '</svg>';
        btn.addEventListener('click', toggleDrawer);
        return btn;
    }

    // ── Inject hamburger into the appropriate header ──
    function injectHamburger() {
        // New layout: <header class="header">
        var header = document.querySelector('header.header');
        if (header) {
            var left = header.querySelector('.header-left');
            if (left) {
                left.insertBefore(hamburger, left.firstChild);
                return;
            }
        }
        // Old layout: <header id="hdr">
        var hdr = document.getElementById('hdr');
        if (hdr) {
            var hdrLeft = hdr.querySelector('.hdr-left');
            if (hdrLeft) {
                hdrLeft.insertBefore(hamburger, hdrLeft.firstChild);
                return;
            }
        }
        // Fallback: just prepend to body
        document.body.insertBefore(hamburger, document.body.firstChild);
    }

    // ── Open / Close drawer ──
    function openDrawer() {
        if (!sidebar || isOpen) return;
        isOpen = true;
        sidebar.classList.add('drawer-open');
        overlay.style.display = 'block';
        // Force reflow so transition fires
        overlay.offsetHeight; // jshint ignore:line
        overlay.classList.add('visible');
        document.body.classList.add('drawer-open');
        hamburger.setAttribute('aria-label', 'Close navigation');
    }

    function closeDrawer() {
        if (!sidebar || !isOpen) return;
        isOpen = false;
        sidebar.classList.remove('drawer-open');
        overlay.classList.remove('visible');
        document.body.classList.remove('drawer-open');
        hamburger.setAttribute('aria-label', 'Open navigation');
        setTimeout(function () {
            if (!isOpen) overlay.style.display = 'none';
        }, 300);
    }

    function toggleDrawer() {
        if (isOpen) closeDrawer();
        else openDrawer();
    }

    // ── Swipe-to-close gesture ──
    var touchStartX = 0;
    var touchStartY = 0;
    var tracking = false;

    function onTouchStart(e) {
        if (!isOpen) {
            // Swipe from left edge to open
            var touch = e.touches[0];
            if (touch.clientX < 20) {
                touchStartX = touch.clientX;
                touchStartY = touch.clientY;
                tracking = true;
            }
            return;
        }
        var touch = e.touches[0];
        touchStartX = touch.clientX;
        touchStartY = touch.clientY;
        tracking = true;
    }

    function onTouchMove(e) {
        if (!tracking) return;
        var dx = e.touches[0].clientX - touchStartX;
        var dy = e.touches[0].clientY - touchStartY;
        // Must be primarily horizontal
        if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > 10) {
            if (isOpen && dx < -30) {
                closeDrawer();
                tracking = false;
            } else if (!isOpen && dx > 50) {
                openDrawer();
                tracking = false;
            }
        }
    }

    function onTouchEnd() {
        tracking = false;
    }

    // ── Escape key ──
    function onKeyDown(e) {
        if (e.key === 'Escape' && isOpen) {
            closeDrawer();
        }
    }

    // ── Resize handler: close drawer if going above breakpoint ──
    function onResize() {
        if (window.innerWidth > BREAKPOINT && isOpen) {
            closeDrawer();
        }
    }

    // ── FAB MENU (airspace page only) ──
    function initFAB() {
        // Only activate on airspace page
        var isAirspace = document.body.classList.contains('airspace-page') ||
                          window.location.pathname === '/airspace' ||
                          window.location.pathname === '/airspace.html';
        if (!isAirspace) return;

        document.body.classList.add('airspace-page');

        // Create FAB button
        var fab = document.createElement('button');
        fab.className = 'mobile-fab';
        fab.setAttribute('aria-label', 'Map tools');
        fab.innerHTML =
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">' +
            '<circle cx="12" cy="12" r="1"/>' +
            '<circle cx="12" cy="5" r="1"/>' +
            '<circle cx="12" cy="19" r="1"/>' +
            '</svg>';

        // Create FAB menu
        var menu = document.createElement('div');
        menu.className = 'mobile-fab-menu';

        var items = [
            { label: 'Contacts', icon: 'M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2M9 11a4 4 0 100-8 4 4 0 000 8', action: 'contacts' },
            { label: 'Fit All', icon: 'M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7', action: 'fit' },
            { label: 'Trails', icon: 'M22 12h-4l-3 9L9 3l-3 9H2', action: 'trails' },
            { label: 'Refresh', icon: 'M23 4v6h-6M1 20v-6h6M3.51 9a9 9 0 0114.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0020.49 15', action: 'refresh' },
        ];

        items.forEach(function (item) {
            var el = document.createElement('button');
            el.className = 'fab-menu-item';
            el.innerHTML =
                '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
                '<path d="' + item.icon + '"/>' +
                '</svg>' +
                '<span>' + item.label + '</span>';
            el.addEventListener('click', function () {
                handleFABAction(item.action);
                closeFABMenu();
            });
            menu.appendChild(el);
        });

        var fabOpen = false;
        fab.addEventListener('click', function () {
            if (fabOpen) closeFABMenu();
            else openFABMenu();
        });

        function openFABMenu() {
            fabOpen = true;
            menu.classList.add('open');
            fab.innerHTML =
                '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round">' +
                '<line x1="18" y1="6" x2="6" y2="18"/>' +
                '<line x1="6" y1="6" x2="18" y2="18"/>' +
                '</svg>';
        }

        function closeFABMenu() {
            fabOpen = false;
            menu.classList.remove('open');
            fab.innerHTML =
                '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">' +
                '<circle cx="12" cy="12" r="1"/>' +
                '<circle cx="12" cy="5" r="1"/>' +
                '<circle cx="12" cy="19" r="1"/>' +
                '</svg>';
        }

        function handleFABAction(action) {
            var btn;
            switch (action) {
                case 'contacts':
                    var pl = document.getElementById('panel-left');
                    if (pl) {
                        pl.classList.toggle('mobile-expanded');
                        // Close detail panel when toggling contacts
                        var pr = document.getElementById('panel-right');
                        if (pr && pl.classList.contains('mobile-expanded')) {
                            pr.classList.remove('mobile-expanded');
                        }
                    }
                    break;
                case 'fit':
                    btn = document.getElementById('tb-fit');
                    if (btn) btn.click();
                    break;
                case 'trails':
                    btn = document.getElementById('tb-trails');
                    if (btn) btn.click();
                    break;
                case 'refresh':
                    btn = document.getElementById('tb-refresh');
                    if (btn) btn.click();
                    break;
            }
        }

        document.body.appendChild(fab);
        document.body.appendChild(menu);
    }

    // ── AIRSPACE PANEL MANAGEMENT ──
    function initAirspacePanels() {
        var isAirspace = document.body.classList.contains('airspace-page') ||
                          window.location.pathname === '/airspace' ||
                          window.location.pathname === '/airspace.html';
        if (!isAirspace) return;

        var panelLeft = document.getElementById('panel-left');
        var panelRight = document.getElementById('panel-right');
        var detailView = document.getElementById('detail-view');
        var detailEmpty = document.getElementById('detail-empty');

        if (!panelLeft) return;

        function isMobile() {
            return window.innerWidth <= BREAKPOINT;
        }

        // -- State tracking --
        var lastDetailVisible = false;

        function showDetailPanel() {
            if (!isMobile() || !panelRight) return;
            panelRight.classList.add('mobile-expanded');
            panelRight.classList.remove('hidden');
            panelLeft.classList.remove('mobile-expanded');
            lastDetailVisible = true;
        }

        function hideDetailPanel() {
            if (!panelRight) return;
            panelRight.classList.remove('mobile-expanded');
            lastDetailVisible = false;
            // Re-expand contacts when detail is hidden
            if (isMobile()) {
                panelLeft.classList.add('mobile-expanded');
            }
        }

        function checkDetailVisibility() {
            if (!isMobile() || !detailView) return;
            var isVisible = detailView.style.display !== 'none';
            if (isVisible && !lastDetailVisible) {
                showDetailPanel();
            } else if (!isVisible && lastDetailVisible) {
                hideDetailPanel();
            }
        }

        // -- Start contacts panel expanded on mobile --
        if (isMobile()) {
            panelLeft.classList.add('mobile-expanded');
        }

        // -- Contacts panel: tap header to expand/collapse --
        var plHdr = panelLeft.querySelector('.pl-hdr');
        if (plHdr) {
            plHdr.addEventListener('click', function (e) {
                if (!isMobile()) return;
                e.stopPropagation();
                panelLeft.classList.toggle('mobile-expanded');
                if (panelLeft.classList.contains('mobile-expanded') && panelRight) {
                    panelRight.classList.remove('mobile-expanded');
                }
            });
        }

        // -- Detail panel: MutationObserver on style only --
        // No childList — that fires on every telemetry innerHTML rebuild
        if (panelRight && detailView) {
            var detailObserver = new MutationObserver(function () {
                checkDetailVisibility();
            });
            detailObserver.observe(detailView, {
                attributes: true, attributeFilter: ['style']
            });

            if (detailEmpty) {
                var emptyObserver = new MutationObserver(function () {
                    if (!isMobile()) return;
                    if (detailEmpty.style.display !== 'none') {
                        hideDetailPanel();
                    }
                });
                emptyObserver.observe(detailEmpty, { attributes: true, attributeFilter: ['style'] });
            }
        }

        // -- Contact card click: open detail on mobile --
        var uavList = document.getElementById('uav-list');
        if (uavList) {
            uavList.addEventListener('click', function (e) {
                if (!isMobile()) return;
                var card = e.target.closest('.uc');
                if (!card) return;
                if (e.target.closest('.uc-btn')) return;
                // Single rAF — selectDrone runs synchronously so detail
                // is already updated by the time this fires
                requestAnimationFrame(function () {
                    if (detailView && detailView.style.display !== 'none') {
                        showDetailPanel();
                    }
                });
            });
        }

        // -- Close detail panel via close button --
        if (panelRight) {
            panelRight.addEventListener('click', function (e) {
                if (!isMobile()) return;
                if (e.target.closest('.det-close')) {
                    // Instant — no rAF delay
                    hideDetailPanel();
                }
            });
        }

        // -- Swipe down to collapse (only when scrolled to top) --
        [panelLeft, panelRight].forEach(function (panel) {
            if (!panel) return;
            var startY = 0;
            var panelTracking = false;

            panel.addEventListener('touchstart', function (e) {
                if (!isMobile()) return;
                if (!panel.classList.contains('mobile-expanded')) return;
                if (panel.scrollTop > 5) return;
                startY = e.touches[0].clientY;
                panelTracking = true;
            }, { passive: true });

            panel.addEventListener('touchmove', function (e) {
                if (!panelTracking || !isMobile()) return;
                var dy = e.touches[0].clientY - startY;
                if (dy > 50) {
                    panel.classList.remove('mobile-expanded');
                    panelTracking = false;
                    if (panel === panelRight) {
                        lastDetailVisible = false;
                        // Re-show contacts after swiping down detail
                        panelLeft.classList.add('mobile-expanded');
                    }
                }
            }, { passive: true });

            panel.addEventListener('touchend', function () {
                panelTracking = false;
            }, { passive: true });
        });

        // -- Hide panels when modals open --
        var modalBg = document.getElementById('modal-bg');
        if (modalBg) {
            var modalObserver = new MutationObserver(function () {
                if (!isMobile()) return;
                if (modalBg.style.display !== 'none') {
                    panelLeft.classList.remove('mobile-expanded');
                    if (panelRight) panelRight.classList.remove('mobile-expanded');
                    lastDetailVisible = false;
                }
            });
            modalObserver.observe(modalBg, { attributes: true, attributeFilter: ['style'] });
        }

        // -- Resize: restore contacts on return to mobile --
        window.addEventListener('resize', function () {
            if (isMobile() && !panelLeft.classList.contains('mobile-expanded') &&
                (!panelRight || !panelRight.classList.contains('mobile-expanded'))) {
                panelLeft.classList.add('mobile-expanded');
            }
        });
    }

    // ── INIT ──
    function init() {
        sidebar = getSidebar();
        if (!sidebar && !document.body.classList.contains('airspace-page') &&
            window.location.pathname !== '/airspace') {
            // Login page or page without sidebar — still load FAB if needed
            initFAB();
            return;
        }

        // Only create drawer elements if sidebar exists
        if (sidebar) {
            overlay = createOverlay();
            hamburger = createHamburger();
            injectHamburger();

            document.addEventListener('touchstart', onTouchStart, { passive: true });
            document.addEventListener('touchmove', onTouchMove, { passive: true });
            document.addEventListener('touchend', onTouchEnd, { passive: true });
        }

        document.addEventListener('keydown', onKeyDown);
        window.addEventListener('resize', onResize);

        initFAB();
        initAirspacePanels();
    }

    // Run after DOM is ready
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
