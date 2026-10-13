#!/usr/bin/env python3
"""Summarise tests/experiments/results/*.csv into Markdown tables (stdout).

    python3 tests/experiments/summarize.py [results-dir]
"""
import collections
import csv
import math
import os
import sys

DIR = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "results")


def rows(name):
    path = os.path.join(DIR, name)
    if not os.path.exists(path):
        return []
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


def pct(values, p):
    if not values:
        return float("nan")
    s = sorted(values)
    k = p / 100 * (len(s) - 1)
    lo, hi = math.floor(k), math.ceil(k)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def wilson(k, n, z=1.96):
    """95% Wilson interval for a proportion, in percent."""
    if n == 0:
        return float("nan"), float("nan")
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return 100 * (c - h), 100 * (c + h)


def f(x, digits=1):
    return "—" if x is None or (isinstance(x, float) and math.isnan(x)) else f"{x:.{digits}f}"


def table(header, body):
    print("| " + " | ".join(header) + " |")
    print("|" + "|".join("---" for _ in header) + "|")
    for line in body:
        print("| " + " | ".join(str(c) for c in line) + " |")
    print()


def access():
    data = rows("access.csv")
    if not data:
        return
    print("### 接入成功率与首屏时间\n")
    groups = collections.OrderedDict()
    for r in data:
        groups.setdefault((r["profile"], r["system"]), []).append(r)
    body = []
    for (profile, system), items in groups.items():
        ok = [float(r["first_screen_ms"]) for r in items if r["ok"] == "1"]
        lo, hi = wilson(len(ok), len(items))
        s1 = [float(r["stage1_ms"]) for r in items if r["ok"] == "1"]
        s2 = [float(r["stage2_ms"]) for r in items if r["ok"] == "1"]
        body.append([profile, system, f"{len(ok)}/{len(items)}", f"{100*len(ok)/len(items):.1f}% [{lo:.1f}, {hi:.1f}]",
                     f(pct(s1, 50)), f(pct(s2, 50)), f(pct(ok, 50)), f(pct(ok, 95)), f(max(ok) if ok else None)])
    table(["网络", "系统", "成功", "成功率 [95% CI]", "阶段1 P50", "阶段2 P50", "首屏 P50", "首屏 P95", "首屏 最大"], body)
    errors = collections.Counter((r["system"], r["error"]) for r in data if r["ok"] != "1")
    if errors:
        print("失败原因：" + "；".join(f"{s} {e} ×{n}" for (s, e), n in errors.items()) + "\n")


def latency():
    data = rows("latency.csv")
    if not data:
        return
    print("### 按键回显延迟（ms）\n")
    groups = collections.OrderedDict()
    for r in data:
        groups.setdefault((r["profile"], r["system"]), []).append(r)
    body = []
    for (profile, system), items in groups.items():
        ok = [float(r["latency_ms"]) for r in items if r["ok"] == "1"]
        body.append([profile, system, f"{len(ok)}/{len(items)}", f(pct(ok, 50), 2), f(pct(ok, 95), 2), f(pct(ok, 99), 2),
                     f(sum(ok) / len(ok) if ok else None, 2)])
    table(["网络", "系统", "样本", "P50", "P95", "P99", "均值"], body)


def recovery():
    data = rows("recovery.csv")
    if not data:
        return
    print("### 断网恢复（秒）\n")
    groups = collections.OrderedDict()
    for r in data:
        groups.setdefault((r["scenario"], r["strategy"]), []).append(r)
    body = []
    for (scenario, strategy), items in groups.items():
        rec = [r for r in items if r["recovered"] == "1"]
        after = [float(r["recover_after_restore_ms"]) / 1000 for r in rec]
        total = [float(r["total_ms"]) / 1000 for r in rec]
        detect = [float(r["detect_ms"]) / 1000 for r in items if r["detect_ms"]]
        body.append([scenario, strategy, f"{len(rec)}/{len(items)}", f(pct(detect, 50)), f(pct(after, 50)), f(max(after) if after else None),
                     f(pct(total, 50)), f(max(total) if total else None)])
    table(["场景", "策略", "恢复", "发现断线 P50", "恢复网络后 P50", "恢复网络后 最大", "总中断 P50", "总中断 最大"], body)


