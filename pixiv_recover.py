#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-only
"""Try to recover recently deleted Pixiv images still present on the CDN.

The script uses public metadata from neighbouring artwork IDs to infer the upload
time, then probes the small set of possible original-image URLs.  It does not
bypass authentication or access control; it only downloads URLs that the Pixiv
image CDN still serves publicly.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path


PIXIV = "https://www.pixiv.net"
CDN = "https://i.pximg.net"
JST = timezone(timedelta(hours=9))
USER_AGENT = "Mozilla/5.0 (compatible; pixiv-recover/1.0)"
MAGIC = {
    "jpg": (b"\xff\xd8\xff",),
    "jpeg": (b"\xff\xd8\xff",),
    "png": (b"\x89PNG\r\n\x1a\n",),
    "gif": (b"GIF87a", b"GIF89a"),
    "webp": (b"RIFF",),
}


@dataclass(frozen=True)
class Candidate:
    timestamp: datetime
    page: int
    ext: str

    def url(self, artwork_id: int) -> str:
        stamp = self.timestamp.astimezone(JST).strftime("%Y/%m/%d/%H/%M/%S")
        return f"{CDN}/img-original/img/{stamp}/{artwork_id}_p{self.page}.{self.ext}"


def request(url: str, *, byte_range: bool = False, timeout: float = 8.0):
    headers = {"User-Agent": USER_AGENT, "Referer": f"{PIXIV}/"}
    if byte_range:
        headers["Range"] = "bytes=0-31"
    return urllib.request.urlopen(
        urllib.request.Request(url, headers=headers), timeout=timeout
    )


def artwork_timestamp(artwork_id: int, timeout: float) -> datetime | None:
    """Return the precise timestamp exposed for a live neighbouring artwork."""
    url = f"{PIXIV}/ajax/illust/{artwork_id}"
    try:
        with request(url, timeout=timeout) as response:
            payload = json.load(response)
    except (OSError, ValueError, urllib.error.URLError):
        return None
    if payload.get("error") or not isinstance(payload.get("body"), dict):
        return None

    body = payload["body"]
    # The top-level createDate is sometimes rounded to :00.  Embedded cards
    # normally retain the actual second, so prefer those.
    cards = body.get("userIllusts") or {}
    card = cards.get(str(artwork_id)) if isinstance(cards, dict) else None
    values = [card.get("createDate") if isinstance(card, dict) else None]
    values.append(body.get("createDate"))
    for value in values:
        if not value:
            continue
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
            return parsed.astimezone(JST)
        except ValueError:
            pass
    return None


def infer_window(
    artwork_id: int, radius: int, timeout: float
) -> tuple[datetime, datetime, list[tuple[int, datetime]]]:
    ids = [artwork_id + offset for offset in range(-radius, radius + 1) if offset]
    found: list[tuple[int, datetime]] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(8, len(ids))) as pool:
        futures = {pool.submit(artwork_timestamp, item, timeout): item for item in ids}
        for future in concurrent.futures.as_completed(futures):
            stamp = future.result()
            if stamp is not None:
                found.append((futures[future], stamp))

    before = max((x for x in found if x[0] < artwork_id), default=None)
    after = min((x for x in found if x[0] > artwork_id), default=None)
    if before is None or after is None:
        raise RuntimeError(
            "无法从相邻 ID 同时找到前后时间；请增大 --radius，或用 --minute 手动指定。"
        )
    start = before[1].replace(microsecond=0)
    end = after[1].replace(microsecond=0)
    if end < start or end - start > timedelta(minutes=5):
        raise RuntimeError(f"推断出的时间窗口异常：{start.isoformat()} .. {end.isoformat()}")
    return start, end, sorted(found)


def valid_header(ext: str, data: bytes) -> bool:
    if ext == "webp":
        return data.startswith(b"RIFF") and data[8:12] == b"WEBP"
    return any(data.startswith(prefix) for prefix in MAGIC[ext])


def probe(artwork_id: int, candidate: Candidate, timeout: float) -> Candidate | None:
    try:
        with request(candidate.url(artwork_id), byte_range=True, timeout=timeout) as response:
            head = response.read(32)
        return candidate if valid_header(candidate.ext, head) else None
    except (OSError, urllib.error.HTTPError, urllib.error.URLError):
        return None


def find_page(
    artwork_id: int,
    timestamps: list[datetime],
    page: int,
    exts: list[str],
    workers: int,
    timeout: float,
) -> Candidate | None:
    candidates = [Candidate(stamp, page, ext) for stamp in timestamps for ext in exts]
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(probe, artwork_id, item, timeout) for item in candidates]
        for future in concurrent.futures.as_completed(futures):
            result = future.result()
            if result is not None:
                for other in futures:
                    other.cancel()
                return result
    return None


def download(
    artwork_id: int,
    candidate: Candidate,
    output: Path,
    timeout: float,
    retries: int,
) -> Path:
    output.mkdir(parents=True, exist_ok=True)
    destination = output / f"{artwork_id}_p{candidate.page}.{candidate.ext}"
    temporary = destination.with_suffix(destination.suffix + ".part")
    if destination.exists():
        with destination.open("rb") as handle:
            if valid_header(candidate.ext, handle.read(32)):
                return destination

    last_error: OSError | urllib.error.URLError | None = None
    for attempt in range(retries + 1):
        try:
            with request(candidate.url(artwork_id), timeout=max(timeout, 30.0)) as response:
                with temporary.open("wb") as handle:
                    while chunk := response.read(1024 * 1024):
                        handle.write(chunk)
            os.replace(temporary, destination)
            return destination
        except (OSError, urllib.error.URLError) as error:
            last_error = error
            if temporary.exists():
                temporary.unlink()
            if attempt < retries:
                time.sleep(min(2**attempt, 8))
    assert last_error is not None
    raise last_error


def parse_minute(value: str) -> datetime:
    try:
        return datetime.strptime(value, "%Y-%m-%dT%H:%M").replace(tzinfo=JST)
    except ValueError as error:
        raise argparse.ArgumentTypeError("格式应为 YYYY-MM-DDTHH:MM（日本时间）") from error


def seconds_between(start: datetime, end: datetime) -> list[datetime]:
    count = int((end - start).total_seconds())
    return [start + timedelta(seconds=n) for n in range(count + 1)]


def main() -> int:
    parser = argparse.ArgumentParser(description="枚举 Pixiv CDN URL，尝试找回近期删除的图片")
    parser.add_argument("id", type=int, help="作品 ID")
    parser.add_argument("--minute", type=parse_minute, help="已知投稿分钟（日本时间）")
    parser.add_argument("--radius", type=int, default=8, help="查询相邻 ID 的半径")
    parser.add_argument("--max-pages", type=int, default=20, help="最多尝试的页数")
    parser.add_argument("--ext", nargs="+", default=["jpg", "png", "gif", "webp"])
    parser.add_argument("--workers", type=int, default=12, help="并发探测数")
    parser.add_argument("--timeout", type=float, default=8.0, help="单请求超时秒数")
    parser.add_argument("--retries", type=int, default=3, help="下载失败重试次数")
    parser.add_argument("--output", type=Path, default=Path("recovered"))
    args = parser.parse_args()

    unknown = set(args.ext) - MAGIC.keys()
    if unknown:
        parser.error(f"不支持的扩展名：{', '.join(sorted(unknown))}")
    if args.minute:
        start, end = args.minute, args.minute + timedelta(seconds=59)
        print(f"使用手动时间窗口：{start.isoformat()} .. {end.isoformat()}")
    else:
        print(f"正在通过相邻 ID 推断作品 {args.id} 的投稿时间……")
        try:
            start, end, neighbours = infer_window(args.id, args.radius, args.timeout)
        except RuntimeError as error:
            print(f"错误：{error}", file=sys.stderr)
            return 2
        for neighbour_id, stamp in neighbours:
            print(f"  相邻作品 {neighbour_id}: {stamp.isoformat()}")
        print(f"推断时间窗口：{start.isoformat()} .. {end.isoformat()}")

    timestamps = seconds_between(start, end)
    print(f"探测 p0：{len(timestamps) * len(args.ext)} 个候选 URL")
    first = find_page(args.id, timestamps, 0, args.ext, args.workers, args.timeout)
    if first is None:
        print("未命中：CDN 文件可能已清理，或文件名包含不可枚举的哈希。")
        return 1

    print(f"命中：{first.url(args.id)}")
    saved = [download(args.id, first, args.output, args.timeout, args.retries)]
    # All pages share a timestamp.  The extension can differ, so keep probing it.
    exact_time = [first.timestamp]
    for page in range(1, args.max_pages):
        candidate = find_page(args.id, exact_time, page, args.ext, args.workers, args.timeout)
        if candidate is None:
            break
        saved.append(download(args.id, candidate, args.output, args.timeout, args.retries))
        print(f"命中：{candidate.url(args.id)}")

    print(f"完成，共保存 {len(saved)} 张：")
    for path in saved:
        print(f"  {path.resolve()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
