#!/usr/bin/env python3
"""PCDS - hand out per-student Azure credentials from the instructor machine.

Reads the student_credentials.csv written by provision_students.sh and serves a
password-protected page on 0.0.0.0:2225. Each student gets a button; clicking it
copies a paste-able shell block to the clipboard and grays the button out.

Pasting the block into a student's terminal saves the exports to ~/.bashrc
(replacing any earlier PCDS block) and loads them into the current shell.

Standard library only. Python 3.8+.
"""

import argparse
import base64
import csv
import hmac
import io
import json
import os
import re
import sys
import threading
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

USERNAME = "instructor"
PASSWORD = "alta3"
DEFAULT_PORT = 8092
MAX_UPLOAD_BYTES = 1024 * 1024

BLOCK_START = "# >>> PCDS >>>"
BLOCK_END = "# <<< PCDS <<<"
VAR_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
RG_NUMBER_RE = re.compile(r"-(\d+)$")


# --- CSV handling -------------------------------------------------------------

def parse_csv(text):
    """Parse provision_students.sh output into a list of {var: value} dicts."""
    text = text.lstrip("﻿")
    rows = [r for r in csv.reader(io.StringIO(text)) if any(c.strip() for c in r)]
    if not rows:
        raise ValueError("CSV is empty")
    headers = [h.strip() for h in rows[0]]
    bad = [h for h in headers if not VAR_NAME_RE.match(h)]
    if bad:
        raise ValueError("header has invalid variable names: " + ", ".join(repr(b) for b in bad))
    if len(rows) < 2:
        raise ValueError("CSV has a header but no student rows")
    students = []
    for line_no, row in enumerate(rows[1:], start=2):
        if len(row) != len(headers):
            raise ValueError("line %d has %d fields, expected %d" % (line_no, len(row), len(headers)))
        students.append({h: v.strip() for h, v in zip(headers, row)})
    return students


def student_number(row, index):
    """rg-student-07 -> '07'. Falls back to the 1-based row position."""
    m = RG_NUMBER_RE.search(row.get("ARM_RESOURCE_GROUP", ""))
    return m.group(1) if m else "%02d" % (index + 1)


def sh_quote(value):
    return "'" + value.replace("'", "'\\''") + "'"


def build_snippet(row, number):
    """Shell block the instructor pastes on a student machine."""
    exports = ["export STUDENT_NUMBER=%s" % sh_quote(number)]
    exports += ["export %s=%s" % (k, sh_quote(v)) for k, v in row.items() if k != "STUDENT_NUMBER"]
    block_re = "/^%s$/,/^%s$/" % (BLOCK_START, BLOCK_END)
    lines = [
        "touch ~/.bashrc",
        "sed -i '%sd' ~/.bashrc" % block_re,
        "cat >> ~/.bashrc <<'PCDS_EOF'",
        BLOCK_START,
        *exports,
        BLOCK_END,
        "PCDS_EOF",
        "eval \"$(sed -n '%sp' ~/.bashrc)\"" % block_re,
        "echo 'PCDS: student %s environment saved to ~/.bashrc and loaded.'" % number,
    ]
    return "\n".join(lines) + "\n"


# --- State --------------------------------------------------------------------

