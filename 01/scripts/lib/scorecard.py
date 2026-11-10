#!/usr/bin/env python3
"""Scorecard edits and A13 checks for measure.sh.

    scorecard.py RUN_DIR ACTION [ARG]

ACTION: invalid-new REASON | invalid REASON | banner TEXT | prestop |
control-ok | problem | is-invalid (see each branch).
"""
import json, os, sys
d, action = sys.argv[1], sys.argv[2]
arg = sys.argv[3] if len(sys.argv) > 3 else ""
jp, tp = os.path.join(d, "scorecard.json"), os.path.join(d, "scorecard.txt")

def load():
    return json.load(open(jp))

def save(s, text):
    json.dump(s, open(jp + ".tmp", "w"), indent=2, ensure_ascii=False)
    os.replace(jp + ".tmp", jp)
    open(tp + ".tmp", "w").write(text)
    os.replace(tp + ".tmp", tp)

MAIN = [("訂單", "orders"), ("第一次送出沒有成功回應", "first_try_failed"),
        ("重試後仍沒有成功回應", "failed_after_retry"), ("重複訂單", "duplicate_client_refs"),
        ("多出的訂單", "duplicate_extra_orders")]
BOARDS = ("board-1", "board-2", "board-3")

def items(s):
    it = s.get("items") or {}
    main = [("訂單", it.get("orders")), ("第一次送出沒有成功回應", it.get("first_try_failed")),
            ("重試後仍沒有成功回應", it.get("failed_after_retry")),
            ("重複訂單", it.get("duplicate_client_refs")), ("多出的訂單", it.get("duplicate_extra_orders"))]
    for name, v in sorted((it.get("missing") or {}).items()):
        main.append(("漏號 " + name, v))
    return main

if action == "invalid-new":
    # No scorecard from rush-hour: write one that only says invalid.
    s = {"run": os.path.basename(d), "invalid": True, "invalid_reason": arg,
         "invalid_reasons": [arg], "has_unknown": False, "items": None,
         "invalid_by": "measure.sh"}
    save(s, "本次量測無效：%s\nRush Hour 成績單　量測編號 %s\n\n這次量測沒有完成，不列出主要成績，也不可用於比較。\n" % (arg, s["run"]))
elif action == "invalid":
    # rush-hour wrote a scorecard, but measure.sh found the run invalid.
    s = load()
    if os.path.exists(tp) and not os.path.exists(tp.replace(".txt", ".rush-hour.txt")):
        os.replace(tp, tp.replace(".txt", ".rush-hour.txt"))
    reasons = [arg] + list(s.get("invalid_reasons") or [])
    s.update({"invalid": True, "invalid_reasons": reasons, "invalid_reason": "；".join(reasons),
              "invalid_by": "measure.sh"})
    save(s, "本次量測無效：%s\nRush Hour 成績單　量測編號 %s\n\n這次量測不符合量測條件，不列出主要成績，也不可用於比較。\n"
         "（Rush Hour 原本的成績單保留在 scorecard.rush-hour.txt，只供診斷。）\n" % (s["invalid_reason"], s.get("run")))
elif action == "banner":
    s = load()
    text = open(tp).read() if os.path.exists(tp) else ""
    if not text.startswith(arg + "\n"):
        text = arg + "\n" + text
    s["diagnostic"] = True
    s["diagnostic_banner"] = arg
    save(s, text)
elif action == "prestop":
    # Design 4.x / plan A11: a preStop that is the teacher's conservative
    # value (calibration failed) is labelled 「preStop 未經校正」. The source is
    # the pod template annotation koupa.dev/prestop-calibrated of the live
    # Deployment read after preflight (deployment-at-start.json): "true" |
    # "false"; missing or anything else is unknown, never "calibrated".
    # Runs without preStop (step 0, 1, control) and calibrate get no line.
    KEY = "koupa.dev/prestop-calibrated"
    s = load()
    try:
        params = json.load(open(os.path.join(d, "params.json")))
    except (OSError, ValueError):
        params = {}
    mode, overlay = params.get("mode"), params.get("overlay")
    expects = overlay in ("step-2", "step-3", "step-4", "step-5")
    seconds = marker = reason = None
    try:
        dep = json.load(open(os.path.join(d, "deployment-at-start.json")))
        tmpl = dep["spec"]["template"]
        ctr = [c for c in tmpl["spec"]["containers"] if c.get("name") == "koupa"][0]
        seconds = (((ctr.get("lifecycle") or {}).get("preStop") or {}).get("sleep") or {}).get("seconds")
        marker = ((tmpl.get("metadata") or {}).get("annotations") or {}).get(KEY)
        have = True
    except (OSError, ValueError, KeyError, IndexError, TypeError, AttributeError):
        have = False
    if mode == "calibrate" or (not expects and (not have or seconds is None)):
        status, line = "not_applicable", None
    elif not have:
        status, reason = "unknown", "沒有讀到量測開始時的 Deployment（deployment-at-start.json）"
    elif marker == "true":
        status = "calibrated"
    elif marker == "false":
        status = "uncalibrated"
    else:
        status = "unknown"
        reason = ("Deployment 沒有 %s 標記" % KEY if marker is None
                  else "%s 標記值 %r 不是 true 或 false" % (KEY, marker))
    sec = "%s 秒" % seconds if seconds is not None else "秒數不明"
    line = {"calibrated": "preStop：%s（已依本環境校正）" % sec,
            "uncalibrated": "preStop 未經校正：%s是講師手冊的保守值，不是本環境量出的候選值" % sec,
            "unknown": "preStop 是否經過校正：未知（%s；%s）" % (sec, reason)}.get(status)
    s["prestop_calibration"] = {"status": status, "prestop_s": seconds, "marker": marker,
                                "marker_key": KEY, "source": "deployment-at-start.json",
                                "reason": reason}
    text = open(tp).read() if os.path.exists(tp) else ""
    lines = [l for l in text.split("\n") if not l.startswith(("preStop：", "preStop 未經校正", "preStop 是否經過校正"))]
    if line:
        # Right after the title line (a banner, if any, stays on line 1).
        at = next((i + 1 for i, l in enumerate(lines) if l.startswith("Rush Hour 成績單")), 0)
        lines.insert(at, line)
    save(s, "\n".join(lines))
