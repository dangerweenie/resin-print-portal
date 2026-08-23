"""Test-only forward AES-256-CBC ENCRYPT, needed only to build the synthetic
encrypted-.ctb fixture used by test_sliced_file_info.py. pure_aes.py itself
is decrypt-only in production ("Decrypt-only path used/tested here" per its
own docstring) -- this reuses its existing primitives (Sbox, gmul,
key_expansion, add_round_key, bytes_to_state, state_to_bytes) and adds the
missing forward-direction operations (mirroring pure_aes.py's inv_sub_bytes/
inv_shift_rows/inv_mix_columns). Never imported by application code.
"""
from pure_aes import Sbox, add_round_key, bytes_to_state, gmul, key_expansion, state_to_bytes


def sub_bytes(state):
    for r in range(4):
        for c in range(4):
            state[r][c] = Sbox[state[r][c]]


def shift_rows(state):
    for r in range(1, 4):
        state[r] = state[r][r:] + state[r][:r]


def mix_columns(state):
    for c in range(4):
        a = [state[r][c] for r in range(4)]
        state[0][c] = gmul(a[0], 2) ^ gmul(a[1], 3) ^ a[2] ^ a[3]
        state[1][c] = a[0] ^ gmul(a[1], 2) ^ gmul(a[2], 3) ^ a[3]
        state[2][c] = a[0] ^ a[1] ^ gmul(a[2], 2) ^ gmul(a[3], 3)
        state[3][c] = gmul(a[0], 3) ^ a[1] ^ a[2] ^ gmul(a[3], 2)


def encrypt_block(block, w, Nr):
    state = bytes_to_state(block)
    add_round_key(state, w, 0)
    for round_ in range(1, Nr):
        sub_bytes(state)
        shift_rows(state)
        mix_columns(state)
        add_round_key(state, w, round_)
    sub_bytes(state)
    shift_rows(state)
    add_round_key(state, w, Nr)
    return state_to_bytes(state)


def aes_cbc_encrypt_no_padding(plaintext, key, iv):
    assert len(plaintext) % 16 == 0
    w, Nr = key_expansion(key)
    out = bytearray()
    prev = iv
    for i in range(0, len(plaintext), 16):
        block = bytes(a ^ b for a, b in zip(plaintext[i:i + 16], prev))
        encrypted = encrypt_block(block, w, Nr)
        out.extend(encrypted)
        prev = encrypted
    return bytes(out)
