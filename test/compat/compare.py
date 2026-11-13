#!/usr/bin/env python3
"""Differential test: run the same command through the reference AWS CLI
(1.31.13, in Docker) and through gs3, against the same fake S3 server and the
same local files, and report every observable difference.

    test/compat/compare.py test/compat/cases/*.json          # run cases
    test/compat/compare.py -k sync test/compat/cases/*.json  # filter by name
    test/compat/compare.py -v ...                            # show outputs

Compared per case: exit status, stdout, stderr, the resulting bucket contents
(key, size, md5, stored headers), the resulting local files (path, size, md5,
mtime to the second) and, when a case asks for it, the S3 operations issued.

A case file is a JSON list of objects:

    {
      "name": "ls prefix",
      "args": ["s3", "ls", "s3://bkt/dir/"],
      "seed": [{"bucket": "bkt", "key": "dir/a", "size": 3, "mtime": "..."}],
      "files": {"local/a.txt": "content", "big.bin": {"size": 1048576, "mtime": 1700000000}},
      "symlinks": {"link": "local/a.txt"},
      "env": {"TZ": "Asia/Seoul"},
      "stdin": "text piped to the command",
      "ordered": true,        # compare stdout lines in order (ls, --dryrun)
      "requests": true,       # also compare the multiset of S3 operations
      "skip_stderr": true     # stderr is known to differ; compare status only
    }

Both tools run inside the reference container, so they see the same files,
user and system configuration; only the tool differs.

Requires: the reference image (make ref-image), bin/gs3-linux (make
build-linux) and tools/s3fake listening on GS3_FAKE_ADDR (default
127.0.0.1:19090).
"""
import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request

IMAGE = "gs3-awscli-ref:1.31.13"
CONTAINER = "gs3-awsref-compat"
FAKE_ADDR = os.environ.get("GS3_FAKE_ADDR", "127.0.0.1:19090")
FAKE_PORT = FAKE_ADDR.rsplit(":", 1)[1]
REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
GS3 = os.environ.get("GS3_BIN", os.path.join(REPO, "bin", "gs3"))
GS3_LINUX = os.environ.get("GS3_LINUX_BIN", os.path.join(REPO, "bin", "gs3-linux"))
# With GS3_COMPAT_HOST=1 the host build of gs3 runs directly on this machine
# instead of inside the reference container.
HOST_MODE = os.environ.get("GS3_COMPAT_HOST") == "1"

BASE_ENV = {
    "AWS_ACCESS_KEY_ID": "gs3test",
    "AWS_SECRET_ACCESS_KEY": "gs3testsecret",
    "AWS_DEFAULT_REGION": "us-east-1",
    "TZ": "UTC",
    # Keep both tools away from the developer's own configuration.
    "AWS_CONFIG_FILE": "/nonexistent/config",
    "AWS_SHARED_CREDENTIALS_FILE": "/nonexistent/credentials",
    "AWS_EC2_METADATA_DISABLED": "true",
}


