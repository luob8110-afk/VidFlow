"""Render the completed raw benchmark results into a reviewable Markdown report."""
import json
from pathlib import Path
import sys

from benchmark import percentile


def main():
    folder=Path(sys.argv[1]).resolve()
    rows=json.loads((folder/"summary.json").read_text(encoding="utf-8"))
    meta=json.loads((folder/"metadata.json").read_text(encoding="utf-8"))
    expected=meta["arguments"]["repeats"]*len(meta["arguments"]["rates"])*4
    if len(rows)!=expected:
        raise RuntimeError(f"incomplete experiment: {len(rows)} / {expected} runs")
    groups={}
    for row in rows:
        key=(row["distribution"],row["requested_rps"],row["variant"])
        group=groups.setdefault(key,dict(latencies=[],runs=[]))
        raw=json.loads((folder/f'{row["repetition"]}-{row["distribution"]}-{row["requested_rps"]}-{row["variant"]}.json').read_text(encoding="utf-8"))
        group["latencies"].extend(r["latency_ms"] for r in raw["responses"] if r["status"]==200)
        group["runs"].append(row)
    def sum_value(group,key): return sum(r[key] for r in group["runs"])
    def interval(group,key):
        values=[r[key] for r in group["runs"]]
        if any(v is None for v in values): return "未追平"
        return f"{min(values):.2f}–{max(values):.2f}"
    lines=["# 点赞计数：真实链路 A/B 压测结果", "",
           f'执行时间：{meta["utc"]}（UTC）；MySQL {meta["mysql_version"]}；{meta["compiler"]}。',"",
           f'旧版为 Git `{meta["baseline_commit"]}`，新版为工作区冻结快照 `{meta["candidate_source_sha256"]}`。详细配额与复现步骤见 [实验说明](../../README.md)。',"",
           f'每个场景持续 {meta["arguments"]["seconds"]:g} 秒，重复 {meta["arguments"]["repeats"]} 轮，交替 A→B、B→A。热点为一个视频，分散为 64 个视频。两个版本按相同固定请求率到达；主测每个请求使用不同用户。',"",
           "## 响应与最终追平", "",
           "P95/P99 合并同场景两轮的成功请求原始样本计算；吞吐、追平时间给出各轮范围。所有箭头均为旧版 → 新版。", "",
           "| 场景 | 请求/秒 | 每版本总请求 | HTTP 成功数（旧→新） | P95 ms | P99 ms | 成功吞吐 req/s | 停写后追平 s |",
           "|---|---:|---:|---|---|---|---|---|"]
    conditions=sorted({(r["distribution"],r["requested_rps"]) for r in rows})
    for dist,rate in conditions:
        a,b=(groups[dist,rate,v] for v in ("baseline","candidate"))
        lines.append(f'| {"热点" if dist=="hot" else "分散"} | {rate} | {sum_value(a,"requests")} | {sum_value(a,"successful")} → {sum_value(b,"successful")} | {percentile(a["latencies"],.95):.2f} → {percentile(b["latencies"],.95):.2f} | {percentile(a["latencies"],.99):.2f} → {percentile(b["latencies"],.99):.2f} | {interval(a,"successful_rps")} → {interval(b,"successful_rps")} | {interval(a,"convergence_s")} → {interval(b,"convergence_s")} |')
    lines += ["", "## 数据库写入与缓存成本", "",
              "下表为两轮合计。视频行更新包含 likes_count 和 popularity：旧版分两次 UPDATE，新版同批合并。全部行写入包含关系、通知、计数日志及其完成标记、刷新锁等；它不是磁盘写入字节，也不是 SQL 语句数。", "",
              "| 场景 | 请求/秒 | 视频行更新（旧→新） | 视频行更新减少 | 全部行写入（旧→新） | 全部行写入变化 | 新版计数缓存 KiB/轮 |",
              "|---|---:|---|---:|---|---:|---|"]
    for dist,rate in conditions:
        a,b=(groups[dist,rate,v] for v in ("baseline","candidate"))
        av,bv=sum_value(a,"video_row_updates"),sum_value(b,"video_row_updates")
        aw,bw=sum_value(a,"total_row_write_operations"),sum_value(b,"total_row_write_operations")
        memory=[r["redis_count_bytes"]/1024 for r in b["runs"]]
        lines.append(f'| {"热点" if dist=="hot" else "分散"} | {rate} | {av:,} → {bv:,} | {(1-bv/av)*100:.2f}% | {aw:,} → {bw:,} | {(bw/aw-1)*100:+.2f}% | {min(memory):.2f}–{max(memory):.2f} |')
    max_rate=max(r["requested_rps"] for r in rows)
    a,b=(groups["hot",max_rate,v] for v in ("baseline","candidate"))
    journal_inserts=sum(r["tables"].get("like_count_events",{}).get("COUNT_INSERT",0) for r in b["runs"])
    journal_updates=sum(r["tables"].get("like_count_events",{}).get("COUNT_UPDATE",0) for r in b["runs"])
    all_ok=all(r["consistent"] and r["duplicate_consistent"] for r in rows)
    attempts=sum(r["requests"] for r in rows)
    success=sum(r["successful"] for r in rows)
    lines += ["", "## 判断与边界", "",
              f'本次 {len(rows)} 轮主测共 {attempts:,} 次请求，HTTP 成功 {success:,} 次。逐视频关系数、持久化计数、请求预期及重复点赞验证：{"全部通过" if all_ok else "存在失败，请检查原始数据"}。',"",
              f'在 {max_rate} req/s 的热点场景，两轮视频行更新从 {sum_value(a,"video_row_updates"):,} 次降至 {sum_value(b,"video_row_updates"):,} 次；接口 P95 从 {percentile(a["latencies"],.95):.2f} ms 变为 {percentile(b["latencies"],.95):.2f} ms；新版停写后 {interval(b,"convergence_s")} 秒完成数据库计数追平。',"",
              f'同一热点场景新版新增计数日志行插入 {journal_inserts:,} 次、日志完成标记更新 {journal_updates:,} 次。这些操作分散在独立事件行，保留恢复能力，同时解释了热点更新大幅减少却未减少全库行写入。',"",
              "收益集中在减少视频热点行更新及部分积压追平时间。新增关系变更日志和完成标记有写入成本；不能表述为数据库总写入显著下降。分散场景每批需要更新更多视频，聚合收益下降。", "",
              "旧版 HTTP 成功时关系可能还在 MQ，新版已提交关系和恢复日志，响应承诺不同。新版 P95/P99 变慢，应与持久化确认及日志成本一起说明。此次请求率均未证明接口吞吐上限，不支持‘整体吞吐提高’的结论。旧版仅有一个关系消费者，也不能用本次结果证明多消费者下的行锁竞争改善。", "",
              "追平时间从所有 HTTP 请求结束开始测量，包含约 200 ms 轮询和两次一致性确认，是观测上界；也受 5 秒批处理周期相位影响，未记录每条增量的独立延迟。主测为有限持续时间、两个重复轮次，宿主机另有原项目容器，延迟结果不能直接推广到生产或更长时段。", "",
              "计数缓存内存包含本次事件去重字段和总数字段，只测 MEMORY USAGE，不代表 Redis 进程总内存；没有触发 1 小时计数代际过期或 24 小时日志清理，不能推断稳态空间。Redis 计数逐状态恢复、故障重试由后端集成测试覆盖，本次负载主要验证正常路径的关系与数据库总数追平。", "",
              "原始证据：`summary.json` 保存每轮指标和每表 I/O 差值；`<轮次>-<分布>-<请求率>-<版本>.json` 保存逐请求延迟、状态码、追平检查与并发重复请求；`metadata.json` 保存源码与环境。", ""]
    (folder/"REPORT.md").write_text("\n".join(lines),encoding="utf-8")
    print(folder/"REPORT.md")


if __name__=="__main__": main()
