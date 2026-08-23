import io
import os
import sqlite3
import stat

import sliced_files as sf


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def _login_member(client, email, password):
    return client.post('/', data={'email': email, 'password': password}, follow_redirects=False)


def _admin_login(client, app_module):
    pw = open(f'{app_module.BASE}/.initial_admin_password').read().strip()
    return client.post('/admin', data={'username': 'captain', 'password': pw}, follow_redirects=False)


def _checklist_data(app_module, checked=True):
    n = len(app_module.DEFAULT_SAFETY_CHECKLIST)
    return {f'check_{i}': 'on' for i in range(n)} if checked else {}


def _exit0_script(tmp_path):
    p = tmp_path / 'refresh-ok.sh'
    p.write_text('#!/bin/sh\nexit 0\n')
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    return str(p)


# ---------------------------------------------------------------------------
# Member auth
# ---------------------------------------------------------------------------

def test_member_login_success_redirects_to_own_folder(client, make_member, app_module):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    resp = _login_member(client, 'ed@example.com', 'pw123')
    assert resp.status_code == 302
    assert '/my/diamond_ed' in resp.headers['Location']


def test_member_login_wrong_password(client, make_member):
    make_member(email='ed@example.com', password='pw123')
    resp = _login_member(client, 'ed@example.com', 'wrong')
    assert resp.status_code == 200
    assert b'Invalid email or password' in resp.data


def test_inactive_member_cannot_login(client, make_member):
    make_member(email='ed@example.com', password='pw123', active=0)
    resp = _login_member(client, 'ed@example.com', 'pw123')
    assert b'Invalid email or password' in resp.data


def test_forced_password_change_redirect(client, make_member):
    make_member(email='ed@example.com', password='temp123', must_change_password=1)
    resp = _login_member(client, 'ed@example.com', 'temp123')
    assert resp.status_code == 302
    assert '/change-password' in resp.headers['Location']


def test_change_password_mismatch(client, make_member):
    make_member(email='ed@example.com', password='temp123', must_change_password=1)
    _login_member(client, 'ed@example.com', 'temp123')
    resp = client.post('/change-password', data={'new_password': 'a', 'confirm_password': 'b'})
    assert b"didn&#39;t match" in resp.data or b"didn't match" in resp.data


def test_change_password_success_clears_flag(client, make_member, app_module):
    make_member(email='ed@example.com', password='temp123', must_change_password=1)
    _login_member(client, 'ed@example.com', 'temp123')
    resp = client.post('/change-password', data={'new_password': 'newpw1', 'confirm_password': 'newpw1'})
    assert resp.status_code == 302
    c = sqlite3.connect(app_module.DB)
    row = c.execute("SELECT must_change_password FROM members WHERE email='ed@example.com'").fetchone()
    c.close()
    assert row[0] == 0


