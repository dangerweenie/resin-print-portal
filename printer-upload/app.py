import os, json, sqlite3, secrets, subprocess, threading
from datetime import datetime, timedelta
from functools import wraps
from flask import Flask, request, render_template, redirect, url_for, session, jsonify, send_file
from werkzeug.utils import secure_filename
from werkzeug.security import check_password_hash, generate_password_hash

import sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sliced_file_info import get_print_info, SlicedFileError

app = Flask(__name__)
app.config['MAX_CONTENT_LENGTH'] = 500 * 1024 * 1024
app.logger.setLevel(os.environ.get('LOG_LEVEL', 'INFO'))

BASE        = os.environ.get('PRINTER_UPLOAD_BASE', '/opt/printer-upload')
UPLOAD_BASE = f'{BASE}/files'
SETTINGS    = f'{BASE}/settings.json'
DB          = f'{BASE}/uploads.db'
USB_REFRESH_SCRIPT = os.environ.get('USB_REFRESH_SCRIPT', '/usr/local/bin/usb-refresh.sh')
os.makedirs(UPLOAD_BASE, exist_ok=True)

import tempfile
os.makedirs(f'{BASE}/tmp', exist_ok=True)
tempfile.tempdir = f'{BASE}/tmp'

_kf = f'{BASE}/.secret_key'
if os.path.exists(_kf):
    app.secret_key = open(_kf,'rb').read()
else:
    k = os.urandom(32)
    open(_kf,'wb').write(k)
    app.secret_key = k

_akf = f'{BASE}/.api_key'
if os.path.exists(_akf):
    API_KEY = open(_akf).read().strip()
else:
    API_KEY = secrets.token_hex(24)
    open(_akf,'w').write(API_KEY)

# First-run only: generate a random admin password rather than shipping a
# fixed one in source. Written once to .initial_admin_password and printed
# to the log — read it from either place, log in, then change it via
# Settings (and delete that file). Has no effect once settings.json exists,
# since the real password hash lives there from then on.
_apf = f'{BASE}/.initial_admin_password'
if not os.path.exists(SETTINGS):
    _initial_admin_password = secrets.token_urlsafe(9)
    open(_apf,'w').write(_initial_admin_password + '\n')
    print(f"[printer-upload] First run — generated admin password: {_initial_admin_password}\n"
          f"    (also saved to {_apf}) — log in, change it via Settings, then delete that file.")
else:
    _initial_admin_password = secrets.token_urlsafe(9)  # unused placeholder; real hash already in settings.json

DEFAULT_SAFETY_CHECKLIST = [
    "I have checked the build plate for unremoved prints",
    "I have inspected the resin vat for debris or cured resin chunks",
    "I have confirmed there is sufficient resin in the vat for this print",
    "I have confirmed the build plate is properly secured and level",
    "I have confirmed the FEP film is free of damage, tears, or major cloudiness",
]

DEFAULTS = {
    "printer_name": "Resin Printer",
    "consent_text": (
        "By uploading files to this printer, you agree to:\n"
        "• Only print files you have the right to print\n"
        "• No printing of weapons, dangerous objects, or prohibited items\n"
        "• You are responsible for your print and for removing it when complete\n"
        "• Post your print status and estimated completion time in #resin-3d-printing on Slack\n"
        "• Files are stored with your name attached\n"
        "• Makerspace rules apply — be excellent to each other"
    ),
    "file_filter_enabled": False,
    "allowed_extensions": ".ctb,.cbddlp,.photon,.pwmx,.pm3,.lgs,.cws,.slc",
    "admin_password_hash": generate_password_hash(_initial_admin_password),
    "supports_folders": False,
    "safety_checklist": DEFAULT_SAFETY_CHECKLIST,
    "slack_webhook_url": "",
}

def cfg():
    if not os.path.exists(SETTINGS): return DEFAULTS.copy()
    s = json.load(open(SETTINGS))
    for k,v in DEFAULTS.items():
        if k not in s: s[k] = v
    return s

def save_cfg(s):
    json.dump(s, open(SETTINGS,'w'), indent=2)

