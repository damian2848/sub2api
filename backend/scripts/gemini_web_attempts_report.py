#!/usr/bin/env python3
"""Summarize Gemini Web attempt records to study canned failure replies.

Sub2API writes one JSON line per Gemini Web call to
$DATA_DIR/gemini-web-analytics/attempts-YYYYMMDD.jsonl (30 days kept).
This report shows how often calls end as canned failure replies and which
request facts go with them, so changes can be measured over time.

Usage (on the server, data dir mounted at /data/sub2api/data):
    python3 gemini_web_attempts_report.py /data/sub2api/data/gemini-web-analytics
    python3 gemini_web_attempts_report.py --since 24 --until 2026-10-19T00:00 DIR
    python3 gemini_web_attempts_report.py --json DIR > summary.json

Only the standard library is used.
"""

import argparse
import collections
import datetime as dt
import glob
import json
import os
import sys

TZ = dt.timezone(dt.timedelta(hours=8))  # report hours in Asia/Shanghai
ANSWERED = ("ok", "canned", "refusal")


def parse_time(value):
    if not value:
        return None
    text = value.replace("Z", "+00:00")
    try:
        parsed = dt.datetime.fromisoformat(text)
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=TZ)


def load(paths, since, until):
    files = []
    for path in paths:
        if os.path.isdir(path):
            files.extend(sorted(glob.glob(os.path.join(path, "attempts-*.jsonl"))))
        else:
            files.append(path)
    attempts, cooldowns = [], []
    for name in files:
        with open(name, encoding="utf-8", errors="replace") as handle:
            for line in handle:
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                at = parse_time(rec.get("time"))
                if at is None or (since and at < since) or (until and at >= until):
                    continue
                rec["_time"] = at
                (cooldowns if rec.get("type") == "cooldown" else attempts).append(rec)
    return attempts, cooldowns


def prompt_bucket(chars):
    for limit, label in ((5_000, "<5k"), (50_000, "5k-50k"), (150_000, "50k-150k")):
        if chars < limit:
            return label
    return ">=150k"


def since_bucket(ms):
    if ms is None or ms < 0:
        return "first seen"
    for limit, label in ((60_000, "<1m"), (600_000, "1-10m"), (3_600_000, "10-60m")):
        if ms < limit:
            return label
    return ">=60m"


def inflight_bucket(n):
    n = n or 1
    return str(n) if n < 3 else "3+"


def attempt_bucket(n):
    n = n or 0
    return str(n) if n < 2 else "2+"


DIMENSIONS = (
    ("account", lambda r: str(r.get("account_id"))),
    ("model", lambda r: r.get("model") or "?"),
    ("response_model", lambda r: r.get("response_model") or "(none)"),
    ("protocol", lambda r: (r.get("protocol") or "?") + (" stream" if r.get("stream") else "")),
    ("tools", lambda r: "with tools" if r.get("tools") else "no tools"),
    ("prompt_chars", lambda r: prompt_bucket(r.get("prompt_chars") or 0)),
    ("account_inflight", lambda r: inflight_bucket(r.get("account_inflight"))),
    ("since_account_last", lambda r: since_bucket(r.get("since_account_last_ms"))),
    ("attempt", lambda r: attempt_bucket(r.get("attempt"))),
    ("hour_utc8", lambda r: r["_time"].astimezone(TZ).strftime("%H")),
)


def breakdown(answered, key):
    groups = collections.defaultdict(lambda: [0, 0])
    for rec in answered:
        bucket = groups[key(rec)]
        bucket[0] += 1
        bucket[1] += rec.get("outcome") == "canned"
    rows = [{"bucket": k, "answered": n, "canned": c, "rate": c / n if n else 0.0} for k, (n, c) in groups.items()]
    return sorted(rows, key=lambda row: (-row["rate"], -row["answered"], row["bucket"]))


