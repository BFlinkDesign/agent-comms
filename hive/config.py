"""Shared runtime configuration and validation for HIVE.

This module stays stdlib-only so core transports, shell-adjacent scripts, MCP,
and dashboard code can agree on paths and channel safety without extra
dependencies.
"""
from __future__ import annotations

import os
import re


class ConfigError(ValueError):
    """Raised when runtime configuration or identifiers are unsafe."""


_CHANNEL_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")
CANONICAL_CHANNELS_DIR = "C:/Users/Brady.EAGLE/.ai/channels"
CANONICAL_DB_PATH = "C:/tools/agent-comms/hive.db"


def default_channels_dir() -> str:
    """Return the configured channel directory.

    HIVE_CHANNELS_DIR is the canonical env var. COMMS_CHANNELS remains a
    compatibility alias for existing shell workflows.
    """
    configured = os.environ.get("HIVE_CHANNELS_DIR") or os.environ.get("COMMS_CHANNELS")
    if configured:
        return configured
    return CANONICAL_CHANNELS_DIR


def default_db_path() -> str:
    """Return the configured SQLite database path."""
    configured = os.environ.get("HIVE_DB_PATH")
    if configured:
        return configured
    return CANONICAL_DB_PATH


def validate_channel_name(channel: str) -> str:
    """Validate and return a safe channel identifier.

    Channel names become filenames. Keep them intentionally small and boring:
    lowercase ASCII alphanumerics, underscores, and hyphens only.
    """
    if not isinstance(channel, str) or not _CHANNEL_RE.fullmatch(channel):
        raise ConfigError(
            "invalid channel name: use lowercase letters, numbers, underscores, "
            "or hyphens; start with a letter or number"
        )
    return channel


def channel_file_path(channels_dir: str, channel: str) -> str:
    """Return the JSONL path for a validated channel under channels_dir."""
    safe_channel = validate_channel_name(channel)
    base = os.path.realpath(channels_dir)
    path = os.path.abspath(os.path.join(base, f"{safe_channel}.jsonl"))
    real_path = os.path.realpath(path)
    if os.path.commonpath([base, real_path]) != base:
        raise ConfigError("channel path escapes channels directory")
    if os.path.islink(path):
        raise ConfigError("channel file must not be a symlink")
    return path