def init_db():
    c = sqlite3.connect(DB, timeout=10)
    c.execute('''CREATE TABLE IF NOT EXISTS uploads(
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        ts TEXT, first TEXT, last TEXT, email TEXT, folder TEXT, filename TEXT
    )''')
    c.execute('''CREATE TABLE IF NOT EXISTS members(
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        first TEXT, last TEXT, email TEXT UNIQUE,
        password_hash TEXT, must_change_password INTEGER DEFAULT 1,
        active INTEGER DEFAULT 1,
        created_at TEXT, created_by TEXT
    )''')
    c.execute('''CREATE TABLE IF NOT EXISTS print_jobs(
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        member_id INTEGER, folder TEXT, filename TEXT,
        started_at TEXT, estimated_seconds INTEGER, eta_exact INTEGER,
        estimated_complete_at TEXT, ended_at TEXT, status TEXT DEFAULT 'printing'
    )''')
    c.commit(); c.close()

def log(first, last, email, folder, fname):
    c = sqlite3.connect(DB, timeout=10)
    c.execute('INSERT INTO uploads VALUES(NULL,?,?,?,?,?,?)',
        (datetime.now().strftime('%Y-%m-%d %H:%M:%S'), first, last, email, folder, fname))
    c.commit(); c.close()

def folder_name(last, first):
    def clean(s): return ''.join(x for x in s.strip().lower() if x.isalpha() or x==' ').strip().replace(' ','-')
    return f"{clean(last)}_{clean(first)}"

def extract_thumb(src):
    PNG_SIG = bytes([0x89,0x50,0x4e,0x47,0x0d,0x0a,0x1a,0x0a])
    PNG_END = bytes([0x49,0x45,0x4e,0x44,0xae,0x42,0x60,0x82])
    # Embedded preview thumbnails live near the header (a few hundred KB at
    # most), not scattered through gigabytes of layer data — cap the scan so
    # a 100MB+ upload doesn't get fully loaded into RAM on a 512MB Pi.
    MAX_SCAN = 16 * 1024 * 1024
    try:
        with open(src,'rb') as f:
            data = f.read(MAX_SCAN)
        pngs = []; pos = 0
        while True:
            s2 = data.find(PNG_SIG, pos)
            if s2 == -1: break
            e2 = data.find(PNG_END, s2)
            if e2 == -1: break
            pngs.append(data[s2:e2+8]); pos = e2+8
        if pngs: open(src+'.thumb.png','wb').write(max(pngs,key=len))
    except Exception as e:
        app.logger.warning('thumbnail extraction failed for %s: %s', src, e)

def admin_only(f):
    @wraps(f)
    def d(*a,**k):
        if not session.get('admin'): return redirect(url_for('admin_login'))
        return f(*a,**k)
    return d

def get_member(member_id):
    c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
    m = c.execute('SELECT * FROM members WHERE id=? AND active=1', (member_id,)).fetchone()
    c.close()
    return m

def member_required(f):
    @wraps(f)
    def d(*a,**k):
        mid = session.get('member_id')
        if not mid or not get_member(mid):
            session.pop('member_id', None)
            return redirect(url_for('index'))
        if session.get('must_change_password'):
            return redirect(url_for('change_password'))
        return f(*a,**k)
    return d

def post_slack(text):
    s = cfg()
    url = s.get('slack_webhook_url','').strip()
    if not url: return
    try:
        import urllib.request
        req = urllib.request.Request(url, data=json.dumps({'text': text}).encode(),
            headers={'Content-Type':'application/json'})
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        # Slack posting is best-effort; never block the print flow on it —
        # but a broken webhook should still be visible somewhere.
        app.logger.warning('slack webhook post failed: %s', e)

def _log_usb_refresh_result(proc):
    rc = proc.wait()
    (app.logger.error if rc else app.logger.info)('usb-refresh.sh exited %s', rc)

def trigger_usb_refresh():
    """Launch usb-refresh.sh in the background — non-blocking, since the
    unmount/copy/remount cycle takes several seconds and must not stall a
    request thread. Returns (launched, thread): `launched` reflects whether
    the process started at all, not whether the refresh itself succeeded —
    that outcome is logged asynchronously once the script exits."""
    try:
        proc = subprocess.Popen([USB_REFRESH_SCRIPT])
    except OSError as e:
        app.logger.error('failed to launch usb-refresh.sh (%s): %s', USB_REFRESH_SCRIPT, e)
        return False, None
    t = threading.Thread(target=_log_usb_refresh_result, args=(proc,), daemon=True)
    t.start()
    return True, t

init_db()
if not os.path.exists(SETTINGS): save_cfg(DEFAULTS)


