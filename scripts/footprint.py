#!/usr/bin/env python3
"""What a registry costs to start and to keep around, measured as a process.

Not a load test, and nothing about pull or push throughput: those depend on
the database and the storage behind a deployment far more than on the
registry. This is what the process itself costs.

  startup   exec to the first `GET /v2/` answered 200, polled every 2 ms;
            cold on an empty data directory, warm on the one a cold run
            left; one start is thrown away first, so no measured one is
            reading the binary from disk
  memory    /proc/<pid>/status right after ready, and after a light touch
            (one small image pushed, pulled and listed 20 times) followed by
            an idle period; RssAnon is what the process allocated, RssFile
            mostly its own binary, mapped and reclaimable
  idle cpu  user+system CPU over the idle periods, as a % of one core

    footprint.py [--baseline NAME] [--json PATH] [--markdown PATH] NAME=KIND:BINARY...

KIND is `cr`, `zot` or `distribution`. With --baseline every other server is
also given as a change from that one. RUNS (default 20) is how many cold and
warm starts, IDLE (default 60) how many seconds each idle period lasts, and
PORT (default 5077) where the servers listen, one at a time. Linux only.
"""
import argparse
import hashlib
import http.client
import json
import math
import os
import shutil
import statistics
import subprocess
import sys
import tempfile
import time

PORT = int(os.environ.get("PORT", "5077"))
RUNS = int(os.environ.get("RUNS", "20"))
IDLE = int(os.environ.get("IDLE", "60"))


def cr(binary, work):
    cfg = os.path.join(work, "cr.yaml")
    with open(cfg, "w") as f:
        f.write(f"""
db:
  driver: sqlite3
  dsn: "file:{work}/cr.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
  migrate: true
server:
  addr: "127.0.0.1:0"
  http:
    addr: "127.0.0.1:{PORT}"
watch:
  broker: memory
registry:
  storage:
    driver: os
    os:
      root: "{work}/data"
""")
    return [binary, "--config", cfg, "serve"]


def zot(binary, work):
    cfg = os.path.join(work, "zot.json")
    with open(cfg, "w") as f:
        json.dump({
            "distSpecVersion": "1.1.1",
            "storage": {"rootDirectory": os.path.join(work, "data")},
            "http": {"address": "127.0.0.1", "port": str(PORT)},
            "log": {"level": "info"},
        }, f)
    return [binary, "serve", cfg]


def distribution(binary, work):
    cfg = os.path.join(work, "config.yml")
    with open(cfg, "w") as f:
        f.write(f"""
version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: {work}/data
http:
  addr: 127.0.0.1:{PORT}
""")
    return [binary, "serve", cfg]


KINDS = {"cr": cr, "zot": zot, "distribution": distribution}


def ready():
    try:
        c = http.client.HTTPConnection("127.0.0.1", PORT, timeout=1)
        c.request("GET", "/v2/")
        ok = c.getresponse().status == 200
        c.close()
        return ok
    except OSError:
        return False


def start(argv, work):
    log = open(os.path.join(work, "server.log"), "ab")
    t0 = time.monotonic()
    p = subprocess.Popen(argv, stdout=log, stderr=log, cwd=work)
    while not ready():
        if p.poll() is not None:
            with open(os.path.join(work, "server.log")) as f:
                sys.stderr.write(f.read())
            raise RuntimeError(f"{argv[0]} exited before it was ready")
        if time.monotonic() - t0 > 30:
            raise RuntimeError(f"{argv[0]} not ready in 30s")
        time.sleep(0.002)
    return p, time.monotonic() - t0


def stop(p):
    p.terminate()
    try:
        p.wait(10)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait()


def status(pid):
    out = {}
    with open(f"/proc/{pid}/status") as f:
        for line in f:
            k, _, v = line.partition(":")
            if k in ("VmRSS", "VmHWM", "RssAnon", "RssFile", "Threads"):
                out[k] = int(v.split()[0])
    return out


def cpu(pid):
    with open(f"/proc/{pid}/stat") as f:
        fields = f.read().rsplit(")", 1)[1].split()
    return (int(fields[11]) + int(fields[12])) / os.sysconf("SC_CLK_TCK")


def req(method, path, body=None, headers=None):
    c = http.client.HTTPConnection("127.0.0.1", PORT, timeout=10)
    c.request(method, path, body=body, headers=headers or {})
    r = c.getresponse()
    data = r.read()
    c.close()
    return r, data


def push_blob(repo, b, mt):
    d = "sha256:" + hashlib.sha256(b).hexdigest()
    r, _ = req("POST", f"/v2/{repo}/blobs/uploads/")
    assert r.status == 202, f"upload start: {r.status}"
    loc = r.getheader("Location")
    if loc.startswith("http"):
        loc = "/" + loc.split("/", 3)[3]
    sep = "&" if "?" in loc else "?"
    r, _ = req("PUT", f"{loc}{sep}digest={d}", b, {"Content-Type": "application/octet-stream"})
    assert r.status == 201, f"upload end: {r.status}"
    return {"mediaType": mt, "digest": d, "size": len(b)}