def request_recovery(attempts):
    by_request = collections.defaultdict(list)
    for rec in attempts:
        if rec.get("request_id"):
            by_request[rec["request_id"]].append(rec)
    hit, recovered, failed, extra_ms = 0, 0, 0, []
    for recs in by_request.values():
        recs.sort(key=lambda r: (r.get("attempt") or 0, r["_time"]))
        canned = [r for r in recs if r.get("outcome") == "canned"]
        if not canned:
            continue
        hit += 1
        if recs[-1].get("outcome") in ("ok", "refusal"):
            recovered += 1
        else:
            failed += 1
        extra_ms.append(sum(max(r.get("total_ms") or 0, 0) for r in canned))
    extra_ms.sort()
    median = extra_ms[len(extra_ms) // 2] if extra_ms else 0
    return {"requests": len(by_request), "with_canned": hit, "recovered": recovered,
            "failed": failed, "median_canned_ms": median}


def summarize(attempts, cooldowns):
    outcomes = collections.Counter(r.get("outcome") or "?" for r in attempts)
    answered = [r for r in attempts if r.get("outcome") in ANSWERED]
    texts = {kind: collections.Counter() for kind in ("canned", "refusal", "suspected")}
    for rec in attempts:
        text = (rec.get("text") or "").strip()
        if not text:
            continue
        if rec.get("outcome") == "canned":
            texts["canned"][text] += 1
        elif rec.get("outcome") == "refusal":
            texts["refusal"][text] += 1
        elif rec.get("suspected"):
            texts["suspected"][text] += 1
    times = [r["_time"] for r in attempts]
    return {
        "from": min(times).isoformat() if times else None,
        "to": max(times).isoformat() if times else None,
        "attempts": len(attempts),
        "outcomes": dict(outcomes.most_common()),
        "answered": len(answered),
        "canned_rate": outcomes["canned"] / len(answered) if answered else 0.0,
        "flagged_without_phrase": sum(1 for r in attempts if r.get("generation_failed") and not r.get("phrase_match")),
        "requests": request_recovery(attempts),
        "breakdowns": {name: breakdown(answered, key) for name, key in DIMENSIONS},
        "cooldowns": dict(collections.Counter(str(c.get("account_id")) for c in cooldowns).most_common()),
        "top_texts": {kind: counter.most_common(10) for kind, counter in texts.items()},
    }


def print_report(summary, min_n):
    print(f"Gemini Web attempts {summary['from']} .. {summary['to']}")
    print(f"attempts {summary['attempts']}, answered {summary['answered']}, "
          f"canned rate {summary['canned_rate']:.1%} of answered")
    print("outcomes: " + ", ".join(f"{k} {v}" for k, v in summary["outcomes"].items()))
    print(f"flagged by payload[31] but not in the phrase catalog: {summary['flagged_without_phrase']}")
    req = summary["requests"]
    print(f"requests {req['requests']}: {req['with_canned']} hit a canned reply, "
          f"{req['recovered']} recovered on another attempt, {req['failed']} failed; "
          f"median time lost to canned attempts {req['median_canned_ms'] / 1000:.1f}s")
    for name, rows in summary["breakdowns"].items():
        print(f"\ncanned rate by {name} (answered >= {min_n}):")
        for row in rows:
            if row["answered"] < min_n:
                continue
            print(f"  {row['bucket']:<24} {row['canned']:>5}/{row['answered']:<6} {row['rate']:6.1%}")
    if summary["cooldowns"]:
        print("\ncooldowns by account: " + ", ".join(f"{k} x{v}" for k, v in summary["cooldowns"].items()))
    for kind, items in summary["top_texts"].items():
        if items:
            print(f"\ntop {kind} texts:")
            for text, count in items:
                print(f"  {count:>4}  {text[:140]!r}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("paths", nargs="*", default=["/data/sub2api/data/gemini-web-analytics"])
    parser.add_argument("--since", type=float, help="only the last N hours")
    parser.add_argument("--until", help="only records before this time (ISO, default UTC+8)")
    parser.add_argument("--min-n", type=int, default=5, help="hide buckets with fewer answered attempts")
    parser.add_argument("--json", action="store_true", help="print the summary as JSON")
    args = parser.parse_args(argv)
    now = dt.datetime.now(TZ)
    since = now - dt.timedelta(hours=args.since) if args.since else None
    until = parse_time(args.until) if args.until else None
    attempts, cooldowns = load(args.paths, since, until)
    summary = summarize(attempts, cooldowns)
    if args.json:
        json.dump(summary, sys.stdout, ensure_ascii=False, indent=2, default=str)
        print()
    else:
        print_report(summary, args.min_n)
    return 0


if __name__ == "__main__":
    sys.exit(main())