# ── Member auth ──────────────────────────────────────────────────────────

@app.route('/', methods=['GET','POST'])
def index():
    s = cfg(); err = None
    if session.get('member_id') and get_member(session['member_id']):
        if session.get('must_change_password'):
            return redirect(url_for('change_password'))
        m = get_member(session['member_id'])
        return redirect(url_for('folder_view', folder=folder_name(m['last'], m['first'])))
    if request.method == 'POST':
        email = request.form.get('email','').strip().lower()
        password = request.form.get('password','')
        c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
        m = c.execute('SELECT * FROM members WHERE email=? AND active=1', (email,)).fetchone()
        c.close()
        if m and check_password_hash(m['password_hash'], password):
            session.permanent = True
            session['member_id'] = m['id']
            session['must_change_password'] = bool(m['must_change_password'])
            if m['must_change_password']:
                return redirect(url_for('change_password'))
            return redirect(url_for('folder_view', folder=folder_name(m['last'], m['first'])))
        err = "Invalid email or password. Ask an admin to certify you if you don't have an account yet."
    return render_template('index.html', s=s, err=err)

@app.route('/logout')
def member_logout():
    session.pop('member_id', None); session.pop('must_change_password', None)
    return redirect(url_for('index'))

@app.route('/change-password', methods=['GET','POST'])
def change_password():
    mid = session.get('member_id')
    if not mid or not get_member(mid): return redirect(url_for('index'))
    err = None
    if request.method == 'POST':
        pw = request.form.get('new_password',''); confirm = request.form.get('confirm_password','')
        if not pw or pw != confirm:
            err = "Passwords didn't match or were empty."
        else:
            c = sqlite3.connect(DB, timeout=10)
            c.execute('UPDATE members SET password_hash=?, must_change_password=0 WHERE id=?',
                (generate_password_hash(pw), mid))
            c.commit(); c.close()
            session['must_change_password'] = False
            m = get_member(mid)
            return redirect(url_for('folder_view', folder=folder_name(m['last'], m['first'])))
    return render_template('change_password.html', s=cfg(), err=err, forced=session.get('must_change_password'))


# ── User (member-only) ───────────────────────────────────────────────────

def current_job_display(job):
    """Compute display status for a print_jobs row (auto-concludes at ETA)."""
    if job is None: return None
    now = datetime.now()
    started = datetime.strptime(job['started_at'], '%Y-%m-%d %H:%M:%S')
    d = dict(job)
    if job['ended_at']:
        d['display_status'] = 'ended'
    elif job['estimated_complete_at']:
        eta = datetime.strptime(job['estimated_complete_at'], '%Y-%m-%d %H:%M:%S')
        d['display_status'] = 'overdue' if now >= eta else 'printing'
        remaining = (eta - now).total_seconds()
        d['remaining_human'] = None
        if d['display_status'] == 'printing':
            h, rem = divmod(int(remaining), 3600); m2, _ = divmod(rem, 60)
            d['remaining_human'] = f"{h}h {m2}m" if h else f"{m2}m"
    else:
        d['display_status'] = 'printing'
    return d

def get_current_job():
    c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
    job = c.execute("SELECT * FROM print_jobs WHERE status='printing' ORDER BY id DESC LIMIT 1").fetchone()
    c.close()
    return current_job_display(job)

@app.route('/my/<folder>')
@member_required
def folder_view(folder):
    m = get_member(session['member_id'])
    own_folder = folder_name(m['last'], m['first'])
    if folder != own_folder:
        return redirect(url_for('folder_view', folder=own_folder))
    fp = os.path.join(UPLOAD_BASE, folder)
    os.makedirs(fp, exist_ok=True)
    files = []
    for f in sorted(os.listdir(fp)):
        if f.endswith('.thumb.png'): continue
        fpath = os.path.join(fp, f)
        has_thumb = os.path.exists(fpath + '.thumb.png')
        files.append({'name': f, 'size': round(os.path.getsize(fpath)/1024/1024, 2), 'thumb': has_thumb})
    s = cfg()
    return render_template('folder.html', folder=folder, files=files, s=s,
        member=m, current_job=get_current_job(), safety_checklist=s.get('safety_checklist', DEFAULT_SAFETY_CHECKLIST))

