// Doables' service worker. It does one thing: show the notifications the
// server sends, and open the right page when one is tapped. It does not
// cache anything or sit between the pages and the server.

self.addEventListener('install', function () { self.skipWaiting(); });
self.addEventListener('activate', function (e) { e.waitUntil(self.clients.claim()); });

self.addEventListener('push', function (e) {
  var m = {};
  try { m = e.data ? e.data.json() : {}; } catch (err) { m = { body: e.data.text() }; }
  e.waitUntil(self.registration.showNotification(m.title || 'Doables', {
    body: m.body || '',
    tag: m.tag || undefined,
    renotify: !!m.tag, // a replacement still gets noticed
    icon: '/static/icon-192.png',
    badge: '/static/icon-192.png',
    data: { url: m.url || '/' }
  }));
});

// Tapping a notification goes to what it was about: in a Doables window that
// is already open, if there is one, or in a new one.
self.addEventListener('notificationclick', function (e) {
  e.notification.close();
  var url = new URL((e.notification.data && e.notification.data.url) || '/', self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (windows) {
    var open = windows.filter(function (w) { return new URL(w.url).origin === self.location.origin; })[0];
    if (open) {
      open.postMessage({ navigate: url });
      return open.focus();
    }
    return self.clients.openWindow(url);
  }));
});
