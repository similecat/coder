from __future__ import annotations

import json
import os
import struct
import sys
import unittest
from pathlib import Path


def _resolve_workspace(marker_file: str) -> Path:
    env_dir = os.environ.get("WORKSPACE_DIR")
    if env_dir:
        p = Path(env_dir).resolve()
        if (p / marker_file).exists():
            return p
    cwd = Path.cwd().resolve()
    if (cwd / marker_file).exists():
        return cwd
    here = Path(__file__).resolve().parent
    if (here / marker_file).exists():
        return here
    if (here.parent / marker_file).exists():
        return here.parent
    if (here.parent / "workspace" / marker_file).exists():
        return (here.parent / "workspace").resolve()
    return cwd


WORKSPACE_DIR = _resolve_workspace("codec.py")
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

import codec  # noqa: E402


def _ref_crc16(data: bytes) -> int:
    crc = 0xFFFF
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            if crc & 0x8000:
                crc = ((crc << 1) ^ 0x1021) & 0xFFFF
            else:
                crc = (crc << 1) & 0xFFFF
    return crc


class VerifyCopilotSkillCompatSuite(unittest.TestCase):
    def test_copilot_instructions_exports_and_constants(self) -> None:
        # Must adhere to .github/copilot-instructions.md
        self.assertEqual(getattr(codec, "CODEC_SCHEMA_VERSION", None), "telemetry.v2")
        self.assertEqual(getattr(codec, "FRAME_SYNC_WORD", None), 0xCAFE)

        expected_exports = {
            "CODEC_SCHEMA_VERSION",
            "FRAME_SYNC_WORD",
            "TelemetryCodecError",
            "ChecksumMismatchError",
            "FrameFormatError",
            "encode_packet",
            "decode_packet",
        }
        dunder_all = set(getattr(codec, "__all__", ()))
        self.assertTrue(
            expected_exports.issubset(dunder_all),
            f"Missing required __all__ exports: {expected_exports - dunder_all}",
        )

        self.assertTrue(issubclass(codec.TelemetryCodecError, ValueError))
        self.assertTrue(issubclass(codec.ChecksumMismatchError, codec.TelemetryCodecError))
        self.assertTrue(issubclass(codec.FrameFormatError, codec.TelemetryCodecError))

    def test_exact_wire_format_from_skill_and_roundtrip(self) -> None:
        pkt = {
            "device_id": "sensor-node-07",
            "seq": 1025,
            "status": "WARN",
            "flags": 0x15,
            "metrics": {"temp_c": 42.5, "fan_on": True, "rpm": 1800},
        }
        frame = codec.encode_packet(pkt)
        self.assertIsInstance(frame, bytes)

        # Verify exact binary layout from .github/skills/telemetry-codec/SKILL.md:
        # >HBBBBHH header + ASCII device_id + canonical sorted compact JSON + >H CRC-16/CCITT-FALSE
        expected_dev = b"sensor-node-07"
        expected_json = json.dumps(
            pkt["metrics"], sort_keys=True, separators=(",", ":"), ensure_ascii=False
        ).encode("utf-8")
        expected_header = struct.pack(
            ">HBBBBHH",
            0xCAFE,
            0x02,
            0x02,  # WARN -> 0x02
            0x15,
            len(expected_dev),
            1025,
            len(expected_json),
        )
        expected_body = expected_header + expected_dev + expected_json
        expected_crc = struct.pack(">H", _ref_crc16(expected_body))
        self.assertEqual(frame, expected_body + expected_crc)

        decoded = codec.decode_packet(frame)
        self.assertEqual(
            decoded,
            {
                "schema": "telemetry.v2",
                "device_id": "sensor-node-07",
                "seq": 1025,
                "status": "WARN",
                "flags": 0x15,
                "metrics": {"temp_c": 42.5, "fan_on": True, "rpm": 1800},
            },
        )

    def test_all_status_codes_and_default_flags(self) -> None:
        for idx, status in enumerate(["OK", "WARN", "CRIT", "MAINT"], start=1):
            frame = codec.encode_packet(
                {
                    "device_id": f"dev-{idx}",
                    "seq": idx * 10,
                    "status": status,
                    "metrics": {"code": idx},
                }
            )
            # Byte 3 is status_code (1..4), Byte 4 is default flags (0)
            self.assertEqual(frame[3], idx)
            self.assertEqual(frame[4], 0)
            decoded = codec.decode_packet(bytearray(frame))
            self.assertEqual(decoded["status"], status)
            self.assertEqual(decoded["flags"], 0)
            self.assertEqual(decoded["schema"], "telemetry.v2")

    def test_checksum_mismatch_raises_specific_exception(self) -> None:
        frame = bytearray(
            codec.encode_packet(
                {
                    "device_id": "edge-gw-1",
                    "seq": 7,
                    "status": "OK",
                    "metrics": {"v": 1},
                }
            )
        )
        # Flip one bit in payload so header lengths stay valid but CRC mismatches
        frame[-3] ^= 0x01
        with self.assertRaises(codec.ChecksumMismatchError):
            codec.decode_packet(bytes(frame))

        # Flip one bit in CRC trailer itself
        frame[-3] ^= 0x01
        frame[-1] ^= 0x80
        with self.assertRaises(codec.ChecksumMismatchError):
            codec.decode_packet(bytes(frame))

    def test_malformed_frames_and_invalid_inputs_raise_frame_format_error(self) -> None:
        valid = codec.encode_packet(
            {
                "device_id": "edge-1",
                "seq": 42,
                "status": "CRIT",
                "metrics": {"err": "overheat"},
            }
        )

        # Truncated frame
        with self.assertRaises(codec.FrameFormatError):
            codec.decode_packet(valid[:8])

        # Trailing garbage byte
        with self.assertRaises(codec.FrameFormatError):
            codec.decode_packet(valid + b"\x00")

        # Wrong sync word
        bad_sync = b"\xBE\xEF" + valid[2:]
        with self.assertRaises(codec.FrameFormatError):
            codec.decode_packet(bad_sync)

        # Wrong protocol version
        bad_ver = bytearray(valid)
        bad_ver[2] = 0x99
        bad_ver[-2:] = struct.pack(">H", _ref_crc16(bytes(bad_ver[:-2])))
        with self.assertRaises(codec.FrameFormatError):
            codec.decode_packet(bytes(bad_ver))

        # Unknown status wire byte (0x09) with valid CRC
        bad_status = bytearray(valid)
        bad_status[3] = 0x09
        bad_status[-2:] = struct.pack(">H", _ref_crc16(bytes(bad_status[:-2])))
        with self.assertRaises(codec.FrameFormatError):
            codec.decode_packet(bytes(bad_status))

        # Encode validation failures
        invalid_packets = [
            {"device_id": "", "seq": 1, "status": "OK", "metrics": {}},
            {"device_id": "x" * 65, "seq": 1, "status": "OK", "metrics": {}},
            {"device_id": "non-ascii-\u00e9", "seq": 1, "status": "OK", "metrics": {}},
            {"device_id": "ok", "seq": -1, "status": "OK", "metrics": {}},
            {"device_id": "ok", "seq": 70000, "status": "OK", "metrics": {}},
            {"device_id": "ok", "seq": True, "status": "OK", "metrics": {}},
            {"device_id": "ok", "seq": 1, "status": "UNKNOWN", "metrics": {}},
            {"device_id": "ok", "seq": 1, "status": "OK", "flags": 256, "metrics": {}},
            {"device_id": "ok", "seq": 1, "status": "OK", "metrics": [1, 2, 3]},
        ]
        for bad_pkt in invalid_packets:
            with self.assertRaises(codec.FrameFormatError):
                codec.encode_packet(bad_pkt)


if __name__ == "__main__":
    unittest.main()
