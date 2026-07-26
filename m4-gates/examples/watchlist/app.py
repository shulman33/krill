# watchlist — F4's demo app: a shared list of things to watch and places to
# eat, at watchlist.krill.run.
#
# Chosen (ROADMAP decision #10a) because every item carries "added by whoever,
# when", which puts X-App-User on screen where a non-technical person can look
# at it and say yes, that's me. F1 demonstrated rather than asserted.
#
# Two contracts it deliberately honors, because the platform's own examples
# should be the ones that get them right:
#
#   /data/app.db, WAL + synchronous=FULL — the M3 data-disk contract. This is
#   why `guestbook` was not used: it writes /var/lib, outside /data, so its
#   head LSN is permanently 0 and nothing it stores is durable in the sense
#   the platform means.
#
#   X-Krill-Token is VERIFIED, not trusted. The doorman signs a short-lived
#   ed25519 token naming the caller and this one app; krilld hands the public
#   key to the guest on its kernel command line. An app that reads X-App-User
#   without checking the token is trusting a header, and the whole point of
#   the front door is that it does not have to.
import html
import http.server
import json
import os
import sqlite3
import time
from urllib.parse import parse_qs, urlparse

import krill_identity

DB = "/data/app.db"
APP_NAME = os.environ.get("KRILL_APP", "watchlist")
KINDS = ("movie", "show", "restaurant", "other")


def conn():
    c = sqlite3.connect(DB, timeout=10)
    c.execute("PRAGMA journal_mode=WAL")
    # FULL, not NORMAL: the host-side tailer sees a commit when the guest
    # fsyncs, so NORMAL would let an acked write be invisible to durability
    # for a while — the same commits SQLite itself would lose on power loss.
    c.execute("PRAGMA synchronous=FULL")
    return c


def init():
    c = conn()
    c.execute(
        """CREATE TABLE IF NOT EXISTS items (
             id        INTEGER PRIMARY KEY,
             title     TEXT NOT NULL,
             kind      TEXT NOT NULL DEFAULT 'other',
             note      TEXT NOT NULL DEFAULT '',
             added_by  TEXT NOT NULL,
             added_at  INTEGER NOT NULL,
             done_by   TEXT NOT NULL DEFAULT '',
             done_at   INTEGER NOT NULL DEFAULT 0
           )"""
    )
    c.commit()
    c.close()


class Caller:
    """Who is asking, and how sure we are."""

    def __init__(self, email, name, verified, why=""):
        self.email = email
        self.name = name or email
        self.verified = verified
        self.why = why

    @property
    def label(self):
        return self.name or self.email or "someone"


def caller(headers) -> Caller:
    token = headers.get("X-Krill-Token", "")
    header_user = headers.get("X-App-User", "")
    try:
        claims = krill_identity.verify(token, APP_NAME)
    except krill_identity.Unverified as e:
        # Unverified callers may READ, and may not write. Refusing everything
        # would make the app unusable behind a doorman that is misconfigured;
        # accepting writes would make the byline a lie.
        return Caller(header_user, header_user, False, str(e))
    return Caller(claims.get("email", ""), claims.get("name", ""), True)


def rows():
    c = conn()
    out = c.execute(
        "SELECT id, title, kind, note, added_by, added_at, done_by, done_at"
        " FROM items ORDER BY done_at != 0, added_at DESC"
    ).fetchall()
    c.close()
    return out


def ago(ts):
    if not ts:
        return ""
    d = max(0, int(time.time()) - int(ts))
    for n, unit in ((86400, "d"), (3600, "h"), (60, "m")):
        if d >= n:
            return f"{d // n}{unit} ago"
    return "just now"


