'use strict';
'require view';
'require form';
'require uci';

return view.extend({
	render: function() {
		var m, s, o;

		m = new form.Map('cloud_manager', _('Cloud Manager'),
			_('Configuration for the OpenWrt Cloud Manager telemetry and remote administration service.'));

		s = m.section(form.NamedSection, 'main', 'cloud_manager', _('General Settings'));
		s.anonymous = false;
		s.addremove = false;

		o = s.option(form.Flag, 'enabled', _('Enable Service'),
			_('Enable or disable the Cloud Manager background daemon.'));
		o.default = '1';
		o.rmempty = false;

		o = s.option(form.Value, 'server_url', _('Controller Server URL'),
			_('WebSocket URL of the central dashboard controller (supports DDNS).'));
		o.default = 'ws://riderok.mooo.com:8080';
		o.placeholder = 'ws://riderok.mooo.com:8080';
		o.rmempty = false;

		o = s.option(form.Value, 'heartbeat_interval', _('Heartbeat Interval (seconds)'),
			_('Base interval between lightweight telemetry pings (random jitter is added automatically).'));
		o.datatype = 'uinteger';
		o.default = '30';

		o = s.option(form.Value, 'local_luci_addr', _('Local LuCI Address'),
			_('Internal HTTP address of uhttpd for on-demand web interface proxying.'));
		o.default = 'http://127.0.0.1:80';

		o = s.option(form.Value, 'local_ttyd_addr', _('Local Terminal Service Address'),
			_('Internal WebSocket address of the local terminal service (e.g. ttyd).'));
		o.default = 'ws://127.0.0.1:7681/ws';

		return m.render();
	}
});