@app.route('/my/<folder>/upload', methods=['POST'])
@member_required
def folder_upload(folder):
    m = get_member(session['member_id'])
    own_folder = folder_name(m['last'], m['first'])
    if folder != own_folder: return jsonify({'error':'forbidden'}), 403
    fp = os.path.join(UPLOAD_BASE, folder)
    os.makedirs(fp, exist_ok=True)
    s = cfg(); uploaded = []
    for file in request.files.getlist('file'):
        if not file or not file.filename: continue
        if s.get('file_filter_enabled'):
            exts = [e.strip().lower() for e in s.get('allowed_extensions','').split(',')]
            if os.path.splitext(file.filename)[1].lower() not in exts: continue
        fname = secure_filename(file.filename)
        save_path = os.path.join(fp, fname)
        file.save(save_path)
        extract_thumb(save_path)
        log(m['first'], m['last'], m['email'], folder, fname)
        uploaded.append(fname)
    if request.headers.get('X-Requested-With') == 'XMLHttpRequest':
        return jsonify({'uploaded': uploaded})
    return redirect(url_for('folder_view', folder=folder))

@app.route('/my/<folder>/delete', methods=['POST'])
@member_required
def folder_delete(folder):
    m = get_member(session['member_id'])
    own_folder = folder_name(m['last'], m['first'])
    if folder != own_folder: return redirect(url_for('index'))
    fname = os.path.basename(request.form.get('filename',''))
    fp = os.path.join(UPLOAD_BASE, folder)
    for f in [fname, fname+'.thumb.png']:
        t = os.path.join(fp, f)
        if os.path.isfile(t): os.remove(t)
    return redirect(url_for('folder_view', folder=folder))

@app.route('/my/<folder>/print', methods=['POST'])
@member_required
def start_print(folder):
    m = get_member(session['member_id'])
    own_folder = folder_name(m['last'], m['first'])
    if folder != own_folder: return redirect(url_for('index'))
    fname = os.path.basename(request.form.get('filename',''))
    fp = os.path.join(UPLOAD_BASE, folder, fname)
    s = cfg()
    checklist = s.get('safety_checklist', DEFAULT_SAFETY_CHECKLIST)
    for i in range(len(checklist)):
        if not request.form.get(f'check_{i}'):
            return redirect(url_for('folder_view', folder=folder))  # incomplete checklist, no-op
    if not os.path.isfile(fp):
        return redirect(url_for('folder_view', folder=folder))

    est_seconds, exact = None, False
    try:
        info = get_print_info(fp)
        est_seconds, exact = info['estimated_seconds'], info['exact']
    except SlicedFileError as e:
        app.logger.warning('no ETA for %s: %s', fp, e)
    except Exception:
        app.logger.exception('unexpected error parsing print info for %s', fp)

    now = datetime.now()
    started_at = now.strftime('%Y-%m-%d %H:%M:%S')
    eta_at = (now + timedelta(seconds=est_seconds)).strftime('%Y-%m-%d %H:%M:%S') if est_seconds else None

    c = sqlite3.connect(DB, timeout=10)
    # Only one physical job at a time on this printer — supersede whatever was marked 'printing'.
    c.execute("UPDATE print_jobs SET status='ended', ended_at=? WHERE status='printing'", (started_at,))
    c.execute('''INSERT INTO print_jobs
        (member_id, folder, filename, started_at, estimated_seconds, eta_exact, estimated_complete_at, status)
        VALUES (?,?,?,?,?,?,?,'printing')''',
        (m['id'], folder, fname, started_at, est_seconds, int(exact), eta_at))
    c.commit(); c.close()

    trigger_usb_refresh()

    eta_txt = f" — ETA {est_seconds//3600}h{(est_seconds%3600)//60}m ({'exact' if exact else 'estimated'})" if est_seconds else " — ETA unknown"
    post_slack(f":large_green_circle: *{m['first']} {m['last']}* started printing `{fname}` on *{s['printer_name']}*{eta_txt}")

    return redirect(url_for('folder_view', folder=folder))

@app.route('/thumb/<folder>/<filename>')
def serve_thumb(folder, filename):
    folder = os.path.basename(folder)
    filename = os.path.basename(filename)
    path = os.path.join(UPLOAD_BASE, folder, filename + '.thumb.png')
    if os.path.exists(path):
        return send_file(path, mimetype='image/png')
    return '', 404


# ── Status API (for a future Slack bot / anything else to query) ─────────

