#!/usr/bin/env python3
"""Render the next ECS task definition for ignite-upside-down.

REQ-092. Env is the send-path control plane, so it lives in git:
deploy/env.manifest.json is the source of truth and this script is the only
thing that renders it. The 19 hard-coded upserts that used to live here are
gone -- edit the manifest, not this file.

Contract
  * every manifest entry with a non-null "value" is upserted verbatim
  * class "secret"  -> value null, carried from the running revision untouched
                       (or injected from the deploy shell, as before)
  * class "stamp"   -> written here from argv (APP_*)
  * class "removed" -> stripped from the rendered revision
  * "default": true -> read in code but deliberately unset; must stay absent
  * ANY env present in the running revision that is not in the manifest is a
    hard FAILURE. That is the whole point: a hand-registered flag flip can no
    longer survive the next deploy unnoticed.

Usage (deploy.sh):
  prepare_task_definition.py <in> <out> <container> <image_uri> <git_sha> \
      <build_time> <image_digest> [--tree-dirty 0|1] [--dry-run]

Usage (audit, no AWS credentials needed beyond describe-*):
  prepare_task_definition.py --dry-run --from-revision ignite-upside-down:1077
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path


DISALLOWED_KEYS = {
    "taskDefinitionArn",
    "revision",
    "status",
    "requiresAttributes",
    "compatibilities",
    "registeredAt",
    "registeredBy",
    "deregisteredAt",
}

DEFAULT_MANIFEST = Path(__file__).resolve().parent / "env.manifest.json"

# Names whose VALUES may come from the deploy shell when exported -- the
# historical PASSTHROUGH_ENV_VARS list, now declared per-entry in the manifest.
# Only entries with a null manifest value are eligible: a name that carries a
# value in git is owned by git, and a stray shell variable must never silently
# overwrite it.
def passthrough_names(manifest):
    return [e["name"] for e in manifest["env"]
            if e.get("passthrough") and e.get("value") is None]


def load_manifest(path: Path):
    raw = path.read_bytes()
    doc = json.loads(raw.decode())
    sha = hashlib.sha256(raw).hexdigest()
    by_name = {}
    for e in doc.get("env", []):
        name = e["name"]
        if name in by_name:
            raise SystemExit(f"env.manifest.json: duplicate entry {name!r}")
        if e.get("class") not in {"kill", "route", "capacity", "infra",
                                  "secret", "stamp", "removed"}:
            raise SystemExit(f"env.manifest.json: {name!r} has bad class {e.get('class')!r}")
        by_name[name] = e
    doc["_by_name"] = by_name
    doc["_sha256"] = sha
    return doc


def upsert_env(env_list, name, value):
    for item in env_list:
        if item.get("name") == name:
            item["value"] = value
            return
    env_list.append({"name": name, "value": value})


def env_map(env_list):
    return {e.get("name"): e.get("value", "") for e in env_list}


def fetch_task_def(revision, region, profile):
    cmd = ["aws", "ecs", "describe-task-definition", "--task-definition", revision,
           "--region", region, "--output", "json"]
    if profile:
        cmd += ["--profile", profile]
    return json.loads(subprocess.run(cmd, capture_output=True, text=True,
                                     check=True).stdout)


def parse_args(argv):
    p = argparse.ArgumentParser(add_help=True)
    p.add_argument("input", nargs="?")
    p.add_argument("output", nargs="?")
    p.add_argument("container_name", nargs="?", default="ignite-server")
    p.add_argument("image_uri", nargs="?")
    p.add_argument("git_sha", nargs="?")
    p.add_argument("build_time", nargs="?")
    p.add_argument("image_digest", nargs="?")
    p.add_argument("--manifest", default=str(DEFAULT_MANIFEST))
    p.add_argument("--tree-dirty", default="0", choices=["0", "1"])
    p.add_argument("--dry-run", action="store_true",
                   help="print the planned diff; write nothing")
    p.add_argument("--from-revision",
                   help="describe this task definition instead of reading <input>")
    p.add_argument("--region", default=os.environ.get("AWS_REGION", "us-west-2"))
    p.add_argument("--profile", default=os.environ.get("AWS_PROFILE", "jamesventure"))
    return p.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv if argv is not None else sys.argv[1:])

    manifest = load_manifest(Path(args.manifest))
    by_name = manifest["_by_name"]
    manifest_sha = manifest["_sha256"]

    if args.from_revision:
        payload = fetch_task_def(args.from_revision, args.region, args.profile)
        baseline = args.from_revision
    else:
        if not args.input:
            raise SystemExit("need <input> or --from-revision")
        payload = json.loads(Path(args.input).read_text())
        baseline = args.input
    task_def = payload.get("taskDefinition", payload)
    sanitized = {k: v for k, v in task_def.items() if k not in DISALLOWED_KEYS}

    container_name = args.container_name or manifest.get("container", "ignite-server")
    containers = sanitized.get("containerDefinitions", [])
    target = None
    for container in containers:
        if container.get("name") == container_name:
            target = container
            break
    if target is None:
        raise SystemExit(f"container {container_name!r} not found in task definition")

    before = env_map(target.setdefault("environment", []))
    env_list = target["environment"]

    # ---------------------------------------------------------------- gate 1
    # Every env on the running revision must be declared. An undeclared name is
    # a hand-registered flip (or a fossil) and the deploy stops here.
    unknown = sorted(n for n in before if n not in by_name)
    unmanaged = sorted(n for n in before
                       if n in by_name and by_name[n].get("default"))
    if unknown or unmanaged:
        print("MANIFEST DRIFT — refusing to render a task definition", file=sys.stderr)
        for n in unknown:
            print(f"  UNKNOWN env on {baseline}: {n} "
                  f"(add it to deploy/env.manifest.json with a class and a reader)",
                  file=sys.stderr)
        for n in unmanaged:
            print(f"  env {n} is declared 'default: true' (must stay unset) but is "
                  f"SET on {baseline} — give it a value in the manifest or drop it",
                  file=sys.stderr)
        return 2

    # ---------------------------------------------------------------- render
    if args.image_uri:
        target["image"] = args.image_uri

    stamps = {
        "APP_VERSION": args.git_sha,
        "APP_GIT_SHA": args.git_sha,
        "APP_BUILD_TIME": args.build_time,
        "APP_IMAGE_URI": args.image_uri,
        "APP_IMAGE_DIGEST": args.image_digest,
        "APP_ENV_MANIFEST_SHA": manifest_sha,
        "APP_TREE_DIRTY": args.tree_dirty,
    }
    for name, value in stamps.items():
        if value:
            upsert_env(env_list, name, value)

    for entry in manifest["env"]:
        if entry.get("class") == "removed" or entry.get("value") is None:
            continue
        upsert_env(env_list, entry["name"], entry["value"])

    # Secrets and optional names still come from the deploy shell when exported.
    for name in filter(None, os.environ.get("DEPLOY_UPSERT_ENVS", "").split(",")):
        name = name.strip()
        value = os.environ.get(name, "")
        if not value:
            print(f"WARNING: DEPLOY_UPSERT_ENVS names {name!r} but it is empty in "
                  "the deploy shell — NOT upserted", file=sys.stderr)
        elif name not in by_name or by_name[name].get("value") is not None:
            print(f"ERROR: DEPLOY_UPSERT_ENVS names {name!r}, which is either "
                  "absent from deploy/env.manifest.json or already carries a "
                  "value there (the manifest owns it)", file=sys.stderr)
            return 2
        else:
            upsert_env(env_list, name, value)

    for name in passthrough_names(manifest):
        val = os.environ.get(name)
        if val:
            upsert_env(env_list, name, val)

    removed = {e["name"] for e in manifest["env"] if e.get("class") == "removed"}
    env_list[:] = [e for e in env_list if e.get("name") not in removed]

    # ReviewForge engine sidecar (Creative Studio). awsvpc networking shares the
    # namespace, so the Go server reaches it at localhost:3100. Set
    # REVIEW_FORGE_IMAGE in the deploy shell to add/update the sidecar; when
    # unset, the existing sidecar definition carries forward unchanged (it has
    # been pinned since :780 — see manifest.sidecar).
    rf_image = os.environ.get("REVIEW_FORGE_IMAGE")
    if rf_image:
        upsert_env(env_list, "REVIEW_FORGE_INTERNAL_URL", "http://localhost:3100")
        sidecar = None
        for container in containers:
            if container.get("name") == "review-forge-engine":
                sidecar = container
                break
        if sidecar is None:
            sidecar = {"name": "review-forge-engine"}
            containers.append(sidecar)
        sidecar["image"] = rf_image
        sidecar["essential"] = False
        sidecar["portMappings"] = [{"containerPort": 3100, "protocol": "tcp"}]
        sidecar_env = sidecar.setdefault("environment", [])
        upsert_env(sidecar_env, "PORT", "3100")
        # Next standalone binds to $HOSTNAME; in ECS that's the task hostname,
        # which leaves 127.0.0.1 unbound and the Go server locked out.
        upsert_env(sidecar_env, "HOSTNAME", "0.0.0.0")
        upsert_env(sidecar_env, "REVIEW_FORGE_DISABLE_LOCAL_FEEDS", "1")
        if target.get("logConfiguration") and not sidecar.get("logConfiguration"):
            log_cfg = json.loads(json.dumps(target["logConfiguration"]))
            opts = log_cfg.get("options", {})
            if "awslogs-stream-prefix" in opts:
                opts["awslogs-stream-prefix"] = opts["awslogs-stream-prefix"] + "-rf"
            sidecar["logConfiguration"] = log_cfg

    # ---------------------------------------------------------------- report
    after = env_map(env_list)
    stamp_names = {e["name"] for e in manifest["env"] if e.get("class") == "stamp"}
    changed = sorted(n for n in after
                     if n in before and after[n] != before[n] and n not in stamp_names)
    added = sorted(n for n in after if n not in before and n not in stamp_names)
    dropped = sorted(n for n in before if n not in after)
    stamped = sorted(n for n in stamp_names
                     if n in after and after.get(n) != before.get(n))

    print(f"manifest       : {args.manifest}")
    print(f"manifest sha256: {manifest_sha}")
    print(f"baseline       : {baseline} ({len(before)} env)")
    print(f"rendered       : {len(after)} env")
    print(f"unknown env    : 0")
    print(f"value changes  : {len(changed)}" + (" -> " + ", ".join(changed) if changed else ""))
    print(f"added          : {len(added)}" + (" -> " + ", ".join(added) if added else ""))
    print(f"removed        : {len(dropped)}" + (" -> " + ", ".join(dropped) if dropped else ""))
    print(f"deploy stamps  : {len(stamped)}" + (" -> " + ", ".join(stamped) if stamped else ""))
    for n in changed:
        cls = by_name[n]["class"]
        if cls == "secret":
            print(f"  ~ {n} [{cls}] <redacted> -> <redacted>")
        else:
            print(f"  ~ {n} [{cls}] {before[n]!r} -> {after[n]!r}")

    if args.dry_run:
        print("DRY RUN — nothing written, nothing registered.")
        return 0

    if not args.output:
        raise SystemExit("need <output> unless --dry-run")
    Path(args.output).write_text(json.dumps(sanitized, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
