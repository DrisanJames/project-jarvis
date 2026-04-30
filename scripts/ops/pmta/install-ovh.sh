#!/bin/sh
# Run on each PMTA OVH host (as root or with sudo) after copying this directory to e.g. /root/pmta-ops
set -e
DIR=$(cd "$(dirname "$0")" && pwd)
install -m755 "$DIR/pmta-acct-drainer" /usr/local/bin/pmta-acct-drainer
install -m755 "$DIR/pmta-acct-forward" /usr/local/bin/pmta-acct-forward
cp "$DIR/pmta-acct-drainer.timer" /etc/systemd/system/pmta-acct-drainer.timer
systemctl daemon-reload
systemctl restart pmta-acct-drainer.timer
systemctl list-timers pmta-acct-drainer.timer --no-pager
sha256sum /usr/local/bin/pmta-acct-drainer /usr/local/bin/pmta-acct-forward