@app.route('/api/status')
def api_status():
    key = request.headers.get('X-Api-Key','') or request.args.get('key','')
    if key != API_KEY:
        return jsonify({'error':'unauthorized'}), 401
    s = cfg()
    job = get_current_job()
    current = None
    if job and job['display_status'] != 'ended':
        c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
        mem = c.execute('SELECT first,last FROM members WHERE id=?', (job['member_id'],)).fetchone()
        c.close()
        current = {
            'member': f"{mem['first']} {mem['last']}" if mem else 'unknown',
            'filename': job['filename'],
            'started_at': job['started_at'],
            'status': job['display_status'],
            'estimated_complete_at': job['estimated_complete_at'],
            'remaining': job.get('remaining_human'),
            'eta_exact': bool(job['eta_exact']),
        }
    c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
    hist_rows = c.execute('''SELECT print_jobs.*, members.first, members.last FROM print_jobs
        LEFT JOIN members ON members.id = print_jobs.member_id
        ORDER BY print_jobs.id DESC LIMIT 25''').fetchall()
    c.close()
    history = [{
        'member': f"{r['first']} {r['last']}" if r['first'] else 'unknown',
        'filename': r['filename'], 'started_at': r['started_at'], 'ended_at': r['ended_at'],
        'estimated_seconds': r['estimated_seconds'], 'eta_exact': bool(r['eta_exact']),
    } for r in hist_rows]
    return jsonify({'printer_name': s['printer_name'], 'current_job': current, 'history': history})


# ── Admin ─────────────────────────────────────────────────────────────────

@app.route('/admin', methods=['GET','POST'])
def admin_login():
    if session.get('admin'): return redirect(url_for('admin_dashboard'))
    err = None
    if request.method == 'POST':
        s = cfg()
        if (request.form.get('username') == 'captain' and
                check_password_hash(s['admin_password_hash'], request.form.get('password',''))):
            session.permanent = True; session['admin'] = True
            return redirect(url_for('admin_dashboard'))
        err = "Invalid credentials."
    return render_template('admin_login.html', err=err, s=cfg())

@app.route('/admin/logout')
def admin_logout():
    session.clear(); return redirect(url_for('admin_login'))

@app.route('/admin/dashboard')
@admin_only
def admin_dashboard():
    folders = files_count = total_bytes = 0
    for d in os.listdir(UPLOAD_BASE):
        dp = os.path.join(UPLOAD_BASE, d)
        if not os.path.isdir(dp): continue
        folders += 1
        for f in os.listdir(dp):
            if f.endswith('.thumb.png'): continue
            files_count += 1
            total_bytes += os.path.getsize(os.path.join(dp, f))
    c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
    recent = c.execute('SELECT * FROM uploads ORDER BY id DESC LIMIT 10').fetchall()
    c.close()
    return render_template('admin_dashboard.html', s=cfg(),
        folders=folders, files_count=files_count,
        total_mb=round(total_bytes/1024/1024,1), recent=recent, current_job=get_current_job())

@app.route('/admin/files')
@admin_only
def admin_files():
    tree = {}
    for d in sorted(os.listdir(UPLOAD_BASE)):
        dp = os.path.join(UPLOAD_BASE, d)
        if not os.path.isdir(dp): continue
        items = []
        for f in sorted(os.listdir(dp)):
            if f.endswith('.thumb.png'): continue
            fp2 = os.path.join(dp, f)
            items.append({'name': f,
                'size': round(os.path.getsize(fp2)/1024/1024, 2),
                'modified': datetime.fromtimestamp(os.path.getmtime(fp2)).strftime('%m/%d %H:%M'),
                'thumb': os.path.exists(fp2+'.thumb.png')})
        tree[d] = items
    return render_template('admin_files.html', tree=tree, s=cfg())

@app.route('/admin/files/delete', methods=['POST'])
@admin_only
def admin_delete():
    folder = os.path.basename(request.form.get('folder',''))
    fname  = os.path.basename(request.form.get('filename',''))
    dp = os.path.join(UPLOAD_BASE, folder)
    for f in [fname, fname+'.thumb.png']:
        t = os.path.join(dp, f)
        if os.path.isfile(t): os.remove(t)
    if os.path.isdir(dp) and not [f for f in os.listdir(dp) if not f.endswith('.thumb.png')]:
        import shutil; shutil.rmtree(dp)
    return redirect(url_for('admin_files'))

