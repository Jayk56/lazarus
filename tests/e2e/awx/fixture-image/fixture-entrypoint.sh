#!/bin/sh
set -eu

# Commands supplied to the image are useful for local inspection and tests.
# The normal container command is the SSH server setup below.
if [ "$#" -gt 0 ]; then
    exec "$@"
fi

profile=${FIXTURE_PROFILE:-}
case "$profile" in
    node|dmgr) ;;
    *)
        echo "FIXTURE_PROFILE must be node or dmgr" >&2
        exit 2
        ;;
esac

state_dir=${FIXTURE_STATE_DIR:-/var/lib/awx-fixture}
authorized_keys_file=${SSH_AUTHORIZED_KEYS_FILE:-/run/secrets/awx-fixture/authorized_keys}

# SSH sessions do not inherit arbitrary container environment variables. Save
# the non-secret profile so fixture-service and its service-manager shims can
# resolve the correct contract after login.
printf '%s\n' "$profile" >/run/awx-fixture-profile
chmod 0444 /run/awx-fixture-profile

if [ ! -s "$authorized_keys_file" ]; then
    echo "missing non-empty SSH authorized-keys Secret at $authorized_keys_file" >&2
    exit 1
fi

if ! id awx-fixture >/dev/null 2>&1; then
    echo "fixture user is missing from the image" >&2
    exit 1
fi

install -d -m 0700 -o awx-fixture -g awx-fixture /home/awx-fixture/.ssh
install -m 0600 -o awx-fixture -g awx-fixture "$authorized_keys_file" \
    /home/awx-fixture/.ssh/authorized_keys
install -d -m 0755 "$state_dir"

if [ ! -f "$state_dir/services.state" ]; then
    FIXTURE_PROFILE="$profile" FIXTURE_STATE_DIR="$state_dir" \
        /usr/local/bin/fixture-service seed >/dev/null
fi
chown -R awx-fixture:awx-fixture "$state_dir"

# Host keys are intentionally generated in the pod's ephemeral /run volume.
# They are not fixture credentials and do not need to be committed or shared
# between pods.
if [ ! -f /run/sshd/ssh_host_ed25519_key ]; then
    ssh-keygen -q -t ed25519 -N '' -f /run/sshd/ssh_host_ed25519_key
fi
if [ ! -f /run/sshd/ssh_host_rsa_key ]; then
    ssh-keygen -q -t rsa -b 3072 -N '' -f /run/sshd/ssh_host_rsa_key
fi
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
