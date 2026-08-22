"""
Parse metadata (print time, layer count, target machine) out of sliced resin
print files, for display/logging in the upload portal and Slack posts.

Supported formats, reverse-engineered/confirmed against real sample files in
sliced_scenes/ on 2026-08-21:

  .goo              Chitubox-family binary format (Elegoo Saturn 3, etc).
                     Has an EXACT firmware-computed `PrintTime` field (uint32
                     seconds) in the file header. Field layout confirmed
                     against the open-source UVtools project's GooFile.cs
                     (https://github.com/sn4k3/UVtools), byte-offsets
                     validated by round-tripping a real Saturn 3 file.

  .ctb (encrypted)   Newer Chitubox-family binary format, magic 0x12FD0107
                     ("CTBEncryptedFile" per UVtools, used by e.g. Elegoo
                     Mars-family slicer exports). The settings block (which
                     contains the EXACT firmware `PrintTime` field) is
                     AES-256-CBC encrypted with a key/IV that's hardcoded in
                     UVtools (XOR-obfuscated in their source, trivially
                     reversible — same fixed key for every file, not a
                     per-file secret). Decrypted here with a small pure-
                     Python AES implementation (pure_aes.py, self-tested
                     against a NIST test vector — no crypto library was
                     available in this environment). Validated against the
                     real sample: decrypted LayerHeight/ExposureTime matched
                     the values embedded in the file's own filename exactly.

  .ctb (plain,       Older/unencrypted Chitubox-family binary (magic
  UNVALIDATED)       0x12FD0019/0086/0106 — "cbddlp"/"ctb"/"ctbv4"). Field
                     layout taken from UVtools' ChituboxFile.cs but NOT
                     validated against a real sample file (none available in
                     this session) — treat parse_ctb_plain()'s output with
                     more skepticism than the other parsers until it's been
                     checked against an actual file.

  .pwsz / .pp1 /     Anycubic "Photon Workshop" ZIP container (confirmed
  .pm7 / .pm7m       identical structure for both M7 Pro `.pwsz` and P1
                     `.pp1` samples). Contains:
                       - anycubic_photon_resins.pwsp  (JSON: machine + resin
                         profile, incl. per-layer timing constants)
                       - layers_controller.conf        (JSON: per-layer
                         exposure_time / zup_height / zup_speed array)
                     No single precomputed "total print time" field is
                     stored — Anycubic's own firmware apparently computes it
                     at print time from a full kinematic model (acceleration/
                     jerk/min-segment-time constants are embedded under
                     machine_extern.firmware_calc_print_time_paras, but
                     replicating that model exactly is out of scope). Instead
                     we ESTIMATE: sum(exposure_time) + per-layer Z lift/
                     retract time (up at zup_speed, down at zdown_speed) +
                     per-layer light-off delay (off_time). This is an
                     approximation, not a firmware-exact value — flag it as
                     such in the UI (e.g. "~4h 56m (estimated)" vs a `.goo`
                     file's exact "4h 53m").

Usage:
    from sliced_file_info import get_print_info
    info = get_print_info("/path/to/file.pwsz")
    # -> {"format": "photon_workshop", "machine_name": "Anycubic Photon Mono M7 Pro",
    #     "layer_count": 1763, "estimated_seconds": 17772, "exact": False, ...}
"""
import base64
import json
import struct
import zipfile
from pathlib import Path

from pure_aes import aes_cbc_decrypt_no_padding


class SlicedFileError(Exception):
    pass


# ---------------------------------------------------------------------------
# Shared AES key/IV for the encrypted .ctb variant. Derived from UVtools'
# XOR-obfuscated constants (UVtools.Core/FileFormats/CTBEncryptedFile.cs) —
# these are fixed constants baked into UVtools itself, not a per-file or
# per-printer secret, so deriving them here is just reimplementing published
# open-source logic, not "breaking" anything.
# ---------------------------------------------------------------------------
_UVTOOLS_SOFTWARE_NAME = "UVtools"


def _xor_cipher(data: bytes, key: str) -> bytes:
    kb = key.encode("ascii")
    return bytes(b ^ kb[i % len(kb)] for i, b in enumerate(data))


_CTB_ENC_SECRET_KEY = "hQ36XB6yTk+zO02ysyiowt8yC1buK+nbLWyfY40EXoU="
_CTB_ENC_SECRET_IV = "Wld+ampndVJecmVjYH5cWQ=="
_CTB_ENC_AES_KEY = _xor_cipher(base64.b64decode(_CTB_ENC_SECRET_KEY), _UVTOOLS_SOFTWARE_NAME)
_CTB_ENC_AES_IV = _xor_cipher(base64.b64decode(_CTB_ENC_SECRET_IV), _UVTOOLS_SOFTWARE_NAME)


