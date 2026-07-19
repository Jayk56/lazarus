#!/bin/sh
set -eu

# Compatibility shim for Ansible jobs.  The fixture is intentionally not
# running systemd; these commands map the familiar service operations to the
# idempotent state-file CLI.

quiet=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --quiet|--no-pager|--plain) quiet=true; shift ;;
        --) shift; break ;;
        *) break ;;
    esac
done

[ "$#" -gt 0 ] || {
    echo "usage: systemctl ACTION [SERVICE ...]" >&2
    exit 2
}

action=$1
shift

case "$action" in
    daemon-reload|reset-failed)
        [ "$#" -eq 0 ] || { echo "$action takes no service" >&2; exit 2; }
        exit 0
        ;;
    is-active|is-enabled|is-failed|status)
        [ "$#" -ge 1 ] || { echo "$action needs a service" >&2; exit 2; }
        failed=0
        for service_name in "$@"; do
            case "$service_name" in
                --quiet) quiet=true; continue ;;
            esac
            if state=$(/usr/local/bin/fixture-service get "$service_name" 2>/dev/null); then
                case "$action:$state" in
                    is-active:running|is-enabled:running)
                        [ "$quiet" = true ] || printf '%s\n' active
                        ;;
                    is-failed:stopped)
                        [ "$quiet" = true ] || printf '%s\n' inactive
                        failed=1
                        ;;
                    is-active:stopped|is-enabled:stopped|is-failed:running)
                        [ "$quiet" = true ] || printf '%s\n' inactive
                        failed=1
                        ;;
                    status:*)
                        [ "$quiet" = true ] || printf '%s %s\n' "$service_name" "$state"
                        [ "$state" = running ] || failed=3
                        ;;
                esac
            else
                [ "$quiet" = true ] || printf '%s\n' 'unknown'
                failed=4
            fi
        done
        exit "$failed"
        ;;
    start|stop|restart|try-restart|reload)
        [ "$#" -ge 1 ] || { echo "$action needs a service" >&2; exit 2; }
        failed=0
        for service_name in "$@"; do
            case "$service_name" in --quiet) continue ;; esac
            case "$action" in
                start|try-restart) requested_state=running ;;
                stop) requested_state=stopped ;;
                restart|reload) requested_state=running ;;
            esac
            if ! /usr/local/bin/fixture-service set "$service_name" "$requested_state"; then
                failed=1
            fi
        done
        exit "$failed"
        ;;
    enable|disable|mask|unmask)
        # Enablement is not meaningful for an ephemeral fixture.  Keep these
        # commands idempotent for generic Ansible service tasks.
        exit 0
        ;;
    *)
        echo "unsupported fixture systemctl action: $action" >&2
        exit 2
        ;;
esac
