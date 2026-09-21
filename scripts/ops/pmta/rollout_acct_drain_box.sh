set -uo pipefail
# ---- runs as root on the PMTA box; PAYLOAD_B64 is prepended by make_bundle.sh
TS=$(date +%Y%m%d-%H%M%S); W=/root/acct-v6-$TS; mkdir -p "$W"; cd "$W"
echo "$PAYLOAD_B64" | base64 -d | tar -xzf -
echo "== host $(hostname) $(date -u +%FT%TZ) workdir $W =="

echo "== BEFORE =="
sha256sum /usr/local/bin/pmta-acct-forward /usr/local/bin/pmta-acct-drainer
systemctl cat pmta-acct-drainer.service --no-pager | grep -E '^(ExecStart|Type|User|TimeoutStartSec)=' 
systemctl cat pmta-acct-drainer.timer --no-pager | grep -E '^OnUnit'
SPOOL_BEFORE=$(find /var/spool/pmta-forward -maxdepth 1 -name '*.json' | wc -l); echo "spool files before: $SPOOL_BEFORE"
OLD_FWD_PIDS=$(pgrep -f '/usr/local/bin/pmta-acct-forward' | tr '\n' ' '); echo "running forwarder pids: ${OLD_FWD_PIDS:-none}"
python3 --version

echo "== PRE-FLIGHT: compile under the box's python =="
python3 -m py_compile pmta-acct-forward pmta-acct-drainer || { echo "COMPILE FAILED — aborting, nothing changed"; exit 2; }

echo "== BACKUP =="
[ -e /usr/local/bin/pmta-acct-forward.v5.bak ] || cp -p /usr/local/bin/pmta-acct-forward /usr/local/bin/pmta-acct-forward.v5.bak
[ -e /usr/local/bin/pmta-acct-drainer.v5.bak ] || cp -p /usr/local/bin/pmta-acct-drainer /usr/local/bin/pmta-acct-drainer.v5.bak
cp -p /etc/systemd/system/pmta-acct-drainer.service "$W/pmta-acct-drainer.service.before"
cp -p /etc/systemd/system/pmta-acct-drainer.timer   "$W/pmta-acct-drainer.timer.before"
ls -l /usr/local/bin/pmta-acct-*.v5.bak

echo "== INSTALL =="
install -m755 pmta-acct-drainer /usr/local/bin/pmta-acct-drainer
install -m755 pmta-acct-forward /usr/local/bin/pmta-acct-forward
mkdir -p /etc/systemd/system/pmta-acct-drainer.service.d
install -m644 pmta-acct-drainer.service.d-override.conf /etc/systemd/system/pmta-acct-drainer.service.d/override.conf
systemctl daemon-reload
systemctl restart pmta-acct-drainer.timer
sha256sum /usr/local/bin/pmta-acct-forward /usr/local/bin/pmta-acct-drainer
systemctl cat pmta-acct-drainer.service --no-pager | grep -E '^(ExecStart|TimeoutStartSec|Environment)='
systemctl list-timers pmta-acct-drainer.timer --no-pager

echo "== FORWARDER SWITCH (v5 process keeps the pipe until PMTA reopens it) =="
# Clean path: ask PMTA to reopen its accounting files. v5 sees EOF, flushes, exits 0;
# PMTA re-spawns the pipe command => v6 starts. UNVERIFIED that `rotate` applies to
# pipe acct-files on 5.0r7 — the pid check below is the truth. If it did not switch,
# leave it: v2 drainer merges v5's 5-record files too, so throughput is fixed either way,
# and v6 activates on the next PMTA restart. Do NOT kill -9 the v5 process (loses <=64 KB
# of pipe buffer + <=4 buffered records).
pmta rotate --acct 2>&1 || pmta rotate 2>&1 || true
sleep 3
NEW_FWD_PIDS=$(pgrep -f '/usr/local/bin/pmta-acct-forward' | tr '\n' ' ')
echo "forwarder pids before: ${OLD_FWD_PIDS:-none} | after: ${NEW_FWD_PIDS:-none}"
if [ "${OLD_FWD_PIDS:-}" != "${NEW_FWD_PIDS:-}" ]; then echo "FORWARDER: switched to v6"; else echo "FORWARDER: v5 still attached (ok; v6 on next PMTA reopen)"; fi
tail -n 3 /var/log/pmta/acct-forward.log

echo "== FIRST DRAINER RUN (waiting for the timer, up to 75 s) =="
for i in $(seq 1 15); do sleep 5; grep -q 'run complete' /var/log/pmta/acct-drainer.log 2>/dev/null && break; done
tail -n 5 /var/log/pmta/acct-drainer.log 2>/dev/null || echo "no acct-drainer.log yet"
systemctl status pmta-acct-drainer.service --no-pager 2>&1 | sed -n 1,6p
SPOOL_AFTER=$(find /var/spool/pmta-forward -maxdepth 1 -name '*.json' | wc -l)
echo "spool files before: $SPOOL_BEFORE after: $SPOOL_AFTER delta: $((SPOOL_BEFORE - SPOOL_AFTER))"
echo "VERIFY LINE: $(hostname) drainer=$(sha256sum /usr/local/bin/pmta-acct-drainer | cut -c1-12) forwarder=$(sha256sum /usr/local/bin/pmta-acct-forward | cut -c1-12) spool $SPOOL_BEFORE->$SPOOL_AFTER $(tail -n 1 /var/log/pmta/acct-drainer.log 2>/dev/null | sed 's/.*run complete: //')"
echo "== DONE $(date -u +%FT%TZ) =="
