#!/bin/bash
set -e

setup=/etc/wormtamer/setup.sh
if [[ -e "$setup" ]]; then
    echo "Running $setup (10-minute limit)." >&2
    timeout --signal=KILL 10m /bin/bash -e -o pipefail "$setup" &
    setup_pid=$!
    trap 'kill -KILL -- "-$setup_pid" 2>/dev/null || true; wait "$setup_pid" 2>/dev/null || true' EXIT
    trap 'exit 143' TERM
    trap 'exit 130' INT
    if wait "$setup_pid"; then
        trap - EXIT TERM INT
    else
        status=$?
        echo "$setup failed or timed out (exit $status); Wormtamer will not start." >&2
        exit "$status"
    fi
fi

exec /usr/local/bin/wormtamer "$@"
