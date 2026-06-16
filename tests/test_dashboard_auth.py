"""Tests for dashboard Bearer-token auth and loopback bypass.

Requires FastAPI + httpx (installed alongside dashboard deps).
Skips gracefully when those packages are absent so the core test suite
still runs in lightweight envs.
"""
from __future__ import annotations

import os
import sys
from collections.abc import Generator
from pathlib import Path

import pytest

fastapi = pytest.importorskip("fastapi", reason="fastapi not installed")
httpx = pytest.importorskip("httpx", reason="httpx not installed")
from starlette.testclient import TestClient  # noqa: E402 — after importorskip


def _make_client(token: str | None = None, channels_dir: str | None = None) -> TestClient:
    """Reload dashboard.server with patched environment, return a TestClient."""
    env_patch: dict[str, str] = {}
    if token is not None:
        env_patch["HIVE_DASHBOARD_TOKEN"] = token
    if channels_dir is not None:
        env_patch["COMMS_CHANNELS"] = channels_dir

    # We need to reload the module so module-level env reads pick up our patches.
    old_env = {k: os.environ.get(k) for k in env_patch}
    try:
        for k, v in env_patch.items():
            os.environ[k] = v
        for k in list(old_env):
            if old_env[k] is None and k not in env_patch:
                os.environ.pop(k, None)

        # Force reload so _DASHBOARD_TOKEN is re-evaluated.
        if "dashboard.server" in sys.modules:
            del sys.modules["dashboard.server"]

        sys.path.insert(0, str(Path(__file__).parent.parent))
        import dashboard.server as srv  # type: ignore[import-untyped]

        return TestClient(srv.app, raise_server_exceptions=True)
    finally:
        # Restore env
        for k, orig in old_env.items():
            if orig is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = orig


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> Generator[None, None, None]:
    """Ensure no real HIVE_DASHBOARD_TOKEN leaks in from the test runner env."""
    monkeypatch.delenv("HIVE_DASHBOARD_TOKEN", raising=False)
    monkeypatch.setenv("COMMS_CHANNELS", str(tmp_path / "channels"))
    yield


class TestNoTokenConfigured:
    def test_root_always_accessible(self) -> None:
        client = _make_client()
        resp = client.get("/")
        assert resp.status_code == 200

    def test_api_agents_open_when_no_token(self) -> None:
        client = _make_client()
        resp = client.get("/api/agents")
        assert resp.status_code == 200

    def test_api_stats_open_when_no_token(self) -> None:
        client = _make_client()
        resp = client.get("/api/stats")
        assert resp.status_code == 200


class TestTokenConfigured:
    TOKEN = "s3cr3t-token"

    def test_loopback_detection_logic(self) -> None:
        if "dashboard.server" in sys.modules:
            del sys.modules["dashboard.server"]
        sys.path.insert(0, str(Path(__file__).parent.parent))
        import dashboard.server as srv  # type: ignore[import-untyped]

        assert srv._is_loopback("127.0.0.1") is True
        assert srv._is_loopback("::1") is True
        assert srv._is_loopback("10.0.0.1") is False
        assert srv._is_loopback("192.168.1.1") is False

    def test_is_loopback_ipv4(self) -> None:
        if "dashboard.server" in sys.modules:
            del sys.modules["dashboard.server"]
        sys.path.insert(0, str(Path(__file__).parent.parent))
        import dashboard.server as srv  # type: ignore[import-untyped]

        assert srv._is_loopback("127.0.0.1") is True
        assert srv._is_loopback("127.0.0.2") is True
        assert srv._is_loopback("10.0.0.1") is False

    def test_is_loopback_ipv6(self) -> None:
        if "dashboard.server" in sys.modules:
            del sys.modules["dashboard.server"]
        sys.path.insert(0, str(Path(__file__).parent.parent))
        import dashboard.server as srv  # type: ignore[import-untyped]

        assert srv._is_loopback("::1") is True
        assert srv._is_loopback("2001:db8::1") is False

    def test_is_loopback_hostname_fallback(self) -> None:
        if "dashboard.server" in sys.modules:
            del sys.modules["dashboard.server"]
        sys.path.insert(0, str(Path(__file__).parent.parent))
        import dashboard.server as srv  # type: ignore[import-untyped]

        assert srv._is_loopback("localhost") is True
        assert srv._is_loopback("example.com") is False

    def test_non_loopback_without_bearer_gets_401(self) -> None:
        """TestClient uses 'testclient' as host (not loopback) — should 401 when token set."""
        client = _make_client(token=self.TOKEN)
        resp = client.get("/api/channels")
        assert resp.status_code == 401

    def test_valid_bearer_token_accepted(self) -> None:
        client = _make_client(token=self.TOKEN)
        resp = client.get("/api/channels", headers={"Authorization": f"Bearer {self.TOKEN}"})
        assert resp.status_code == 200

    def test_wrong_bearer_token_rejected(self) -> None:
        client = _make_client(token=self.TOKEN)
        resp = client.get("/api/channels", headers={"Authorization": "Bearer wrong-token"})
        assert resp.status_code == 401

    def test_malformed_auth_header_rejected(self) -> None:
        client = _make_client(token=self.TOKEN)
        resp = client.get("/api/channels", headers={"Authorization": self.TOKEN})
        assert resp.status_code == 401
