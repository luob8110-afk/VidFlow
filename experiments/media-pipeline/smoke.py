"""End-to-end media contract check against a disposable VidFlow Compose stack."""

import argparse
import hashlib
import json
import os
import struct
import subprocess
import time
import uuid
import xml.etree.ElementTree as ET
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import ProxyHandler, Request, build_opener


opener = build_opener(ProxyHandler({}))


def request(url, method="GET", body=None, headers=None, expected=(200,)):
    try:
        response = opener.open(Request(url, data=body, headers=headers or {}, method=method), timeout=20)
    except HTTPError as exc:
        response = exc
    with response:
        data = response.read()
        if response.status not in expected:
            raise AssertionError(f"{method} {url.split('?')[0]}: HTTP {response.status} {data[:300]!r}")
        return data, response.headers, response.status


def api(base, route, body=None, token=None, method="POST", expected=(200,)):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    payload = json.dumps(body or {}).encode() if method == "POST" else None
    response, _, _ = request(base + "/api" + route, method, payload, headers, expected)
    return json.loads(response) if response else None


def wait_for(description, condition, timeout=45, interval=0.25):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if result := condition():
            return result
        time.sleep(interval)
    raise AssertionError(f"Timed out waiting for {description}")


def login(base):
    user = {"username": "media_" + uuid.uuid4().hex[:10], "password": "test-media-42"}
    api(base, "/account/register", user)
    return api(base, "/account/login", user)["token"]


def sample(container):
    data = subprocess.check_output(
        ["docker", "exec", container, "ffmpeg", "-v", "error", "-f", "lavfi", "-i",
         "testsrc2=size=360x640:rate=24", "-t", "3", "-c:v", "mpeg4", "-q:v", "5",
         "-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1"]
    )
    # Append a valid MP4 free box so the fixture always exercises two parts.
    size = (5 << 20) + 8192 - len(data)
    assert size > 8
    return data + struct.pack(">I4s", size, b"free") + bytes(size - 8)


def new_asset(base, token, content, sha256=None):
    descriptor = {
        "draft_id": uuid.uuid4().hex,
        "size": len(content),
        "sha256": sha256 or hashlib.sha256(content).hexdigest(),
    }
    return descriptor, api(base, "/video/assets", descriptor, token)


def part(base, token, asset, number, content, check_cors=False):
    path = f"/video/assets/{asset['asset_id']}/parts/{number}"
    url = api(base, path, token=token)["url"]
    size = asset["part_size"]
    blob = content[(number - 1) * size:number * size]
    if check_cors:
        _, headers, _ = request(url, "OPTIONS", headers={
            "Origin": base, "Access-Control-Request-Method": "PUT",
        }, expected=(200, 204))
        assert headers.get("Access-Control-Allow-Origin") in ("*", base)
        request(url, "PUT", blob + b"x", expected=(400, 403))
    _, headers, _ = request(url, "PUT", blob, headers={"Origin": base})
    assert headers.get("Access-Control-Allow-Origin") in ("*", base)
    return headers.get("ETag")


def status(base, token, asset):
    return api(base, f"/video/assets/{asset['asset_id']}", token=token, method="GET")


def wait_ready(base, token, asset, timeout=45):
    def probe():
        value = status(base, token, asset)
        if value["status"] == "failed":
            raise AssertionError(value["failure"])
        return value if value["status"] == "ready" else None
    return wait_for("ready media", probe, timeout=timeout)


def publish_and_check(base, token, asset):
    spec = {"title": "Media contract " + uuid.uuid4().hex[:6], "description": "#media",
            "asset_id": asset["asset_id"]}
    video = api(base, "/video/publish", spec, token)
    assert api(base, "/video/publish", spec, token)["id"] == video["id"]
    cover, hdr, _ = request(base + video["cover_url"])
    assert hdr.get_content_type() == "image/jpeg" and cover.startswith(b"\xff\xd8")
    sample_bytes, hdr, code = request(base + video["play_url"], headers={"Range": "bytes=0-63"}, expected=(206,))
    assert hdr.get_content_type() == "video/mp4" and code == 206
    assert len(sample_bytes) == 64 and sample_bytes[4:8] == b"ftyp"

    def feed_item():
        feed = api(base, "/feed/listLatest", {"limit": 20})
        return next((row for row in feed["video_list"] if row["id"] == video["id"]), None)
    assert wait_for("video visible in latest feed", feed_item)["play_url"] == video["play_url"]
    return video


