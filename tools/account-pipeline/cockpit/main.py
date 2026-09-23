"""把 Codex 账号 JSON 交给本机 Cockpit Tools 导入。"""

from __future__ import annotations

import argparse
import ctypes
import json
import os
import socket
import subprocess
import sys
import uuid
from ctypes import wintypes
from pathlib import Path
from typing import Iterable, Optional
from urllib.parse import quote, urlsplit


MAX_IMPORT_BYTES = 8 * 1024 * 1024
MAX_HEADER_BYTES = 16 * 1024


class CockpitError(RuntimeError):
    """Cockpit Tools 导入错误。"""


def choose_input_file() -> Optional[Path]:
    try:
        import tkinter
        from tkinter import filedialog
    except ImportError as exc:
        raise CockpitError("未提供 JSON 文件，且当前 Python 不包含文件选择组件") from exc
    root = tkinter.Tk()
    root.withdraw()
    try:
        selected = filedialog.askopenfilename(
            title="选择 Codex 账号 JSON",
            filetypes=(("JSON files", "*.json *.jsonl"), ("All files", "*.*")),
        )
    finally:
        root.destroy()
    return Path(selected) if selected else None


def read_bundle(path: Path) -> bytes:
    path = path.expanduser().resolve()
    if not path.is_file():
        raise CockpitError(f"找不到 Cockpit 输入文件：{path}")
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise CockpitError(f"无法读取 Cockpit 输入文件：{path}") from exc
    if not raw:
        raise CockpitError("Cockpit 输入文件为空")
    if len(raw) > MAX_IMPORT_BYTES:
        raise CockpitError("Cockpit 输入文件超过 8 MiB")
    try:
        text = raw.decode("utf-8-sig")
        json.loads(text)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise CockpitError("Cockpit 输入文件必须是有效的 UTF-8 JSON") from exc
    return text.encode("utf-8")


def running_executables() -> Iterable[Path]:
    if os.name != "nt":
        return []

    class ProcessEntry32(ctypes.Structure):
        _fields_ = (
            ("dwSize", wintypes.DWORD),
            ("cntUsage", wintypes.DWORD),
            ("th32ProcessID", wintypes.DWORD),
            ("th32DefaultHeapID", ctypes.POINTER(ctypes.c_ulong)),
            ("th32ModuleID", wintypes.DWORD),
            ("cntThreads", wintypes.DWORD),
            ("th32ParentProcessID", wintypes.DWORD),
            ("pcPriClassBase", ctypes.c_long),
            ("dwFlags", wintypes.DWORD),
            ("szExeFile", wintypes.WCHAR * 260),
        )

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.CreateToolhelp32Snapshot.argtypes = (wintypes.DWORD, wintypes.DWORD)
    kernel32.CreateToolhelp32Snapshot.restype = wintypes.HANDLE
    kernel32.Process32FirstW.argtypes = (wintypes.HANDLE, ctypes.POINTER(ProcessEntry32))
    kernel32.Process32FirstW.restype = wintypes.BOOL
    kernel32.Process32NextW.argtypes = (wintypes.HANDLE, ctypes.POINTER(ProcessEntry32))
    kernel32.Process32NextW.restype = wintypes.BOOL
    kernel32.OpenProcess.argtypes = (wintypes.DWORD, wintypes.BOOL, wintypes.DWORD)
    kernel32.OpenProcess.restype = wintypes.HANDLE
    kernel32.QueryFullProcessImageNameW.argtypes = (
        wintypes.HANDLE,
        wintypes.DWORD,
        wintypes.LPWSTR,
        ctypes.POINTER(wintypes.DWORD),
    )
    kernel32.QueryFullProcessImageNameW.restype = wintypes.BOOL
    kernel32.CloseHandle.argtypes = (wintypes.HANDLE,)
    kernel32.CloseHandle.restype = wintypes.BOOL

    snapshot = kernel32.CreateToolhelp32Snapshot(0x00000002, 0)
    if snapshot == wintypes.HANDLE(-1).value:
        return []
    paths = []
    try:
        entry = ProcessEntry32()
        entry.dwSize = ctypes.sizeof(entry)
        has_entry = kernel32.Process32FirstW(snapshot, ctypes.byref(entry))
        while has_entry:
            if entry.szExeFile.lower() == "cockpit-tools.exe":
                process = kernel32.OpenProcess(0x1000, False, entry.th32ProcessID)
                if process:
                    try:
                        capacity = wintypes.DWORD(32768)
                        buffer = ctypes.create_unicode_buffer(capacity.value)
                        if kernel32.QueryFullProcessImageNameW(
                            process, 0, buffer, ctypes.byref(capacity)
                        ):
                            paths.append(Path(buffer.value))
                    finally:
                        kernel32.CloseHandle(process)
            has_entry = kernel32.Process32NextW(snapshot, ctypes.byref(entry))
    finally:
        kernel32.CloseHandle(snapshot)
    return paths