elif action == "control-ok":
    # A13: valid, no unknown, orders sent, every failure item exactly 0, and
    # every item and all three boards present and determinable: an absent or
    # malformed value is a failure, never a 0.
    try:
        s = load()
    except (OSError, ValueError) as e:
        print("沒有可讀的 scorecard.json（%s）" % e); sys.exit(1)
    if not isinstance(s, dict):
        print("scorecard.json 不是物件"); sys.exit(1)
    why = []
    if s.get("invalid") is not False:
        why.append("本次量測無效（%s）" % s.get("invalid_reason") if s.get("invalid")
                   else "成績單沒有明確的 invalid: false")
    else:
        if s.get("has_unknown") is not False:
            why.append("有未知項目" if s.get("has_unknown") else "成績單沒有明確的 has_unknown: false")
        it = s.get("items")
        if not isinstance(it, dict):
            why.append("成績單沒有 items")
            it = {}
        def count(name, v):
            # (confirmed, unknown) or None when absent/malformed.
            ok = isinstance(v, dict) and all(
                type(v.get(k)) is int and v.get(k) >= 0 for k in ("confirmed", "unknown"))
            if not ok:
                why.append("%s 無法判定（%r）" % (name, v))
                return None
            return v["confirmed"], v["unknown"]
        want = [(n, it.get(k)) for n, k in MAIN]
        missing = it.get("missing")
        if not isinstance(missing, dict):
            why.append("漏號沒有每台看板的結果（%r）" % missing)
            missing = {}
        for b in BOARDS:
            want.append(("漏號 " + b, missing.get(b)))
        extra = sorted(set(missing) - set(BOARDS))
        if extra:
            why.append("多出預期以外的看板 %s" % ",".join(extra))
        for name, v in want:
            c = count(name, v)
            if c is None:
                continue
            if c[1] > 0:
                why.append("%s 另有 %d 項未知" % (name, c[1]))
            if name == "訂單":
                if c[0] <= 0:
                    why.append("沒有送出任何訂單")
            elif c[0] > 0:
                why.append("%s 已確認 %d" % (name, c[0]))
        # The board check needs called numbers: with cursor C = 0 nothing
        # was marked ready and "0 missed" says nothing about the boards.
        cursor = ((s.get("facts") or {}).get("audit") or {}).get("cursor")
        if type(cursor) is not int or cursor <= 0:
            why.append("沒有任何叫號（截止游標 C = %s），漏號 0 不代表看板沒有漏號" % cursor)
    print("；".join(why) if why else "0 失敗、0 漏號、0 重複，沒有未知")
    sys.exit(1 if why else 0)
elif action == "problem":
    # A13: did this (valid) step-0 run observe a predefined problem?
    try:
        s = load()
    except (OSError, ValueError):
        sys.exit(1)
    if not isinstance(s, dict) or s.get("invalid") is not False:
        sys.exit(1)
    # Only a real integer count is an observation (JSON true == 1, 1.0 ...).
    def confirmed(v):
        c = v.get("confirmed") if isinstance(v, dict) else None
        return c if type(c) is int else 0
    seen = [("%s 已確認 %d" % (n, confirmed(v))) for n, v in items(s)
            if n not in ("訂單", "多出的訂單") and confirmed(v) > 0]
    print("；".join(seen))
    sys.exit(0 if seen else 1)
elif action == "is-invalid":
    try:
        # Valid only with an explicit "invalid": false.
        s = load()
        sys.exit(1 if isinstance(s, dict) and s.get("invalid") is False else 0)
    except (OSError, ValueError):
        sys.exit(0)
