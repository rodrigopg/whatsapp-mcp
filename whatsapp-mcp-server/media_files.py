"""Files the MCP server reads to send and writes after a download, on its own machine.

The bridge may run on another machine, so the server moves media as bytes: it reads a file the
client named (send_file, send_audio_message) and uploads it, and it saves downloaded media into a
local folder. The client asking may be an AI that just read an untrusted message, so reading
follows the bridge's rule for media_path (whatsapp-bridge/media_path.go):

- with MEDIA_ALLOWED_ROOTS set (a path list), the file must be inside one of those folders;
- without it, inside the temp folder, /tmp, the Downloads folder (or XDG_DOWNLOAD_DIR), with no
  hidden component (a name starting with ".") below that folder;
- either way, a file directly inside the download folder (download_dir) may be sent: forwarding
  downloaded media works;
- a bridge store found on the way (a folder holding whatsapp.db, when bridge and MCP server share
  a machine) is never entered, whatever the roots say, except for its downloaded media
  (store/<jid>/<file>), which may always be sent: the session keys and history stay put;
- regular files only, at most MAX_MEDIA_BYTES; opened one component at a time without following
  links and checked on the open file, so a path swapped after the check is refused.

Errors are MediaError with a reason and no path.
"""

import os
import stat
import tempfile

MAX_MEDIA_BYTES = 100 << 20

_CHANGED = "the path changed while it was being read"
_STORE = "files inside the bridge store cannot be sent, except downloaded media"
_HIDDEN = 'path has a hidden component (a name starting with "."); set MEDIA_ALLOWED_ROOTS to allow a folder'


class MediaError(Exception):
    """A reason safe to return to the MCP client: it never carries a path."""


def _within(root, path):
    try:
        return os.path.commonpath([root, path]) == root
    except ValueError:
        return False


def _downloads_folders():
    home = os.path.expanduser("~")
    folders = [os.path.join(home, "Downloads")]
    cfg = os.environ.get("XDG_CONFIG_HOME") or os.path.join(home, ".config")
    try:
        with open(os.path.join(cfg, "user-dirs.dirs"), encoding="utf-8") as f:
            lines = f.read().splitlines()
    except OSError:
        lines = []
    for line in lines:
        line = line.strip()
        if line.startswith("XDG_DOWNLOAD_DIR="):
            d = os.path.normpath(line[len("XDG_DOWNLOAD_DIR="):].strip('"').replace("$HOME", home, 1))
            # "$HOME/" is what xdg-user-dirs writes for a missing folder; never open the whole home.
            if os.path.isabs(d) and not _within(d, home):
                folders.append(d)
    return folders


def default_roots():
    roots = [tempfile.gettempdir()]
    if os.name != "nt":
        roots.append("/tmp")  # nosec B108 -- a folder files may be sent from, nothing is written there
    return roots + _downloads_folders()


def download_dir():
    """Where download_media saves files: WHATSAPP_DOWNLOAD_DIR, else <Downloads>/whatsapp."""
    configured = os.environ.get("WHATSAPP_DOWNLOAD_DIR", "").strip()
    return os.path.abspath(os.path.expanduser(configured)) if configured else os.path.join(_downloads_folders()[0], "whatsapp")


def _store_rest(parts, i, is_store):
    """Inside a bridge store only <jid>/<file> passes; returns whether hidden names are fine now."""
    if not is_store:
        return False
    if len(parts) - i != 2 or "@" not in parts[i]:
        raise MediaError(_STORE)
    return True


def _has_session_db(fd):
    try:
        os.stat("whatsapp.db", dir_fd=fd, follow_symlinks=False)
        return True
    except OSError:
        return False


def _open_walk(root, parts, hidden_ok):
    """Open root/parts... for reading without following any link; see the module docstring."""
    if os.open not in os.supports_dir_fd or not hasattr(os, "O_NOFOLLOW"):
        return _open_plain(root, parts, hidden_ok)
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for i, name in enumerate(parts):
            hidden_ok = _store_rest(parts, i, _has_session_db(fd)) or hidden_ok
            if not hidden_ok and name.startswith("."):
                raise MediaError(_HIDDEN)
            want = os.stat(name, dir_fd=fd, follow_symlinks=False)
            last = i == len(parts) - 1
            if last and not stat.S_ISREG(want.st_mode):
                raise MediaError("not a regular file")
            if not last and not stat.S_ISDIR(want.st_mode):
                raise MediaError(_CHANGED)
            flags = os.O_RDONLY | os.O_NOFOLLOW | (os.O_NONBLOCK if last else os.O_DIRECTORY)
            nxt = os.open(name, flags, dir_fd=fd)
            got = os.fstat(nxt)
            os.close(fd)
            fd = nxt
            if (got.st_dev, got.st_ino) != (want.st_dev, want.st_ino):
                raise MediaError(_CHANGED)
        f, fd = os.fdopen(fd, "rb"), None
        return f
    finally:
        if fd is not None:
            os.close(fd)