def page(who: Caller) -> bytes:
    items = rows()
    todo = [r for r in items if not r[7]]
    done = [r for r in items if r[7]]

    def card(r):
        rid, title, kind, note, added_by, added_at, done_by, done_at = r
        cls = "item done" if done_at else "item"
        marks = f'<span class="kind {html.escape(kind)}">{html.escape(kind)}</span>'
        byline = f"added by <b>{html.escape(added_by)}</b> · {ago(added_at)}"
        if done_at:
            byline += f" · seen by <b>{html.escape(done_by)}</b> {ago(done_at)}"
        note_html = f'<div class="note">{html.escape(note)}</div>' if note else ""
        action = "un-mark" if done_at else "mark seen"
        return f"""
        <li class="{cls}">
          <div class="row">
            <div class="grow">
              <div class="title">{html.escape(title)} {marks}</div>
              {note_html}
              <div class="byline">{byline}</div>
            </div>
            <form method="post" action="/toggle">
              <input type="hidden" name="id" value="{rid}">
              <button class="ghost">{action}</button>
            </form>
          </div>
        </li>"""

    warn = ""
    if not who.verified:
        warn = (
            '<div class="warn">Krill could not verify who you are, so this list is '
            "read-only right now.<br><small>" + html.escape(who.why) + "</small></div>"
        )
    form = "" if not who.verified else f"""
      <form class="add" method="post" action="/add">
        <input name="title" placeholder="Add a movie, show or restaurant…" required autocomplete="off">
        <div class="row2">
          <select name="kind">{"".join(f'<option value="{k}">{k}</option>' for k in KINDS)}</select>
          <input name="note" placeholder="why? (optional)" autocomplete="off">
          <button>Add</button>
        </div>
      </form>"""

    body = f"""<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>Watchlist</title>
<style>
  :root {{ color-scheme: light dark;
    --bg:#fbfbfa; --card:#fff; --fg:#1c1c1a; --mut:#6f6f69; --line:#e6e6e1;
    --accent:#2f6f5e; --warn:#8a5a00; --warnbg:#fff6e0; }}
  @media (prefers-color-scheme: dark) {{ :root {{
    --bg:#141417; --card:#1c1c20; --fg:#eaeae8; --mut:#9c9c97; --line:#2b2b31;
    --accent:#6fd6b4; --warn:#e0b061; --warnbg:#2e2410; }} }}
  * {{ box-sizing:border-box }}
  body {{ margin:0; background:var(--bg); color:var(--fg); padding:1rem 1rem 4rem;
    font:16px/1.5 ui-sans-serif,-apple-system,"Segoe UI",Roboto,sans-serif; }}
  header {{ max-width:38rem; margin:0 auto 1rem; display:flex; align-items:baseline; gap:.6rem }}
  h1 {{ font-size:1.3rem; margin:0; letter-spacing:-.02em }}
  .me {{ margin-left:auto; color:var(--mut); font-size:.82rem }}
  main {{ max-width:38rem; margin:0 auto }}
  form.add {{ background:var(--card); border:1px solid var(--line); border-radius:14px;
    padding:.75rem; margin-bottom:1rem }}
  input, select, button {{ font:inherit; color:inherit }}
  input, select {{ background:transparent; border:1px solid var(--line); border-radius:9px;
    padding:.55rem .6rem; width:100% }}
  .row2 {{ display:flex; gap:.5rem; margin-top:.5rem }}
  .row2 select {{ flex:0 0 8.5rem }}
  button {{ background:var(--accent); color:#fff; border:0; border-radius:9px;
    padding:.55rem 1rem; cursor:pointer; white-space:nowrap }}
  @media (prefers-color-scheme: dark) {{ button {{ color:#0d1a16 }} }}
  button.ghost {{ background:transparent; color:var(--mut); border:1px solid var(--line) }}
  ul {{ list-style:none; padding:0; margin:0 }}
  .item {{ background:var(--card); border:1px solid var(--line); border-radius:14px;
    padding:.7rem .8rem; margin-bottom:.55rem }}
  .item.done .title {{ text-decoration:line-through; opacity:.6 }}
  .row {{ display:flex; gap:.6rem; align-items:flex-start }}
  .grow {{ flex:1; min-width:0 }}
  .title {{ font-weight:600 }}
  .kind {{ font-size:.68rem; text-transform:uppercase; letter-spacing:.06em;
    color:var(--mut); border:1px solid var(--line); border-radius:99px;
    padding:.05rem .45rem; margin-left:.35rem; vertical-align:.1em }}
  .note {{ color:var(--mut); font-size:.9rem; margin-top:.15rem }}
  .byline {{ color:var(--mut); font-size:.78rem; margin-top:.3rem }}
  .sec {{ color:var(--mut); font-size:.78rem; text-transform:uppercase;
    letter-spacing:.08em; margin:1.2rem 0 .5rem }}
  .warn {{ background:var(--warnbg); color:var(--warn); border-radius:12px;
    padding:.7rem .8rem; margin-bottom:1rem; font-size:.9rem }}
  .empty {{ color:var(--mut); text-align:center; padding:2rem 0 }}
</style>
<header>
  <h1>Watchlist</h1>
  <span class="me">{html.escape(who.label)}{"" if who.verified else " (unverified)"}</span>
</header>
<main>
  {warn}
  {form}
  <ul>{"".join(card(r) for r in todo)}</ul>
  {'<div class="empty">Nothing on the list yet. Add the first thing.</div>' if not todo else ""}
  {'<div class="sec">already seen</div><ul>' + "".join(card(r) for r in done) + "</ul>" if done else ""}
</main>
"""
    return body.encode()