def database_options(args):
    return dict(host="127.0.0.1", port=args.mysql_port, user="root",
                password=os.environ.get("MYSQL_ROOT_PASSWORD", "123456"),
                database=os.environ.get("MYSQL_DATABASE", "feedsystem"))


def storage_request(key, *options):
    auth = os.environ.get("MEDIA_S3_ACCESS_KEY", "vidflow-dev") + ":" + os.environ.get(
        "MEDIA_S3_SECRET_KEY", "vidflow-local-media-secret")
    bucket = os.environ.get("MEDIA_S3_BUCKET", "vidflow-media")
    origin = os.environ.get("MEDIA_S3_PUBLIC_ENDPOINT", "http://localhost:8333")
    return subprocess.run(["curl", "--silent", "--fail", "--max-time", "5",
                           "--aws-sigv4", "aws:amz:us-east-1:s3", "--user", auth,
                           *options, f"{origin}/{bucket}/{key}"], capture_output=True)


def reconcile_and_cleanup(args, token, content):
    import pymysql

    base = args.base
    descriptor, asset = new_asset(base, token, content)
    ident = asset["asset_id"]
    manifest = ET.Element("CompleteMultipartUpload")
    for number in range(1, asset["part_count"] + 1):
        etag = part(base, token, asset, number, content)
        assert etag
        entry = ET.SubElement(manifest, "Part")
        ET.SubElement(entry, "PartNumber").text = str(number)
        ET.SubElement(entry, "ETag").text = etag
    with pymysql.connect(**database_options(args), autocommit=True) as db, db.cursor() as cursor:
        cursor.execute("SELECT upload_id, status FROM assets WHERE id=%s", (ident,))
        upload_id, state = cursor.fetchone()
        assert state == "uploading"
        # Commit the object directly, as if the API exited before committing SQL.
        source = f"assets/{ident}/source"
        committed = storage_request(f"{source}?{urlencode({'uploadId': upload_id})}",
                                    "-H", "Content-Type: application/xml",
                                    "--data-binary", ET.tostring(manifest, encoding="unicode"))
        assert committed.returncode == 0 and b"CompleteMultipartUploadResult" in committed.stdout
        resumed = api(base, "/video/assets", descriptor, token)
        assert resumed["uploaded_parts"] == list(range(1, asset["part_count"] + 1))
        route = f"/video/assets/{ident}"
        api(base, f"{route}/complete", token=token)
        wait_ready(base, token, asset)
        print("PASS reconciled completed object before the SQL state update")
        api(base, route, token=token, method="DELETE", expected=(204,))

        def cleaned():
            cursor.execute("SELECT cleaned_at FROM assets WHERE id=%s AND status='expired'", (ident,))
            return cursor.fetchone()[0]

        wait_for("discarded draft cleanup", cleaned, timeout=90, interval=1)
        assert storage_request(source, "--head").returncode == 22
        print("PASS abandoned ready draft removed from object storage")


