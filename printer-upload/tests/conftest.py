import sqlite3
import sys
from pathlib import Path

import pytest

APP_DIR = Path(__file__).resolve().parent.parent
FIXTURES_DIR = Path(__file__).resolve().parent / 'fixtures'
sys.path.insert(0, str(APP_DIR))
sys.path.insert(0, str(FIXTURES_DIR))


@pytest.fixture
def app_module(tmp_path, monkeypatch):
    """A freshly-imported app module, pointed at a disposable tmp_path via
    PRINTER_UPLOAD_BASE. Must be a *fresh* import (not just a monkeypatched
    already-imported module) because BASE and all the paths/side effects
    derived from it (key generation, init_db(), save_cfg(DEFAULTS)) run once
    at module import time."""
    monkeypatch.setenv('PRINTER_UPLOAD_BASE', str(tmp_path))
    monkeypatch.setenv('USB_REFRESH_SCRIPT', str(tmp_path / 'nonexistent-by-default.sh'))
    for mod in ('app', 'sliced_file_info', 'pure_aes'):
        sys.modules.pop(mod, None)
    import app
    yield app
    sys.modules.pop('app', None)


@pytest.fixture
def client(app_module):
    app_module.app.config['TESTING'] = True
    return app_module.app.test_client()


@pytest.fixture
def make_member(app_module):
    def _make(first='Test', last='User', email='test@example.com', password='pw',
              must_change_password=0, active=1):
        from werkzeug.security import generate_password_hash
        c = sqlite3.connect(app_module.DB)
        c.execute(
            '''INSERT INTO members (first,last,email,password_hash,must_change_password,active,created_at,created_by)
               VALUES (?,?,?,?,?,?,?,?)''',
            (first, last, email, generate_password_hash(password), must_change_password, active,
             '2026-01-01 00:00:00', 'test'))
        c.commit()
        mid = c.execute('SELECT id FROM members WHERE email=?', (email,)).fetchone()[0]
        c.close()
        import os
        os.makedirs(app_module.UPLOAD_BASE + '/' + app_module.folder_name(last, first), exist_ok=True)
        return mid
    return _make
