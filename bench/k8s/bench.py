#!/usr/bin/env python3
"""In-cluster benchmark on a real dataset: the AWS CLI the workflows use today,
gs3 and s5cmd, run from a pod next to the object store.

For each tool:

    1  download the whole dataset           sync s3://B/<src-all>/   ->  local
    2  sync it back to the same path        sync local  ->  s3://B/<src-all>/
    3  sync it to a different path          sync local  ->  s3://B/out/<tool>/all/
    4  download one large object            sync s3://B/<src-one>/   ->  local
    5  sync it back to the same path        sync local  ->  s3://B/<src-one>/
    6  sync it to a different path          sync local  ->  s3://B/out/<tool>/one/

The "same path" is a copy of the dataset inside the benchmark bucket, not the
original: s5cmd does not keep object timestamps on download, so its sync back
re-uploads everything, and the original data must not be rewritten by a test.

Everything a tool uploads under out/<tool>/ is deleted again when the tool is
done. The source copies are left in place so the benchmark can be repeated.

BENCH_CONNECTIONS=N gives every tool the same number of concurrent requests
(unset or 0: each tool's defaults). Part sizes stay at each tool's default.

    AWS CLI   s3.max_concurrent_requests = N in a private copy of the config
    gs3       GS3_MAX_CONCURRENT_REQUESTS=N
    s5cmd     --numworkers N (files at once) and --concurrency N (parts of one
              file); it has no shared budget, so while a multipart file is in
              flight next to small files it can go up to 2N-1

What each tool really opened is measured: the number of sockets the process
holds is sampled twice a second (Linux only).

Credentials and endpoint come from the usual AWS_* variables. No secret is
printed.
"""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import threading
import time

BUCKET = os.environ["BENCH_BUCKET"]
ENDPOINT = os.environ["AWS_ENDPOINT_URL"]
ROOT = os.environ.get("BENCH_ROOT", "/bench")
SRC_ALL = os.environ.get("BENCH_SRC_ALL", "src/csi-fs/")
SRC_ONE = os.environ.get("BENCH_SRC_ONE", "src-one/")
TOOL_NAMES = [t for t in os.environ.get("BENCH_TOOLS", "aws-1.15.85,gs3,s5cmd").split(",") if t]
RUN_TIMEOUT = int(os.environ.get("BENCH_RUN_TIMEOUT", "5400"))
CONNECTIONS = int(os.environ.get("BENCH_CONNECTIONS", "0") or 0)
DISK_TEST = os.environ.get("BENCH_DISK_TEST", "1") != "0"

TOOLS_DIR = os.environ.get("BENCH_TOOLS_DIR", ROOT + "/tools")
GS3 = TOOLS_DIR + "/gs3"
S5 = TOOLS_DIR + "/s5cmd"
AWS = os.environ.get("BENCH_AWS", "/usr/local/bin/aws")

MIB = 1 << 20
results = []


def log(msg):
    print(msg, flush=True)


def base_env(extra=None):
    env = dict(os.environ)
    # s5cmd's SDK reads the region only from AWS_REGION.
    env.setdefault("AWS_REGION", env.get("AWS_DEFAULT_REGION", ""))
    if extra:
        env.update(extra)
    return env


def s3url(prefix):
    return "s3://%s/%s" % (BUCKET, prefix)


# --- the tools -------------------------------------------------------------

