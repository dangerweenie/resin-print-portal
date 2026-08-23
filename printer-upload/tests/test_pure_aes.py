import forward_aes
from pure_aes import aes_cbc_decrypt_no_padding


def test_decrypt_matches_nist_vector():
    # NIST SP800-38A F.2.6 -- the same vector pure_aes.py self-tests via its
    # own __main__ block; promoted here so it runs under pytest.
    key = bytes.fromhex("603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4")[:32]
    iv = bytes.fromhex("000102030405060708090a0b0c0d0e0f")
    ct = bytes.fromhex("f58c4c04d6e5f1ba779eabfb5f7bfbd6")
    expected = bytes.fromhex("6bc1bee22e409f96e93d7e117393172a")
    assert aes_cbc_decrypt_no_padding(ct, key, iv) == expected


def test_forward_encrypt_matches_nist_vector():
    key = bytes.fromhex("603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4")[:32]
    iv = bytes.fromhex("000102030405060708090a0b0c0d0e0f")
    plaintext = bytes.fromhex("6bc1bee22e409f96e93d7e117393172a")
    expected_ct = bytes.fromhex("f58c4c04d6e5f1ba779eabfb5f7bfbd6")
    assert forward_aes.aes_cbc_encrypt_no_padding(plaintext, key, iv) == expected_ct


def test_encrypt_decrypt_round_trip_multi_block():
    key = bytes(range(32))
    iv = bytes(range(16))
    plaintext = b'0123456789ABCDEF' * 5  # 5 blocks, no padding needed
    ct = forward_aes.aes_cbc_encrypt_no_padding(plaintext, key, iv)
    assert aes_cbc_decrypt_no_padding(ct, key, iv) == plaintext
