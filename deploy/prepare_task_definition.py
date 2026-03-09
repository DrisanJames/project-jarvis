#!/usr/bin/env python3
import json
import os
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

PASSTHROUGH_ENV_VARS = [
    "PMTA_SSH_KEY",
    "PMTA_SSH_PASSPHRASE",
]

REMOVE_ENV_VARS = [
    "DB_ADMIN_URL",
]


def upsert_env(env_list, name, value):
    for item in env_list:
        if item.get("name") == name:
            item["value"] = value
            return
    env_list.append({"name": name, "value": value})


def main() -> int:
    if len(sys.argv) != 8:
        print(
            "usage: prepare_task_definition.py <input> <output> <container_name> <image_uri> <git_sha> <build_time> <image_digest>",
            file=sys.stderr,
        )
        return 1

    input_path = Path(sys.argv[1])
    output_path = Path(sys.argv[2])
    container_name = sys.argv[3]
    image_uri = sys.argv[4]
    git_sha = sys.argv[5]
    build_time = sys.argv[6]
    image_digest = sys.argv[7]

    payload = json.loads(input_path.read_text())
    task_def = payload.get("taskDefinition", payload)
    sanitized = {key: value for key, value in task_def.items() if key not in DISALLOWED_KEYS}

    containers = sanitized.get("containerDefinitions", [])
    target = None
    for container in containers:
        if container.get("name") == container_name:
            target = container
            break

    if target is None:
        raise SystemExit(f"container {container_name!r} not found in task definition")

    target["image"] = image_uri
    env_list = target.setdefault("environment", [])
    upsert_env(env_list, "APP_VERSION", git_sha)
    upsert_env(env_list, "APP_GIT_SHA", git_sha)
    upsert_env(env_list, "APP_BUILD_TIME", build_time)
    upsert_env(env_list, "APP_IMAGE_URI", image_uri)
    upsert_env(env_list, "APP_IMAGE_DIGEST", image_digest)

    for var_name in PASSTHROUGH_ENV_VARS:
        val = os.environ.get(var_name)
        if val:
            upsert_env(env_list, var_name, val)

    env_list[:] = [e for e in env_list if e.get("name") not in REMOVE_ENV_VARS]

    output_path.write_text(json.dumps(sanitized, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
