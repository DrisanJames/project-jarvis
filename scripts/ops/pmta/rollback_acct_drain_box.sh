#!/usr/bin/env bash
# ROLLBACK to v5 forwarder + v1 drainer — ONE ssh session per box: ssh ... 'sudo bash -s' < rollback_bundle.sh
set -uo pipefail
echo "== ROLLBACK on $(hostname) $(date -u +%FT%TZ) =="
ls -l /usr/local/bin/pmta-acct-forward.v5.bak /usr/local/bin/pmta-acct-drainer.v5.bak || { echo "no .v5.bak — aborting"; exit 2; }
cp -p /usr/local/bin/pmta-acct-forward.v5.bak /usr/local/bin/pmta-acct-forward
cp -p /usr/local/bin/pmta-acct-drainer.v5.bak /usr/local/bin/pmta-acct-drainer
rm -f /etc/systemd/system/pmta-acct-drainer.service.d/override.conf
rmdir /etc/systemd/system/pmta-acct-drainer.service.d 2>/dev/null || true
systemctl daemon-reload
systemctl restart pmta-acct-drainer.timer
pmta rotate --acct 2>&1 || pmta rotate 2>&1 || true
sha256sum /usr/local/bin/pmta-acct-forward /usr/local/bin/pmta-acct-drainer
systemctl cat pmta-acct-drainer.service --no-pager | grep -E '^(ExecStart|TimeoutStartSec)='
systemctl list-timers pmta-acct-drainer.timer --no-pager
sleep 40; tail -n 2 /var/log/pmta/acct-forward.log
echo "== ROLLBACK DONE =="