@app.route('/admin/log')
@admin_only
def admin_log():
    c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
    logs = c.execute('SELECT * FROM uploads ORDER BY id DESC').fetchall()
    jobs = c.execute('''SELECT print_jobs.*, members.first, members.last FROM print_jobs
        LEFT JOIN members ON members.id = print_jobs.member_id
        ORDER BY print_jobs.id DESC''').fetchall()
    c.close()
    return render_template('admin_log.html', logs=logs, jobs=jobs, s=cfg())

@app.route('/admin/members', methods=['GET','POST'])
@admin_only
def admin_members():
    msg = err = None
    if request.method == 'POST':
        a = request.form.get('action')
        if a == 'add':
            first = request.form.get('first','').strip()
            last  = request.form.get('last','').strip()
            email = request.form.get('email','').strip().lower()
            pw    = request.form.get('password','')
            if not (first and last and email and pw):
                err = "All fields are required."
            else:
                try:
                    c = sqlite3.connect(DB, timeout=10)
                    c.execute('''INSERT INTO members (first,last,email,password_hash,must_change_password,active,created_at,created_by)
                        VALUES (?,?,?,?,1,1,?,?)''',
                        (first, last, email, generate_password_hash(pw),
                         datetime.now().strftime('%Y-%m-%d %H:%M:%S'), 'captain'))
                    c.commit(); c.close()
                    os.makedirs(os.path.join(UPLOAD_BASE, folder_name(last, first)), exist_ok=True)
                    msg = f"Certified {first} {last} — give them the password you set; they'll be asked to change it on first login."
                except sqlite3.IntegrityError:
                    err = f"A member with email {email} already exists."
        elif a == 'reset_password':
            mid = request.form.get('member_id')
            newpw = request.form.get('new_password','')
            if newpw:
                c = sqlite3.connect(DB, timeout=10)
                c.execute('UPDATE members SET password_hash=?, must_change_password=1 WHERE id=?',
                    (generate_password_hash(newpw), mid))
                c.commit(); c.close()
                msg = "Password reset — member will be asked to change it on next login."
        elif a == 'toggle_active':
            mid = request.form.get('member_id')
            c = sqlite3.connect(DB, timeout=10)
            c.execute('UPDATE members SET active = 1 - active WHERE id=?', (mid,))
            c.commit(); c.close()
            msg = "Member status updated."
    c = sqlite3.connect(DB, timeout=10); c.row_factory = sqlite3.Row
    members = c.execute('SELECT * FROM members ORDER BY last, first').fetchall()
    c.close()
    return render_template('admin_members.html', s=cfg(), members=members, msg=msg, err=err)

@app.route('/admin/settings', methods=['GET','POST'])
@admin_only
def admin_settings():
    s = cfg(); msg = None
    if request.method == 'POST':
        a = request.form.get('action')
        if a == 'consent':
            s['consent_text'] = request.form.get('consent_text','')
            save_cfg(s); msg = "Consent text saved."
        elif a == 'filter':
            s['file_filter_enabled'] = 'enabled' in request.form
            s['allowed_extensions'] = request.form.get('allowed_extensions','')
            save_cfg(s); msg = "File filter saved."
        elif a == 'general':
            s['printer_name'] = request.form.get('printer_name','')
            s['supports_folders'] = 'supports_folders' in request.form
            save_cfg(s); msg = "Settings saved."
        elif a == 'safety':
            items = [line.strip() for line in request.form.get('safety_checklist','').split('\n') if line.strip()]
            s['safety_checklist'] = items or DEFAULT_SAFETY_CHECKLIST
            save_cfg(s); msg = "Safety checklist saved."
        elif a == 'slack':
            s['slack_webhook_url'] = request.form.get('slack_webhook_url','').strip()
            save_cfg(s); msg = "Slack settings saved."
        elif a == 'password':
            pw = request.form.get('new_password','')
            if pw and pw == request.form.get('confirm_password',''):
                s['admin_password_hash'] = generate_password_hash(pw)
                save_cfg(s); msg = "Password updated."
            else: msg = "Passwords didn't match or were empty."
        elif a == 'usb':
            ok, _ = trigger_usb_refresh()
            msg = "USB refresh triggered." if ok else "Failed to launch USB refresh — check the logs."
    return render_template('admin_settings.html', s=s, msg=msg, api_key=API_KEY)

if __name__ == '__main__':
    app.run(host='0.0.0.0', port=80)
