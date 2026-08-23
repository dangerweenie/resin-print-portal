import logging
import os
from datetime import datetime, timedelta

import pytest

PNG_SIG = bytes([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])
PNG_END = bytes([0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82])


def _fake_png(payload=b'fake-png-bytes'):
    return PNG_SIG + payload + PNG_END


# ---------------------------------------------------------------------------
# extract_thumb
# ---------------------------------------------------------------------------

def test_extract_thumb_finds_embedded_png(app_module, tmp_path):
    src = tmp_path / 'model.ctb'
    src.write_bytes(b'junk-before' + _fake_png() + b'junk-after')
    app_module.extract_thumb(str(src))
    thumb = tmp_path / 'model.ctb.thumb.png'
    assert thumb.exists()
    assert thumb.read_bytes() == _fake_png()


def test_extract_thumb_picks_largest_candidate(app_module, tmp_path):
    src = tmp_path / 'model.ctb'
    small = _fake_png(b'x')
    large = _fake_png(b'x' * 50)
    src.write_bytes(small + b'gap' + large)
    app_module.extract_thumb(str(src))
    thumb = tmp_path / 'model.ctb.thumb.png'
    assert thumb.read_bytes() == large


def test_extract_thumb_no_png_writes_nothing(app_module, tmp_path):
    src = tmp_path / 'model.ctb'
    src.write_bytes(b'no png signature anywhere in here')
    app_module.extract_thumb(str(src))
    assert not (tmp_path / 'model.ctb.thumb.png').exists()


def test_extract_thumb_missing_file_logs_warning(app_module, tmp_path, caplog):
    with caplog.at_level(logging.WARNING):
        app_module.extract_thumb(str(tmp_path / 'does-not-exist.ctb'))
    assert any('thumbnail extraction failed' in r.message for r in caplog.records)


def test_extract_thumb_respects_scan_cap(app_module, tmp_path):
    # A PNG signature fully inside the first MAX_SCAN bytes is found...
    src = tmp_path / 'inside.ctb'
    src.write_bytes(b'\x00' * 1000 + _fake_png() + b'\x00' * 1000)
    app_module.extract_thumb(str(src))
    assert (tmp_path / 'inside.ctb.thumb.png').exists()

    # ...but one whose PNG_END falls past the cap is not (documents the
    # accepted limitation of the bounded scan introduced to fix the
    # whole-file-into-RAM bug).
    src2 = tmp_path / 'outside.ctb'
    scan_cap = 16 * 1024 * 1024  # matches MAX_SCAN inside extract_thumb()
    payload = PNG_SIG + (b'\x00' * (scan_cap - len(PNG_SIG) - 4)) + PNG_END
    src2.write_bytes(payload)
    app_module.extract_thumb(str(src2))
    assert not (tmp_path / 'outside.ctb.thumb.png').exists()


# ---------------------------------------------------------------------------
# folder_name
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('last,first,expected', [
    ('Diamond', 'Ed', 'diamond_ed'),
    ('Van Der Berg', 'Anne-Marie', 'van-der-berg_annemarie'),
    ("O'Brien", 'Sam', 'obrien_sam'),
])
def test_folder_name_normalizes(app_module, last, first, expected):
    assert app_module.folder_name(last, first) == expected


# ---------------------------------------------------------------------------
# current_job_display
# ---------------------------------------------------------------------------

def _job(**overrides):
    base = {
        'id': 1, 'member_id': 1, 'folder': 'test_user', 'filename': 'x.goo',
        'started_at': datetime.now().strftime('%Y-%m-%d %H:%M:%S'),
        'estimated_seconds': None, 'eta_exact': 0,
        'estimated_complete_at': None, 'ended_at': None, 'status': 'printing',
    }
    base.update(overrides)
    return base


def test_current_job_display_none(app_module):
    assert app_module.current_job_display(None) is None


def test_current_job_display_no_eta(app_module):
    d = app_module.current_job_display(_job())
    assert d['display_status'] == 'printing'
    assert d.get('remaining_human') is None


def test_current_job_display_future_eta(app_module):
    eta = (datetime.now() + timedelta(hours=1, minutes=30)).strftime('%Y-%m-%d %H:%M:%S')
    d = app_module.current_job_display(_job(estimated_complete_at=eta))
    assert d['display_status'] == 'printing'
    assert d['remaining_human'] is not None


def test_current_job_display_overdue(app_module):
    eta = (datetime.now() - timedelta(minutes=5)).strftime('%Y-%m-%d %H:%M:%S')
    d = app_module.current_job_display(_job(estimated_complete_at=eta))
    assert d['display_status'] == 'overdue'


def test_current_job_display_ended(app_module):
    d = app_module.current_job_display(_job(ended_at='2026-01-01 00:00:00'))
    assert d['display_status'] == 'ended'


# ---------------------------------------------------------------------------
# Regression test for the tempdir-not-created latent bug
# ---------------------------------------------------------------------------

def test_tmp_dir_created_on_import(app_module, tmp_path):
    assert os.path.isdir(str(tmp_path / 'tmp'))