def fake(path, method="GET", body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request("http://%s/_gs3fake/%s" % (FAKE_ADDR, path), data=data, method=method)
    with urllib.request.urlopen(req, timeout=30) as resp:
        raw = resp.read()
    return json.loads(raw) if raw else None


def ensure_container(workroot):
    probe = subprocess.run(
        ["docker", "inspect", "-f", "{{.State.Running}} {{range .Mounts}}{{.Source}}{{end}}", CONTAINER],
        capture_output=True, text=True)
    if probe.returncode == 0 and probe.stdout.strip() == "true " + workroot:
        return
    subprocess.run(["docker", "rm", "-f", CONTAINER], capture_output=True)
    subprocess.run(
        ["docker", "run", "-d", "--name", CONTAINER,
         "--add-host", "host.docker.internal:host-gateway",
         "-v", "%s:/work" % workroot,
         "--entrypoint", "sleep", IMAGE, "infinity"],
        check=True, capture_output=True)


DEFAULT_MTIME = 1700000000
DEFAULT_SEED_MTIME = "2024-01-01T00:00:00Z"


def wait_for_mount(workdir):
    """Wait until the container sees the freshly written work directory.

    Docker Desktop's file sharing can lag a moment behind the host.
    """
    want = sum(len(files) + len(dirs) for _, dirs, files in os.walk(workdir))
    for _ in range(50):
        probe = subprocess.run(
            ["docker", "exec", CONTAINER, "sh", "-c", "find /work/case -mindepth 1 | wc -l"],
            capture_output=True, text=True)
        if probe.returncode == 0 and probe.stdout.strip() == str(want):
            return
        time.sleep(0.1)


def prepare_files(workdir, case):
    # Files a case made unreadable must be removable again.
    for root, dirs, files in os.walk(workdir):
        for name in dirs + files:
            path = os.path.join(root, name)
            if not os.path.islink(path):
                os.chmod(path, 0o755)
    shutil.rmtree(workdir, ignore_errors=True)
    os.makedirs(workdir)
    for rel, spec in sorted(case.get("files", {}).items()):
        path = os.path.join(workdir, rel)
        if rel.endswith("/"):
            os.makedirs(path, exist_ok=True)
            continue
        os.makedirs(os.path.dirname(path) or workdir, exist_ok=True)
        mtime = None
        if isinstance(spec, dict):
            if "data" in spec:
                content = spec["data"].encode()
            else:
                seed = hashlib.md5(rel.encode()).digest()
                size = spec.get("size", 0)
                content = (seed * (size // len(seed) + 1))[:size]
            mtime = spec.get("mtime")
            mode = spec.get("mode")
        else:
            content, mode = spec.encode(), None
        with open(path, "wb") as f:
            f.write(content)
        if mode is not None:
            os.chmod(path, mode)
        # A fixed default keeps the two runs of a case identical.
        mtime = DEFAULT_MTIME if mtime is None else mtime
        os.utime(path, (mtime, mtime))
    for rel, target in sorted(case.get("symlinks", {}).items()):
        path = os.path.join(workdir, rel)
        os.makedirs(os.path.dirname(path) or workdir, exist_ok=True)
        os.symlink(target, path)


def snapshot_files(workdir):
    out = {}
    for root, dirs, files in os.walk(workdir):
        dirs.sort()
        for name in sorted(files):
            path = os.path.join(root, name)
            rel = os.path.relpath(path, workdir)
            if os.path.islink(path):
                out[rel] = {"link": os.readlink(path)}
                continue
            st = os.stat(path)
            try:
                with open(path, "rb") as f:
                    digest = hashlib.md5(f.read()).hexdigest()
            except PermissionError:
                digest = "unreadable"
            out[rel] = {"size": st.st_size, "md5": digest, "mtime": int(st.st_mtime)}
        for name in dirs:
            path = os.path.join(root, name)
            if not os.listdir(path):
                out[os.path.relpath(path, workdir) + "/"] = {"dir": True}
    return out


def run_tool(tool, case, workroot):
    """Run one tool for a case from a clean state; return what it did."""
    fake("reset", "POST")
    if case.get("seed"):
        # Without an explicit mtime the two runs would see different object
        # times, and downloads copy that time onto local files.
        seed = [dict(o, mtime=o.get("mtime", DEFAULT_SEED_MTIME)) for o in case["seed"]]
        fake("seed", "POST", seed)
    workdir = os.path.join(workroot, "case")
    prepare_files(workdir, case)
    if tool == "aws":
        wait_for_mount(workdir)
    fake("requests", "DELETE")

    env = dict(BASE_ENV)
    env.update(case.get("env", {}))
    stdin = case.get("stdin")
    stdin_bytes = stdin.encode() if isinstance(stdin, str) else None

    if tool == "aws":
        env.setdefault("AWS_ENDPOINT_URL", "http://host.docker.internal:%s" % FAKE_PORT)
        cmd = ["docker", "exec", "-i", "-w", "/work/case"]
        for key, value in env.items():
            cmd += ["-e", "%s=%s" % (key, value)]
        cmd += [CONTAINER, "aws"] + case["args"]
        proc = subprocess.run(cmd, input=stdin_bytes, capture_output=True)
    elif HOST_MODE:
        env.setdefault("AWS_ENDPOINT_URL", "http://%s" % FAKE_ADDR)
        full_env = {"PATH": os.environ.get("PATH", ""), "HOME": os.environ.get("HOME", "")}
        full_env.update(env)
        proc = subprocess.run([GS3] + case["args"], input=stdin_bytes, capture_output=True,
                              cwd=workdir, env=full_env)
    else:
        # Same container, same filesystem view, same user as the reference
        # CLI: only the tool differs.
        env.setdefault("AWS_ENDPOINT_URL", "http://host.docker.internal:%s" % FAKE_PORT)
        cmd = ["docker", "exec", "-i", "-w", "/work/case"]
        for key, value in env.items():
            cmd += ["-e", "%s=%s" % (key, value)]
        cmd += [CONTAINER, "/work/.bin/gs3"] + case["args"]
        proc = subprocess.run(cmd, input=stdin_bytes, capture_output=True)

    state = fake("state")
    requests = fake("requests")
    return {
        "rc": proc.returncode,
        "stdout": proc.stdout.decode("utf-8", "replace"),
        "stderr": proc.stderr.decode("utf-8", "replace"),
        "buckets": state,
        "files": snapshot_files(workdir),
        "ops": sorted(r["op"] for r in (requests or [])),
    }


WORKDIR_HOST = [""]
ENDPOINT_RE = re.compile(r"(host\.docker\.internal|127\.0\.0\.1):%s" % FAKE_PORT)
USAGE_RE = re.compile(r"^usage: .*?(?=^\S+: error: )", re.S | re.M)
TEMP_SUFFIX_RE = re.compile(r"\.[0-9A-Fa-f]{8}\b")


def normalize_stdout(text, ordered):
    lines = []
    for segment in re.split(r"[\r\n]", text):
        segment = segment.rstrip()
        if not segment or segment.startswith("Completed "):
            continue
        lines.append(segment)
    return lines if ordered else sorted(lines)


def _sort_query(match):
    params = sorted(match.group(2).split("&"))
    return '"%s?%s"' % (match.group(1), "&".join(params))


def normalize_stderr(text):
    text = ENDPOINT_RE.sub("ENDPOINT", text)
    # Absolute local paths differ only by where the work directory is mounted.
    text = text.replace(WORKDIR_HOST[0], "WORKDIR").replace("/work/case", "WORKDIR")
    # Query parameter order inside a quoted URL is not significant.
    text = re.sub(r'"(https?://[^"?]+)\?([^"]*)"', _sort_query, text)
    # The usage banner differs by design (no "AWS CLI version 2" notice, and
    # the program is called gs3); the error line after it must match.
    text = USAGE_RE.sub("", text)
    text = re.sub(r"^(aws|gs3): error: ", "PROG: error: ", text, flags=re.M)
    lines = [line.rstrip() for line in text.split("\n")]
    return [line for line in lines if line]


def normalize_buckets(buckets):
    out = {}
    for name, objects in (buckets or {}).items():
        out[name] = [
            {"key": o["key"], "size": o["size"], "md5": o["md5"], "headers": o["headers"]}
            for o in objects
        ]
    return out


def normalize_presign(text):
    text = ENDPOINT_RE.sub("ENDPOINT", text)
    text = re.sub(r"X-Amz-Signature=[0-9a-f]+", "X-Amz-Signature=SIG", text)
    text = re.sub(r"X-Amz-Date=\d{8}T\d{6}Z", "X-Amz-Date=DATE", text)
    text = re.sub(r"(X-Amz-Credential=[^%]+%2F)\d{8}", r"\1DAY", text)
    return text


def describe_diff(a, b):
    """List only the entries that differ between two dicts."""
    lines = []
    for key in sorted(set(a) | set(b)):
        if a.get(key) != b.get(key):
            lines.append("\n    %s\n      aws: %s\n      gs3: %s" % (
                key, json.dumps(a.get(key), sort_keys=True), json.dumps(b.get(key), sort_keys=True)))
    return "".join(lines)


def compare(case, ref, got):
    diffs = []
    if ref["rc"] != got["rc"]:
        diffs.append("exit status: aws=%d gs3=%d" % (ref["rc"], got["rc"]))
    ordered = case.get("ordered", False)
    a, b = normalize_stdout(ref["stdout"], ordered), normalize_stdout(got["stdout"], ordered)
    if case.get("raw_stdout"):
        a, b = [ref["stdout"]], [got["stdout"]]
    if case.get("presign"):
        # Signatures and timestamps differ by nature; the shape must not.
        a, b = [normalize_presign(ref["stdout"])], [normalize_presign(got["stdout"])]
    if a != b:
        diffs.append("stdout differs:\n    aws: %r\n    gs3: %r" % (a, b))
    if not case.get("skip_stderr"):
        a, b = normalize_stderr(ref["stderr"]), normalize_stderr(got["stderr"])
        if not ordered:
            a, b = sorted(a), sorted(b)
        if a != b:
            diffs.append("stderr differs:\n    aws: %r\n    gs3: %r" % (a, b))
    a, b = normalize_buckets(ref["buckets"]), normalize_buckets(got["buckets"])
    if a != b:
        flat_a = {"%s/%s" % (bkt, o["key"]): o for bkt, objs in a.items() for o in objs}
        flat_b = {"%s/%s" % (bkt, o["key"]): o for bkt, objs in b.items() for o in objs}
        diffs.append("bucket state differs:" + describe_diff(flat_a, flat_b))
        if set(a) != set(b):
            diffs.append("buckets differ: aws=%s gs3=%s" % (sorted(a), sorted(b)))
    if ref["files"] != got["files"]:
        diffs.append("local files differ:" + describe_diff(ref["files"], got["files"]))
    if case.get("requests") and ref["ops"] != got["ops"]:
        diffs.append("S3 operations differ:\n    aws: %s\n    gs3: %s" % (ref["ops"], got["ops"]))
    return diffs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("case_files", nargs="+")
    ap.add_argument("-k", dest="keyword", help="only run cases whose name contains this")
    ap.add_argument("-v", dest="verbose", action="store_true")
    opts = ap.parse_args()

    workroot = os.environ.get("GS3_COMPAT_WORKDIR") or tempfile.mkdtemp(prefix="gs3-compat-", dir="/tmp")
    workroot = os.path.realpath(workroot)
    os.makedirs(workroot, exist_ok=True)
    WORKDIR_HOST[0] = os.path.join(workroot, "case")
    ensure_container(workroot)
    if not HOST_MODE:
        os.makedirs(os.path.join(workroot, ".bin"), exist_ok=True)
        shutil.copy2(GS3_LINUX, os.path.join(workroot, ".bin", "gs3"))

    failed = total = 0
    for case_file in opts.case_files:
        with open(case_file) as f:
            cases = json.load(f)
        for case in cases:
            if opts.keyword and opts.keyword not in case["name"]:
                continue
            total += 1
            ref = run_tool("aws", case, workroot)
            got = run_tool("gs3", case, workroot)
            diffs = compare(case, ref, got)
            label = "%s :: %s" % (os.path.basename(case_file), case["name"])
            if diffs:
                failed += 1
                print("FAIL %s\n  $ %s" % (label, " ".join(case["args"])))
                for d in diffs:
                    print("  " + d)
            else:
                print("ok   %s" % label)
            if opts.verbose or (diffs and os.environ.get("GS3_COMPAT_SHOW")):
                for tool, res in (("aws", ref), ("gs3", got)):
                    print("  [%s] rc=%d\n    stdout=%r\n    stderr=%r" % (
                        tool, res["rc"], res["stdout"][:1500], res["stderr"][:1500]))
    print("\n%d cases, %d failed" % (total, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
