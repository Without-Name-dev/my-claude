const CACHE_NAME = 'aa-cache-v1';
const RUNTIME_CACHE = 'aa-runtime-v1';

// 静态资源缓存列表
const STATIC_ASSETS = [
  '/',
  '/offline',
  '/icon-192.png',
  '/icon-512.png',
  '/placeholder.svg',
];

// 安装事件 - 缓存静态资源
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => {
      return cache.addAll(STATIC_ASSETS);
    }).then(() => {
      return self.skipWaiting();
    })
  );
});

// 激活事件 - 清理旧缓存
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((cacheNames) => {
      return Promise.all(
        cacheNames.map((cacheName) => {
          if (cacheName !== CACHE_NAME && cacheName !== RUNTIME_CACHE) {
            return caches.delete(cacheName);
          }
        })
      );
    }).then(() => {
      return self.clients.claim();
    })
  );
});

// Fetch 事件 - 网络优先，失败时使用缓存
self.addEventListener('fetch', (event) => {
  const { request } = event;
  const url = new URL(request.url);

  // 跳过非 GET 请求
  if (request.method !== 'GET') {
    return;
  }

  // 跳过 chrome-extension 和其他协议
  if (!url.protocol.startsWith('http')) {
    return;
  }

  // API 请求 - 仅网络。这些响应携带账户凭据，进入按 URL 共享的缓存
  // 会把数据泄露给同一浏览器配置文件的下一个使用者，因此永远不缓存。
  if (url.pathname.startsWith('/api/')) {
    event.respondWith(
      fetch(request).catch(() => new Response(
        JSON.stringify({ error: 'Offline', detail: 'Network unavailable' }),
        { status: 503, headers: { 'Content-Type': 'application/json' } }
      ))
    );
    return;
  }

  // 导航请求 - 网络优先。缓存优先的固定命名缓存会让已安装的客户端
  // 在每次部署后仍然永远启动旧的应用外壳。
  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request)
        .then((response) => {
          if (response.ok && url.origin === self.location.origin) {
            const responseClone = response.clone();
            caches.open(RUNTIME_CACHE).then((cache) => {
              cache.put(request, responseClone);
            });
          }
          return response;
        })
        .catch(() => caches.match(request).then((cached) => cached || caches.match('/offline')))
    );
    return;
  }

  // 静态资源 - 缓存优先
  event.respondWith(
    caches.match(request).then((cached) => {
      if (cached) {
        return cached;
      }

      return fetch(request).then((response) => {
        if (response.ok && url.origin === self.location.origin) {
          const responseClone = response.clone();
          caches.open(RUNTIME_CACHE).then((cache) => {
            cache.put(request, responseClone);
          });
        }
        return response;
      }).catch(() => {
        if (request.mode === 'navigate') {
          return caches.match('/offline') || new Response(
            '<html><body><h1>Offline</h1><p>Please check your network connection.</p></body></html>',
            { status: 503, headers: { 'Content-Type': 'text/html' } }
          );
        }
        return new Response('Offline', { status: 503 });
      });
    })
  );
});

// 后台同步已移除：服务端仅接受 Bearer 头认证，Service Worker 无法携带
// 令牌，任何后台抓取都只会得到 401。需要时请通过 postMessage 把短期
// 令牌交给 worker 再恢复此功能。
