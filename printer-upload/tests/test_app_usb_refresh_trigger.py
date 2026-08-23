import logging
import os
import stat


def _make_script(path, body):
    path.write_text(f'#!/bin/sh\n{body}\n')
    path.chmod(path.stat().st_mode | stat.S_IEXEC)
    return str(path)


def test_trigger_usb_refresh_success_is_logged(app_module, tmp_path, caplog, monkeypatch):
    script = _make_script(tmp_path / 'ok.sh', 'exit 0')
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', script)
    with caplog.at_level(logging.INFO):
        ok, thread = app_module.trigger_usb_refresh()
        assert ok is True
        thread.join(timeout=5)
    assert any('usb-refresh.sh exited 0' in r.message for r in caplog.records)


def test_trigger_usb_refresh_failure_is_logged(app_module, tmp_path, caplog, monkeypatch):
    script = _make_script(tmp_path / 'fail.sh', 'exit 1')
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', script)
    with caplog.at_level(logging.INFO):
        ok, thread = app_module.trigger_usb_refresh()
        # launch succeeds even though the script itself will fail --
        # that's the distinction the helper exists to preserve
        assert ok is True
        thread.join(timeout=5)
    assert any('usb-refresh.sh exited 1' in r.message for r in caplog.records)


def test_trigger_usb_refresh_missing_script_fails_to_launch(app_module, tmp_path, caplog, monkeypatch):
    missing = str(tmp_path / 'does-not-exist.sh')
    monkeypatch.setattr(app_module, 'USB_REFRESH_SCRIPT', missing)
    with caplog.at_level(logging.ERROR):
        ok, thread = app_module.trigger_usb_refresh()
    assert ok is False
    assert thread is None
    assert any('failed to launch usb-refresh.sh' in r.message for r in caplog.records)
