# Project Coding Instructions — Telemetry Edge SDK

All modules in this repository MUST follow these conventions:

## Module Exports & Constants (`codec.py`)
1. Define the following module-level constants in `codec.py`:
   - `CODEC_SCHEMA_VERSION = "telemetry.v2"`
   - `FRAME_SYNC_WORD = 0xCAFE`
2. Define the following exception hierarchy in `codec.py`:
   - `TelemetryCodecError(ValueError)`: Base exception for all telemetry codec errors.
   - `ChecksumMismatchError(TelemetryCodecError)`: Raised by `decode_packet` when the trailing 16-bit frame checksum does not match the computed CRC-16 checksum.
   - `FrameFormatError(TelemetryCodecError)`: Raised on malformed frames, wrong sync word, unsupported protocol version, length mismatch/trailing bytes, invalid field ranges/types, non-ASCII `device_id`, unknown status values, or invalid metrics JSON objects.
3. Explicitly define `__all__` in `codec.py` containing at least:
   - `"CODEC_SCHEMA_VERSION"`
   - `"FRAME_SYNC_WORD"`
   - `"TelemetryCodecError"`
   - `"ChecksumMismatchError"`
   - `"FrameFormatError"`
   - `"encode_packet"`
   - `"decode_packet"`

## Packet Dictionary Envelope Contract
- `encode_packet(packet: dict) -> bytes`:
  Accepts a dictionary with:
  - `"device_id"` (`str`, required): printable ASCII string (`0x20..0x7E`), `1..64` bytes long.
  - `"seq"` (`int`, required): unsigned 16-bit sequence number `0..65535` (reject `bool`).
  - `"status"` (`str`, required): one of `"OK"`, `"WARN"`, `"CRIT"`, `"MAINT"`.
  - `"metrics"` (`dict`, required): JSON-serializable `dict` mapping metric names (`str`) to scalar values (`int`, `float`, `str`, `bool`).
  - `"flags"` (`int`, optional): unsigned 8-bit integer `0..255`, defaulting to `0` when omitted.
  Raise `FrameFormatError` if `packet` is not a `dict`, is missing any required key, or has any out-of-range/invalid field value.

- `decode_packet(frame: bytes | bytearray) -> dict`:
  Validates and decodes a binary telemetry frame (see the `telemetry-codec` skill for the binary frame layout, status codes, JSON encoding rules, and CRC-16 algorithm) and MUST return a `dict` with these exact 6 keys:
  ```python
  {
      "schema": CODEC_SCHEMA_VERSION,  # always "telemetry.v2"
      "device_id": str,
      "seq": int,
      "status": str,
      "flags": int,
      "metrics": dict,
  }
  ```