# ---------------------------------------------------------------------------
# .goo (Chitubox-family binary, e.g. Elegoo Saturn 3)
# ---------------------------------------------------------------------------

def _read_fixed_str(buf, offset, length):
    raw = buf[offset:offset + length]
    end = raw.find(b"\x00")
    if end == -1:
        end = length
    return raw[:end].decode("ascii", errors="replace")


def parse_goo(path):
    """Parse a .goo file header. Returns exact firmware-computed metadata."""
    with open(path, "rb") as f:
        buf = f.read(300_000)  # header + previews; plenty of margin

    if buf[0:4] != b"V3.0":
        raise SlicedFileError(f"{path}: unrecognized .goo version marker {buf[0:4]!r}")

    off = 4 + 8  # Version + Magic
    software_name = _read_fixed_str(buf, off, 32); off += 32
    off += 24  # SoftwareVersion
    off += 24  # FileCreateTime
    machine_name = _read_fixed_str(buf, off, 32); off += 32
    off += 32  # MachineType
    off += 32  # ProfileName
    off += 2 + 2 + 2  # AntiAliasingLevel, GreyLevel, BlurLevel
    off += 116 * 116 * 2 + 2  # SmallPreview565 + delimiter
    off += 290 * 290 * 2 + 2  # BigPreview565 + delimiter

    layer_count, = struct.unpack_from(">I", buf, off); off += 4
    off += 2 + 2  # ResolutionX, ResolutionY
    off += 1 + 1  # MirrorX, MirrorY
    off += 4 + 4 + 4  # DisplayWidth, DisplayHeight, MachineZ
    layer_height, = struct.unpack_from(">f", buf, off); off += 4
    exposure_time, = struct.unpack_from(">f", buf, off); off += 4
    off += 1  # DelayMode
    off += 4  # LightOffDelay
    off += 4 * 6  # BottomWaitTimeAfterCure..WaitTimeBeforeCure
    bottom_exposure_time, = struct.unpack_from(">f", buf, off); off += 4
    bottom_layer_count, = struct.unpack_from(">I", buf, off); off += 4
    off += 4 * 16  # BottomLiftHeight .. RetractSpeed2 (16 float fields)
    off += 2 + 2  # BottomLightPWM, LightPWM
    off += 1  # PerLayerSettings
    print_time, = struct.unpack_from(">I", buf, off); off += 4
    volume, = struct.unpack_from(">f", buf, off); off += 4

    return {
        "format": "goo",
        "software_name": software_name,
        "machine_name": machine_name,
        "layer_count": layer_count,
        "layer_height_mm": round(layer_height, 4),
        "exposure_time_s": round(exposure_time, 2),
        "bottom_exposure_time_s": round(bottom_exposure_time, 2),
        "bottom_layer_count": bottom_layer_count,
        "estimated_seconds": print_time,
        "volume_mm3": round(volume, 1),
        "exact": True,  # firmware-computed value straight from the header
    }


# ---------------------------------------------------------------------------
# .ctb — encrypted variant (magic 0x12FD0107). VALIDATED against a real file.
# ---------------------------------------------------------------------------

_MAGIC_CTB_ENCRYPTED = 0x12FD0107

# FileHeader: 48 bytes, little-endian, all fixed-size, no strings.
_CTB_ENC_HEADER_FMT = "<IIIIIIIIHHIII"
_CTB_ENC_HEADER_FIELDS = [
    "Magic", "SettingsSize", "SettingsOffset", "Unknown1", "Version",
    "SignatureSize", "SignatureOffset", "Unknown", "Unknown4", "Unknown5",
    "Unknown6", "Unknown7", "Unknown8",
]

# SlicerSettings: first 19 fields of a 288-byte block (we only need up to
# PrintTime; the rest of the block — offsets 80..288 — is left unparsed).
_CTB_ENC_SETTINGS_FMT = "<QIfffIIfffffIIIIIII"
_CTB_ENC_SETTINGS_FIELDS = [
    "ChecksumValue", "LayerPointersOffset", "DisplayWidth", "DisplayHeight",
    "MachineZ", "Unknown1", "Unknown2", "TotalHeightMillimeter", "LayerHeight",
    "ExposureTime", "BottomExposureTime", "LightOffDelay", "BottomLayerCount",
    "ResolutionX", "ResolutionY", "LayerCount", "LargePreviewOffset",
    "SmallPreviewOffset", "PrintTime",
]


