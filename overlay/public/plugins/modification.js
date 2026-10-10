// modification.js — lampa-go runtime integration.
// Rewrites requests to the cub mirrors into our /cub/ proxy namespace.
// Lives in overlay public/plugins/ so the gulpfile packs it verbatim and
// it deploys as plugins/modification.js — Lampa loads this file
// automatically from the hosting server (see src/core/plugins.js in
// lampa-source), so it needs no patching.
(function () {
	'use strict';

	var MIRRORS = ['cub.best', 'cub.black', 'durex.monster', 'cubnotrip.top'];
	var MARKERS = ['tmdb', 'geo', 'ws', 'imagetmdb', 'cdn', 'ad'];

	// Old TV webviews (Orsay, early webOS) have no location.origin.
	var origin = location.origin || (location.protocol + '//' + location.host);

	function ownHost(host) {
		return (
			host === location.hostname ||
			host.indexOf('.' + location.hostname, host.length - location.hostname.length - 1) !== -1
		);
	}

	// Exact mirrors and their subdomains (tmdb.cub.best) are all ours.
	function mirrorHost(host) {
		for (var i = 0; i < MIRRORS.length; i++) {
			var m = MIRRORS[i];
			if (host === m || host.indexOf('.' + m, host.length - m.length - 1) !== -1) {
				return true;
			}
		}
		return false;
	}

	Lampa.Listener.follow('request_before', function (e) {
		if (!e || !e.params) return;
		var url = e.params.url;
		if (typeof url !== 'string' || url.indexOf('/cub/') !== -1) return;

		var match = url.match(/^https?:\/\/([^\/?#]+)([^?#]*)(\?[^#]*)?/i);
		if (!match) return;

		var host = match[1].toLowerCase();
		var path = match[2] || '/';
		var query = match[3] || '';

		if (!ownHost(host) && !mirrorHost(host)) return;

		var marker = '';
		var labels = host.split('.');
		if (labels.length > 1 && MARKERS.indexOf(labels[0]) !== -1) {
			marker = labels[0] + '/';
		}

		if (path === '/') path = '';

		e.params.url =
			origin + '/cub/' + marker + path.replace(/^\//, '') + query;
	});
})();
