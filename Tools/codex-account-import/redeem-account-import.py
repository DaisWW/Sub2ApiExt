#!/usr/bin/env python3
"""从兑换站提取账号，保存结果并解压下载包。"""

from __future__ import annotations

import argparse
import csv
import io
import json
import os
import re
import secrets
import shutil
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from datetime import datetime
from http.cookiejar import Cookie, CookieJar
from pathlib import Path


BASE_URL = "https://redeem.plusproteam.xyz"
TOOL_DIR = Path(__file__).resolve().parent
CODES_FILE = TOOL_DIR / "redeem-codes.txt"
CACHE_DIR = TOOL_DIR / "cache"
RESULT_FILE = TOOL_DIR / "redeem-result.txt"
MAX_CODES = 200
MAX_TEXT_BYTES = 8000
MAX_ARCHIVE_BYTES = 64 * 1024 * 1024
MAX_UNCOMPRESSED_BYTES = 256 * 1024 * 1024
MAX_ARCHIVE_FILES = 1000

RESULT_COLUMNS = (
    "卡密",
    "账号",
    "首次提取时间",
    "剩余质保期",
    "结果",
    "说明",
)
STATUS_NAMES = {
    "queued": "排队中",
    "repairing": "处理中",
    "wait_nv": "处理中",
    "no_order": "未提取",
    "banned": "封禁",
    "workspace_banned": "封禁",
    "repair_failed": "修复异常",
    "quota_full": "额度已满",
}


class RedeemError(RuntimeError):
    pass


def shown(path: Path) -> str:
    return str(path.resolve())


def read_codes(path: Path) -> list[str]:
    if not path.is_file():
        raise RedeemError(f"找不到卡密文件：{shown(path)}")
    try:
        raw = path.read_bytes()
        try:
            text = raw.decode("utf-8-sig")
        except UnicodeDecodeError:
            try:
                text = raw.decode("gb18030")
            except UnicodeDecodeError as exc:
                raise RedeemError("卡密文件必须是 UTF-8 或 GB18030 编码") from exc
    except OSError as exc:
        raise RedeemError(f"读取卡密文件失败：{exc}") from exc
    except UnicodeDecodeError as exc:
        raise RedeemError("卡密文件必须是 UTF-8 编码") from exc
    if not raw:
        raise RedeemError(f"卡密文件为空：{shown(path)}")
    if len(raw) > MAX_TEXT_BYTES:
        raise RedeemError(f"卡密文件超过 {MAX_TEXT_BYTES} 字节限制")

    codes = []
    for number, line in enumerate(text.splitlines(), 1):
        code = line.strip()
        if not code or code.startswith("#"):
            continue
        if any(char.isspace() for char in code):
            raise RedeemError(f"第 {number} 行包含空格；请一行只填写一个卡密")
        codes.append(code)
    if not codes:
        raise RedeemError(f"卡密文件没有可用内容：{shown(path)}")
    if len(codes) > MAX_CODES:
        raise RedeemError(f"一次最多支持 {MAX_CODES} 个卡密")
    if len("\n".join(codes).encode("utf-8")) > MAX_TEXT_BYTES:
        raise RedeemError(f"卡密内容超过网页的 {MAX_TEXT_BYTES} 字节限制")
    return codes


def multipart(fields: dict[str, str]) -> tuple[bytes, str]:
    boundary = "----CodexRedeem" + secrets.token_hex(12)
    parts = []
    for name, value in fields.items():
        parts.append(
            (
                f"--{boundary}\r\n"
                f'Content-Disposition: form-data; name="{name}"\r\n\r\n'
            ).encode()
        )
        parts.extend((value.encode(), b"\r\n"))
    parts.append(f"--{boundary}--\r\n".encode())
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


def add_cookie(jar: CookieJar, host: str, name: str, value: str) -> None:
    jar.set_cookie(
        Cookie(
            version=0,
            name=name,
            value=value,
            port=None,
            port_specified=False,
            domain=host,
            domain_specified=True,
            domain_initial_dot=False,
            path="/",
            path_specified=True,
            secure=True,
            expires=None,
            discard=True,
            comment=None,
            comment_url=None,
            rest={},
            rfc2109=False,
        )
    )