def parse_ctb_encrypted(path):
    """Parse the encrypted .ctb variant (magic 0x12FD0107). Returns the exact
    firmware-computed PrintTime, decrypted out of the AES-256-CBC settings
    block. See module docstring for validation notes."""
    with open(path, "rb") as f:
        header_bytes = f.read(48)
        header = dict(zip(_CTB_ENC_HEADER_FIELDS,
                           struct.unpack(_CTB_ENC_HEADER_FMT, header_bytes)))
        if header["Magic"] != _MAGIC_CTB_ENCRYPTED:
            raise SlicedFileError(
                f"{path}: not an encrypted .ctb (magic 0x{header['Magic']:08X}, "
                f"expected 0x{_MAGIC_CTB_ENCRYPTED:08X}) — try parse_ctb_plain()")

        f.seek(header["SettingsOffset"])
        encrypted_block = f.read(header["SettingsSize"])

    plain = aes_cbc_decrypt_no_padding(encrypted_block, _CTB_ENC_AES_KEY, _CTB_ENC_AES_IV)
    settings = dict(zip(_CTB_ENC_SETTINGS_FIELDS,
                         struct.unpack_from(_CTB_ENC_SETTINGS_FMT, plain, 0)))

    return {
        "format": "ctb_encrypted",
        "machine_name": None,  # not in this portion of the settings block
        "layer_count": settings["LayerCount"],
        "layer_height_mm": round(settings["LayerHeight"], 4),
        "exposure_time_s": round(settings["ExposureTime"], 2),
        "bottom_exposure_time_s": round(settings["BottomExposureTime"], 2),
        "bottom_layer_count": settings["BottomLayerCount"],
        "estimated_seconds": settings["PrintTime"],
        "exact": True,  # firmware-computed value, decrypted straight from the header
    }


# ---------------------------------------------------------------------------
# .ctb — plain/legacy variant (magic 0x12FD0019 / 0086 / 0106). NOT validated
# against a real sample in this session — see module docstring.
# ---------------------------------------------------------------------------

_MAGIC_CBDDLP = 0x12FD0019
_MAGIC_CTB = 0x12FD0086
_MAGIC_CTBV4 = 0x12FD0106

_CTB_PLAIN_HEADER_FMT = "<IIfffIIfffffIIIIIIIIIIIIHHIII"
_CTB_PLAIN_HEADER_FIELDS = [
    "Magic", "Version", "BedSizeX", "BedSizeY", "BedSizeZ", "Unknown1", "Unknown2",
    "TotalHeightMillimeter", "LayerHeightMillimeter", "LayerExposureSeconds",
    "BottomExposureSeconds", "LightOffDelay", "BottomLayersCount", "ResolutionX",
    "ResolutionY", "PreviewLargeOffsetAddress", "LayersDefinitionOffsetAddress",
    "LayerCount", "PreviewSmallOffsetAddress", "PrintTime", "ProjectorType",
    "PrintParametersOffsetAddress", "PrintParametersSize", "AntiAliasLevel",
    "LightPWM", "BottomLightPWM", "EncryptionKey", "SlicerOffset", "SlicerSize",
]


def parse_ctb_plain(path):
    """UNVALIDATED — see module docstring. Parse the older/unencrypted .ctb
    header (cbddlp / ctb / ctbv4)."""
    with open(path, "rb") as f:
        header_bytes = f.read(struct.calcsize(_CTB_PLAIN_HEADER_FMT))
    header = dict(zip(_CTB_PLAIN_HEADER_FIELDS,
                       struct.unpack(_CTB_PLAIN_HEADER_FMT, header_bytes)))
    if header["Magic"] not in (_MAGIC_CBDDLP, _MAGIC_CTB, _MAGIC_CTBV4):
        raise SlicedFileError(
            f"{path}: unrecognized .ctb magic 0x{header['Magic']:08X} — "
            f"if this is a newer/encrypted variant, try parse_ctb_encrypted()")

    return {
        "format": "ctb_plain",
        "machine_name": None,
        "layer_count": header["LayerCount"],
        "layer_height_mm": round(header["LayerHeightMillimeter"], 4),
        "exposure_time_s": round(header["LayerExposureSeconds"], 2),
        "bottom_exposure_time_s": round(header["BottomExposureSeconds"], 2),
        "bottom_layer_count": header["BottomLayersCount"],
        "estimated_seconds": header["PrintTime"],
        "exact": True,  # per format docs, IF this parse is correct (unvalidated)
    }