def test_logout_clears_session(client, make_member):
    make_member(email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    resp = client.get('/logout', follow_redirects=False)
    assert resp.status_code == 302
    resp2 = client.get('/my/user_test', follow_redirects=False)
    assert resp2.status_code == 302
    assert resp2.headers['Location'].endswith('/') or resp2.headers['Location'] == '/'


# ---------------------------------------------------------------------------
# Upload / folder view / delete
# ---------------------------------------------------------------------------

def test_folder_view_redirects_to_own_folder(client, make_member):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    resp = client.get('/my/someone-elses-folder', follow_redirects=False)
    assert resp.status_code == 302
    assert '/my/diamond_ed' in resp.headers['Location']


def test_upload_and_thumb_extraction(client, make_member):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    png = (bytes([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]) + b'x' +
           bytes([0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82]))
    data = {'file': (io.BytesIO(b'header' + png), 'model.ctb')}
    resp = client.post('/my/diamond_ed/upload', data=data,
                        content_type='multipart/form-data',
                        headers={'X-Requested-With': 'XMLHttpRequest'})
    assert resp.status_code == 200
    assert resp.get_json()['uploaded'] == ['model.ctb']
    resp2 = client.get('/thumb/diamond_ed/model.ctb')
    assert resp2.status_code == 200
    assert resp2.mimetype == 'image/png'


def test_upload_respects_extension_filter(client, make_member, app_module):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    s = app_module.cfg()
    s['file_filter_enabled'] = True
    s['allowed_extensions'] = '.goo'
    app_module.save_cfg(s)
    data = {'file': (io.BytesIO(b'nope'), 'model.ctb')}
    resp = client.post('/my/diamond_ed/upload', data=data, content_type='multipart/form-data',
                        headers={'X-Requested-With': 'XMLHttpRequest'})
    assert resp.get_json()['uploaded'] == []


def test_upload_forbidden_for_other_folder(client, make_member):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    resp = client.post('/my/someone-else/upload', data={'file': (io.BytesIO(b'x'), 'a.ctb')},
                        content_type='multipart/form-data')
    assert resp.status_code == 403


def test_delete_removes_file_and_thumb(client, make_member, app_module):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    folder_dir = os.path.join(app_module.UPLOAD_BASE, 'diamond_ed')
    open(os.path.join(folder_dir, 'a.ctb'), 'wb').write(b'x')
    open(os.path.join(folder_dir, 'a.ctb.thumb.png'), 'wb').write(b'x')
    client.post('/my/diamond_ed/delete', data={'filename': 'a.ctb'})
    assert not os.path.exists(os.path.join(folder_dir, 'a.ctb'))
    assert not os.path.exists(os.path.join(folder_dir, 'a.ctb.thumb.png'))


def test_serve_thumb_404_when_missing(client, make_member):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    resp = client.get('/thumb/diamond_ed/nope.ctb')
    assert resp.status_code == 404


# ---------------------------------------------------------------------------
# start_print
# ---------------------------------------------------------------------------

def test_start_print_requires_full_checklist(client, make_member, app_module, tmp_path):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    folder_dir = os.path.join(app_module.UPLOAD_BASE, 'diamond_ed')
    sf.write_goo(os.path.join(folder_dir, 'model.goo'), print_time=100)
    client.post('/my/diamond_ed/print', data={'filename': 'model.goo'})  # no checkboxes
    c = sqlite3.connect(app_module.DB)
    count = c.execute("SELECT COUNT(*) FROM print_jobs").fetchone()[0]
    c.close()
    assert count == 0


def test_start_print_exact_eta_from_goo(client, make_member, app_module, monkeypatch, tmp_path):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', _exit0_script(tmp_path))
    folder_dir = os.path.join(app_module.UPLOAD_BASE, 'diamond_ed')
    sf.write_goo(os.path.join(folder_dir, 'model.goo'), print_time=7200)
    data = {'filename': 'model.goo', **_checklist_data(app_module)}
    resp = client.post('/my/diamond_ed/print', data=data, follow_redirects=False)
    assert resp.status_code == 302
    c = sqlite3.connect(app_module.DB); c.row_factory = sqlite3.Row
    job = c.execute("SELECT * FROM print_jobs WHERE status='printing'").fetchone()
    c.close()
    assert job is not None
    assert job['estimated_seconds'] == 7200
    assert job['eta_exact'] == 1


def test_start_print_unparseable_file_still_creates_job(client, make_member, app_module, monkeypatch, tmp_path):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', _exit0_script(tmp_path))
    folder_dir = os.path.join(app_module.UPLOAD_BASE, 'diamond_ed')
    open(os.path.join(folder_dir, 'garbage.goo'), 'wb').write(b'not a real goo file')
    data = {'filename': 'garbage.goo', **_checklist_data(app_module)}
    resp = client.post('/my/diamond_ed/print', data=data, follow_redirects=False)
    assert resp.status_code == 302
    c = sqlite3.connect(app_module.DB); c.row_factory = sqlite3.Row
    job = c.execute("SELECT * FROM print_jobs WHERE status='printing'").fetchone()
    c.close()
    assert job is not None
    assert job['estimated_seconds'] is None


def test_start_print_supersedes_previous_job(client, make_member, app_module, monkeypatch, tmp_path):
    make_member(first='Ed', last='Diamond', email='ed@example.com', password='pw123')
    _login_member(client, 'ed@example.com', 'pw123')
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', _exit0_script(tmp_path))
    folder_dir = os.path.join(app_module.UPLOAD_BASE, 'diamond_ed')
    sf.write_goo(os.path.join(folder_dir, 'first.goo'), print_time=100)
    sf.write_goo(os.path.join(folder_dir, 'second.goo'), print_time=200)
    client.post('/my/diamond_ed/print', data={'filename': 'first.goo', **_checklist_data(app_module)})
    client.post('/my/diamond_ed/print', data={'filename': 'second.goo', **_checklist_data(app_module)})
    c = sqlite3.connect(app_module.DB); c.row_factory = sqlite3.Row
    rows = c.execute("SELECT filename, status FROM print_jobs ORDER BY id").fetchall()
    c.close()
    assert [dict(r) for r in rows] == [
        {'filename': 'first.goo', 'status': 'ended'},
        {'filename': 'second.goo', 'status': 'printing'},
    ]


# ---------------------------------------------------------------------------
# /api/status
# ---------------------------------------------------------------------------

def test_api_status_requires_key(client):
    resp = client.get('/api/status')
    assert resp.status_code == 401


def test_api_status_accepts_header_key(client, app_module):
    resp = client.get('/api/status', headers={'X-Api-Key': app_module.API_KEY})
    assert resp.status_code == 200
    assert resp.get_json()['current_job'] is None


def test_api_status_accepts_query_key(client, app_module):
    resp = client.get(f'/api/status?key={app_module.API_KEY}')
    assert resp.status_code == 200


# ---------------------------------------------------------------------------
# Admin
# ---------------------------------------------------------------------------

def test_admin_only_redirects_when_unauthenticated(client):
    resp = client.get('/admin/dashboard', follow_redirects=False)
    assert resp.status_code == 302
    assert '/admin' in resp.headers['Location']


def test_admin_login_and_dashboard(client, app_module):
    resp = _admin_login(client, app_module)
    assert resp.status_code == 302
    resp2 = client.get('/admin/dashboard')
    assert resp2.status_code == 200


def test_admin_members_add_and_duplicate(client, app_module):
    _admin_login(client, app_module)
    resp = client.post('/admin/members', data={
        'action': 'add', 'first': 'Ed', 'last': 'Diamond',
        'email': 'ed@example.com', 'password': 'pw123',
    })
    assert b'Certified Ed Diamond' in resp.data
    resp2 = client.post('/admin/members', data={
        'action': 'add', 'first': 'Ed', 'last': 'Diamond',
        'email': 'ed@example.com', 'password': 'pw123',
    })
    assert b'already exists' in resp2.data


def test_admin_members_toggle_active(client, app_module, make_member):
    _admin_login(client, app_module)
    mid = make_member(email='ed@example.com')
    client.post('/admin/members', data={'action': 'toggle_active', 'member_id': mid})
    c = sqlite3.connect(app_module.DB)
    active = c.execute('SELECT active FROM members WHERE id=?', (mid,)).fetchone()[0]
    c.close()
    assert active == 0


def test_admin_settings_usb_reports_failure_by_default(client, app_module):
    # conftest points USB_REFRESH_SCRIPT at a nonexistent path by default
    _admin_login(client, app_module)
    resp = client.post('/admin/settings', data={'action': 'usb'})
    assert b'Failed to launch USB refresh' in resp.data


def test_admin_settings_usb_reports_success_when_launchable(client, app_module, monkeypatch, tmp_path):
    _admin_login(client, app_module)
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', _exit0_script(tmp_path))
    resp = client.post('/admin/settings', data={'action': 'usb'})
    assert b'USB refresh triggered.' in resp.data


def test_admin_settings_general(client, app_module):
    _admin_login(client, app_module)
    client.post('/admin/settings', data={'action': 'general', 'printer_name': 'M7 Pro'})
    assert app_module.cfg()['printer_name'] == 'M7 Pro'