def registry_executables() -> Iterable[Path]:
    if os.name != "nt":
        return []
    import winreg

    paths = []
    locations = (
        (winreg.HKEY_CURRENT_USER, r"Software\Microsoft\Windows\CurrentVersion\Uninstall"),
        (winreg.HKEY_LOCAL_MACHINE, r"Software\Microsoft\Windows\CurrentVersion\Uninstall"),
        (winreg.HKEY_LOCAL_MACHINE, r"Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall"),
    )
    for hive, location in locations:
        try:
            parent = winreg.OpenKey(hive, location)
        except OSError:
            continue
        with parent:
            index = 0
            while True:
                try:
                    name = winreg.EnumKey(parent, index)
                except OSError:
                    break
                index += 1
                try:
                    with winreg.OpenKey(parent, name) as entry:
                        display_name = winreg.QueryValueEx(entry, "DisplayName")[0]
                        if display_name != "Cockpit Tools":
                            continue
                        install = str(
                            winreg.QueryValueEx(entry, "InstallLocation")[0]
                        ).strip().strip('"')
                        if install:
                            paths.append(Path(install) / "cockpit-tools.exe")
                except OSError:
                    continue
    return paths


def find_executable() -> Path:
    if os.name != "nt":
        raise CockpitError("Cockpit Tools 导入只支持 Windows")
    candidates = list(running_executables()) + list(registry_executables())
    local_app_data = os.environ.get("LOCALAPPDATA")
    if local_app_data:
        candidates.append(Path(local_app_data) / "Cockpit Tools" / "cockpit-tools.exe")
    seen = set()
    for candidate in candidates:
        normalized = str(candidate).lower()
        if normalized in seen:
            continue
        seen.add(normalized)
        if candidate.is_file():
            return candidate.resolve()
    raise CockpitError("找不到 Cockpit Tools；请先为当前 Windows 用户安装")


def receive_headers(client: socket.socket) -> bytes:
    chunks = bytearray()
    while b"\r\n\r\n" not in chunks:
        block = client.recv(2048)
        if not block:
            break
        chunks.extend(block)
        if len(chunks) > MAX_HEADER_BYTES:
            raise CockpitError("Cockpit Tools 本地请求头过大")
    return bytes(chunks)


def execute(input_file: Path, *, wait_seconds: int = 60) -> int:
    if not 10 <= wait_seconds <= 300:
        raise CockpitError("Cockpit 等待时间必须在 10 到 300 秒之间")
    payload = read_bundle(input_file)
    executable = find_executable()
    secret_path = "/" + uuid.uuid4().hex

    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        listener.settimeout(wait_seconds)
        port = listener.getsockname()[1]
        import_url = f"http://127.0.0.1:{port}{secret_path}"
        deep_link = (
            "cockpit-tools://import?provider=codex"
            f"&import_url={quote(import_url, safe='')}"
            "&auto_import=true&min_app_version=0.22.19"
        )
        print("正在打开 Cockpit Tools 并等待本地导入请求……")
        try:
            subprocess.Popen([str(executable), deep_link], close_fds=True)
        except OSError as exc:
            raise CockpitError("无法启动 Cockpit Tools") from exc
        try:
            client, _ = listener.accept()
        except socket.timeout as exc:
            raise CockpitError(f"Cockpit Tools 在 {wait_seconds} 秒内没有请求导入数据") from exc
        with client:
            client.settimeout(5)
            try:
                headers = receive_headers(client)
            except (OSError, socket.timeout) as exc:
                raise CockpitError("读取 Cockpit Tools 本地请求失败") from exc
            try:
                request_line = headers.split(b"\r\n", 1)[0].decode("ascii")
            except UnicodeDecodeError as exc:
                raise CockpitError("Cockpit Tools 发出了无效的本地请求") from exc
            parts = request_line.split()
            if (
                len(parts) != 3
                or parts[0] != "GET"
                or urlsplit(parts[1]).path != secret_path
                or parts[2] not in ("HTTP/1.0", "HTTP/1.1")
            ):
                raise CockpitError("Cockpit Tools 请求了意外的本地地址，请重新运行")
            response = (
                "HTTP/1.1 200 OK\r\n"
                "Content-Type: application/json; charset=utf-8\r\n"
                f"Content-Length: {len(payload)}\r\n"
                "Cache-Control: no-store\r\n"
                "Connection: close\r\n\r\n"
            ).encode("ascii")
            try:
                client.sendall(response + payload)
            except OSError as exc:
                raise CockpitError("向 Cockpit Tools 发送导入数据失败") from exc
    print("账号数据已交给 Cockpit Tools，请在应用中查看逐账号结果。")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="把 Codex 账号 JSON 导入 Cockpit Tools")
    parser.add_argument("input_file", nargs="?", type=Path)
    parser.add_argument("--wait-seconds", type=int, default=60)
    args = parser.parse_args()
    try:
        input_file = args.input_file or choose_input_file()
        if input_file is None:
            print("已取消导入。")
            return 2
        return execute(input_file, wait_seconds=args.wait_seconds)
    except (CockpitError, OSError, ValueError) as exc:
        print(f"Cockpit 导入失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
