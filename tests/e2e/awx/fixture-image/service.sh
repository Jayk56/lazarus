#!/bin/sh
set -eu

# SysV-style compatibility entrypoint used by Ansible when it selects the
# generic service module instead of systemd.
[ "$#" -ge 2 ] || {
    echo "usage: service SERVICE start|stop|restart|status" >&2
    exit 2
}

service_name=$1
action=$2
shift 2
exec /usr/local/bin/systemctl "$action" "$service_name" "$@"