def parse_ctb(path):
    """Dispatch to the encrypted or plain .ctb parser based on magic bytes."""
    with open(path, "rb") as f:
        magic, = struct.unpack("<I", f.read(4))
    if magic == _MAGIC_CTB_ENCRYPTED:
        return parse_ctb_encrypted(path)
    return parse_ctb_plain(path)


# ---------------------------------------------------------------------------
# Anycubic "Photon Workshop" ZIP container (.pwsz, .pp1, .pm7, .pm7m, ...)
# ---------------------------------------------------------------------------

def parse_photon_workshop(path):
    """Parse the Anycubic Photon Workshop ZIP container. Returns an ESTIMATE
    (see module docstring) — no exact precomputed total is stored in these
    files; Anycubic's firmware derives it from a full kinematic model we
    don't replicate here."""
    with zipfile.ZipFile(path) as z:
        names = z.namelist()
        pwsp_name = next((n for n in names if n.endswith(".pwsp")), None)
        conf_name = next((n for n in names if n.endswith("layers_controller.conf")), None)
        if not pwsp_name or not conf_name:
            raise SlicedFileError(f"{path}: doesn't look like a Photon Workshop container "
                                   f"(missing .pwsp or layers_controller.conf)")
        pwsp = json.loads(z.read(pwsp_name))
        conf = json.loads(z.read(conf_name))

    machine_name = pwsp.get("machine_type", {}).get("name", "unknown")
    paras = conf.get("paras", [])
    if not paras:
        raise SlicedFileError(f"{path}: layers_controller.conf has no layer data")

    # Light-off delay comes from the active resin profile, if resolvable;
    # fall back to 0 if the structure doesn't match what we've seen so far.
    off_time = 0.0
    zdown_speed = None
    try:
        active_code = pwsp["machine_extern"]["active_resins"][0]
        resin = next(r for r in pwsp["machine_extern"]["user_resins"]
                     if r.get("property", {}).get("name") == active_code)
        slicepara = resin["slicepara"]
        off_time = slicepara.get("off_time", 0.0)
        zdown_speed = slicepara.get("zdown_speed")
    except (KeyError, StopIteration, IndexError):
        pass  # keep defaults; estimate will just be slightly less accurate

    total_exposure = sum(p.get("exposure_time", 0) for p in paras)
    total_up = sum(p.get("zup_height", 0) / p["zup_speed"] for p in paras if p.get("zup_speed"))
    if zdown_speed:
        total_down = sum(p.get("zup_height", 0) / zdown_speed for p in paras)
    else:
        total_down = total_up  # assume symmetric if we couldn't resolve zdown_speed
    total_off = off_time * len(paras)

    estimated_seconds = total_exposure + total_up + total_down + total_off

    return {
        "format": "photon_workshop",
        "machine_name": machine_name,
        "layer_count": len(paras),
        "estimated_seconds": round(estimated_seconds),
        "exact": False,  # our estimate, not a value read from the file
    }


# ---------------------------------------------------------------------------
# Dispatch
# ---------------------------------------------------------------------------

_EXT_HANDLERS = {
    ".goo": parse_goo,
    ".ctb": parse_ctb,
    ".pwsz": parse_photon_workshop,
    ".pp1": parse_photon_workshop,
    ".pm7": parse_photon_workshop,
    ".pm7m": parse_photon_workshop,
}


def get_print_info(path):
    """Parse a sliced file and return a dict of metadata, or raise
    SlicedFileError if the format isn't recognized/parseable."""
    ext = Path(path).suffix.lower()
    handler = _EXT_HANDLERS.get(ext)
    if handler is None:
        raise SlicedFileError(f"{path}: unsupported extension {ext!r}")
    return handler(path)


def format_duration(seconds):
    seconds = int(seconds)
    h, rem = divmod(seconds, 3600)
    m, s = divmod(rem, 60)
    if h:
        return f"{h}h {m}m"
    return f"{m}m {s}s"


if __name__ == "__main__":
    import sys
    for path in sys.argv[1:]:
        try:
            info = get_print_info(path)
        except SlicedFileError as e:
            print(f"{path}: ERROR — {e}")
            continue
        dur = format_duration(info["estimated_seconds"])
        tag = "exact" if info["exact"] else "estimated"
        print(f"{path}")
        print(f"  machine: {info['machine_name']}")
        print(f"  layers:  {info['layer_count']}")
        print(f"  time:    {dur} ({tag}, {info['estimated_seconds']}s)")
        print()