class AwsCli:
    def __init__(self, name):
        self.name = name
        self.env = {}
        self.settings = "defaults (10 concurrent requests)"
        if CONNECTIONS:
            # The CLI reads its concurrency only from the config file, so it
            # gets a private copy of the config with that one value set.
            path = ROOT + "/aws-config"
            current = os.path.expanduser(os.environ.get("AWS_CONFIG_FILE", "~/.aws/config"))
            if os.path.exists(current):
                shutil.copyfile(current, path)
            self.env = {"AWS_CONFIG_FILE": path}
            subprocess.run([AWS, "configure", "set", "s3.max_concurrent_requests", str(CONNECTIONS)],
                           env=base_env(self.env), check=True)
            with open(path) as f:
                written = [line.strip() for line in f if "max_concurrent_requests" in line]
            self.settings = "config file: " + "; ".join(written)

    def sync_down(self, prefix, dst_dir):
        return [AWS, "s3", "sync", s3url(prefix), dst_dir, "--endpoint-url", ENDPOINT, "--only-show-errors"]

    def sync_up(self, src_dir, prefix):
        return [AWS, "s3", "sync", src_dir, s3url(prefix), "--endpoint-url", ENDPOINT, "--only-show-errors"]

    def version(self):
        return subprocess.run([AWS, "--version"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                              universal_newlines=True).stdout.strip()


class Gs3:
    name = "gs3"

    def __init__(self):
        self.env = {}
        self.settings = "defaults (by CPU limit)"
        if CONNECTIONS:
            self.env = {"GS3_MAX_CONCURRENT_REQUESTS": str(CONNECTIONS)}
            self.settings = "GS3_MAX_CONCURRENT_REQUESTS=%d" % CONNECTIONS

    def sync_down(self, prefix, dst_dir):
        return [GS3, "s3", "sync", s3url(prefix), dst_dir, "--only-show-errors"]

    def sync_up(self, src_dir, prefix):
        return [GS3, "s3", "sync", src_dir, s3url(prefix), "--only-show-errors"]

    def version(self):
        return subprocess.run([GS3, "--version"], stdout=subprocess.PIPE, universal_newlines=True).stdout.strip()


class S5cmd:
    name = "s5cmd"

    def __init__(self):
        self.env = {}
        self.workers, self.per_file = [], []
        self.settings = "defaults (256 files at once, 5 parts per file)"
        if CONNECTIONS:
            self.workers = ["--numworkers", str(CONNECTIONS)]
            self.per_file = ["--concurrency", str(CONNECTIONS)]
            self.settings = "--numworkers %d, sync --concurrency %d" % (CONNECTIONS, CONNECTIONS)

    def _sync(self):
        return [S5, "--endpoint-url", ENDPOINT, "--log", "error"] + self.workers + ["sync"] + self.per_file

    def sync_down(self, prefix, dst_dir):
        return self._sync() + [s3url(prefix) + "*", dst_dir.rstrip("/") + "/"]

    def sync_up(self, src_dir, prefix):
        return self._sync() + [src_dir.rstrip("/") + "/", s3url(prefix)]

    def version(self):
        return "s5cmd " + subprocess.run([S5, "version"], stdout=subprocess.PIPE, universal_newlines=True).stdout.strip()


def make_tool(name):
    if name == "gs3":
        return Gs3()
    if name == "s5cmd":
        return S5cmd()
    return AwsCli(name)


# --- helpers ---------------------------------------------------------------

def list_objects(prefix):
    """Return {relative key: (size, etag, last_modified)} for a prefix."""
    proc = subprocess.run([AWS, "s3api", "list-objects-v2", "--bucket", BUCKET, "--prefix", prefix,
                           "--endpoint-url", ENDPOINT, "--output", "json"],
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, env=base_env())
    if proc.returncode != 0:
        log("   !! listing %s failed: %s" % (prefix, proc.stderr.strip()[-200:]))
        return {}
    data = json.loads(proc.stdout) if proc.stdout.strip() else {}
    return {o["Key"][len(prefix):]: (o["Size"], o["ETag"].strip('"'), o["LastModified"])
            for o in data.get("Contents", [])}


def summarize(objects):
    return len(objects), sum(v[0] for v in objects.values())


def changed_since(before, after):
    """How many objects were rewritten or added between two listings."""
    return sum(1 for k, v in after.items() if k not in before or before[k][2] != v[2])


def tree_stats(path):
    count = total = 0
    for root, _, files in os.walk(path):
        for name in files:
            count += 1
            total += os.path.getsize(os.path.join(root, name))
    return count, total


def file_md5(path):
    h = hashlib.md5()
    with open(path, "rb", buffering=0) as f:
        while True:
            chunk = f.read(8 * MIB)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


def sample_digest(path, every=50):
    """A digest over every Nth file's content, to compare downloads across tools."""
    files = []
    for root, _, names in os.walk(path):
        for name in names:
            files.append(os.path.relpath(os.path.join(root, name), path))
    files.sort()
    h = hashlib.md5()
    picked = 0
    for rel in files[::every]:
        full = os.path.join(path, rel)
        if os.path.getsize(full) > 64 * MIB:
            continue
        h.update(("%s %s\n" % (rel, file_md5(full))).encode())
        picked += 1
    return "%s (%d files)" % (h.hexdigest()[:12], picked)


def sample_sockets(pid, stop, out):
    """Sample how many sockets a process holds open, i.e. its connections.

    Records the peak, the average, and how many different sockets were seen:
    a lower bound on connections opened, since short-lived ones fall between
    samples. A tool that keeps its connections alive sees about as many as
    its peak; one that keeps reopening them sees many more.
    """
    fd_dir = "/proc/%d/fd" % pid
    seen = set()
    peak = total = samples = 0
    while not stop.wait(0.5):
        try:
            fds = os.listdir(fd_dir)
        except OSError:
            break  # the process is gone, or there is no /proc
        now = 0
        for fd in fds:
            try:
                link = os.readlink("%s/%s" % (fd_dir, fd))
            except OSError:
                continue
            if link.startswith("socket:"):
                now += 1
                seen.add(link)
        peak = max(peak, now)
        total += now
        samples += 1
    if samples:
        out.update(sockets_peak=peak, sockets_avg=round(total / float(samples)), sockets_seen=len(seen))


def measure(test, tool, cmd, size_bytes=0, files=0):
    """Run one command; record wall time, CPU time, peak memory and sockets."""
    err_path = "%s/stderr.txt" % ROOT
    sockets = {}
    start = time.time()
    with open(err_path, "wb") as err:
        proc = subprocess.Popen(cmd, env=base_env(tool.env), stdout=subprocess.DEVNULL, stderr=err)
        killer = threading.Timer(RUN_TIMEOUT, proc.kill)
        killer.start()
        stop = threading.Event()
        sampler = threading.Thread(target=sample_sockets, args=(proc.pid, stop, sockets))
        sampler.start()
        # wait4 gives the resource usage of exactly this child.
        _, status, usage = os.wait4(proc.pid, 0)
        proc.returncode = os.WEXITSTATUS(status) if os.WIFEXITED(status) else -os.WTERMSIG(status)
        stop.set()
        sampler.join()
        killer.cancel()
    elapsed = time.time() - start

    with open(err_path, "rb") as f:
        err_lines = [line for line in f.read().decode("utf-8", "replace").splitlines() if line.strip()]
    # ru_maxrss is kilobytes on Linux, bytes on macOS.
    rss = usage.ru_maxrss / (1024.0 * 1024.0 if sys.platform == "darwin" else 1024.0)
    row = {
        "test": test, "tool": tool.name, "seconds": round(elapsed, 1), "rc": proc.returncode,
        "cpu_seconds": round(usage.ru_utime + usage.ru_stime, 1), "peak_rss_mb": round(rss),
    }
    if size_bytes:
        row["mib_per_s"] = round(size_bytes / MIB / elapsed, 1)
    if files:
        row["files_per_s"] = round(files / elapsed)
    row.update(sockets)
    if err_lines:
        row["stderr_lines"] = len(err_lines)
        row["stderr_last"] = err_lines[-1][:200]
    results.append(row)
    return row


def report(row, detail=""):
    if detail:
        row["detail"] = detail
    rate = ""
    if "mib_per_s" in row:
        rate += "%8.1f MiB/s" % row["mib_per_s"]
    if "files_per_s" in row:
        rate += "%7d files/s" % row["files_per_s"]
    conns = ""
    if "sockets_peak" in row:
        conns = " conns peak=%d avg=%d seen=%d" % (row["sockets_peak"], row["sockets_avg"], row["sockets_seen"])
    log("%-28s %-12s %8.1fs %s  cpu=%5.0fs rss=%5dMB%s rc=%d  %s%s" % (
        row["test"], row["tool"], row["seconds"], rate, row["cpu_seconds"], row["peak_rss_mb"], conns, row["rc"],
        detail, ("  stderr(%d): %s" % (row["stderr_lines"], row["stderr_last"])) if "stderr_lines" in row else ""))


def delete_prefix(prefix):
    """Remove everything under a prefix (not timed)."""
    start = time.time()
    # Key-by-key deletes: a 1,000-key batch can take this store about a minute.
    proc = subprocess.run([GS3, "s3", "rm", s3url(prefix), "--recursive", "--only-show-errors", "--cli-read-timeout", "900"],
                          env=base_env({"GS3_BATCH_DELETE": "0"}), stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                          universal_newlines=True)
    left = len(list_objects(prefix))
    log("   cleanup %s: rc=%d, %d objects left, %.0fs %s" % (prefix, proc.returncode, left, time.time() - start,
                                                            proc.stdout.strip()[-200:]))


# --- setup -----------------------------------------------------------------

def disk_speed():
    """Raw speed of the scratch disk, bypassing the page cache."""
    path = ROOT + "/ddtest"
    try:
        for label, cmd in (("write", ["dd", "if=/dev/zero", "of=" + path, "bs=1M", "count=4096", "oflag=direct"]),
                           ("read", ["dd", "if=" + path, "of=/dev/null", "bs=1M", "iflag=direct"])):
            out = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True).stdout
            log("scratch disk %s: %s" % (label, out.strip().splitlines()[-1][:120]))
    except (OSError, IndexError) as exc:
        log("scratch disk speed not measured: %s" % exc)
    finally:
        if os.path.exists(path):
            os.remove(path)


