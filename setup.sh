#!/bin/sh
# OpenWrt Cloud Manager & LuCI App Deployment Script
# Compatible with BusyBox ash (/bin/sh), opkg, and apk

set -e

DEFAULT_SERVER="ws://riderok.mooo.com:8080"
GITHUB_REPO="${GITHUB_REPO:-RiderFyCfy/oprwrtcld}"
RELEASE_TAG="${RELEASE_TAG:-latest}"
SERVER_URL="$DEFAULT_SERVER"

while [ $# -gt 0 ]; do
	case "$1" in
		--server)
			shift
			[ -n "$1" ] && SERVER_URL="$1"
			;;
		--server=*)
			SERVER_URL="${1#*=}"
			;;
		--repo)
			shift
			[ -n "$1" ] && GITHUB_REPO="$1"
			;;
	esac
	shift
done

echo "[*] Deploying OpenWrt Cloud Manager (Controller: $SERVER_URL)"

# 1. Detect OpenWrt architecture
ARCH=""
if [ -f /etc/openwrt_release ]; then
	. /etc/openwrt_release
	ARCH="$DISTRIB_ARCH"
fi
if [ -z "$ARCH" ]; then
	ARCH="$(uname -m)"
fi

BIN_SUFFIX=""
case "$ARCH" in
	aarch64*|arm64*)
		BIN_SUFFIX="linux-arm64"
		;;
	mipsel*|mipsle*)
		BIN_SUFFIX="linux-mipsle-softfloat"
		;;
	mips_24kc*|mips-*|mips64*)
		BIN_SUFFIX="linux-mips-softfloat"
		;;
	arm_cortex*|armv7*|arm*)
		BIN_SUFFIX="linux-armv7"
		;;
	x86_64|amd64)
		BIN_SUFFIX="linux-amd64"
		;;
	*)
		echo "[!] Unsupported architecture: $ARCH"
		exit 1
		;;
esac

echo "[*] Detected architecture: $ARCH -> binary target: $BIN_SUFFIX"

# 2. Ensure optional web console package (ttyd) and CA certificates are available via opkg or apk
if command -v opkg >/dev/null 2>&1; then
	echo "[*] Package manager: opkg"
	if ! command -v ttyd >/dev/null 2>&1; then
		opkg update >/dev/null 2>&1 || true
		opkg install ttyd >/dev/null 2>&1 || true
	fi
elif command -v apk >/dev/null 2>&1; then
	echo "[*] Package manager: apk"
	if ! command -v ttyd >/dev/null 2>&1; then
		apk update >/dev/null 2>&1 || true
		apk add ttyd >/dev/null 2>&1 || true
	fi
fi

# 3. Download binary to /tmp (RAM)
TMP_BIN="/tmp/cloud_manager.$$"
if [ "$RELEASE_TAG" = "latest" ]; then
	DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/cloud_manager-${BIN_SUFFIX}"
else
	DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/${RELEASE_TAG}/cloud_manager-${BIN_SUFFIX}"
fi

echo "[*] Downloading $DOWNLOAD_URL to $TMP_BIN ..."
wget -qO "$TMP_BIN" "$DOWNLOAD_URL"
chmod +x "$TMP_BIN"
mv "$TMP_BIN" /usr/bin/cloud_manager
rm -f "$TMP_BIN"

# 4. Write UCI configuration (/etc/config/cloud_manager)
if [ ! -f /etc/config/cloud_manager ]; then
	cat <<EOF > /etc/config/cloud_manager
config cloud_manager 'main'
	option enabled '1'
	option server_url '$SERVER_URL'
	option heartbeat_interval '30'
	option local_luci_addr 'http://127.0.0.1:80'
	option local_ttyd_addr 'ws://127.0.0.1:7681/ws'
EOF
else
	uci set cloud_manager.main=cloud_manager
	uci set cloud_manager.main.enabled='1'
	uci set cloud_manager.main.server_url="$SERVER_URL"
	uci commit cloud_manager
fi

# 5. Create procd init script (/etc/init.d/cloud_manager)
cat <<'EOF' > /etc/init.d/cloud_manager
#!/bin/sh /etc/rc.common

START=99
STOP=10
USE_PROCD=1

PROG=/usr/bin/cloud_manager

start_service() {
	config_load 'cloud_manager'
	local enabled
	config_get_bool enabled main 'enabled' 1
	[ "$enabled" -eq 0 ] && return 0

	procd_open_instance
	procd_set_param command "$PROG"
	procd_set_param respawn "${respawn_threshold:-3600}" "${respawn_timeout:-5}" "${respawn_retry:-5}"
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_set_param file /etc/config/cloud_manager
	procd_close_instance
}

service_triggers() {
	procd_add_reload_trigger "cloud_manager"
}
EOF
chmod +x /etc/init.d/cloud_manager

# 6. Install LuCI Client-Side JS View & ACLs (luci-app-cloud-manager)
mkdir -p /www/luci-static/resources/view/cloud_manager
mkdir -p /usr/share/luci/menu.d
mkdir -p /usr/share/rpcd/acl.d

cat <<'EOF' > /www/luci-static/resources/view/cloud_manager/config.js
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
EOF

cat <<'EOF' > /usr/share/luci/menu.d/luci-app-cloud-manager.json
{
	"admin/services/cloud_manager": {
		"title": "Cloud Manager",
		"order": 60,
		"action": {
			"type": "view",
			"path": "cloud_manager/config"
		},
		"depends": {
			"acl": [ "luci-app-cloud-manager" ],
			"uci": { "cloud_manager": true }
		}
	}
}
EOF

cat <<'EOF' > /usr/share/rpcd/acl.d/luci-app-cloud-manager.json
{
	"luci-app-cloud-manager": {
		"description": "Grant UCI access for luci-app-cloud-manager",
		"read": {
			"uci": [ "cloud_manager" ]
		},
		"write": {
			"uci": [ "cloud_manager" ]
		}
	}
}
EOF

rm -rf /tmp/luci-indexcache* /tmp/luci-modulecache* 2>/dev/null || true
/etc/init.d/rpcd reload 2>/dev/null || true

# 7. Enable and start service
/etc/init.d/cloud_manager enable
/etc/init.d/cloud_manager restart

echo "[+] OpenWrt Cloud Manager installed and running!"
