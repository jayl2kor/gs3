#!/usr/bin/env python3
"""Run command lines through the reference AWS CLI (1.31.13) and show exactly
what it prints.

Development helper for answering "what does the real CLI do here?":

    printf '%s\n' 's3 cp a' 's3 ls --no' | test/compat/refrun.py

Each stdin line is one argv (shell-quoted) passed to `aws`. For every line the
exit code and the repr() of stdout/stderr are printed, so control characters
such as "\r" stay visible.

A long-lived container named gs3-awsref is started on first use.
Extra environment for the CLI: -e KEY=VALUE (repeatable).
Working directory inside the container: -w DIR (default /work).
"""
import argparse
import shlex
import subprocess
import sys

IMAGE = "gs3-awscli-ref:1.31.13"
NAME = "gs3-awsref"
BASE_ENV = {
    "AWS_ACCESS_KEY_ID": "gs3test",
    "AWS_SECRET_ACCESS_KEY": "gs3testsecret",
    "AWS_DEFAULT_REGION": "us-east-1",
}


def ensure_container():
    probe = subprocess.run(
        ["docker", "inspect", "-f", "{{.State.Running}}", NAME],
        capture_output=True, text=True)
    if probe.returncode == 0 and probe.stdout.strip() == "true":
        return
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run(
        ["docker", "run", "-d", "--name", NAME,
         "--add-host", "host.docker.internal:host-gateway",
         "--entrypoint", "sleep", IMAGE, "infinity"],
        check=True, capture_output=True)
    subprocess.run(["docker", "exec", NAME, "mkdir", "-p", "/work"], check=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-e", dest="env", action="append", default=[])
    ap.add_argument("-w", dest="workdir", default="/work")
    ap.add_argument("--limit", type=int, default=600,
                    help="max characters of each stream to show")
    opts = ap.parse_args()

    ensure_container()
    env = dict(BASE_ENV)
    for item in opts.env:
        key, _, value = item.partition("=")
        env[key] = value
    env_args = []
    for key, value in env.items():
        env_args += ["-e", "%s=%s" % (key, value)]

    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        argv = shlex.split(line)
        proc = subprocess.run(
            ["docker", "exec", "-w", opts.workdir] + env_args + [NAME, "aws"] + argv,
            capture_output=True)
        out = proc.stdout.decode("utf-8", "replace")
        err = proc.stderr.decode("utf-8", "replace")
        print("$ aws %s" % line)
        print("  rc=%d" % proc.returncode)
        print("  stdout=%r" % out[:opts.limit])
        print("  stderr=%r" % err[:opts.limit])


if __name__ == "__main__":
    main()
