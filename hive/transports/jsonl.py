"""JSONL projection transport for HIVE.

Appends cells as one-JSON-per-line to channel-named files.
Provides backward compatibility with the existing comms.sh readers.
This is a write-only projection -- reads go through SQLite.
"""
import json
import os

from hive.cell import Cell, cell_to_dict
from hive.config import channel_file_path


class JSONLTransport:
    """Append-only JSONL file writer, one file per channel."""

    def __init__(self, channels_dir: str):
        self._dir = channels_dir
        os.makedirs(self._dir, exist_ok=True)

    def put(self, cell: Cell) -> str:
        """Append cell to the channel's JSONL file.

        The path is validated (no traversal, no symlink) and opened with
        O_NOFOLLOW where available, so a pre-planted symlink cannot redirect
        the append to a file outside the channel directory.
        """
        filepath = channel_file_path(self._dir, cell.channel)
        line = json.dumps(cell_to_dict(cell), ensure_ascii=False, separators=(",", ":"))
        flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        fd = os.open(filepath, flags, 0o666)
        with os.fdopen(fd, "a", encoding="utf-8") as f:
            f.write(line + "\n")
        return cell.id