def setup():
    os.makedirs(TOOLS_DIR, exist_ok=True)
    os.makedirs(ROOT + "/work", exist_ok=True)

    log("== environment")
    quota = "unlimited"
    try:
        q = int(open("/sys/fs/cgroup/cpu/cpu.cfs_quota_us").read())
        p = int(open("/sys/fs/cgroup/cpu/cpu.cfs_period_us").read())
        if q > 0:
            quota = "%.1f" % (q / float(p))
    except (IOError, ValueError):
        pass
    free = shutil.disk_usage(ROOT).free
    log("node=%s host_cpus=%d cpu_limit=%s scratch_free=%.0fGiB endpoint=%s" % (
        os.environ.get("NODE_NAME", "?"), os.cpu_count(), quota, free / 2.0 ** 30, ENDPOINT))
    if DISK_TEST:
        disk_speed()

    log("== tools  (connections: %s)" % (CONNECTIONS or "each tool's default"))
    if not (os.path.exists(GS3) and os.path.exists(S5)):
        proc = subprocess.run([AWS, "s3", "cp", s3url("tools/"), TOOLS_DIR, "--recursive",
                               "--endpoint-url", ENDPOINT, "--only-show-errors"],
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True)
        if proc.returncode != 0:
            log("could not fetch the tools: " + proc.stdout[-500:])
            sys.exit(1)
        for path in (GS3, S5):
            os.chmod(path, 0o755)
    tools = [make_tool(name) for name in TOOL_NAMES]
    for t in tools:
        log("%-12s %s" % (t.name, t.version()))
        log("%-12s   %s" % ("", t.settings))

    log("== source data")
    src_all, src_one = list_objects(SRC_ALL), list_objects(SRC_ONE)
    for label, prefix, objects in (("dataset", SRC_ALL, src_all), ("large object", SRC_ONE, src_one)):
        count, total = summarize(objects)
        markers = sum(1 for k in objects if k.endswith("/"))
        log("%-13s s3://%s/%s  %d objects, %.2f GiB%s" % (
            label, BUCKET, prefix, count, total / 2.0 ** 30,
            (", %d folder markers" % markers) if markers else ""))
    if not src_all or not src_one:
        log("source data is missing; aborting")
        sys.exit(1)
    return tools


