"""Tests for pmta-acct-forward v6 and pmta-acct-drainer v2.

Run:  pytest -q test_acct_drain.py   (from this directory)
Only stdlib + pytest. Everything runs against temp dirs and a local fake
webhook; nothing touches /var/spool, /var/log or projectjarvis.io.
"""
import importlib.machinery
import importlib.util
import io
import json
import os
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))


def _load(name, filename):
    loader = importlib.machinery.SourceFileLoader(name, os.path.join(HERE, filename))
    spec = importlib.util.spec_from_loader(name, loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


@pytest.fixture
def fwd(tmp_path):
    m = _load('pmta_acct_forward', 'pmta-acct-forward')
    m.LOG_FILE = str(tmp_path / 'acct-forward.log')
    return m


@pytest.fixture
def drn(tmp_path):
    m = _load('pmta_acct_drainer', 'pmta-acct-drainer')
    m.LOG_FILE = str(tmp_path / 'acct-drainer.log')
    return m


HEADER = ('type,timeLogged,timeQueued,orig,rcpt,orcpt,dsnAction,dsnStatus,dsnDiag,dsnMta,'
          'bounceCat,srcType,srcMta,dlvType,dlvSourceIp,dlvDestinationIp,dlvEsmtpAvailable,'
          'dlvSize,vmta,jobId,envId,queue,vmtaPool\n')


def _csv_line(i, rtype='d'):
    return (f'{rtype},2026-09-21 12:00:{i % 60:02d}+0000,2026-09-21 11:59:00+0000,'
            f'b@em.discountblog.com,u{i}@gmail.com,,relayed,2.0.0,"250 ok",mx.gmail.com,,'
            f'smtp,,smtp,15.204.101.125,142.250.1.1,1,1234,db-gmail-pool,'
            f'job{i},,gmail.com/db-gmail-pool,db-gmail-pool\n')


def _spool_files(spool_dir):
    return sorted(n for n in os.listdir(spool_dir) if n.endswith('.json'))


# ---------------------------------------------------------------- forwarder

def test_forwarder_batches_1003_rows_into_4x250_plus_3(fwd, tmp_path):
    spool = tmp_path / 'spool'
    stream = io.StringIO(HEADER + ''.join(_csv_line(i) for i in range(1003)))
    files, rows, bounces = fwd.run(stream, spool_dir=str(spool), batch_size=250, flush_secs=5.0)
    assert (files, rows, bounces) == (5, 1003, 0)
    names = _spool_files(spool)
    assert len(names) == 5
    sizes = [len(json.load(open(spool / n))) for n in names]
    assert sizes == [250, 250, 250, 250, 3]
    assert not [n for n in os.listdir(spool) if n.endswith('.tmp')]
    # v5 file-name contract: <epoch_ms>-<pid>-<hex8>.json
    assert all(re.fullmatch(r'\d{13}-\d+-[0-9a-f]{8}\.json', n) for n in names)
    # v5 FIELD_MAP contract on a record
    rec = json.load(open(spool / names[0]))[0]
    assert rec['recipient'] == 'u0@gmail.com'
    assert rec['sender'] == 'b@em.discountblog.com'
    assert rec['domain'] == 'gmail.com'          # queue split on '/'
    assert rec['pool'] == 'db-gmail-pool'
    assert rec['source_ip'] == '15.204.101.125'
    assert rec['job_id'] == 'job0'
    assert rec['time_logged'].startswith('2026-09-21 12:00:00')
    assert set(rec) <= set(fwd.FIELD_MAP.values())


def test_forwarder_time_bound_flush_while_pipe_stays_open(fwd, tmp_path):
    spool = tmp_path / 'spool'
    spool.mkdir()
    rfd, wfd = os.pipe()
    reader = os.fdopen(rfd, 'r')
    result = {}

    def run():
        result['r'] = fwd.run(reader, spool_dir=str(spool), batch_size=250, flush_secs=0.5)

    t = threading.Thread(target=run, daemon=True)
    t.start()
    os.write(wfd, (HEADER + ''.join(_csv_line(i) for i in range(10))).encode())
    # 10 rows < 250: only the time bound can flush them. Pipe is still open.
    deadline = time.time() + 3.0
    while time.time() < deadline and len(_spool_files(spool)) < 1:
        time.sleep(0.05)
    names = _spool_files(spool)
    assert len(names) == 1, 'partial batch was not flushed on the time bound'
    assert len(json.load(open(spool / names[0]))) == 10
    assert t.is_alive(), 'forwarder must keep reading while PMTA keeps the pipe open'
    # A second trickle flushes again after the bound, not before.
    os.write(wfd, _csv_line(10, 'b').encode())
    time.sleep(0.2)
    assert len(_spool_files(spool)) == 1
    time.sleep(0.6)
    assert len(_spool_files(spool)) == 2
    os.close(wfd)
    t.join(timeout=3)
    assert not t.is_alive()
    assert result['r'] == (2, 11, 1)
    log = open(fwd.LOG_FILE).read()
    assert 'BOUNCE: u10@gmail.com' in log
    assert '--- done: 11 rows (1 bounces) in 2 files ---' in log


def test_forwarder_stop_event_flushes_partial_batch(fwd, tmp_path):
    spool = tmp_path / 'spool'
    spool.mkdir()
    rfd, wfd = os.pipe()
    reader = os.fdopen(rfd, 'r')
    stop = threading.Event()
    t = threading.Thread(target=fwd.run, args=(reader,),
                         kwargs=dict(spool_dir=str(spool), batch_size=250, flush_secs=0.2,
                                     stop_event=stop), daemon=True)
    t.start()
    os.write(wfd, (HEADER + _csv_line(1)).encode())
    time.sleep(0.05)
    stop.set()  # SIGTERM path
    t.join(timeout=2)
    assert not t.is_alive()
    assert len(_spool_files(spool)) == 1


# ------------------------------------------------------------------ drainer

class FakeWebhook:
    """Local /engine/webhook. Records every body; 503s once on the body that
    contains file FAIL_503; sleeps past the client timeout on the body that
    contains file FAIL_TIMEOUT."""

    def __init__(self, fail_503_idx, fail_timeout_idx, timeout_sleep):
        self.bodies = []           # list of lists of file indexes, arrival order
        self.lock = threading.Lock()
        self.fail_503_idx = fail_503_idx
        self.fail_timeout_idx = fail_timeout_idx
        self.timeout_sleep = timeout_sleep
        self.did_503 = False
        self.kept_503 = None
        self.kept_timeout = None
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                n = int(self.headers.get('Content-Length', 0))
                recs = json.loads(self.rfile.read(n))
                assert self.headers.get('Content-Type') == 'application/json'
                idxs = sorted({int(r['recipient'].split('-')[0][1:]) for r in recs})
                with outer.lock:
                    outer.bodies.append((idxs, len(recs)))
                    do_503 = outer.fail_503_idx in idxs and not outer.did_503
                    if do_503:
                        outer.did_503 = True
                        outer.kept_503 = idxs
                    do_timeout = outer.fail_timeout_idx in idxs and outer.kept_timeout is None
                    if do_timeout:
                        outer.kept_timeout = idxs
                if do_503:
                    self.send_response(503)
                    self.end_headers()
                    self.wfile.write(b'accounting queue saturated')
                    return
                if do_timeout:
                    time.sleep(outer.timeout_sleep)
                body = json.dumps({'received': len(recs), 'queued': len(recs)}).encode()
                try:
                    self.send_response(200)
                    self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), H)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f'http://127.0.0.1:{self.server.server_address[1]}/engine/webhook'

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def _make_spool(spool_dir, n_files, recs_per_file=3, base_ms=1_758_470_000_000):
    os.makedirs(spool_dir, exist_ok=True)
    names = []
    for i in range(n_files):
        name = f'{base_ms + i * 7}-{4242}-{i:08x}.json'
        recs = [{'type': 'd', 'recipient': f'u{i}-{j}@gmail.com', 'job_id': f'j{i}',
                 'time_logged': '2026-09-21 12:00:00+0000', 'vmta': 'db-gmail-pool',
                 'pool': 'db-gmail-pool', 'domain': 'gmail.com', 'dsn_status': '2.0.0'}
                for j in range(recs_per_file)]
        with open(os.path.join(spool_dir, name), 'w') as f:
            json.dump(recs, f, separators=(',', ':'))
        names.append(name)
    # noise the drainer must ignore
    open(os.path.join(spool_dir, 'partial.json.tmp'), 'w').write('[')
    open(os.path.join(spool_dir, 'old.json.corrupt'), 'w').write('x')
    return names


