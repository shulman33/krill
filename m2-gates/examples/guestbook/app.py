# Guestbook — the M2 gate app. A deliberately ordinary small-software app:
# FastAPI + SQLite, no krill-specific code anywhere. The deploy pipeline, not
# the app author, makes this bootable in a microVM.
import sqlite3
import threading

from fastapi import FastAPI
from pydantic import BaseModel

VERSION = "v1"  # b2-iterate.sh bumps this and asserts the router serves it

app = FastAPI()

# sqlite3 connections are thread-bound and FastAPI sync endpoints run in a
# threadpool (learned the hard way in the M1 gates).
db = sqlite3.connect("/var/lib/guestbook.db", check_same_thread=False)
db_lock = threading.Lock()
db.execute("CREATE TABLE IF NOT EXISTS guests (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
db.commit()


class Guest(BaseModel):
    name: str


@app.get("/")
def index():
    with db_lock:
        rows = db.execute("SELECT name FROM guests ORDER BY id").fetchall()
    return {"app": "guestbook", "version": VERSION, "guests": [r[0] for r in rows]}


@app.post("/guests")
def sign(guest: Guest):
    with db_lock:
        db.execute("INSERT INTO guests (name) VALUES (?)", (guest.name,))
        db.commit()
    return {"ok": True, "name": guest.name}


@app.get("/healthz")
def healthz():
    return {"ok": True}
