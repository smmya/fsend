"""Exercise the built application in a real POSIX pseudo-terminal, not a pipe."""
import os
import fcntl
from pathlib import Path
import pty
import queue
import re
import select
import signal
import struct
import subprocess
import sys
import tempfile
import threading
import termios
import time


def terminal_printed_text(transcript):
    # tcell repositions the cursor after every wide glyph and clears the two
    # cells with spaces/backspaces before drawing it. Remove those controls,
    # but keep the original decoded characters: '?' must never become Chinese.
    text = bytes(transcript).decode('utf-8')
    text = re.sub(r'\x1b\][^\x07]*(?:\x07|\x1b\\)', '', text)
    text = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', text)
    text = re.sub(r'\x1b[()][0-2A-Z]', '', text)
    return text.replace('  \b\b', '')


def terminal_run(binary, cwd, args, keys, locale=None):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 28, 110, 0, 0))
    pid = os.fork()
    if pid == 0:
        os.close(master)
        os.setsid()
        fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
        for fd in (0, 1, 2):
            os.dup2(slave, fd)
        if slave > 2:
            os.close(slave)
        os.chdir(cwd)
        os.environ['TERM'] = 'xterm-256color'
        if locale is not None:
            for name in ('LC_ALL', 'LC_CTYPE', 'LANG'):
                os.environ.pop(name, None)
            os.environ.update(locale)
        os.execv(binary, [binary, *args])
    os.close(slave)
    transcript = bytearray()
    sent = False
    reaped = False
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if select.select([master], [], [], .1)[0]:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    data = b''
                transcript.extend(data)
                if not sent and b'<DIR>' in transcript:
                    os.write(master, keys)
                    sent = True
            child, status = os.waitpid(pid, os.WNOHANG)
            if child:
                reaped = True
                # The process can exit before its final terminal-reset bytes
                # have been consumed from the pseudo-terminal buffer.
                while select.select([master], [], [], 0)[0]:
                    try:
                        data = os.read(master, 65536)
                    except OSError:
                        break
                    if not data:
                        break
                    transcript.extend(data)
                assert os.waitstatus_to_exitcode(status) == 0, 'terminal process failed'
                assert sent, 'file browser did not render its directory listing'
                printed = terminal_printed_text(transcript)
                assert '文件浏览器' in printed, f'Chinese UI was replaced or misencoded: {printed!r}'
                assert '资料' in printed, f'Unicode filenames were replaced or misencoded: {printed!r}'
                assert b'\x1b[?1049l' in transcript, 'terminal alternate screen was not restored'
                return bytes(transcript)
        raise AssertionError('terminal process timed out')
    finally:
        if not reaped:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        os.close(master)


binary = str(Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory(prefix='fsend-terminal-') as directory:
    root = Path(directory)
    source, target = root / 'source', root / 'target'
    (source / '资料').mkdir(parents=True)
    (source / '资料' / 'alpha.txt').write_bytes(b'alpha')
    (source / 'beta.txt').write_bytes(b'beta')
    locales = [
        ('C overrides UTF-8', {'LC_ALL': 'C', 'LC_CTYPE': 'en_US.UTF-8', 'LANG': 'en_US.UTF-8'}),
        ('POSIX', {'LC_ALL': 'POSIX'}),
        ('LC_CTYPE C', {'LC_CTYPE': 'C', 'LANG': 'en_US.UTF-8'}),
        ('LANG C', {'LANG': 'C'}),
        ('UTF-8', {'LC_ALL': 'C.UTF-8'}),
        ('unset locale', {}),
    ]
    for label, locale in locales:
        terminal_run(binary, source, ['send', '--browse'], b'q', locale)
        print(f'Locale rendering passed: {label}, Chinese UI and Unicode paths remained UTF-8.')
    receiver = subprocess.Popen([binary, 'receive', '-o', str(target), '-b', '127.0.0.1', '--ip', '127.0.0.1', '-p', '0', '--wait', '30'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, encoding='utf-8')
    lines = queue.Queue()
    def read_lines():
        for line in receiver.stdout:
            lines.put(line)
    threading.Thread(target=read_lines, daemon=True).start()
    try:
        key = None
        while key is None:
            match = re.search(r'fs://\S+', lines.get(timeout=10))
            if match:
                key = match[0]
        # The first row is '..', then the directory, then the regular file.
        terminal_run(binary, source, ['send', '--browse', key], b'\x1b[By\x1b[B \x13', {'LC_ALL': 'C'})
        assert receiver.wait(timeout=10) == 0, 'receiver failed'
        assert (target / '资料' / 'alpha.txt').read_bytes() == b'alpha'
        assert (target / 'beta.txt').read_bytes() == b'beta'
        assert len(list(target.rglob('*.txt'))) == 2
        # Cancel exits before asking for a connection code and restores the TTY.
        cancelled = terminal_run(binary, source, ['send', '--browse'], b'q')
        assert '输入接收端混合密钥'.encode() not in cancelled
        print('Real terminal passed: multiselect directory/file, Unicode, encrypted transfer, cancellation, terminal restoration.')
    finally:
        if receiver.poll() is None:
            receiver.kill()
            receiver.wait()