class Store:
    """CSV text plus the set of rows marked used, persisted to a JSON file."""

    def __init__(self, state_path):
        self.state_path = state_path
        self.lock = threading.Lock()
        self.csv_text = ""
        self.source = ""
        self.loaded_at = ""
        self.used = set()
        self.rows = []

    # callers hold self.lock for the methods below

    def _set_csv(self, text, source):
        rows = parse_csv(text)
        self.rows = rows
        self.csv_text = text
        self.source = source
        self.loaded_at = datetime.now(timezone.utc).isoformat(timespec="seconds")
        self.used = set()

    def _save(self):
        tmp = self.state_path + ".tmp"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump({
                "csv_text": self.csv_text,
                "source": self.source,
                "loaded_at": self.loaded_at,
                "used": sorted(self.used),
            }, f, indent=2)
        os.replace(tmp, self.state_path)

    def startup(self, csv_path):
        with self.lock:
            if os.path.exists(self.state_path):
                try:
                    with open(self.state_path) as f:
                        st = json.load(f)
                    self.rows = parse_csv(st["csv_text"])
                    self.csv_text = st["csv_text"]
                    self.source = st.get("source", "")
                    self.loaded_at = st.get("loaded_at", "")
                    self.used = {i for i in st.get("used", []) if 0 <= i < len(self.rows)}
                    log("restored %d students (%d used) from %s" % (len(self.rows), len(self.used), self.state_path))
                except (OSError, ValueError, KeyError) as e:
                    log("WARN: ignoring unreadable state file %s: %s" % (self.state_path, e))

            if os.path.exists(csv_path):
                with open(csv_path, newline="") as f:
                    text = f.read()
                if text != self.csv_text:
                    try:
                        self._set_csv(text, os.path.abspath(csv_path))
                        self._save()
                        log("loaded %d students from %s" % (len(self.rows), csv_path))
                    except ValueError as e:
                        log("WARN: could not load %s: %s" % (csv_path, e))
            elif not self.rows:
                log("no %s found - upload one from the web page" % csv_path)

    def upload(self, text):
        with self.lock:
            self._set_csv(text, "uploaded via web page")
            self._save()
            return len(self.rows)

    def mark(self, index, used):
        with self.lock:
            if not 0 <= index < len(self.rows):
                raise ValueError("no such student index")
            (self.used.add if used else self.used.discard)(index)
            self._save()

    def clear_marks(self):
        with self.lock:
            self.used = set()
            self._save()

    def snapshot(self):
        with self.lock:
            students = []
            for i, row in enumerate(self.rows):
                num = student_number(row, i)
                students.append({
                    "index": i,
                    "number": num,
                    "resource_group": row.get("ARM_RESOURCE_GROUP", ""),
                    "client_id": row.get("ARM_CLIENT_ID", ""),
                    "used": i in self.used,
                    "snippet": build_snippet(row, num),
                })
            return {"source": self.source, "loaded_at": self.loaded_at, "students": students}


# --- HTTP ---------------------------------------------------------------------

def log(msg):
    print("[%s] %s" % (datetime.now().strftime("%H:%M:%S"), msg), file=sys.stderr, flush=True)


EXPECTED_AUTH = "Basic " + base64.b64encode(("%s:%s" % (USERNAME, PASSWORD)).encode()).decode()


class Handler(BaseHTTPRequestHandler):
    store = None  # set in main()
    server_version = "PCDS/2"

    def log_message(self, fmt, *args):
        log("%s %s" % (self.address_string(), fmt % args))

    def _authorized(self):
        got = self.headers.get("Authorization", "")
        if hmac.compare_digest(got.encode(), EXPECTED_AUTH.encode()):
            return True
        self.send_response(HTTPStatus.UNAUTHORIZED)
        self.send_header("WWW-Authenticate", 'Basic realm="PCDS", charset="UTF-8"')
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(b"Authentication required.\n")
        return False

    def _send(self, status, body, ctype):
        data = body.encode() if isinstance(body, str) else body
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def _json(self, obj, status=HTTPStatus.OK):
        self._send(status, json.dumps(obj), "application/json")

    def _error(self, status, msg):
        self._json({"error": msg}, status)

    def _body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length > MAX_UPLOAD_BYTES:
            raise ValueError("request too large")
        return self.rfile.read(length).decode("utf-8")

    def do_GET(self):
        if not self._authorized():
            return
        if self.path == "/":
            self._send(HTTPStatus.OK, PAGE_HTML, "text/html; charset=utf-8")
        elif self.path == "/api/state":
            self._json(self.store.snapshot())
        else:
            self._error(HTTPStatus.NOT_FOUND, "not found")

    def do_POST(self):
        if not self._authorized():
            return
        try:
            body = self._body()
            if self.path == "/api/upload":
                n = self.store.upload(body)
                log("CSV uploaded: %d students" % n)
            elif self.path == "/api/mark":
                req = json.loads(body)
                self.store.mark(int(req["index"]), bool(req["used"]))
            elif self.path == "/api/clear-marks":
                self.store.clear_marks()
            else:
                return self._error(HTTPStatus.NOT_FOUND, "not found")
        except (ValueError, KeyError, TypeError) as e:
            return self._error(HTTPStatus.BAD_REQUEST, str(e))
        self._json(self.store.snapshot())


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--csv", default="student_credentials.csv",
                    help="CSV from provision_students.sh to load at startup (default: %(default)s)")
    ap.add_argument("--state", default="pcds_state.json",
                    help="where loaded credentials and used-marks are saved (default: %(default)s)")
    ap.add_argument("--port", type=int, default=DEFAULT_PORT, help="default: %(default)s")
    args = ap.parse_args()

    store = Store(args.state)
    store.startup(args.csv)
    Handler.store = store

    server = ThreadingHTTPServer(("0.0.0.0", args.port), Handler)
    log("PCDS listening on http://0.0.0.0:%d  (login: %s / %s)" % (args.port, USERNAME, PASSWORD))
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


