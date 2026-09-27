"""Drive the real CLI parser/forms in a test subprocess with a fake key provider."""
import errno
import fcntl
import struct
import re
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
base_duration = catalog_duration = 0.0
for mode in ("keyring-create", "keyring-unlock", "keyring-cancel", "keyring-noninteractive", "happy", "no", "ctrl-c", "signal", "enroll", "enroll-no", "enroll-cancel", "diagnostics", "describe", "describe-cancel", "catalog-layout", "catalog-short", "catalog-pager", "catalog-cancel", "catalog-no-pager", "catalog-json", "catalog-redirected"):
    mode_started = time.monotonic()
    print("PTY mode: " + mode, file=sys.stderr, flush=True)
    root = os.path.join(base, mode)
    os.mkdir(root, 0o700)
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    original = termios.tcgetattr(slave)
    env = dict(os.environ, DATA_MATE_P2_PTY_HELPER=mode,
               DATA_MATE_P2_PTY_ROOT=root, NO_COLOR="1", TERM="xterm-256color", LESS="--INVALID-OPTION", LESSOPEN="|false %s")
    proc = subprocess.Popen([binary, "-test.run=^TestConnectionTerminal$"],
                            stdin=subprocess.DEVNULL if mode in ("diagnostics", "catalog-layout", "catalog-redirected") else slave,
                            stdout=slave, stderr=slave, env=env,
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
        if mode == "keyring-noninteractive":
            pass
        elif mode.startswith("catalog"):
            if mode == "catalog-layout":
                send_after("catalog json\r\n", "")
                send_after("catalog end\r\n", "")
            elif mode in ("catalog-pager", "catalog-cancel"):
                send_after("item_0000", "")
                if mode == "catalog-cancel":
                    proc.send_signal(signal.SIGINT)
                else:
                    send_after(":", "/item_0100\r")
                    send_after("item_0101_", " ")
                    send_after("item_0123_", "q")
                send_after("catalog done\r\n", "")
            else:
                send_after("catalog done\r\n", "")
        elif mode == "diagnostics":
            # Drain output while the child runs so the PTY buffer cannot block
            # the JSON write before proc.wait completes.
            send_after("diagnostics json\r\n", "")
            send_after("diagnostics end\r\n", "")
        elif mode.startswith("describe"):
            send_after("Connection number or alias:", "1\r" if mode == "describe" else "\x03")
            if mode == "describe":
                # Drain the result before closing the slave; Darwin can discard
                # unread terminal output on its last close.
                send_after('{"version":', "")
                send_after('\r\n', "")
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
        if mode.startswith(("enroll", "describe", "catalog")) or mode in ("diagnostics", "keyring-noninteractive"):
            pass
        elif mode == "signal":
            send_after("Password:", "")
            proc.send_signal(signal.SIGINT)
        elif mode == "ctrl-c":
            send_after("Password:", "hidden-cancel-secret\x03")
        else:
            send_after("Password:", "pty-hidden-secret\r")
            send_after("Save changes? [y/N]:", "y\r" if mode == "happy" or mode.startswith("keyring") else "\r")
            if mode.startswith("keyring"):
                if mode == "keyring-unlock":
                    send_after("Keyring password:", "pty-wrong-keyring-secret\r")
                    send_after("Keyring password:", "pty-keyring-secret\r")
                else:
                    send_after("Create keyring? [y/N]:", "y\r")
                    send_after("New keyring password:", "pty-keyring-secret\r" if mode != "keyring-cancel" else "pty-keyring-secret\x03")
                    if mode != "keyring-cancel":
                        send_after("Confirm keyring password:", "pty-mismatch-secret\r")
                        send_after("New keyring password:", "pty-keyring-secret\r")
                        send_after("Confirm keyring password:", "pty-keyring-secret\r")
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
        while select.select([master], [], [], 0)[0]:
            try:
                chunk = os.read(master, 65536)
                if not chunk:
                    break
                transcript.extend(chunk)
            except OSError as err:
                if err.errno != errno.EIO:
                    raise
                break
        assert code == (1 if mode=="keyring-noninteractive" else 0 if mode in ("keyring-create", "keyring-unlock", "happy", "enroll", "diagnostics", "describe") or (mode.startswith("catalog") and mode != "catalog-cancel") else 130), (mode, code, transcript)
        assert b"pty-hidden-secret" not in transcript, transcript
        assert b"hidden-cancel-secret" not in transcript, transcript
        for secret in (b"pty-keyring-secret", b"pty-wrong-keyring-secret", b"pty-mismatch-secret"):
            assert secret not in transcript, (mode, transcript)
        if mode == "catalog-layout":
            for variant in ("color", "narrow", "no-color", "empty-no-color", "json"):
                block = transcript.split(("catalog " + variant + "\r\n").encode(), 1)[1].split(b"catalog end\r\n", 1)[0]
                if variant == "json":
                    assert b"\x1b" not in block, block
                    report = json.loads(block)
                    assert report["schemas"][0]["enums"] == [{"name":"status"}], report
                    assert report["schemas"][0]["sequences"] == [{"name":"items_id_seq"}], report
                    assert report["schemas"][0]["indexes"] == [{"name":"alpha_idx"}], report
                    assert report["schemas"][0]["functions"] == [{"name":"lookup"}], report
                    continue
                plain = re.sub(rb"\x1b\[[0-9;]*m", b"", block)
                if variant in ("color", "narrow"):
                    for tint, name in ((b"35", b"status"), (b"32", b"alpha"), (b"36", b"report"), (b"33", b"items_id_seq"), (b"34", b"alpha_idx"), (b"31", b"lookup"), (b"1", b"public")):
                        assert b"\x1b["+tint+b"m"+name+b"\x1b[0m" in block, block
                else:
                    assert b"\x1b" not in block, block
                if variant == "narrow":
                    assert b"  alpha\r\n  beta" in plain, plain
                else:
                    assert b"  alpha  beta  delta  gamma\r\n" in plain, plain
        elif mode == "catalog-json":
            report = json.loads(transcript.split(b"catalog done",1)[0])
            assert len(report["schemas"][0]["tables"]) == 1500, report
        elif mode in ("catalog-no-pager", "catalog-redirected"):
            assert b"item_1499_" in transcript and b"\x1b" not in transcript, transcript
        elif mode == "catalog-pager":
            assert b"item_0101_" in transcript and b"item_1499_" not in transcript, transcript
        elif mode.startswith("catalog"):
            assert b"catalog done" in transcript, transcript
        elif mode == "diagnostics":
            for output in ("color", "no-color", "empty-no-color", "json"):
                block = transcript.split(("diagnostics " + output + "\r\n").encode(), 1)[1].split(b"diagnostics end\r\n", 1)[0]
                lines = block.splitlines()
                assert lines[-1] == b"one or more connection checks failed", block
                if output == "json":
                    assert b"\x1b" not in block, block
                    report = json.loads(lines[0])
                    assert report["version"] == 1 and len(report["results"]) == 2, report
                    assert [r["ok"] for r in report["results"]] == [False, True], report
                    assert len(report["results"][1]["stages"]) == 6, report
                else:
                    passed = b"\x1b[32mPASS\x1b[0m" if output == "color" else b"PASS"
                    failed = b"\x1b[31mFAIL\x1b[0m" if output == "color" else b"FAIL"
                    assert lines[:-1] == [b"bad  " + failed,
                                         b"  authentication: CONNECT_FAILED: authentication failed",
                                         b"good  " + passed], block
        elif mode == "describe":
            assert b'{"version"' in transcript, (mode, transcript)
            report, _ = json.JSONDecoder().raw_decode(transcript[transcript.index(b'{"version"'):].decode())
            assert report["alias"] == "analytics" and len(report["schemas"]) == 4, report
        else:
            assert b"\x1b[36m" not in transcript and b"\x1b[1;36m" not in transcript and b"\x1b[32m" not in transcript, "NO_COLOR ignored"
        if mode == "enroll":
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
        elif mode != "diagnostics" and not mode.startswith(("describe", "catalog", "keyring")):
            assert os.listdir(root) == [], "cancellation created state"
        for parent, _, names in os.walk(root):
            for name in names:
                with open(os.path.join(parent, name), 'rb') as stream:
                    raw = stream.read()
                    for secret in (b"pty-hidden-secret", b"pty-keyring-secret", b"pty-mismatch-secret"):
                        assert secret not in raw, name
    finally:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait()
        if slave is not None:
            os.close(slave)
        os.close(master)
    if mode.startswith("catalog"):
        catalog_duration += time.monotonic() - mode_started
    else:
        base_duration += time.monotonic() - mode_started
print(f"PTY duration: existing modes {base_duration:.3f}s; added catalog modes {catalog_duration:.3f}s")
print("PTY create/edit/remove, hidden input, default-No, Ctrl-C, SIGINT, restoration, diagnostics colors, catalog grids and paging passed")