def crash_recovery(args, token, content):
    # Hold only the test asset row while waiting for both attempt outputs.
    # The worker is then blocked at its success commit, a deterministic crash point.
    import pymysql

    mysql = database_options(args)
    base = args.base
    _, asset = new_asset(base, token, content)
    for number in range(1, asset["part_count"] + 1):
        part(base, token, asset, number, content)
    ident = asset["asset_id"]
    api(base, f"/video/assets/{ident}/complete", token=token)
    with pymysql.connect(**mysql, autocommit=True) as db:
        with db.cursor() as cursor:
            def current_token():
                cursor.execute("SELECT lease_token FROM assets WHERE id=%s AND status='processing'", (ident,))
                row = cursor.fetchone()
                return row[0] if row else None
            original = wait_for("processing lease", current_token, interval=0.03)
    def uploaded(name):
        return storage_request(f"assets/{ident}/outputs/{original}/{name}", "--head").returncode == 0
    with pymysql.connect(**mysql) as db:
        with db.cursor() as cursor:
            cursor.execute("SELECT status, lease_token FROM assets WHERE id=%s FOR UPDATE", (ident,))
            assert cursor.fetchone() == ("processing", original), "test missed the commit window"
            wait_for("both attempt outputs", lambda: uploaded("play.mp4") and uploaded("cover.jpg"))
            subprocess.run(["docker", "stop", "--time", "0", args.worker], check=True, stdout=subprocess.DEVNULL)
            db.rollback()
    start = time.monotonic()
    subprocess.run(["docker", "start", args.worker], check=True, stdout=subprocess.DEVNULL)
    wait_ready(base, token, asset, timeout=250)
    recovery_seconds = time.monotonic() - start
    with pymysql.connect(**mysql) as db:
        with db.cursor() as cursor:
            cursor.execute("SELECT attempts, output_token FROM assets WHERE id=%s", (ident,))
            attempts, winner = cursor.fetchone()
            assert attempts == 2 and winner != original
    result = publish_and_check(base, token, asset)
    print(f"PASS crash after both uploads, before SQL commit; recovered in {recovery_seconds:.1f}s, video {result['id']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="http://localhost:5173")
    parser.add_argument("--worker", help="Worker container ID or name. Defaults to the current Compose worker.")
    parser.add_argument("--crash-recovery", action="store_true", help="Only on a disposable stack. Stops the worker.")
    parser.add_argument("--reconcile-storage", action="store_true", help="Simulate completion before SQL and verify draft cleanup.")
    parser.add_argument("--mysql-port", type=int, default=3307)
    args = parser.parse_args()
    if not args.worker:
        args.worker = subprocess.check_output(["docker", "compose", "ps", "-q", "worker"], text=True).strip()
        if not args.worker:
            parser.error("no running Compose worker found; pass --worker explicitly")
    base = args.base.rstrip("/")
    args.base = base
    content = sample(args.worker)
    owner, stranger = login(base), login(base)
    descriptor, asset = new_asset(base, owner, content)
    assert asset["part_count"] == 2 and asset["status"] == "uploading"
    ident = asset["asset_id"]
    route = f"/video/assets/{ident}"
    api(base, route, token=stranger, method="GET", expected=(404,))
    api(base, f"{route}/parts/1", token=stranger, expected=(404,))
    api(base, "/video/publish", {"title": "Premature", "asset_id": ident}, owner, expected=(409,))
    request(base + f"/media/{ident}/play.mp4", expected=(404,))
    part(base, owner, asset, 1, content, check_cors=True)
    api(base, f"{route}/complete", token=owner, expected=(409,))
    resumed = api(base, "/video/assets", descriptor, owner)
    assert resumed["asset_id"] == ident and resumed["uploaded_parts"] == [1]
    part(base, owner, asset, 2, content)
    started = time.monotonic()
    api(base, f"{route}/complete", token=owner)
    api(base, f"{route}/complete", token=owner)
    ready = wait_ready(base, owner, asset)
    ready_seconds = time.monotonic() - started
    assert ready["width"] == 360 and ready["height"] == 640
    assert 2900 <= ready["duration_ms"] <= 3100
    api(base, "/video/publish", {"title": "Other owner", "asset_id": ident}, stranger, expected=(404,))
    result = publish_and_check(base, owner, asset)
    print(f"PASS resume, ownership, size-bound signature, duplicate completion/publication, JPEG and MP4 Range, Feed; ready in {ready_seconds:.1f}s, video {result['id']}")
    _, wrong = new_asset(base, owner, content[:24], "0" * 64)
    part(base, owner, wrong, 1, content[:24])
    api(base, f"/video/assets/{wrong['asset_id']}/complete", token=owner)
    def failed():
        value = status(base, owner, wrong)
        return value if value["status"] == "failed" else None
    assert "校验" in wait_for("checksum rejection", failed)["failure"]
    api(base, "/video/publish", {"title": "Invalid", "asset_id": wrong["asset_id"]}, owner, expected=(409,))
    print("PASS checksum mismatch is rejected before publication")
    if args.reconcile_storage:
        reconcile_and_cleanup(args, owner, content)
    if args.crash_recovery:
        crash_recovery(args, owner, content)


if __name__ == "__main__":
    main()