class RedeemClient:
    def __init__(self) -> None:
        self.cookies = CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.cookies)
        )
        self.fingerprint = secrets.token_hex(16)
        host = urllib.parse.urlsplit(BASE_URL).hostname
        if not host:
            raise RedeemError("兑换站地址无效")
        add_cookie(self.cookies, host, "nv_fp", self.fingerprint)
        self.headers = {
            "User-Agent": (
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                "AppleWebKit/537.36 (KHTML, like Gecko) "
                "Chrome/140.0.0.0 Safari/537.36"
            ),
            "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
            "Referer": BASE_URL + "/",
            "Origin": BASE_URL,
            "Accept-Encoding": "identity",
        }

    def request(
        self,
        method: str,
        path: str,
        data: bytes | None = None,
        content_type: str | None = None,
        accept: str = "*/*",
        timeout: float = 60,
    ):
        headers = dict(self.headers)
        headers["Accept"] = accept
        if content_type:
            headers["Content-Type"] = content_type
        request = urllib.request.Request(
            BASE_URL + path, data=data, headers=headers, method=method
        )
        try:
            return self.opener.open(request, timeout=timeout)
        except urllib.error.HTTPError as exc:
            body = exc.read(4096).decode("utf-8", "replace")
            try:
                parsed = json.loads(body)
                detail = parsed.get("error", "") if isinstance(parsed, dict) else ""
            except (TypeError, ValueError):
                detail = ""
            suffix = f"：{detail}" if detail else ""
            raise RedeemError(f"兑换站请求失败（HTTP {exc.code}）{suffix}") from exc
        except (urllib.error.URLError, TimeoutError, socket.timeout) as exc:
            raise RedeemError(f"连接兑换站失败：{exc}") from exc

    def start(self) -> None:
        with self.request("GET", "/", accept="text/html", timeout=30) as response:
            response.read(1024 * 1024)

    def submit(self, codes: list[str], action: str) -> str:
        body, content_type = multipart(
            {"code": "\n".join(codes), "action": action, "fp": self.fingerprint}
        )
        with self.request(
            "POST", "/extract", body, content_type, "application/json", 60
        ) as response:
            try:
                result = json.loads(response.read().decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise RedeemError("兑换站返回了无效结果") from exc
        if not isinstance(result, dict) or result.get("ok") is not True:
            message = result.get("error") if isinstance(result, dict) else ""
            raise RedeemError(str(message or "提交卡密失败"))
        task_id = result.get("task_id")
        if not isinstance(task_id, str) or not re.fullmatch(
            r"[A-Za-z0-9_-]{8,128}", task_id
        ):
            raise RedeemError("兑换站没有返回有效任务编号")
        print(f"任务已提交：{task_id}（共 {result.get('total', len(codes))} 个）")
        return task_id

    def wait(self, task_id: str) -> dict:
        with self.request(
            "GET",
            f"/extract/events/{task_id}",
            accept="text/event-stream",
            timeout=180,
        ) as response:
            event = ""
            data = []
            while True:
                line = response.readline()
                if not line:
                    raise RedeemError("任务连接提前结束")
                line = line.decode("utf-8", "replace").rstrip("\r\n")
                if line.startswith("event:"):
                    event = line[6:].strip()
                elif line.startswith("data:"):
                    data.append(line[5:].lstrip())
                elif not line and data:
                    try:
                        payload = json.loads("\n".join(data))
                    except json.JSONDecodeError as exc:
                        raise RedeemError("兑换站返回了无效任务结果") from exc
                    data = []
                    if event == "progress":
                        print(
                            f"处理进度：{payload.get('done', '?')}"
                            f"/{payload.get('total', '?')}"
                        )
                    elif event == "done":
                        return payload
                    elif event == "fail":
                        raise RedeemError(str(payload.get("message") or "任务失败"))
                    event = ""

    def download(self, task_id: str) -> tuple[str, bytes]:
        with self.request(
            "GET",
            f"/extract/download/{task_id}",
            accept="application/zip, application/octet-stream, */*",
            timeout=180,
        ) as response:
            chunks = []
            size = 0
            while True:
                chunk = response.read(1024 * 1024)
                if not chunk:
                    break
                size += len(chunk)
                if size > MAX_ARCHIVE_BYTES:
                    raise RedeemError("下载压缩包超过 64 MiB 限制")
                chunks.append(chunk)
            disposition = response.headers.get("Content-Disposition", "")
        match = re.search(
            r"filename\*=UTF-8''([^;]+)|filename=([^;]+)", disposition, re.I
        )
        filename = "accounts.zip"
        if match:
            filename = urllib.parse.unquote(
                (match.group(1) or match.group(2)).strip('" ')
            )
            filename = Path(filename).name
            if not filename.lower().endswith(".zip"):
                filename = "accounts.zip"
        return filename, b"".join(chunks)


def extract_zip(blob: bytes, destination: Path) -> list[Path]:
    try:
        archive = zipfile.ZipFile(io.BytesIO(blob))
    except zipfile.BadZipFile as exc:
        raise RedeemError("下载内容不是有效的 ZIP 压缩包") from exc
    destination.mkdir(parents=True, exist_ok=True)
    root = destination.resolve()
    extracted = []
    total = 0
    with archive:
        members = archive.infolist()
        if len(members) > MAX_ARCHIVE_FILES:
            raise RedeemError(f"压缩包文件数超过 {MAX_ARCHIVE_FILES} 个")
        for info in members:
            name = info.filename.replace("\\", "/")
            if not name or name.endswith("/"):
                continue
            parts = Path(name).parts
            if Path(name).is_absolute() or ".." in parts:
                raise RedeemError(f"压缩包包含不安全路径：{info.filename}")
            if (info.external_attr >> 16) & 0o170000 == 0o120000:
                raise RedeemError(f"压缩包包含不支持的符号链接：{info.filename}")
            total += info.file_size
            if total > MAX_UNCOMPRESSED_BYTES:
                raise RedeemError("压缩包解压后超过 256 MiB 限制")
            target = (destination / name).resolve()
            if os.path.commonpath((str(root), str(target))) != str(root):
                raise RedeemError(f"压缩包包含不安全路径：{info.filename}")
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(info) as source, target.open("wb") as output:
                shutil.copyfileobj(source, output, 1024 * 1024)
            extracted.append(target)
    return extracted


def field(value) -> str:
    if value is None:
        return ""
    return str(value).replace("\t", " ").replace("\r", " ").replace("\n", " ").strip()


def status(row: dict) -> str:
    if row.get("ok") is True:
        return "正常"
    return STATUS_NAMES.get(str(row.get("state") or ""), "失败")


def save_result(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        with temporary.open("w", encoding="utf-8-sig", newline="") as stream:
            writer = csv.writer(stream, delimiter="\t", lineterminator="\n")
            writer.writerow(RESULT_COLUMNS)
            for row in rows:
                writer.writerow(
                    (
                        field(row.get("code")),
                        field(row.get("account")),
                        field(row.get("extracted_at")),
                        field(row.get("warranty_left")),
                        status(row),
                        field(row.get("message")),
                    )
                )
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise RedeemError(f"写入结果文件失败：{exc}") from exc


def show_results(rows: list[dict]) -> None:
    print(f"\n检测结果（{len(rows)} 个）：")
    print("\t".join(RESULT_COLUMNS[:6]))
    for row in rows:
        print(
            "\t".join(
                (
                    field(row.get("code")),
                    field(row.get("account")) or "-",
                    field(row.get("extracted_at")) or "-",
                    field(row.get("warranty_left")) or "-",
                    status(row),
                    field(row.get("message")) or "-",
                )
            )
        )


def write_manifest(
    path: Path,
    *,
    task_id: str,
    result_file: Path,
    rows: list[dict],
    archive_path: Path | None = None,
    data_dir: Path | None = None,
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = {
        "version": 1,
        "task_id": task_id,
        "result_file": str(result_file.resolve()),
        "archive": str(archive_path.resolve()) if archive_path else None,
        "data_dir": str(data_dir.resolve()) if data_dir else None,
        "total": len(rows),
        "success": sum(1 for row in rows if row.get("ok") is True),
        "failed": sum(1 for row in rows if row.get("ok") is not True),
    }
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise RedeemError(f"写入 manifest 失败：{exc}") from exc


def run(args: argparse.Namespace) -> int:
    input_path = args.input_file or CODES_FILE
    result_file = Path(getattr(args, "result_file", None) or RESULT_FILE)
    cache_dir = Path(getattr(args, "cache_dir", None) or CACHE_DIR)
    requested_run_dir = getattr(args, "run_dir", None)
    manifest_file = getattr(args, "manifest_file", None)
    codes = read_codes(input_path)
    print(f"已读取 {len(codes)} 个卡密：{shown(input_path)}")
    client = RedeemClient()
    client.start()
    task_id = client.submit(codes, args.action)
    payload = client.wait(task_id)
    if not isinstance(payload, dict):
        raise RedeemError("兑换站返回了无效任务结果")
    raw_rows = payload.get("rows", [])
    rows = (
        [row for row in raw_rows if isinstance(row, dict)]
        if isinstance(raw_rows, list)
        else []
    )
    show_results(rows)
    save_result(result_file, rows)
    print(f"结果已覆盖写入：{shown(result_file)}")

    if args.action == "check":
        if manifest_file:
            write_manifest(
                Path(manifest_file),
                task_id=task_id,
                result_file=result_file,
                rows=rows,
            )
        return 0 if rows and all(row.get("ok") is True for row in rows) else 2

    temporary_archive = None
    try:
        filename, blob = client.download(task_id)
        run_dir = Path(requested_run_dir) if requested_run_dir else cache_dir / (
            f"run-{datetime.now().strftime('%Y%m%d-%H%M%S')}-{task_id[:8]}"
        )
        run_dir.mkdir(parents=True, exist_ok=False)
        archive_path = run_dir / filename
        temporary_archive = archive_path.with_name(archive_path.name + ".tmp")
        temporary_archive.write_bytes(blob)
        os.replace(temporary_archive, archive_path)
        data_dir = run_dir / "data"
        files = extract_zip(blob, data_dir)
    except (RedeemError, OSError, ValueError) as exc:
        if temporary_archive is not None:
            try:
                temporary_archive.unlink(missing_ok=True)
            except OSError:
                pass
        print(f"下载或解压失败：{exc}", file=sys.stderr)
        return 1
    print(f"压缩包已缓存：{shown(archive_path)}")
    print(f"已解压 {len(files)} 个文件：{shown(data_dir)}")
    if manifest_file:
        write_manifest(
            Path(manifest_file),
            task_id=task_id,
            result_file=result_file,
            rows=rows,
            archive_path=archive_path,
            data_dir=data_dir,
        )
    return 0 if rows and all(row.get("ok") is True for row in rows) else 2


def main() -> int:
    parser = argparse.ArgumentParser(
        description="提交卡密，下载并解压账号包；不传文件时读取 redeem-codes.txt。"
    )
    parser.add_argument("input_file", nargs="?", type=Path)
    parser.add_argument(
        "--action",
        choices=("sub2api", "cpa", "check"),
        default="sub2api",
        help="默认 sub2api；check 只检测不下载",
    )
    parser.add_argument("--cache-dir", type=Path, default=CACHE_DIR)
    parser.add_argument("--run-dir", type=Path)
    parser.add_argument("--result-file", type=Path, default=RESULT_FILE)
    parser.add_argument("--manifest-file", type=Path)
    args = parser.parse_args()
    try:
        return run(args)
    except (RedeemError, OSError, ValueError) as exc:
        print(f"失败：{exc}", file=sys.stderr)
        try:
            save_result(args.result_file, [])
            print(f"结果已覆盖写入：{shown(args.result_file)}", file=sys.stderr)
        except RedeemError as result_error:
            print(f"结果文件也写入失败：{result_error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
