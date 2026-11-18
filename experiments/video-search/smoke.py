"""Run against a fresh, disposable Compose project with a built but stopped worker."""

import argparse
import base64
import datetime
import json
from pathlib import Path
import runpy
import subprocess
import time
import uuid


media = runpy.run_path(str(Path(__file__).resolve().parents[1] / "media-pipeline" / "smoke.py"))
api = media["api"]
request = media["request"]
wait_for = media["wait_for"]


def search(base, expected=(200,), **params):
    return api(base, "/search/videos", params, expected=expected)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", required=True, help="Disposable Compose project name")
    parser.add_argument("--file", action="append", help="Compose file, repeat for overrides")
    parser.add_argument("--base", default="http://127.0.0.1:5173")
    args = parser.parse_args()
    base = args.base.rstrip("/")
    compose = ["docker", "compose", "-p", args.project]
    for path in args.file or ():
        compose.extend(["-f", path])

    def docker(*command, input_data=None):
        return subprocess.check_output(compose + list(command), input=input_data).decode("utf-8").strip()

    def sql(statement):
        return docker("exec", "-T", "mysql", "sh", "-c",
                      'mysql -N -B --default-character-set=utf8mb4 -uroot -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE" 2>/dev/null',
                      input_data=statement.encode("utf-8"))

    def search_queue():
        table = docker("exec", "-T", "rabbitmq", "rabbitmqctl", "-q", "list_queues",
                       "name", "messages_ready", "messages_unacknowledged")
        for line in table.splitlines():
            queue, *counts = line.split()
            if queue == "video.search.index.queue":
                return sum(map(int, counts))
        raise AssertionError("search queue missing")

    if "worker" in docker("ps", "--status", "running", "--services").splitlines():
        parser.error("start with a stopped worker and no existing search index")
    search(base, expected=(503,), q="银河")
    token = media["login"](base)
    identity = token.split(".")[1]
    identity = json.loads(base64.urlsafe_b64decode(identity + "=" * (-len(identity) % 4)))
    author_id = identity["account_id"]
    topic = "search" + uuid.uuid4().hex[:8]
    rows = [
        (f"银河观测历史档案 {n}", "") if n in (0, 219) else (f"archive {topic} {n}", "")
        for n in range(220)
    ]
    values = ",\n".join(
        f"({author_id}, 'historic', '{title}', '{desc}', '/static/old.mp4', '/static/old.jpg', '2024-10-01 15:00:00')"
        for title, desc in rows
    )
    sql(f"INSERT INTO videos (author_id, username, title, description, play_url, cover_url, create_time) VALUES {values};")
    old_ids = [int(i) for i in sql("SELECT id FROM videos WHERE title LIKE '银河观测历史档案 %' ORDER BY id DESC;").splitlines()]
    assert len(old_ids) == 2

    docker("start", "worker")
    histories = wait_for("both historical batches visible", lambda: (
        response if len((response := search(base, expected=(200, 503), q="银河观测历史")).get("video_list", [])) == 2 else None
    ))
    assert [row["id"] for row in histories["video_list"]] == old_ids
    print("PASS historical backfill across two batches and deterministic tie-break", flush=True)

    worker = docker("ps", "-q", "worker")
    sample = media["sample"](worker)
    assets = []
    for _ in range(3):
        _, asset = media["new_asset"](base, token, sample)
        for number in range(1, asset["part_count"] + 1):
            media["part"](base, token, asset, number, sample)
        api(base, f"/video/assets/{asset['asset_id']}/complete", token=token)
        assets.append(asset)
    for asset in assets:
        media["wait_ready"](base, token, asset)

    def publish(asset, title, desc):
        return api(base, "/video/publish", {"asset_id": asset["asset_id"], "title": title, "description": desc}, token)

    def by_id(video_id, **query):
        return next((v for v in search(base, expected=(200, 503), **query).get("video_list", []) if v["id"] == video_id), None)

    first_page = search(base, q="银河观测", limit=1)
    first_cursor = first_page["next_cursor"]
    search(base, expected=(400,), q="其他查询", limit=1, cursor=first_cursor)
    search(base, expected=(400,), q="银河观测", limit=1, cursor="x" + first_cursor)
    start = time.monotonic()
    video = publish(assets[0], "银河观测实拍", f"延时摄影 #{topic}")
    result = wait_for("new publication searchable", lambda: by_id(video["id"], q="银河观测", tag=topic.upper()))
    print(f"PASS fresh publication searchable in {time.monotonic()-start:.2f}s", flush=True)
    assert result["author_id"] == author_id and result["cover_url"] == video["cover_url"]
    assert by_id(video["id"], q="延时摄影", author_id=author_id, tag="#" + topic)
    assert by_id(video["id"], q=video["username"], tag=topic)
    assert not by_id(video["id"], q="银河观测", author_id=author_id + 1)
    published = datetime.datetime.fromisoformat(result["create_time"])
    stamp = (published - datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc)) // datetime.timedelta(milliseconds=1)
    assert by_id(video["id"], tag=topic, published_from=stamp, published_before=stamp + 1)
    assert not by_id(video["id"], tag=topic, published_before=stamp)
    data, _, _ = request(base + video["play_url"], headers={"Range": "bytes=0-63"}, expected=(206,))
    assert len(data) == 64 and data[4:8] == b"ftyp"
    second_page = search(base, q="银河观测", limit=1, cursor=first_cursor)
    assert [v["id"] for v in first_page["video_list"] + second_page["video_list"]] == old_ids
    assert "next_cursor" not in second_page
    sql(f"DELETE FROM videos WHERE id={old_ids[0]};")
    assert [v["id"] for v in search(base, q="银河观测历史")["video_list"]] == old_ids[1:]
    print("PASS Chinese title/description/author, exact filters, Range playback, stable PIT and stale-hit removal", flush=True)

    latencies = []
    for _ in range(100):
        start = time.perf_counter()
        assert by_id(video["id"], q="银河观测", tag=topic)
        latencies.append((time.perf_counter() - start)*1000)
    print(f"MEASURE 100 serial queries, local HTTP P95={sorted(latencies)[94]:.2f}ms", flush=True)

    docker("stop", "elasticsearch")
    try:
        search(base, expected=(503,), q="银河观测")
        es_outage = publish(assets[1], "银河观测湖畔", f"离线发布 #{topic}")
        ident = es_outage["id"]
        sql(f"INSERT INTO outbox_msgs (video_id, event_type, create_time, status) SELECT id, 'video_published',"
            f" create_time, 'pending' FROM videos WHERE id={ident};")
        wait_for("Redis timeline during ES outage", lambda: docker(
            "exec", "-T", "redis", "sh", "-c",
            f'redis-cli -a "$REDIS_PASSWORD" ZSCORE v1:feed:global_timeline {ident} 2>/dev/null'
        ))
        wait_for("outbox confirmed during ES outage",
                 lambda: sql(f"SELECT COUNT(*) FROM outbox_msgs WHERE video_id={ident};") == "0")
        assert search_queue() == 2
    finally:
        docker("start", "elasticsearch")
    start = time.monotonic()
    wait_for("search catches up after ES recovery", lambda: by_id(ident, q="银河观测湖畔", tag=topic), timeout=90)
    recovered = time.monotonic() - start
    wait_for("both copies consumed", lambda: search_queue() == 0)
    assert len(search(base, q="银河观测湖畔", tag=topic)["video_list"]) == 1
    print(f"PASS ES outage: publication and timeline succeed; search catches up in {recovered:.2f}s with one result after repeated delivery", flush=True)

    docker("stop", "rabbitmq")
    try:
        mq_outage = publish(assets[2], "银河观测山巅", f"消息积压 #{topic}")
        ident = mq_outage["id"]
        assert sql(f"SELECT status FROM outbox_msgs WHERE video_id={ident};") == "pending"
        assert not by_id(ident, q="银河观测山巅", tag=topic)
    finally:
        docker("start", "rabbitmq")
    start = time.monotonic()
    wait_for("search catches up after RabbitMQ recovery", lambda: by_id(ident, q="银河观测山巅", tag=topic), timeout=90)
    assert sql(f"SELECT COUNT(*) FROM outbox_msgs WHERE video_id={ident};") == "0"
    print(f"PASS RabbitMQ outage: MySQL outbox retains event; search catches up in {time.monotonic()-start:.2f}s", flush=True)
    print(f"Browser test: /search?q=银河观测实拍 (expected video {video['id']})", flush=True)


if __name__ == "__main__":
    main()