def _open_plain(root, parts, hidden_ok):
    # ponytail: Windows has no dir_fd; open by name and compare with a stat taken before.
    for i, name in enumerate(parts):
        hidden_ok = _store_rest(parts, i, os.path.lexists(os.path.join(root, *parts[:i], "whatsapp.db"))) or hidden_ok
        if not hidden_ok and name.startswith("."):
            raise MediaError(_HIDDEN)
    path = os.path.join(root, *parts)
    want = os.lstat(path)
    if not stat.S_ISREG(want.st_mode):
        raise MediaError("not a regular file")
    f = open(path, "rb")
    got = os.fstat(f.fileno())
    if (got.st_dev, got.st_ino) != (want.st_dev, want.st_ino):
        f.close()
        raise MediaError(_CHANGED)
    return f


def _open_checked(path):
    resolved = os.path.realpath(os.path.abspath(path))
    chat_dir = os.path.dirname(resolved)
    store = os.path.dirname(chat_dir)
    if "@" in os.path.basename(chat_dir) and os.path.lexists(os.path.join(store, "whatsapp.db")):
        return _open_walk(store, [os.path.basename(chat_dir), os.path.basename(resolved)], True)
    downloads = os.path.realpath(download_dir())
    if os.path.dirname(resolved) == downloads:
        try:
            return _open_walk(downloads, [os.path.basename(resolved)], False)
        except MediaError:
            pass  # not a downloaded file after all; the folder rules below still apply
    configured = [r for r in os.environ.get("MEDIA_ALLOWED_ROOTS", "").split(os.pathsep) if r]
    roots = configured or default_roots()
    err = MediaError(
        "path is outside MEDIA_ALLOWED_ROOTS" if configured else
        "files can be sent only from the Downloads folder, the temp folder or downloaded media; "
        "move the file there or set MEDIA_ALLOWED_ROOTS to allow its folder"
    )
    for root in roots:
        r = os.path.realpath(root)
        if not os.path.isabs(r) or not os.path.isdir(r) or not _within(r, resolved) or r == resolved:
            continue
        try:
            return _open_walk(r, os.path.relpath(resolved, r).split(os.sep), bool(configured))
        except MediaError as e:
            err = e
    raise err


def read_outbound(path):
    """Bytes of the file the client asked to send, after the checks in the module docstring."""
    if not path or "\0" in path:
        raise MediaError("file not found")
    try:
        with _open_checked(path) as f:
            if os.fstat(f.fileno()).st_size > MAX_MEDIA_BYTES:
                raise MediaError(f"file is larger than {MAX_MEDIA_BYTES >> 20} MB")
            data = f.read(MAX_MEDIA_BYTES + 1)
    except FileNotFoundError:
        raise MediaError("file not found on this machine (the one running the WhatsApp MCP server)") from None
    except PermissionError:
        raise MediaError("no permission to read the file") from None
    except OSError:
        raise MediaError("the file could not be read") from None
    if len(data) > MAX_MEDIA_BYTES:
        raise MediaError(f"file is larger than {MAX_MEDIA_BYTES >> 20} MB")
    return data


def plain_name(name):
    """name itself if it is a single plain file name, else MediaError: refused, never cleaned
    (same rule as safeMediaPath and checkUploadName in the bridge)."""
    if (not name or name in (".", "..") or len(name.encode()) > 255 or any(c in name for c in "/\\\0")
            or any(ord(c) < 0x20 or ord(c) == 0x7f for c in name)):
        raise MediaError("the media has an unsafe file name")
    return name


def local_name(message_id, filename):
    """<message_id>_<filename>, as the bridge names it. Both come from WhatsApp."""
    for part in (message_id, filename):
        plain_name(part)
    if ".." in message_id or message_id.startswith("."):
        raise MediaError("the media has an unsafe file name")
    name = f"{message_id}_{filename}"
    if len(name.encode()) > 255:
        stem, ext = os.path.splitext(filename)
        name = f"{message_id}_{stem[:100]}{ext[:20]}"
    return name


def save_download(chunks, message_id, filename):
    """Write downloaded media into download_dir() and return its absolute path. The folder is
    created 0700, the file 0600 through a unique temp name, then renamed into place."""
    name = local_name(message_id, filename)
    folder = download_dir()
    if _within(os.path.realpath(folder), os.path.realpath(os.path.expanduser("~"))):
        raise MediaError("WHATSAPP_DOWNLOAD_DIR must not be the home folder or a folder above it")
    if not os.path.isdir(folder):
        os.makedirs(folder, mode=0o700, exist_ok=True)
        os.chmod(folder, 0o700)
    fd, tmp = tempfile.mkstemp(dir=folder, prefix=".part-")
    try:
        with os.fdopen(fd, "wb") as f:
            for chunk in chunks:
                f.write(chunk)
        final = os.path.join(folder, name)
        os.replace(tmp, final)
    except BaseException:
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise
    return os.path.abspath(final)
