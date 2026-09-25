"""Drive the real CLI parser/forms in a test subprocess with a fake key provider."""
import errno
import json
import sqlite3
import os
import pty
import select
import signal
import subprocess
import sys
import termios
import time

binary, base = sys.argv[1:]
for mode in ("happy", "no", "ctrl-c", "signal", "enroll", "enroll-no", "enroll-cancel", "scope-mixed", "scope-color", "scope-limit", "scope-bulk-fail", "scope-bulk-cancel", "scope-no", "scope-cancel", "scope-fail"):
    root = os.path.join(base, mode)
    os.mkdir(root, 0o700)
    master, slave = pty.openpty()
    original = termios.tcgetattr(slave)
    env = dict(os.environ, DATA_MATE_P2_PTY_HELPER=mode,
               DATA_MATE_P2_PTY_ROOT=root, NO_COLOR="1")
    if mode == "scope-color":
        env.pop("NO_COLOR", None)
    proc = subprocess.Popen([binary, "-test.run=^TestConnectionTerminal$"],
                            stdin=slave, stdout=slave, stderr=slave, env=env,
                            start_new_session=True)
    transcript = bytearray()
    cursor = 0
    deadline = time.monotonic() + 8

    def read_more():
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise AssertionError(f"PTY timeout ({mode}): {transcript!r}")
        ready, _, _ = select.select([master], [], [], remaining)
        if ready:
            transcript.extend(os.read(master, 65536))

    def send_after(prompt, answer):
        global cursor
        target = prompt.encode()
        while transcript.find(target, cursor) < 0:
            read_more()
        cursor = transcript.index(target, cursor) + len(target)
        os.write(master, answer.encode())

    try:
        if mode.startswith("scope"):
            if mode != "scope-fail":
                send_after("Choose schemas", " ")
                if mode == "scope-mixed":
                    send_after('[ ] "Dot.Schema"', 'n')
                    send_after('schema0049', ' /')
                    send_after('Search (blank clears):', 'Dot\r')
                    send_after('[ ] "Dot.Schema"', 'm')
                    send_after('Whitelist · New schemas blocked', '')
                    send_after('[ ] "Dot.Schema"', 'm')
                    send_after('Blacklist · New schemas allowed', '\r')
                    send_after('Save changes? [y/N]:', 'y\r')
                    send_after('Choose schemas', 'a')
                    send_after('[x] "Dot.Schema"', 'a')
                    send_after('[ ] "Dot.Schema"', ' ')
                    send_after('[x] "Dot.Schema"', 'm')
                    send_after('Whitelist · New schemas blocked', '\r')
                    send_after('Save changes? [y/N]:', 'y\r')
                    for _ in range(2):
                        send_after('Connection number or alias:', '1\r')
                        send_after('Choose schemas', 'm')
                        send_after('New schemas', 'a')
                        send_after('Choose schemas', '\r')
                        send_after('Save changes? [y/N]:', 'y\r')
                elif mode == "scope-color":
                    send_after('Choose schemas', '\r')
                    send_after('Save changes? [y/N]:', 'y\r')
                elif mode == "scope-bulk-cancel":
                    send_after('[ ] "Dot.Schema"', 'a')
                    send_after('Loading all schemas', '')
                    proc.send_signal(signal.SIGINT)
                elif mode in ("scope-limit", "scope-bulk-fail"):
                    send_after('[ ] "Dot.Schema"', 'a')
                    send_after('Selection unchanged', '\r' if mode == "scope-limit" else 'q')
                    if mode == "scope-limit":
                        send_after('Save changes? [y/N]:', 'y\r')
                elif mode == "scope-no":
                    send_after('[ ] "Dot.Schema"', '\r')
                    send_after('Save changes? [y/N]:', '\r')
                else:
                    send_after('[ ] "Dot.Schema"', '\x03')
        elif mode.startswith("enroll"):
            send_after("SSH password:", "synthetic\r")
            send_after("Trust this SSH fingerprint? [y/N]:", "\r" if mode == "enroll-no" else "y\r")
            if mode != "enroll-no":
                send_after("Save changes? [y/N]:", "y\r" if mode == "enroll" else "n\r")
        else:
            send_after("Driver [postgres]:", "\r")
            send_after("Alias:", "analytics\r")
            send_after("Host:", "localhost\r")
            send_after("Port [5432]:", "\r")
            send_after("Database:", "app\r")
            send_after("Username:", "reader\r")
        if mode.startswith(("enroll", "scope")):
            pass
        elif mode == "signal":
            send_after("Password:", "")
            proc.send_signal(signal.SIGINT)
        elif mode == "ctrl-c":
            send_after("Password:", "hidden-cancel-secret\x03")
        else:
            send_after("Password:", "pty-hidden-secret\r")
            send_after("Save changes? [y/N]:", "y\r" if mode == "happy" else "\r")
            if mode == "happy":
                send_after("Save changes? [y/N]:", "n\r")
                send_after("Connection number or alias:", "1\r")
                send_after("Driver [postgres]:", "\r")
                send_after("Alias [analytics]:", "renamed\r")
                send_after("Host [localhost]:", "\r")
                send_after("Port [5432]:", "\r")
                send_after("Database [app]:", "\r")
                send_after("Username [reader]:", "\r")
                send_after("Password (blank keeps existing; --clear-password removes):", "\r")
                send_after("Save changes? [y/N]:", "y\r")
                send_after("Connection number or alias:", "renamed\r")
                send_after("Save changes? [y/N]:", "y\r")
        code = proc.wait(timeout=max(0.1, deadline-time.monotonic()))
        restored = termios.tcgetattr(slave)
        # Darwin sets the transient pending-input bit when canonical mode returns.
        # Check every persistent mode/control character, including echo and signals.
        restored[3] &= ~termios.PENDIN
        original[3] &= ~termios.PENDIN
        assert restored == original, ("terminal modes not restored", mode)
        os.close(slave)
        slave = None
        while True:
            try:
                chunk = os.read(master, 65536)
                if not chunk:
                    break
                transcript.extend(chunk)
            except OSError as err:
                if err.errno != errno.EIO:
                    raise
                break
        assert code == (0 if mode in ("happy", "enroll", "scope-mixed", "scope-color", "scope-limit") else 1 if mode == "scope-fail" else 130), (mode, code, transcript)
        assert b"pty-hidden-secret" not in transcript, transcript
        assert b"hidden-cancel-secret" not in transcript, transcript
        if mode == "scope-color":
            assert b"\x1b[1;36m" in transcript and b"\x1b[32m" in transcript, "picker color missing"
        else:
            assert b"\x1b[36m" not in transcript and b"\x1b[1;36m" not in transcript and b"\x1b[32m" not in transcript, "NO_COLOR ignored"
        if mode.startswith("scope"):
            with sqlite3.connect(os.path.join(root, "data-mate.db")) as db:
                scope = json.loads(db.execute("SELECT settings FROM profiles ORDER BY alias").fetchone()[0])["scope"]
                expected = {"mode": "whitelist"} if mode == "scope-mixed" else {"mode": "blacklist"}
                if mode in ("scope-color", "scope-limit"):
                    expected["schemas"] = ["Dot.Schema"]
                assert scope == expected, scope
        elif mode == "enroll":
            with open(os.path.join(root, "known_hosts")) as stream:
                assert 'ssh-ed25519' in stream.read(), "confirmed key missing"
            assert b'SHA256:' in transcript, "fingerprint not shown"
            assert b'synthetic' not in transcript, "SSH password echoed"
        elif mode == "happy":
            with sqlite3.connect(os.path.join(root, "data-mate.db")) as db:
                assert db.execute("SELECT count(*) FROM profiles").fetchone()[0] == 0
            assert b'reader' in transcript, "username must remain visible"
            # Raw-mode review must emit CRLF so subsequent lines start at column 0.
            assert b'\r\nAlias: analytics\r\nDriver: postgres\r\n' in transcript
        else:
            assert os.listdir(root) == [], "cancellation created state"
        for parent, _, names in os.walk(root):
            for name in names:
                with open(os.path.join(parent, name), 'rb') as stream:
                    assert b"pty-hidden-secret" not in stream.read(), name
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.wait()
        if slave is not None:
            os.close(slave)
        os.close(master)
print("PTY create/edit/remove, hidden input, default-No, Ctrl-C, SIGINT and restoration passed")