def authz():
    data = rows("authz.csv")
    if not data:
        return
    print("### 越权请求拦截\n")
    groups = collections.OrderedDict()
    for r in data:
        groups.setdefault(r["group"], []).append(r)
    names = {"role": "角色越权", "space": "跨空间访问", "anon": "未登录", "forgery": "请求伪造", "ticket": "终端票据滥用", "device": "设备身份伪造"}
    body, total, blocked = [], 0, 0
    for group, items in groups.items():
        k = sum(r["intercepted"] == "1" for r in items)
        cases = len({(r["case"], r["actor"]) for r in items})
        statuses = ",".join(sorted({r["status"] for r in items}))
        body.append([names.get(group, group), cases, f"{k}/{len(items)}", statuses])
        total += len(items)
        blocked += k
    lo, hi = wilson(blocked, total)
    body.append(["合计", len({(r["case"], r["actor"]) for r in data}), f"{blocked}/{total}", f"{100*blocked/total:.1f}% [95% CI {lo:.1f}, {hi:.1f}]"])
    table(["类别", "用例数", "拦截/请求", "返回状态"], body)
    control = rows("authz_control.csv")
    if control:
        ok = sum(r["ok"] == "1" for r in control)
        print(f"正向对照：{ok}/{len(control)} 个合法请求成功（" + "，".join(f"{r['actor']} {r['case']} {r['status']}" for r in control) + "）。\n")


def revoke():
    data = rows("revoke.csv")
    if not data:
        return
    print("### 权限撤销生效时间（ms）\n")
    names = {"device": "吊销设备", "downgrade": "降级为 viewer", "remove": "移出空间", "logout": "退出登录"}
    groups = collections.OrderedDict()
    for r in data:
        groups.setdefault(r["kind"], []).append(r)
    body = []
    for kind, items in groups.items():
        closed = [float(r["effect_ms"]) for r in items if r["closed"] == "1"]
        codes = ",".join(sorted({r["close_code"] for r in items if r["close_code"]}))
        reconnect = ",".join(sorted({r["reconnect_status"] for r in items if r["reconnect_status"]})) or "—"
        body.append([names.get(kind, kind), f"{len(closed)}/{len(items)}", f(pct(closed, 50)), f(pct(closed, 95)), f(max(closed) if closed else None), codes, reconnect])
    table(["操作", "终端被关闭", "P50", "P95", "最大", "关闭码", "设备重连"], body)


def load():
    data = rows("load.csv")
    if not data:
        return
    print("### 并发负载\n")
    body = []
    for r in data:
        echo = "—" if not r["echo_n"] or r["echo_n"] == "0" else f"{r['echo_p50_ms']} / {r['echo_p95_ms']} / {r['echo_p99_ms']} ({r['echo_fail']} 失败)"
        body.append([r["devices"], r["terminals"], r["phase"], r["hub_cpu_pct"], r["hub_rss_mib"], r["pg_cpu_pct"], r.get("db_xact_per_s", "—"), echo,
                     f"{r['open_p50_ms']} / {r['open_p95_ms']}" if r["open_p50_ms"] else "—"])
    table(["设备", "终端", "阶段", "Hub CPU %", "Hub RSS MiB", "PG CPU %", "DB 事务/s", "回显 P50/P95/P99 ms", "打开 P50/P95 ms"], body)


if __name__ == "__main__":
    env = os.path.join(DIR, "env.txt")
    if os.path.exists(env):
        print("```\n" + open(env).read().strip() + "\n```\n")
    for section in (access, latency, recovery, authz, revoke, load):
        section()
