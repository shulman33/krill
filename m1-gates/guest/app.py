"""M1 gate guest: SQLite writer with acked, verifiable writes.

The write pattern is designed so lost or corrupted writes are *provable*
after any number of sleep/wake cycles (gate A3):

- every /write inserts seq = MAX(seq)+1 and only answers after COMMIT —
  the HTTP 200 is the durability ack;
- /verify checks PRAGMA integrity_check AND that seq 1..N has no gaps:
  count == max_seq means nothing acked ever vanished.
"""

import re
import sqlite3
import threading

from fastapi import FastAPI

app = FastAPI()
# FastAPI runs sync endpoints in a threadpool; sqlite3 connections are
# thread-bound by default. One shared connection + one lock keeps the write
# ledger strictly serialized (which is exactly what the gate wants anyway).
db = sqlite3.connect("/data.db", check_same_thread=False)
db_lock = threading.Lock()
db.execute("PRAGMA journal_mode=WAL")
db.execute("PRAGMA synchronous=FULL")
db.execute("CREATE TABLE IF NOT EXISTS writes (seq INTEGER PRIMARY KEY)")
db.commit()


def _count() -> int:
    return db.execute("SELECT COUNT(*) FROM writes").fetchone()[0]


@app.get("/ping")
def ping():
    # Read-only on purpose: A3's ledger counts only /write acks.
    with db_lock:
        return {"count": _count()}


@app.post("/write")
def write():
    with db_lock:
        db.execute(
            "INSERT INTO writes (seq) VALUES ((SELECT COALESCE(MAX(seq), 0) + 1 FROM writes))"
        )
        db.commit()  # the ack: the response below exists only after durability
        return {"count": _count()}


@app.get("/verify")
def verify():
    with db_lock:
        ok = db.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
        n, mx = db.execute(
            "SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM writes"
        ).fetchone()
    return {"integrity_ok": ok, "count": n, "max_seq": mx, "gapless": n == mx}


@app.get("/whoami")
def whoami():
    # The guest's own kernel-assigned IP: per-app deterministic
    # (172.16.<subnet>.2), so the router's identity claims are checkable
    # from outside (gate A4).
    with open("/proc/cmdline") as f:
        m = re.search(r"krill_ip=(\S+)", f.read())
    return {"ip": m.group(1) if m else "unknown"}