# --- the six tests ---------------------------------------------------------

def run_tool(tool):
    log("== %s" % tool.name)
    work = "%s/work/%s" % (ROOT, tool.name)
    out = "out/%s/" % tool.name
    shutil.rmtree(work, ignore_errors=True)

    for label, prefix, sub, numbers in (("dataset", SRC_ALL, "all", ("1", "2", "3")),
                                        ("large", SRC_ONE, "one", ("4", "5", "6"))):
        local = "%s/%s" % (work, sub)
        os.makedirs(local)
        before = list_objects(prefix)
        want_files = sum(1 for k in before if not k.endswith("/"))
        want_bytes = sum(v[0] for v in before.values())

        # Download.
        row = measure("%s download %s" % (numbers[0], label), tool, tool.sync_down(prefix, local),
                      size_bytes=want_bytes, files=want_files)
        got_files, got_bytes = tree_stats(local)
        detail = "%d files, %.2f GiB" % (got_files, got_bytes / 2.0 ** 30)
        if (got_files, got_bytes) != (want_files, want_bytes):
            detail += "  !! EXPECTED %d files, %d bytes" % (want_files, want_bytes)
        if sub == "all":
            detail += ", sample md5 " + sample_digest(local)
        else:
            names = sorted(os.listdir(local))
            detail += ", md5 " + (file_md5(os.path.join(local, names[0]))[:12] if names else "none")
        report(row, detail)

        # Sync back to where it came from: ideally nothing to do.
        row = measure("%s sync to same path" % numbers[1], tool, tool.sync_up(local, prefix), files=want_files)
        after = list_objects(prefix)
        rewritten = changed_since(before, after)
        report(row, "%d of %d objects re-uploaded" % (rewritten, len(after)))

        # Sync to a new location: a full upload.
        dest = out + sub + "/"
        row = measure("%s sync to other path" % numbers[2], tool, tool.sync_up(local, dest),
                      size_bytes=got_bytes, files=got_files)
        uploaded = list_objects(dest)
        up_files, up_bytes = summarize(uploaded)
        detail = "%d objects, %.2f GiB" % (up_files, up_bytes / 2.0 ** 30)
        if (up_files, up_bytes) != (got_files, got_bytes):
            detail += "  !! EXPECTED %d objects, %d bytes" % (got_files, got_bytes)
        # Single-part objects have ETag = MD5 of the content, so comparing
        # them with the source checks what was uploaded.
        checked = wrong = 0
        for key, (_, etag, _) in uploaded.items():
            src = after.get(key)
            if src and "-" not in etag and "-" not in src[1]:
                checked += 1
                wrong += etag != src[1]
        detail += ", etag check %d/%d ok" % (checked - wrong, checked)
        report(row, detail)

    shutil.rmtree(work, ignore_errors=True)
    delete_prefix(out)


def main():
    tools = setup()
    try:
        for tool in tools:
            run_tool(tool)
    finally:
        log("== results (json)")
        log("RESULTS_JSON " + json.dumps(results))


if __name__ == "__main__":
    main()