def touch():
    """One small image pushed, then pulled and its tags listed 20 times."""
    repo = "footprint/app"
    config = push_blob(repo, b'{"architecture":"amd64","os":"linux"}', "application/vnd.oci.image.config.v1+json")
    layer = push_blob(repo, os.urandom(1 << 20), "application/vnd.oci.image.layer.v1.tar")
    mt = "application/vnd.oci.image.manifest.v1+json"
    m = json.dumps({"schemaVersion": 2, "mediaType": mt, "config": config, "layers": [layer]}).encode()
    r, _ = req("PUT", f"/v2/{repo}/manifests/latest", m, {"Content-Type": mt})
    assert r.status == 201, f"manifest put: {r.status}"
    for _ in range(20):
        for path, headers in (
            (f"/v2/{repo}/manifests/latest", {"Accept": mt}),
            (f"/v2/{repo}/blobs/{layer['digest']}", {}),
            (f"/v2/{repo}/tags/list", {}),
        ):
            r, _ = req("GET", path, headers=headers)
            assert r.status == 200, f"GET {path}: {r.status}"


def idle(pid, seconds):
    c0 = cpu(pid)
    time.sleep(seconds)
    return (cpu(pid) - c0) / seconds * 100


def measure(kind, binary):
    build = KINDS[kind]

    def fresh():
        return tempfile.mkdtemp(prefix="footprint-")

    work = fresh()
    try:
        p, _ = start(build(binary, work), work)
        stop(p)
    finally:
        shutil.rmtree(work)

    cold, warm = [], []
    for _ in range(RUNS):
        work = fresh()
        try:
            p, t = start(build(binary, work), work)
            cold.append(t)
            touch()
            stop(p)
            p, t = start(build(binary, work), work)
            warm.append(t)
            stop(p)
        finally:
            shutil.rmtree(work)

    work = fresh()
    try:
        p, _ = start(build(binary, work), work)
        time.sleep(0.2)
        ready_status = status(p.pid)
        cpu_before = idle(p.pid, IDLE)
        touch()
        cpu_after = idle(p.pid, IDLE)
        used = status(p.pid)
        stop(p)
    finally:
        shutil.rmtree(work)

    return {
        "kind": kind,
        "binary_bytes": os.path.getsize(binary),
        "startup_cold_s": cold,
        "startup_warm_s": warm,
        "ready": ready_status,
        "used": used,
        "idle_cpu_pct": (cpu_before + cpu_after) / 2,
    }


def median_ms(xs):
    return statistics.median(xs) * 1000


def p90_ms(xs):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, math.ceil(len(xs) * 0.9) - 1)] * 1000


# The rows of the table: a label, how to read the value, how to print it.
ROWS = [
    ("binary", lambda r: r["binary_bytes"] / 1e6, "{:.1f} MB"),
    ("startup, cold (median)", lambda r: median_ms(r["startup_cold_s"]), "{:.1f} ms"),
    ("startup, cold (p90)", lambda r: p90_ms(r["startup_cold_s"]), "{:.1f} ms"),
    ("startup, warm (median)", lambda r: median_ms(r["startup_warm_s"]), "{:.1f} ms"),
    ("startup, warm (p90)", lambda r: p90_ms(r["startup_warm_s"]), "{:.1f} ms"),
    ("anon, ready", lambda r: r["ready"]["RssAnon"] / 1024, "{:.1f} MiB"),
    ("anon, used and idle", lambda r: r["used"]["RssAnon"] / 1024, "{:.1f} MiB"),
    ("RSS, used and idle", lambda r: r["used"]["VmRSS"] / 1024, "{:.1f} MiB"),
    ("RSS, peak", lambda r: r["used"]["VmHWM"] / 1024, "{:.1f} MiB"),
    ("threads", lambda r: r["used"]["Threads"], "{}"),
    ("idle CPU", lambda r: r["idle_cpu_pct"], "{:.3f}%"),
]


def markdown(results, baseline):
    names = list(results)
    out = ["| | " + " | ".join(f"`{n}`" for n in names) + " |",
           "| --- |" + " --- |" * len(names)]
    for label, value, fmt in ROWS:
        cells = []
        for n in names:
            v = value(results[n])
            cell = fmt.format(v)
            if baseline and n != baseline:
                b = value(results[baseline])
                if b:
                    cell += f" ({(v - b) / b * 100:+.0f}%)"
            cells.append(cell)
        out.append(f"| {label} | " + " | ".join(cells) + " |")
    out.append("")
    out.append(f"{RUNS} cold and {RUNS} warm starts each; idle periods of {IDLE} s. "
               "*anon* is what the process allocated; the rest of RSS is mostly its own binary, mapped. "
               "\"used\" is after one 1 MiB image was pushed, pulled and listed 20 times.")
    return "\n".join(out) + "\n"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--baseline")
    ap.add_argument("--json")
    ap.add_argument("--markdown")
    ap.add_argument("servers", nargs="+", metavar="NAME=KIND:BINARY")
    args = ap.parse_args()

    servers = {}
    for s in args.servers:
        name, _, rest = s.partition("=")
        kind, _, binary = rest.partition(":")
        if kind not in KINDS or not binary:
            ap.error(f"{s}: want NAME=KIND:BINARY with KIND one of {', '.join(KINDS)}")
        servers[name] = (kind, os.path.abspath(binary))
    if args.baseline and args.baseline not in servers:
        ap.error(f"--baseline {args.baseline}: not one of the servers")

    results = {}
    for name, (kind, binary) in servers.items():
        print(f"measuring {name}", file=sys.stderr, flush=True)
        results[name] = measure(kind, binary)

    md = markdown(results, args.baseline)
    sys.stdout.write(md)
    if args.markdown:
        with open(args.markdown, "w") as f:
            f.write(md)
    if args.json:
        with open(args.json, "w") as f:
            json.dump({"runs": RUNS, "idle_s": IDLE, "servers": results}, f, indent=1)


if __name__ == "__main__":
    main()
