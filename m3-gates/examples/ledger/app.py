# ledger — the M3 gate guest: a plain Python app whose durable state is a
# SQLite database at /data/app.db (the data-disk contract). Nothing here
# knows about krill: WAL mode + synchronous=FULL is just what a durability-
# minded app would run anyway, and it is what makes every commit visible to
# the host-side tailer the moment it is acked.
#
#   POST /add?k=<int>&v=<str>  insert-or-replace one row
#   GET  /                     {"count", "sum", "digest"} over ordered rows
import hashlib
import http.server
import json
import sqlite3
from urllib.parse import parse_qs, urlparse

DB = "/data/app.db"


def conn():
    c = sqlite3.connect(DB, timeout=10)
    c.execute("PRAGMA journal_mode=WAL")
    c.execute("PRAGMA synchronous=FULL")
    return c


def init():
    c = conn()
    c.execute("CREATE TABLE IF NOT EXISTS ledger (k INTEGER PRIMARY KEY, v TEXT NOT NULL)")
    c.commit()
    c.close()


class H(http.server.BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        c = conn()
        rows = c.execute("SELECT k, v FROM ledger ORDER BY k").fetchall()
        c.close()
        h = hashlib.sha256()
        total = 0
        for k, v in rows:
            total += k
            h.update(f"{k}={v};".encode())
        self._send(200, {"app": "ledger", "count": len(rows), "sum": total,
                         "digest": h.hexdigest()})

    def do_POST(self):
        q = parse_qs(urlparse(self.path).query)
        try:
            k = int(q["k"][0])
            v = q.get("v", ["-"])[0]
        except (KeyError, ValueError):
            self._send(400, {"error": "need ?k=<int>&v=<str>"})
            return
        c = conn()
        c.execute("INSERT OR REPLACE INTO ledger (k, v) VALUES (?, ?)", (k, v))
        c.commit()  # acked to the client only after this commit is durable
        c.close()
        self._send(200, {"ok": True, "k": k})

    def log_message(self, *a):  # keep the serial log for errors only
        pass


init()
http.server.ThreadingHTTPServer(("0.0.0.0", 8000), H).serve_forever()
