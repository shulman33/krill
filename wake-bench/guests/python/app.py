"""Benchmark guest A: FastAPI + SQLite.

/ping does a real SQLite write + read on purpose: a 200 proves the full
stack resumed (network, runtime, database), not just the VMM.
"""
import sqlite3

from fastapi import FastAPI

app = FastAPI()
db = sqlite3.connect("/data.db")
db.execute("CREATE TABLE IF NOT EXISTS hits (ts TEXT DEFAULT CURRENT_TIMESTAMP)")


@app.get("/ping")
def ping():
    db.execute("INSERT INTO hits DEFAULT VALUES")
    db.commit()
    n = db.execute("SELECT COUNT(*) FROM hits").fetchone()[0]
    return {"hits": n}
