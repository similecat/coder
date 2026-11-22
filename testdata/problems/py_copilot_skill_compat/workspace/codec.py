"""Telemetry packet encoder/decoder."""

from __future__ import annotations


def encode_packet(packet: dict) -> bytes:
    """Encodes a telemetry packet dictionary into its wire frame."""
    raise NotImplementedError("encode_packet is not implemented")


def decode_packet(frame: bytes | bytearray) -> dict:
    """Decodes and validates a binary telemetry wire frame."""
    raise NotImplementedError("decode_packet is not implemented")
