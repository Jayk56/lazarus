#!/bin/sh
set -eu

# A tiny, deterministic service-state store used by the AWX fixture.  The
# state file is intentionally plain text so it can be inspected without jq or
# a service manager.  Each line is: <service> <running|stopped>.

state_dir=${FIXTURE_STATE_DIR:-/var/lib/awx-fixture}
state_file=$state_dir/services.state
lock_file=$state_dir/.lock
profile=${FIXTURE_PROFILE:-}
if [ -z "$profile" ] && [ -r /run/awx-fixture-profile ]; then
    IFS= read -r profile </run/awx-fixture-profile
fi

case "$profile" in
    node|dmgr) ;;
    *)
        echo "FIXTURE_PROFILE must be node or dmgr" >&2
        exit 2
        ;;
esac

mkdir -p "$state_dir"

normalize_service() {
    service_name=$1
    case "$service_name" in
        *.service) service_name=${service_name%.service} ;;
    esac
    # Permit the common WebSphere spelling (web-01) while keeping the
    # canonical fixture names compact (web01).
    service_name=$(printf '%s' "$service_name" | tr -d '-')
    printf '%s\n' "$service_name"
}

seed_profile() {
    case "$profile" in
        node)
            cat <<'EOF'
web01 running
web02 running
web03 running
web04 stopped
web05 stopped
service01 running
service02 running
service03 running
nodeagent running
EOF
            ;;
        dmgr)
            printf '%s\n' 'dmgr running'
            ;;
    esac
}

with_lock() {
    # Debian's util-linux provides flock.  Keeping the lock inode around is
    # harmless: flock releases it automatically when this process exits and
    # avoids a cleanup command that could accidentally remove user data.
    if command -v flock >/dev/null 2>&1; then
        exec 9>"$lock_file"
        flock -x 9
    fi
    "$@"
}

seed_locked() {
    tmp_file=$(mktemp "$state_dir/.services.state.XXXXXX")
    seed_profile >"$tmp_file"
    chmod 0644 "$tmp_file"
    changed=true
    if [ -f "$state_file" ] && cmp -s "$state_file" "$tmp_file"; then
        changed=false
    fi
    mv "$tmp_file" "$state_file"
    printf '{"changed":%s}\n' "$changed"
}

require_state_file() {
    if [ ! -f "$state_file" ]; then
        echo "state is not seeded; run fixture-service seed" >&2
        exit 1
    fi
}

get_state() {
    service_name=$1
    awk -v service_name="$service_name" '
        $1 == service_name { print $2; found = 1; exit }
        END { if (!found) exit 1 }
    ' "$state_file"
}

set_locked() {
    service_name=$1
    requested_state=$2
    require_state_file
    if ! current_state=$(get_state "$service_name"); then
        echo "unknown service for profile $profile: $service_name" >&2
        exit 1
    fi
    if [ "$current_state" = "$requested_state" ]; then
        printf '{"changed":false}\n'
        return
    fi
    tmp_file=$(mktemp "$state_dir/.services.state.XXXXXX")
    awk -v service_name="$service_name" -v requested_state="$requested_state" '
        $1 == service_name { print $1, requested_state; next }
        { print }
    ' "$state_file" >"$tmp_file"
    chmod 0644 "$tmp_file"
    mv "$tmp_file" "$state_file"
    printf '{"changed":true}\n'
}

list_text() {
    require_state_file
    cat "$state_file"
}

list_json() {
    require_state_file
    first=true
    printf '{'
    while read -r service_name service_state; do
        [ -n "$service_name" ] || continue
        if [ "$first" = true ]; then
            first=false
        else
            printf ','
        fi
        printf '"%s":"%s"' "$service_name" "$service_state"
    done <"$state_file"
    printf '}\n'
}

usage() {
    cat >&2 <<'EOF'
usage: fixture-service seed | list [--json] | get SERVICE | set SERVICE running|stopped
EOF
    exit 2
}

[ "$#" -gt 0 ] || usage
command=$1
shift

case "$command" in
    seed)
        [ "$#" -eq 0 ] || usage
        with_lock seed_locked
        ;;
    list)
        case "${1:-}" in
            '') list_text ;;
            --json)
                [ "$#" -eq 1 ] || usage
                list_json
                ;;
            *) usage ;;
        esac
        ;;
    get)
        [ "$#" -eq 1 ] || usage
        require_state_file
        service_name=$(normalize_service "$1")
        get_state "$service_name"
        ;;
    set)
        [ "$#" -eq 2 ] || usage
        service_name=$(normalize_service "$1")
        requested_state=$2
        case "$requested_state" in
            running|stopped) ;;
            *) echo "state must be running or stopped" >&2; exit 2 ;;
        esac
        with_lock set_locked "$service_name" "$requested_state"
        ;;
    *) usage ;;
esac
