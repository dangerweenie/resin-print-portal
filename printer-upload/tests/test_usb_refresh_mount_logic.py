"""Off-hardware regression coverage for usb-refresh.sh's partition-layout
detection (the bug fixed this session: it used to assume /piusb.bin is
always MBR-partitioned, which broke against a bare-FAT32 image). Only
`modprobe g_mass_storage` needs real Pi USB gadget hardware -- `losetup`/
`mkdosfs`/`mount`/`parted` don't -- so this exercises both code paths for
real, with a no-op `modprobe` stub shadowing the real one via $PATH, using
plain loop-mounted scratch images. Root-gated; skipped automatically where
that's not available (this dev sandbox included). The Pi-hardware-specific
half of this (does the printer actually see the drive) is what
hw-tests/test-usb-refresh-on-pi.sh is for.
"""
import os
import shutil
import sqlite3
import stat
import subprocess
from pathlib import Path

import pytest

pytestmark = pytest.mark.skipif(
    os.geteuid() != 0 or not all(shutil.which(t) for t in ('losetup', 'mkdosfs', 'mount', 'parted')),
    reason='requires root + losetup/mkdosfs/mount/parted (loop-device mount test, no real Pi USB hardware needed)',
)

USB_REFRESH_SH = Path(__file__).resolve().parent.parent.parent / 'usb-refresh.sh'


def _fake_modprobe_bin(tmp_path):
    bindir = tmp_path / 'fakebin'
    bindir.mkdir()
    modprobe = bindir / 'modprobe'
    modprobe.write_text('#!/bin/sh\nexit 0\n')
    modprobe.chmod(modprobe.stat().st_mode | stat.S_IEXEC)
    return str(bindir)


def _seed_job_db(base, folder, filename):
    os.makedirs(f'{base}/files/{folder}', exist_ok=True)
    src = f'{base}/files/{folder}/{filename}'
    with open(src, 'wb') as f:
        f.write(os.urandom(1024))
    c = sqlite3.connect(f'{base}/uploads.db')
    c.execute('CREATE TABLE print_jobs(id INTEGER PRIMARY KEY, folder TEXT, filename TEXT, status TEXT)')
    c.execute("INSERT INTO print_jobs VALUES (1, ?, ?, 'printing')", (folder, filename))
    c.commit()
    c.close()
    return src


def _run_usb_refresh(image, base, mount_point, fake_bin_dir):
    env = dict(os.environ)
    env['PIUSB_IMAGE'] = str(image)
    env['PRINTER_UPLOAD_BASE'] = str(base)
    env['USB_REFRESH_MOUNT_POINT'] = str(mount_point)
    env['PATH'] = f'{fake_bin_dir}:{env["PATH"]}'
    return subprocess.run(['bash', str(USB_REFRESH_SH)], env=env, capture_output=True, text=True)


def _read_back_files(image, partitioned, mount_point):
    loop = subprocess.run(['losetup', '-fP', '--show', str(image)],
                           capture_output=True, text=True, check=True).stdout.strip()
    try:
        part = f'{loop}p1' if partitioned else loop
        os.makedirs(mount_point, exist_ok=True)
        subprocess.run(['mount', part, mount_point], check=True)
        try:
            return os.listdir(mount_point)
        finally:
            subprocess.run(['umount', mount_point], check=False)
    finally:
        subprocess.run(['losetup', '-d', loop], check=False)


def test_bare_fat32_layout(tmp_path):
    image = tmp_path / 'bare.bin'
    subprocess.run(['dd', 'if=/dev/zero', f'of={image}', 'bs=1M', 'count=32'],
                    check=True, capture_output=True)
    subprocess.run(['mkdosfs', '-F', '32', str(image)], check=True, capture_output=True)

    base = tmp_path / 'base'
    _seed_job_db(str(base), 'testuser', 'model.goo')

    result = _run_usb_refresh(image, base, tmp_path / 'mnt', _fake_modprobe_bin(tmp_path))
    assert result.returncode == 0, result.stderr

    files = _read_back_files(image, partitioned=False, mount_point=str(tmp_path / 'verify'))
    assert files == ['model.goo']


def test_mbr_partitioned_layout(tmp_path):
    image = tmp_path / 'mbr.bin'
    subprocess.run(['dd', 'if=/dev/zero', f'of={image}', 'bs=1M', 'count=32'],
                    check=True, capture_output=True)
    subprocess.run(['parted', '-s', str(image), 'mklabel', 'msdos'], check=True, capture_output=True)
    subprocess.run(['parted', '-s', str(image), 'mkpart', 'primary', 'fat32', '1MiB', '100%'],
                    check=True, capture_output=True)

    loop = subprocess.run(['losetup', '-fP', '--show', str(image)],
                           capture_output=True, text=True, check=True).stdout.strip()
    try:
        subprocess.run(['mkdosfs', '-F', '32', f'{loop}p1'], check=True, capture_output=True)
    finally:
        subprocess.run(['losetup', '-d', loop], check=False)

    base = tmp_path / 'base'
    _seed_job_db(str(base), 'testuser', 'model.goo')

    result = _run_usb_refresh(image, base, tmp_path / 'mnt', _fake_modprobe_bin(tmp_path))
    assert result.returncode == 0, result.stderr

    files = _read_back_files(image, partitioned=True, mount_point=str(tmp_path / 'verify'))
    assert files == ['model.goo']
