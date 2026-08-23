"""Synthetic sliced-file fixture builders, constructed from the exact byte
offsets/formats documented in sliced_file_info.py itself (no real sample
files were available in this environment -- see that module's docstring and
resin_plans.md for how those offsets were originally validated against real
files). Each function writes a minimal-but-structurally-correct file to
`path` for the corresponding parser in sliced_file_info.py to read back.
"""
import json
import struct
import zipfile

import forward_aes
from sliced_file_info import _CTB_ENC_AES_IV, _CTB_ENC_AES_KEY


def _write_str(buf, off, length, s):
    b = s.encode('ascii')[:length]
    buf[off:off + len(b)] = b


# ---------------------------------------------------------------------------
# .goo
# ---------------------------------------------------------------------------
_GOO_LEN = 195454  # header through the `volume` field -- see parse_goo()


def write_goo(path, print_time=17553, layer_count=1200, layer_height=0.05,
              exposure_time=2.5, bottom_exposure_time=35.0, bottom_layer_count=5,
              volume=1234.5, software_name='UVtools', machine_name='Elegoo Saturn 3',
              version_marker=b'V3.0'):
    buf = bytearray(_GOO_LEN)
    buf[0:4] = version_marker
    off = 12  # Version(4) + Magic(8)
    _write_str(buf, off, 32, software_name); off += 32
    off += 24 + 24  # SoftwareVersion, FileCreateTime
    _write_str(buf, off, 32, machine_name); off += 32
    off += 32 + 32  # MachineType, ProfileName
    off += 2 + 2 + 2  # AntiAliasingLevel, GreyLevel, BlurLevel
    off += 116 * 116 * 2 + 2  # SmallPreview565 + delimiter
    off += 290 * 290 * 2 + 2  # BigPreview565 + delimiter
    struct.pack_into('>I', buf, off, layer_count); off += 4
    off += 2 + 2 + 1 + 1 + 4 + 4 + 4  # ResolutionX/Y, MirrorX/Y, DisplayW/H, MachineZ
    struct.pack_into('>f', buf, off, layer_height); off += 4
    struct.pack_into('>f', buf, off, exposure_time); off += 4
    off += 1 + 4 + 4 * 6  # DelayMode, LightOffDelay, 6x BottomWait/Wait fields
    struct.pack_into('>f', buf, off, bottom_exposure_time); off += 4
    struct.pack_into('>I', buf, off, bottom_layer_count); off += 4
    off += 4 * 16 + 2 + 2 + 1  # BottomLiftHeight..RetractSpeed2, PWM fields, PerLayerSettings
    struct.pack_into('>I', buf, off, print_time); off += 4
    struct.pack_into('>f', buf, off, volume); off += 4
    assert off == _GOO_LEN
    with open(path, 'wb') as f:
        f.write(buf)


# ---------------------------------------------------------------------------
# .ctb plain (cbddlp / ctb / ctbv4)
# ---------------------------------------------------------------------------
_CTB_PLAIN_FMT = "<IIfffIIfffffIIIIIIIIIIIIHHIII"
MAGIC_CBDDLP = 0x12FD0019


def write_ctb_plain(path, magic=MAGIC_CBDDLP, layer_count=800, layer_height=0.05,
                     exposure_time=2.5, bottom_exposure_time=35.0, bottom_layer_count=5,
                     print_time=14400):
    values = (
        magic, 1, 68.0, 120.0, 130.0, 0, 0, 60.0, layer_height,
        exposure_time, bottom_exposure_time, 0.5, bottom_layer_count,
        1440, 2560, 0, 0, layer_count, 0, print_time, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    )
    with open(path, 'wb') as f:
        f.write(struct.pack(_CTB_PLAIN_FMT, *values))


# ---------------------------------------------------------------------------
# .ctb encrypted (magic 0x12FD0107)
# ---------------------------------------------------------------------------
_CTB_ENC_HEADER_FMT = "<IIIIIIIIHHIII"
_CTB_ENC_SETTINGS_FMT = "<QIfffIIfffffIIIIIII"
_SETTINGS_BLOCK_SIZE = 288  # documented "288-byte block", divisible by 16 for CBC
MAGIC_CTB_ENCRYPTED = 0x12FD0107


def write_ctb_encrypted(path, layer_count=900, layer_height=0.03, exposure_time=1.8,
                         bottom_exposure_time=30.0, bottom_layer_count=6, print_time=16000):
    settings = struct.pack(
        _CTB_ENC_SETTINGS_FMT,
        0,          # ChecksumValue
        0,          # LayerPointersOffset
        68.04, 120.96, 165.0,   # DisplayWidth, DisplayHeight, MachineZ
        0, 0,       # Unknown1, Unknown2
        45.0,       # TotalHeightMillimeter
        layer_height, exposure_time, bottom_exposure_time,
        0.5,        # LightOffDelay
        bottom_layer_count,
        3840, 2400,  # ResolutionX, ResolutionY
        layer_count,
        0, 0,       # LargePreviewOffset, SmallPreviewOffset
        print_time,
    )
    settings = settings.ljust(_SETTINGS_BLOCK_SIZE, b'\x00')
    encrypted = forward_aes.aes_cbc_encrypt_no_padding(settings, _CTB_ENC_AES_KEY, _CTB_ENC_AES_IV)

    header = struct.pack(
        _CTB_ENC_HEADER_FMT,
        MAGIC_CTB_ENCRYPTED,
        len(encrypted),   # SettingsSize
        48,               # SettingsOffset -- right after the 48-byte header
        0, 1, 0, 0, 0, 0, 0, 0, 0, 0,
    )
    with open(path, 'wb') as f:
        f.write(header)
        f.write(encrypted)


# ---------------------------------------------------------------------------
# .pwsz / .pp1 / .pm7 / .pm7m (Anycubic Photon Workshop ZIP container)
# ---------------------------------------------------------------------------

def write_pwsz(path, machine_name='Anycubic Photon Mono M7 Pro', layer_count=1763,
                exposure_time=8.0, zup_height=6.0, zup_speed=60.0, zdown_speed=90.0,
                off_time=0.5, resin_code='SOME_RESIN', resolvable_resin=True):
    paras = [
        {'exposure_time': exposure_time, 'zup_height': zup_height, 'zup_speed': zup_speed}
        for _ in range(layer_count)
    ]
    conf = {'paras': paras}

    pwsp = {'machine_type': {'name': machine_name}}
    if resolvable_resin:
        pwsp['machine_extern'] = {
            'active_resins': [resin_code],
            'user_resins': [
                {'property': {'name': resin_code},
                 'slicepara': {'off_time': off_time, 'zdown_speed': zdown_speed}},
            ],
        }

    with zipfile.ZipFile(path, 'w') as z:
        z.writestr('anycubic_photon_resins.pwsp', json.dumps(pwsp))
        z.writestr('layers_controller.conf', json.dumps(conf))
