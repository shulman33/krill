// Benchmark guest B: Express + better-sqlite3.
// Same /ping semantics as guest A: real write + read per request.
const express = require("express");
const Database = require("better-sqlite3");

const db = new Database("/data.db");
db.exec("CREATE TABLE IF NOT EXISTS hits (ts TEXT DEFAULT CURRENT_TIMESTAMP)");
const ins = db.prepare("INSERT INTO hits DEFAULT VALUES");
const cnt = db.prepare("SELECT COUNT(*) AS n FROM hits");

const app = express();
app.get("/ping", (_req, res) => {
  ins.run();
  res.json({ hits: cnt.get().n });
});
app.listen(8000, "0.0.0.0");
