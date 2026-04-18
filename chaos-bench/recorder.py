"""recorder.py — append-only JSONL history writer.

Each operation produces one line: {op, task_id, start_ns, end_ns, status, ...}
Files are suitable for later replay into Porcupine via a Go adapter.
"""

from __future__ import annotations

import json
import os
import threading
import time
from typing import Any


class HistoryRecorder:
    def __init__(self, path: str) -> None:
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        self._f = open(path, "a", buffering=1)
        self._lock = threading.Lock()
        self.path = path

    def record(self, **fields: Any) -> None:
        fields.setdefault("wall_ns", time.time_ns())
        line = json.dumps(fields, default=str)
        with self._lock:
            self._f.write(line + "\n")

    def close(self) -> None:
        with self._lock:
            self._f.close()

    def __enter__(self) -> "HistoryRecorder":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()
