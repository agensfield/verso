"""Exercise terminal progress in the real binary with a slow, isolated process probe."""

import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time

binary = Path(sys.argv[1]).resolve(strict=True)
with tempfile.TemporaryDirectory(prefix="verso-progress-smoke-") as temporary:
    root = Path(temporary)
    native = root / ".codex"
    commands = root / "bin"
    native.mkdir(mode=0o755)
    commands.mkdir()
    (native / "config.toml").write_text('cli_auth_credentials_store = "file"\n')
    probe = commands / "ps"
    probe.write_text("#!/bin/sh\nsleep 0.7\nexit 0\n")
    probe.chmod(0o700)
    env = {"HOME": str(root), "CODEX_HOME": str(native), "VERSO_HOME": str(root / "state"),
           "PATH": str(commands) + ":/usr/bin:/bin", "TERM": "xterm", "NO_COLOR": "1",
           "HTTPS_PROXY": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1"}

    def terminal(args, cancel=False, dumb=False):
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 40, 0, 0))
        process = subprocess.Popen([str(binary), *args], cwd=root,
                                   env={**env, "TERM": "dumb" if dumb else "xterm"},
                                   stdin=subprocess.DEVNULL, stdout=slave, stderr=slave)
        os.close(slave)
        output = bytearray()
        deadline = time.monotonic() + 10
        sent = False
        try:
            while time.monotonic() < deadline:
                if select.select([master], [], [], 0.1)[0]:
                    try:
                        chunk = os.read(master, 65536)
                    except OSError as error:
                        if error.errno == errno.EIO:
                            break
                        raise
                    if not chunk:
                        break
                    output.extend(chunk)
                if cancel and not sent and b"\x1b[2K" in output:
                    process.send_signal(signal.SIGINT)
                    sent = True
            code = process.wait(timeout=2)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            os.close(master)
        return code, bytes(output)

    code, output = terminal(["status"])
    assert code == 0, (code, output)
    assert output.count(b"\r\x1b[2K") >= 3, output
    assert b"\x1b[36m" not in output, output
    result = output.index(b"Account:")
    assert b"\x1b" not in output[result:], output
    assert b"\r\x1b[2K" in output[:result], output
    code, output = terminal(["status"], cancel=True)
    assert code != 0 and b"verso:" in output, (code, output)
    assert b"\x1b" not in output[output.index(b"Account:"):], output
    code, output = terminal(["status", "--json"])
    assert code == 0 and json.loads(output)["ok"] and b"\x1b" not in output, output
    code, output = terminal(["status"], dumb=True)
    assert code == 0 and b"\x1b" not in output and b"Inspecting" not in output, output
    result = subprocess.run([str(binary), "status"], cwd=root, env=env,
                            capture_output=True, timeout=10)
    assert result.returncode == 0 and result.stderr == b"", result
    assert not (root / "state").exists(), "status created Verso state"
print("PASS: real-binary transient progress, SIGINT cleanup, JSON/dumb/pipe silence")