PAGE_HTML = r"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PCDS</title>
<style>
:root {
  --bg: #0f1117; --panel: #1a202c; --border: #2d3748; --text: #e2e8f0;
  --muted: #718096; --accent: #3182ce; --accent-hi: #63b3ed; --ok: #68d391; --danger: #fc8181;
}
* { box-sizing: border-box; }
body { margin: 0; padding: 1.5rem; background: var(--bg); color: var(--text);
  font-family: system-ui, 'Segoe UI', sans-serif; }
main { max-width: 1100px; margin: 0 auto; }
header { display: flex; flex-wrap: wrap; gap: 1rem; align-items: center; justify-content: space-between; margin-bottom: 1rem; }
h1 { margin: 0; font-size: 1.4rem; font-weight: 600; }
h1 span { color: var(--accent-hi); }
#summary { color: var(--muted); font-size: .9rem; }
#summary strong { color: var(--accent-hi); }
.toolbar { display: flex; gap: .5rem; flex-wrap: wrap; }
button { font: inherit; cursor: pointer; border: none; border-radius: 6px; }
.btn { background: var(--border); color: var(--text); padding: .45rem .9rem; font-size: .85rem; }
.btn:hover { background: #4a5568; }
.btn-danger { background: #3b1f1f; color: var(--danger); }
.btn-danger:hover { background: #4d2626; }
#source { color: var(--muted); font-size: .78rem; margin-bottom: 1rem; }
#error { color: var(--danger); margin-bottom: 1rem; white-space: pre-wrap; }
#paste-box { display: none; background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 1rem; margin-bottom: 1rem; }
#paste-box.open { display: block; }
#paste-box textarea { width: 100%; min-height: 140px; background: var(--bg); color: var(--ok); border: 1px solid var(--border);
  border-radius: 4px; padding: .6rem; font-family: ui-monospace, monospace; font-size: .8rem; }
#paste-box .row { display: flex; gap: .5rem; margin-top: .5rem; }
#grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(190px, 1fr)); gap: .75rem; }
.card { position: relative; }
.student { width: 100%; text-align: left; background: var(--panel); border: 2px solid var(--accent);
  color: var(--text); padding: .9rem 1rem; border-radius: 10px; transition: transform .08s, background .15s; }
.student:hover { background: #1f2a3d; }
.student:active { transform: scale(.97); }
.student .num { font-size: 1.8rem; font-weight: 700; color: var(--accent-hi); line-height: 1; }
.student .rg { font-size: .9rem; margin-top: .35rem; }
.student .cid { font-family: ui-monospace, monospace; font-size: .72rem; color: var(--muted); margin-top: .2rem;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.student .tag { font-size: .72rem; margin-top: .45rem; color: var(--muted); }
.card.used .student { background: #15181f; border-color: #2a2f3a; color: #4a5568; }
.card.used .student .num { color: #4a5568; }
.card.used .student .tag { color: var(--ok); opacity: .7; }
.unmark { position: absolute; top: .4rem; right: .4rem; background: transparent; color: var(--muted);
  font-size: .72rem; padding: .2rem .4rem; display: none; }
.card.used .unmark { display: block; }
.unmark:hover { color: var(--text); }
#empty { color: var(--muted); font-style: italic; padding: 3rem 0; text-align: center; }
#toast { position: fixed; bottom: 1.5rem; left: 50%; transform: translateX(-50%); background: #22543d; color: #c6f6d5;
  padding: .6rem 1.1rem; border-radius: 6px; font-size: .9rem; opacity: 0; transition: opacity .2s; pointer-events: none; }
#toast.show { opacity: 1; }
#toast.bad { background: #742a2a; color: #fed7d7; }
</style>
</head>
<body>
<main>
  <header>
    <div>
      <h1>PCDS <span>Student Credentials</span></h1>
      <div id="summary"></div>
    </div>
    <div class="toolbar">
      <button class="btn" id="btn-upload">Upload CSV</button>
      <button class="btn" id="btn-paste">Paste CSV</button>
      <button class="btn btn-danger" id="btn-clear">Clear all marks</button>
      <input type="file" id="file" accept=".csv,text/csv" hidden>
    </div>
  </header>
  <div id="source"></div>
  <div id="error"></div>
  <div id="paste-box">
    <textarea id="paste-text" placeholder="ARM_CLIENT_ID,ARM_CLIENT_SECRET,ARM_RESOURCE_GROUP,ARM_SUBSCRIPTION_ID,ARM_TENANT_ID&#10;..."></textarea>
    <div class="row">
      <button class="btn" id="btn-paste-load">Load</button>
      <button class="btn" id="btn-paste-cancel">Cancel</button>
    </div>
  </div>
  <div id="grid"></div>
  <div id="empty" hidden>No credentials loaded. Upload the student_credentials.csv from provision_students.sh.</div>
</main>
<div id="toast"></div>
<script>
let state = { students: [] };
const $ = id => document.getElementById(id);

function toast(msg, bad) {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'show' + (bad ? ' bad' : '');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.className = ''; }, 2200);
}

// navigator.clipboard only exists on HTTPS/localhost, so fall back to
// execCommand('copy') when the page is reached over plain http://<ip>.
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
    ok ? resolve() : reject(new Error('copy blocked by browser'));
  });
}