class H(http.server.BaseHTTPRequestHandler):
    server_version = "watchlist"

    def _redirect(self):
        self.send_response(303)
        self.send_header("Location", "/")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _json(self, code, obj):
        raw = json.dumps(obj, indent=2).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):  # noqa: N802
        who = caller(self.headers)
        path = urlparse(self.path).path
        if path == "/whoami":
            # The gate scripts' handle on F1: what the app itself believes,
            # and whether it proved it.
            return self._json(200, {
                "app": APP_NAME, "email": who.email, "name": who.name,
                "verified": who.verified, "why": who.why,
                "header_user": self.headers.get("X-App-User", ""),
                "plane": self.headers.get("X-App-Plane", ""),
            })
        if path == "/api/items":
            return self._json(200, [
                {"id": r[0], "title": r[1], "kind": r[2], "note": r[3],
                 "added_by": r[4], "added_at": r[5], "done_by": r[6], "done_at": r[7]}
                for r in rows()
            ])
        body = page(who)
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):  # noqa: N802
        who = caller(self.headers)
        if not who.verified:
            # No verified identity, no byline, no write. The list's whole
            # value is that "added by" is true.
            return self._json(403, {"error": "unverified caller", "why": who.why})
        length = int(self.headers.get("Content-Length") or 0)
        form = parse_qs(self.rfile.read(length).decode("utf-8", "replace"))
        path = urlparse(self.path).path
        c = conn()
        try:
            if path == "/add":
                title = (form.get("title", [""])[0] or "").strip()[:200]
                if not title:
                    return self._json(400, {"error": "a title is required"})
                kind = form.get("kind", ["other"])[0]
                if kind not in KINDS:
                    kind = "other"
                note = (form.get("note", [""])[0] or "").strip()[:400]
                c.execute(
                    "INSERT INTO items (title, kind, note, added_by, added_at)"
                    " VALUES (?,?,?,?,?)",
                    (title, kind, note, who.email, int(time.time())),
                )
            elif path == "/toggle":
                rid = int(form.get("id", ["0"])[0] or 0)
                row = c.execute("SELECT done_at FROM items WHERE id=?", (rid,)).fetchone()
                if not row:
                    return self._json(404, {"error": "no such item"})
                if row[0]:
                    c.execute("UPDATE items SET done_by='', done_at=0 WHERE id=?", (rid,))
                else:
                    c.execute("UPDATE items SET done_by=?, done_at=? WHERE id=?",
                              (who.email, int(time.time()), rid))
            else:
                return self._json(404, {"error": "no such endpoint"})
            c.commit()
        finally:
            c.close()
        self._redirect()

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    init()
    http.server.ThreadingHTTPServer(("0.0.0.0", 8000), H).serve_forever()
