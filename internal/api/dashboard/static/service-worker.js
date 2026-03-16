/**
 * SKYLENS Service Worker
 * Provides offline caching for the dashboard PWA
 */

const CACHE_NAME = 'skylens-v15';
const DRONE_CACHE_NAME = 'skylens-drone-state-v1';

// Static assets to cache immediately on install
// NOTE: Do NOT cache HTML pages or frequently-updated JS here.
// HTML pages use network-first so users always get fresh script tags.
// JS files use ?v= cache busting in the HTML, so they must not be pre-cached without the version string.
const STATIC_ASSETS = [
  '/css/dashboard.css',
  '/manifest.json'
];

// External assets to cache
const EXTERNAL_ASSETS = [
  'https://unpkg.com/leaflet@1.9.4/dist/leaflet.css',
  'https://unpkg.com/leaflet@1.9.4/dist/leaflet.js',
  'https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600;700&family=IBM+Plex+Sans:wght@400;500;600;700&display=swap'
];

// API endpoints to cache for offline viewing (GET only)
const CACHEABLE_API = [
  '/api/status',
  '/api/drones',
  '/api/taps',
  '/api/threat',
  '/api/fleet',
  '/api/alerts'
];

// Install event - cache static assets
self.addEventListener('install', (event) => {
  console.log('[SW] Installing service worker...');

  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => {
        console.log('[SW] Caching static assets');
        // Cache static assets (don't fail install if some assets fail)
        return Promise.allSettled([
          ...STATIC_ASSETS.map(url => cache.add(url).catch(() => console.warn('[SW] Failed to cache:', url))),
          ...EXTERNAL_ASSETS.map(url => cache.add(url).catch(() => console.warn('[SW] Failed to cache external:', url)))
        ]);
      })
      .then(() => {
        console.log('[SW] Install complete');
        return self.skipWaiting();
      })
  );
});

// Activate event - cleanup old caches
self.addEventListener('activate', (event) => {
  console.log('[SW] Activating service worker...');

  event.waitUntil(
    caches.keys()
      .then((cacheNames) => {
        return Promise.all(
          cacheNames
            .filter((name) => name !== CACHE_NAME && name !== DRONE_CACHE_NAME)
            .map((name) => {
              console.log('[SW] Deleting old cache:', name);
              return caches.delete(name);
            })
        );
      })
      .then(() => {
        console.log('[SW] Activation complete');
        return self.clients.claim();
      })
  );
});

// Fetch event - serve from cache with network fallback
self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);

  // Skip non-GET requests
  if (event.request.method !== 'GET') {
    return;
  }

  // Skip WebSocket connections
  if (url.protocol === 'ws:' || url.protocol === 'wss:') {
    return;
  }

  // Handle API requests with network-first strategy
  if (url.pathname.startsWith('/api/')) {
    event.respondWith(handleApiRequest(event.request));
    return;
  }

  // HTML navigation requests: always network-first (so users get fresh script tags)
  if (event.request.mode === 'navigate' || event.request.destination === 'document') {
    event.respondWith(handleApiRequest(event.request));
    return;
  }

  // Other static assets (CSS, JS, images) with cache-first strategy
  event.respondWith(handleStaticRequest(event.request));
});

// Network-first strategy for API requests
async function handleApiRequest(request) {
  const url = new URL(request.url);
  const isCacheable = CACHEABLE_API.some(path => url.pathname === path);

  try {
    // Try network first
    const response = await fetch(request);

    // Cache successful GET responses for cacheable endpoints
    if (response.ok && isCacheable) {
      const cache = await caches.open(DRONE_CACHE_NAME);
      cache.put(request, response.clone());
    }

    return response;
  } catch (error) {
    // Network failed, try cache
    if (isCacheable) {
      const cached = await caches.match(request);
      if (cached) {
        console.log('[SW] Serving API from cache:', url.pathname);
        // Add header to indicate cached response
        const headers = new Headers(cached.headers);
        headers.set('X-Skylens-Cached', 'true');
        headers.set('X-Skylens-Cache-Time', cached.headers.get('date') || 'unknown');
        return new Response(cached.body, {
          status: cached.status,
          statusText: cached.statusText,
          headers: headers
        });
      }
    }

    // Return offline error response
    return new Response(
      JSON.stringify({
        error: 'Offline',
        message: 'Network unavailable and no cached data',
        offline: true
      }),
      {
        status: 503,
        headers: { 'Content-Type': 'application/json' }
      }
    );
  }
}

// Cache-first strategy for static assets
async function handleStaticRequest(request) {
  // Check cache first
  const cached = await caches.match(request);
  if (cached) {
    // Return cached response, but also update cache in background
    fetchAndCache(request);
    return cached;
  }

  // Not in cache, fetch from network
  try {
    const response = await fetch(request);

    // Cache successful responses
    if (response.ok) {
      const cache = await caches.open(CACHE_NAME);
      cache.put(request, response.clone());
    }

    return response;
  } catch (error) {
    // If it's a navigation request, return the cached index page
    if (request.mode === 'navigate') {
      const cached = await caches.match('/');
      if (cached) {
        return cached;
      }
    }

    // Return offline page or error
    return new Response(
      '<html><body><h1>SKYLENS Offline</h1><p>The dashboard is currently unavailable. Please check your network connection.</p></body></html>',
      {
        status: 503,
        headers: { 'Content-Type': 'text/html' }
      }
    );
  }
}

// Background cache update
async function fetchAndCache(request) {
  try {
    const response = await fetch(request);
    if (response.ok) {
      const cache = await caches.open(CACHE_NAME);
      cache.put(request, response);
    }
  } catch (error) {
    // Ignore background fetch errors
  }
}

// Listen for messages from the main thread
self.addEventListener('message', (event) => {
  if (event.data.type === 'SKIP_WAITING') {
    self.skipWaiting();
  }

  if (event.data.type === 'CACHE_DRONE_STATE') {
    // Cache the current drone state for offline viewing
    cacheDroneState(event.data.state);
  }

  if (event.data.type === 'CLEAR_CACHE') {
    caches.delete(CACHE_NAME);
    caches.delete(DRONE_CACHE_NAME);
  }
});

// Cache drone state for offline viewing
async function cacheDroneState(state) {
  if (!state) return;

  const cache = await caches.open(DRONE_CACHE_NAME);

  // Create a synthetic response with the drone state
  const response = new Response(JSON.stringify(state), {
    headers: {
      'Content-Type': 'application/json',
      'X-Skylens-Cached-At': new Date().toISOString()
    }
  });

  await cache.put('/api/status', response);
  console.log('[SW] Cached drone state');
}
