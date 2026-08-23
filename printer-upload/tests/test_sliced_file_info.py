import zipfile

import pytest
import sliced_files as sf

from sliced_file_info import (
    SlicedFileError,
    format_duration,
    get_print_info,
    parse_ctb,
    parse_ctb_encrypted,
    parse_ctb_plain,
    parse_goo,
    parse_photon_workshop,
)


# ---------------------------------------------------------------------------
# .goo
# ---------------------------------------------------------------------------

def test_parse_goo_happy_path(tmp_path):
    p = tmp_path / 'test.goo'
    sf.write_goo(p, print_time=17553, layer_count=1200, layer_height=0.05,
                 exposure_time=2.5, bottom_exposure_time=35.0, bottom_layer_count=5,
                 volume=1234.5, machine_name='Elegoo Saturn 3')
    info = parse_goo(str(p))
    assert info['format'] == 'goo'
    assert info['machine_name'] == 'Elegoo Saturn 3'
    assert info['layer_count'] == 1200
    assert info['layer_height_mm'] == pytest.approx(0.05, abs=1e-4)
    assert info['exposure_time_s'] == pytest.approx(2.5, abs=1e-2)
    assert info['bottom_exposure_time_s'] == pytest.approx(35.0, abs=1e-2)
    assert info['bottom_layer_count'] == 5
    assert info['estimated_seconds'] == 17553
    assert info['volume_mm3'] == pytest.approx(1234.5, abs=0.1)
    assert info['exact'] is True


def test_parse_goo_bad_version_marker(tmp_path):
    p = tmp_path / 'bad.goo'
    sf.write_goo(p, version_marker=b'XXXX')
    with pytest.raises(SlicedFileError):
        parse_goo(str(p))


# ---------------------------------------------------------------------------
# .ctb plain
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('magic', [0x12FD0019, 0x12FD0086, 0x12FD0106])
def test_parse_ctb_plain_accepted_magics(tmp_path, magic):
    p = tmp_path / 'test.ctb'
    sf.write_ctb_plain(p, magic=magic, layer_count=800, print_time=14400)
    info = parse_ctb_plain(str(p))
    assert info['format'] == 'ctb_plain'
    assert info['layer_count'] == 800
    assert info['estimated_seconds'] == 14400
    assert info['exact'] is True


def test_parse_ctb_plain_bad_magic(tmp_path):
    p = tmp_path / 'bad.ctb'
    sf.write_ctb_plain(p, magic=0xDEADBEEF)
    with pytest.raises(SlicedFileError):
        parse_ctb_plain(str(p))


# ---------------------------------------------------------------------------
# .ctb encrypted
# ---------------------------------------------------------------------------

def test_parse_ctb_encrypted_happy_path(tmp_path):
    p = tmp_path / 'test.ctb'
    sf.write_ctb_encrypted(p, layer_count=900, layer_height=0.03, exposure_time=1.8,
                            bottom_exposure_time=30.0, bottom_layer_count=6, print_time=16000)
    info = parse_ctb_encrypted(str(p))
    assert info['format'] == 'ctb_encrypted'
    assert info['layer_count'] == 900
    assert info['layer_height_mm'] == pytest.approx(0.03, abs=1e-4)
    assert info['exposure_time_s'] == pytest.approx(1.8, abs=1e-2)
    assert info['bottom_exposure_time_s'] == pytest.approx(30.0, abs=1e-2)
    assert info['bottom_layer_count'] == 6
    assert info['estimated_seconds'] == 16000
    assert info['exact'] is True


def test_parse_ctb_encrypted_wrong_magic(tmp_path):
    p = tmp_path / 'bad.ctb'
    sf.write_ctb_plain(p)  # plain magic, not the encrypted one
    with pytest.raises(SlicedFileError):
        parse_ctb_encrypted(str(p))


def test_parse_ctb_dispatches_by_magic(tmp_path):
    enc = tmp_path / 'enc.ctb'
    sf.write_ctb_encrypted(enc, print_time=111)
    assert parse_ctb(str(enc))['estimated_seconds'] == 111

    plain = tmp_path / 'plain.ctb'
    sf.write_ctb_plain(plain, print_time=222)
    assert parse_ctb(str(plain))['estimated_seconds'] == 222


# ---------------------------------------------------------------------------
# .pwsz (Photon Workshop)
# ---------------------------------------------------------------------------

def test_parse_photon_workshop_happy_path(tmp_path):
    p = tmp_path / 'test.pwsz'
    sf.write_pwsz(p, machine_name='Anycubic Photon Mono M7 Pro', layer_count=10,
                   exposure_time=8.0, zup_height=6.0, zup_speed=60.0, zdown_speed=90.0,
                   off_time=0.5)
    info = parse_photon_workshop(str(p))
    assert info['format'] == 'photon_workshop'
    assert info['machine_name'] == 'Anycubic Photon Mono M7 Pro'
    assert info['layer_count'] == 10
    # sum(exposure) + up + down + off, per-layer, for 10 layers
    expected = 10 * 8.0 + 10 * (6.0 / 60.0) + 10 * (6.0 / 90.0) + 10 * 0.5
    assert info['estimated_seconds'] == round(expected)
    assert info['exact'] is False


def test_parse_photon_workshop_unresolvable_resin_falls_back(tmp_path):
    p = tmp_path / 'test.pwsz'
    sf.write_pwsz(p, layer_count=5, exposure_time=8.0, zup_height=6.0, zup_speed=60.0,
                   resolvable_resin=False)
    info = parse_photon_workshop(str(p))
    # off_time defaults to 0, zdown_speed unresolved -> total_down == total_up
    expected_up = 5 * (6.0 / 60.0)
    expected = 5 * 8.0 + expected_up + expected_up
    assert info['estimated_seconds'] == round(expected)
    assert info['exact'] is False


def test_parse_photon_workshop_missing_members(tmp_path):
    p = tmp_path / 'bad.pwsz'
    with zipfile.ZipFile(p, 'w') as z:
        z.writestr('not_a_pwsp_file.txt', 'irrelevant')
    with pytest.raises(SlicedFileError):
        parse_photon_workshop(str(p))


def test_parse_photon_workshop_empty_paras(tmp_path):
    p = tmp_path / 'empty.pwsz'
    sf.write_pwsz(p, layer_count=0)
    with pytest.raises(SlicedFileError):
        parse_photon_workshop(str(p))


# ---------------------------------------------------------------------------
# get_print_info dispatch + format_duration
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('ext,writer', [
    ('.goo', sf.write_goo),
    ('.ctb', sf.write_ctb_plain),
    ('.pwsz', sf.write_pwsz),
    ('.pp1', sf.write_pwsz),
    ('.pm7', sf.write_pwsz),
    ('.pm7m', sf.write_pwsz),
])
def test_get_print_info_dispatches_by_extension(tmp_path, ext, writer):
    p = tmp_path / f'test{ext}'
    writer(p)
    info = get_print_info(str(p))
    assert 'estimated_seconds' in info


def test_get_print_info_unsupported_extension(tmp_path):
    p = tmp_path / 'test.stl'
    p.write_bytes(b'not a real file')
    with pytest.raises(SlicedFileError):
        get_print_info(str(p))


def test_format_duration_hours_and_minutes():
    assert format_duration(3661) == '1h 1m'


def test_format_duration_minutes_and_seconds():
    assert format_duration(65) == '1m 5s'