async function api(path, body) {
  const opts = body === undefined ? {} : { method: 'POST', body: body };
  const r = await fetch(path, opts);
  const data = await r.json().catch(() => ({ error: r.statusText }));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

function render(s) {
  state = s;
  const list = s.students || [];
  const used = list.filter(x => x.used).length;
  $('summary').innerHTML = list.length
    ? '<strong>' + (list.length - used) + '</strong> of ' + list.length + ' remaining'
    : '';
  $('source').textContent = s.source ? 'Loaded from ' + s.source + (s.loaded_at ? ' at ' + new Date(s.loaded_at).toLocaleString() : '') : '';
  $('empty').hidden = list.length > 0;

  const grid = $('grid');
  grid.textContent = '';
  for (const st of list) {
    const card = document.createElement('div');
    card.className = 'card' + (st.used ? ' used' : '');

    const b = document.createElement('button');
    b.className = 'student';
    b.title = 'Copy setup commands for student ' + st.number;
    const add = (cls, text) => { const d = document.createElement('div'); d.className = cls; d.textContent = text; b.appendChild(d); };
    add('num', st.number);
    add('rg', st.resource_group || '(no resource group)');
    add('cid', st.client_id);
    add('tag', st.used ? '✓ used — click to copy again' : 'click to copy');
    b.onclick = () => onPick(st);

    const un = document.createElement('button');
    un.className = 'unmark';
    un.textContent = 'unmark';
    un.title = 'Mark as not used';
    un.onclick = () => setMark(st.index, false);

    card.appendChild(b);
    card.appendChild(un);
    grid.appendChild(card);
  }
}

function onPick(st) {
  // copy synchronously inside the click so the browser allows it
  copyText(st.snippet).then(
    () => toast('Copied student ' + st.number + ' — paste it into that student’s terminal'),
    e => toast('Copy failed: ' + e.message, true)
  );
  if (!st.used) setMark(st.index, true);
}

async function setMark(index, used) {
  try { render(await api('/api/mark', JSON.stringify({ index, used }))); $('error').textContent = ''; }
  catch (e) { $('error').textContent = e.message; }
}

async function upload(text) {
  const nUsed = (state.students || []).filter(x => x.used).length;
  if (nUsed && !confirm('Loading a new CSV clears all ' + nUsed + ' used mark(s). Continue?')) return false;
  try {
    render(await api('/api/upload', text));
    $('error').textContent = '';
    toast('Loaded ' + state.students.length + ' students');
    return true;
  } catch (e) {
    $('error').textContent = 'CSV not loaded: ' + e.message;
    return false;
  }
}

$('btn-upload').onclick = () => $('file').click();
$('file').onchange = async e => {
  const f = e.target.files[0];
  e.target.value = '';
  if (f) await upload(await f.text());
};
$('btn-paste').onclick = () => { $('paste-box').classList.add('open'); $('paste-text').focus(); };
$('btn-paste-cancel').onclick = () => $('paste-box').classList.remove('open');
$('btn-paste-load').onclick = async () => {
  if (await upload($('paste-text').value)) { $('paste-text').value = ''; $('paste-box').classList.remove('open'); }
};
$('btn-clear').onclick = async () => {
  if (!confirm('Un-gray every student?')) return;
  try { render(await api('/api/clear-marks', '')); } catch (e) { $('error').textContent = e.message; }
};

async function refresh() {
  try { render(await api('/api/state')); } catch (e) { /* keep last view; retry next tick */ }
}
refresh();
setInterval(refresh, 4000);  // keeps several open tabs in sync
</script>
</body>
</html>
"""

if __name__ == "__main__":
    main()
