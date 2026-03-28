// dicode Service Worker — shows browser notifications for run completion.
self.addEventListener('message', function(e) {
  if (!e.data || e.data.type !== 'run:complete') return;
  var d = e.data;
  var icon = d.status === 'success' ? '✓' : '✗';
  var secs = (d.durationMs / 1000).toFixed(1);
  var body = icon + ' ' + d.status + ' — ' + secs + 's';
  if (d.triggerSource && d.triggerSource !== 'manual') {
    body += ' (' + d.triggerSource + ')';
  }
  e.waitUntil(
    self.registration.showNotification('[dicode] ' + d.taskName, {
      body: body,
      tag: d.runID,
      data: { runID: d.runID },
      requireInteraction: false,
    })
  );
});

self.addEventListener('notificationclick', function(e) {
  e.notification.close();
  var runID = e.notification.data && e.notification.data.runID;
  var url = runID ? '/runs/' + runID : '/';
  e.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function(list) {
      for (var i = 0; i < list.length; i++) {
        if (list[i].url.includes(url)) {
          return list[i].focus();
        }
      }
      return clients.openWindow(url);
    })
  );
});
