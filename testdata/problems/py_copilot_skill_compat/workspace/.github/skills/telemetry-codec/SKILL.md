---
name: telemetry-codec
description: Binary/text frame encoding rules and status codes for telemetry packets
---

# Telemetry Codec Wire Specification (`telemetry.v2`)

Use this specification together with `.github/copilot-instructions.md` when implementing or debugging `codec.py`.

## 1. Binary Frame Layout (Big-Endian)

Every frame is a contiguous byte sequence:

```
+------------------+------------------+-----------------+-------------------+------------------+
| Header (10 B)    | Device ID (N B)  | Metrics (M B)   | CRC-16 (2 B)      |
+------------------+------------------+-----------------+-------------------+------------------+
```

### 10-Byte Fixed Header (`struct` format `">HBBBBHH"`)
| Byte Offset | `struct` | Field | Value / Constraint |
| --- | --- | --- | --- |
| `0..1` | `H` (uint16 BE) | `sync` | Must equal `FRAME_SYNC_WORD` (`0xCAFE`) |
| `2` | `B` (uint8) | `version` | Wire protocol version = `0x02` |
| `3` | `B` (uint8) | `status_code` | See Status Code Table below |
| `4` | `B` (uint8) | `flags` | `0..255` (defaults to `0` on encode if omitted) |
| `5` | `B` (uint8) | `device_id_len` | `1..64` (byte length of `device_id`) |
| `6..7` | `H` (uint16 BE) | `seq` | `0..65535` |
| `8..9` | `H` (uint16 BE) | `payload_len` | `0..65535` (byte length of UTF-8 JSON `metrics`) |

### Variable Sections
1. `device_id_bytes` (`device_id_len` bytes):
   - Printable ASCII bytes (`0x20 <= b <= 0x7E` for every byte), length `1..64`.
2. `payload_bytes` (`payload_len` bytes):
   - Canonical compact UTF-8 JSON representation of the `metrics` dictionary:
     `json.dumps(metrics, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")`
   - On decode, `payload_bytes` must be valid UTF-8 JSON that parses to a `dict` (reject JSON arrays/primitives with `FrameFormatError`).

### 2-Byte Trailer (`struct` format `">H"`)
- `crc16` (`uint16` big-endian): **CRC-16/CCITT-FALSE** computed over `frame[:-2]` (the 10-byte header + `device_id_bytes` + `payload_bytes`).

## 2. Status Code Table

| Status String | Wire Byte (`status_code`) |
| --- | --- |
| `"OK"` | `0x01` |
| `"WARN"` | `0x02` |
| `"CRIT"` | `0x03` |
| `"MAINT"` | `0x04` |

Any other status string on `encode_packet` or wire byte on `decode_packet` MUST raise `FrameFormatError`.

## 3. CRC-16/CCITT-FALSE Algorithm

- Polynomial: `0x1021`
- Initial value: `0xFFFF`
- Input reflection: `False`
- Output reflection: `False`
- Final XOR: `0x0000`

Reference implementation:
```python
def crc16_ccitt_false(data: bytes | memoryview) -> int:
    crc = 0xFFFF
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            if crc & 0x8000:
                crc = ((crc << 1) ^ 0x1021) & 0xFFFF
            else:
                crc = (crc << 1) & 0xFFFF
    return crc
```
Standard check value: `crc16_ccitt_false(b"123456789") == 0x29B1`.

## 4. Decode Validation Order

In `decode_packet(frame)`:
1. Require `isinstance(frame, (bytes, bytearray))` and `len(frame) >= 12` (10-byte header + 2-byte CRC); otherwise raise `FrameFormatError`.
2. Unpack the 10-byte header `>HBBBBHH`. Validate:
   - `sync == 0xCAFE`
   - `version == 0x02`
   - `1 <= device_id_len <= 64`
   - `len(frame) == 10 + device_id_len + payload_len + 2` (exact match; no truncated or trailing bytes!)
   If any of these fail, raise `FrameFormatError`.
3. Unpack trailing `expected_crc` (`>H` from `frame[-2:]`) and compare against `crc16_ccitt_false(frame[:-2])`. If unequal, raise `ChecksumMismatchError`.
4. Validate `status_code` (`0x01..0x04`), printable ASCII `device_id`, and UTF-8 JSON `dict` payload (`metrics`). Raise `FrameFormatError` on any violation.
5. Return the decoded dictionary envelope specified in `.github/copilot-instructions.md`.
