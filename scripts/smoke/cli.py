"""Check public CLI contracts with isolated synthetic accounts and no external network."""

import base64
import errno
import json
import os
from pathlib import Path
import pty
import select
import signal
import subprocess
import sys
import tempfile
import time

if len(sys.argv) != 2:
    raise SystemExit("usage: python3 scripts/smoke/cli.py /path/to/verso")
binary = Path(sys.argv[1]).resolve(strict=True)

with tempfile.TemporaryDirectory(prefix="verso-cli-smoke-") as temporary:
    root = Path(temporary)
    home = root / "home"
    native = home / ".codex"
    state = root / "state"
    commands = root / "bin"
    native.mkdir(parents=True, mode=0o755)
    commands.mkdir()

    def write(path, content, mode=0o600):
        path.write_text(content)
        path.chmod(mode)

    write(commands / "ps", "#!/bin/sh\nexit 0\n", 0o700)
    write(commands / "codex", '#!/bin/sh\nif [ "$1" = "--version" ]; then echo "codex-cli 0.154.0"; else exit 99; fi\n', 0o700)
    write(native / "config.toml", 'cli_auth_credentials_store = "file"\n')
    env = {
        "HOME": str(home), "CODEX_HOME": str(native), "VERSO_HOME": str(state),
        "PATH": str(commands) + ":/usr/bin:/bin", "TERM": "xterm-256color",
        "HTTPS_PROXY": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1", "NO_PROXY": "",
    }

    def run(*args, success=True):
        result = subprocess.run([str(binary), *args], env=env, cwd=root,
                                capture_output=True, text=True, timeout=10)
        assert (result.returncode == 0) == success, (args, result.returncode, result.stdout, result.stderr)
        return result

    def machine(*args, success=True):
        result = run("--json", *args, success=success)
        payload = json.loads(result.stdout)
        assert payload["schema"] == "verso/v1"
        assert payload["ok"] == success
        assert "\x1b" not in result.stdout
        return payload

    assert machine("list", "--cached")["accounts"] == []
    for args in [("help",), ("help", "list"), ()]:
        machine(*args)
    for args in [("--bogus",), ("--state-dir",), ("list", "--cached=invalid")]:
        machine(*args, success=False)
    assert not state.exists(), "offline discovery created state"

    def jwt(claims):
        encoded = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
        return "e30." + encoded + ".synthetic"

    def login(account):
        claims = {"email": "shared@example.test", "https://api.openai.com/auth": {
            "chatgpt_user_id": "fixture-user", "chatgpt_account_id": account}}
        write(native / "auth.json", json.dumps({"tokens": {
            "id_token": jwt(claims), "access_token": jwt({"exp": 4102444800}),
            "refresh_token": "synthetic-only", "account_id": account}}))

    for account in ("personal", "work"):
        login(account)
        machine("import", account)
    login("personal")
    original = (native / "auth.json").read_bytes()
    before = machine("list", "--cached")["accounts"]
    machine("remove", "work", "--check", success=False)
    machine("remove", "work", "--cached", success=False)
    assert machine("list", "--cached")["accounts"] == before
    machine("alias", "work", "office")
    assert machine("list", "office", "--cached")["accounts"][0]["alias"] == "office"
    missing = machine("list", "missing", "--cached", success=False)
    assert not missing.get("accounts"), "failed filter returned unrelated accounts"
    assert "personal" not in run("list", "missing", "--cached", success=False).stdout
    assert (native / "auth.json").read_bytes() == original

    os.mkfifo(state / "switch.json", 0o600)
    machine("recovery", success=False)
    (state / "switch.json").unlink()

    master, slave = pty.openpty()
    process = subprocess.Popen([str(binary), "switch"], env=env, cwd=root,
                               stdin=slave, stdout=slave, stderr=slave)
    os.close(slave)
    output = b""
    deadline = time.monotonic() + 10
    try:
        while b"Account number" not in output and time.monotonic() < deadline:
            if process.poll() is not None:
                break
            if select.select([master], [], [], 0.1)[0]:
                try:
                    output += os.read(master, 65536)
                except OSError as exc:
                    if exc.errno != errno.EIO:
                        raise
                    break
        assert b"Account number" in output, output.decode(errors="replace")
        process.send_signal(signal.SIGINT)
        process.wait(timeout=3)
        assert process.returncode != 0, "interrupted picker reported success"
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)
    assert (native / "auth.json").read_bytes() == original
    assert not (state / "switch.json").exists()

print("PASS: JSON/discovery, flag refusal, aliases, filtered errors, FIFO recovery, and picker SIGINT; synthetic state only.")
