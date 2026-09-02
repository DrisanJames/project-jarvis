#!/usr/bin/env python3
"""Append one JSON line to deploy/deploy_log.jsonl (REQ-092 DoD 6).

Called by deploy.sh with the DEPLOY_LOG_* environment set. ECS retains only
~100 service events (about five days), so reconstructing "when did that flag
change" meant walking 98 task definitions by hand. This is the cheap durable
record: one line per deploy, including the failed ones.
"""
import json
import os
import sys
import time


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: append_deploy_log.py <path/to/deploy_log.jsonl>", file=sys.stderr)
        return 1
    row = {
        "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "revision": os.environ.get("DEPLOY_LOG_REVISION", ""),
        "previous_revision": os.environ.get("DEPLOY_LOG_PREVIOUS", ""),
        "git_sha": os.environ.get("DEPLOY_LOG_GIT_SHA", ""),
        "image": os.environ.get("DEPLOY_LOG_IMAGE", ""),
        "env_manifest_sha": os.environ.get("DEPLOY_LOG_MANIFEST_SHA", ""),
        "config_only": os.environ.get("DEPLOY_LOG_CONFIG_ONLY") == "1",
        "tree_dirty": os.environ.get("DEPLOY_LOG_TREE_DIRTY") == "1",
        "operator": os.environ.get("DEPLOY_OPERATOR")
        or os.environ.get("USER")
        or "unknown",
        "result": os.environ.get("DEPLOY_LOG_RESULT", "unknown"),
    }
    with open(sys.argv[1], "a") as fh:
        fh.write(json.dumps(row) + "\n")
    print("deploy_log:", json.dumps(row))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