def test_select_oldest_is_bounded_and_oldest_first(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    names = _make_spool(spool, 3000)
    # shuffle-free check: directory order is arbitrary; the result must be the
    # 100 smallest epochs regardless.
    got, total, oldest_ms = drn.select_oldest(spool, 100)
    assert total == 3000
    assert got == names[:100]
    assert oldest_ms == 1_758_470_000_000
    got_all, total, _ = drn.select_oldest(spool, 5000)
    assert got_all == names and total == 3000


def test_build_bodies_merges_to_cap_without_splitting_files(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    names = _make_spool(spool, 10, recs_per_file=3)
    # one oversize file (>cap) must travel alone
    big = os.path.join(spool, '1758470999999-1-deadbeef.json')
    json.dump([{'recipient': f'u999-{j}@x'} for j in range(12)], open(big, 'w'))
    paths = [os.path.join(spool, n) for n in names] + [big]
    bodies = list(drn.build_bodies(paths, merge_records=7))
    sizes = [len(r) for _, r in bodies]
    assert sizes == [6, 6, 6, 6, 6, 12]          # 2 files of 3 per body, big one alone
    assert [len(f) for f, _ in bodies] == [2, 2, 2, 2, 2, 1]
    assert all(s <= 7 or len(f) == 1 for f, s in zip([f for f, _ in bodies], sizes))


def test_drainer_3000_files_merge_ack_only_200_and_summary(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    names = _make_spool(spool, 3000, recs_per_file=3)
    hook = FakeWebhook(fail_503_idx=700, fail_timeout_idx=1400, timeout_sleep=2.5)
    try:
        t0 = time.time()
        summary = drn.run_once(spool_dir=spool, url=hook.url, max_files=3000, merge_records=500,
                               workers=4, timeout=1.0, backoff_503=0.3, max_run_seconds=60,
                               lock_file=str(tmp_path / 'lock'))
        elapsed = time.time() - t0
    finally:
        hook.close()

    # merge sizes: every body <= 500 records; 3 recs/file -> 166 files = 498 recs
    assert hook.bodies, 'nothing was posted'
    assert max(n for _, n in hook.bodies) <= 500
    assert max(n for _, n in hook.bodies) == 498
    n_bodies = -(-3000 // 166)  # 19
    assert len(hook.bodies) == n_bodies

    # only files in 200-acked bodies were unlinked; the 503 body and the
    # timeout body stay, byte-identical, for the next run
    kept = set(hook.kept_503) | set(hook.kept_timeout)
    assert hook.kept_503 and hook.kept_timeout and not (set(hook.kept_503) & set(hook.kept_timeout))
    remaining = _spool_files(spool)
    assert len(remaining) == len(kept) == 332
    assert {int(n.split('-')[2].split('.')[0], 16) for n in remaining} == kept
    for n in remaining:
        assert len(json.load(open(os.path.join(spool, n)))) == 3
    # noise untouched
    assert os.path.exists(os.path.join(spool, 'partial.json.tmp'))
    assert os.path.exists(os.path.join(spool, 'old.json.corrupt'))

    # oldest-first: each body's files are contiguous and bodies were BUILT in
    # ascending order (arrival order may interleave across 4 workers, so check
    # the index sets partition [0,3000) into contiguous ascending runs)
    runs = sorted(hook.bodies, key=lambda b: b[0][0])
    flat = [i for idxs, _ in runs for i in idxs]
    assert flat == list(range(3000))
    for idxs, _ in runs:
        assert idxs == list(range(idxs[0], idxs[0] + len(idxs)))

    # summary dict + the single summary line
    assert summary['files'] == 3000 - 332
    assert summary['records'] == (3000 - 332) * 3
    assert summary['posts'] == n_bodies
    assert summary['ok'] == n_bodies - 2
    assert summary['failures'] == 2
    assert summary['s503'] == 1
    assert summary['timeouts'] == 1
    assert summary['files_kept'] == 332
    assert summary['remaining'] == 332
    assert summary['oldest_age_s'] >= 0
    assert 0 < summary['seconds'] <= elapsed + 0.01
    log = open(drn.LOG_FILE).read().splitlines()
    summ = [l for l in log if 'run complete:' in l]
    assert len(summ) == 1
    assert re.search(r'run complete: files=2668 records=8004 posts=19 ok=17 failures=2 s503=1 '
                     r'timeouts=1 files_kept=332 records_kept=996 remaining=332 oldest_age_s=\d+ '
                     r'seconds=[\d.]+$', summ[0]), summ[0]
    assert any('HTTP 503' in l and 'backing off 0.3s' in l for l in log)
    assert any('TIMEOUT after 1.0s' in l for l in log)
    # the 503 backoff actually paused the run
    assert elapsed >= 0.3


def test_drainer_second_run_ships_the_kept_files(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    _make_spool(spool, 40, recs_per_file=3)
    hook = FakeWebhook(fail_503_idx=5, fail_timeout_idx=-1, timeout_sleep=0)
    try:
        s1 = drn.run_once(spool_dir=spool, url=hook.url, max_files=100, merge_records=30,
                          workers=2, timeout=1.0, backoff_503=0.0, max_run_seconds=30,
                          lock_file=str(tmp_path / 'lock'))
        assert s1['s503'] == 1 and s1['remaining'] == 10
        s2 = drn.run_once(spool_dir=spool, url=hook.url, max_files=100, merge_records=30,
                          workers=2, timeout=1.0, backoff_503=0.0, max_run_seconds=30,
                          lock_file=str(tmp_path / 'lock'))
        assert s2['files'] == 10 and s2['failures'] == 0 and s2['remaining'] == 0
    finally:
        hook.close()
    assert _spool_files(spool) == []


def test_drainer_oldest_first_strictly_with_one_worker(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    _make_spool(spool, 300, recs_per_file=3)
    hook = FakeWebhook(fail_503_idx=-1, fail_timeout_idx=-1, timeout_sleep=0)
    try:
        s = drn.run_once(spool_dir=spool, url=hook.url, max_files=120, merge_records=60,
                         workers=1, timeout=1.0, backoff_503=0.0, max_run_seconds=30,
                         lock_file=str(tmp_path / 'lock'))
    finally:
        hook.close()
    firsts = [idxs[0] for idxs, _ in hook.bodies]
    assert firsts == sorted(firsts)
    assert [i for idxs, _ in hook.bodies for i in idxs] == list(range(120))  # the 120 OLDEST
    assert s['files'] == 120 and s['remaining'] == 180
    assert _spool_files(spool)[0].startswith(str(1_758_470_000_000 + 120 * 7))


def test_drainer_corrupt_file_is_quarantined_not_retried_forever(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    _make_spool(spool, 4, recs_per_file=2)
    bad = os.path.join(spool, '1758470000001-1-00000bad.json')
    open(bad, 'w').write('{not json')
    hook = FakeWebhook(fail_503_idx=-1, fail_timeout_idx=-1, timeout_sleep=0)
    try:
        s = drn.run_once(spool_dir=spool, url=hook.url, max_files=100, merge_records=500,
                         workers=2, timeout=1.0, lock_file=str(tmp_path / 'lock'))
    finally:
        hook.close()
    assert s['files'] == 4 and s['failures'] == 0
    assert os.path.exists(bad + '.corrupt') and not os.path.exists(bad)


def test_overlap_guard_second_run_exits_without_touching_spool(drn, tmp_path):
    spool = str(tmp_path / 'spool')
    _make_spool(spool, 5)
    lock_file = str(tmp_path / 'lock')
    fd = drn.take_lock(lock_file)          # "another run" holds the lock
    assert fd is not None and fd >= 0
    try:
        s = drn.run_once(spool_dir=spool, url='http://127.0.0.1:9/never', max_files=10,
                         lock_file=lock_file)
    finally:
        os.close(fd)
    assert s == {'overlap': True}
    assert len(_spool_files(spool)) == 5
    assert 'overlap: another drainer run holds the lock; exiting 0' in open(drn.LOG_FILE).read()
    # lock released -> a run proceeds (server unreachable => everything kept, exit clean)
    s = drn.run_once(spool_dir=spool, url='http://127.0.0.1:9/never', max_files=10,
                     workers=1, timeout=0.5, lock_file=lock_file)
    assert s['failures'] >= 1 and s['files'] == 0 and len(_spool_files(spool)) == 5


def test_overlap_guard_across_processes(drn, tmp_path):
    """flock is per open-file-description; prove a real second PROCESS is refused."""
    import subprocess, sys
    spool = str(tmp_path / 'spool')
    _make_spool(spool, 5)
    lock_file = str(tmp_path / 'lock')
    fd = drn.take_lock(lock_file)
    try:
        env = dict(os.environ, DRAIN_SPOOL_DIR=spool, DRAIN_LOCK_FILE=lock_file,
                   DRAIN_LOG_FILE=drn.LOG_FILE, DRAIN_WEBHOOK_URL='http://127.0.0.1:9/never',
                   DRAIN_POST_TIMEOUT='1')
        p = subprocess.run([sys.executable, os.path.join(HERE, 'pmta-acct-drainer')],
                           env=env, capture_output=True, timeout=20)
    finally:
        os.close(fd)
    assert p.returncode == 0, p.stderr
    assert len(_spool_files(spool)) == 5
    assert 'overlap:' in open(drn.LOG_FILE).read()
