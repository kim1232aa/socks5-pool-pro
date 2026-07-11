#!/bin/sh
set -eu

# Named volumes are created as root, and existing installations may contain
# root-owned state from older images. Repair only the application's data path,
# then run the service itself without root privileges.
if [ "$(id -u)" = "0" ]; then
	mkdir -p /app/data
	chown -R socks5:socks5 /app/data
	chmod -R u=rwX,go= /app/data
	exec su-exec socks5:socks5 "$@"
fi

exec "$@"
