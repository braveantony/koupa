#!/usr/bin/env bash
# Raw SSE stream capture and field-grammar check (plan A14 協定, design 5.6).
#
#   curl-stream.sh capture URL OUT_PREFIX [MAX_SECONDS]
#       curl -N URL; the body byte for byte in OUT_PREFIX.raw, response
#       headers in OUT_PREFIX.headers (X-Koupa-Pod-UID names the Pod). Ends
#       when the server closes the stream, at MAX_SECONDS (default 300), or
#       on SIGTERM.
#   curl-stream.sh check OUT_PREFIX.raw [--expect-shutdown] [--json FILE]
#       Checks the grammar the board relies on (plan A4, WHATWG SSE):
#         - LF line endings only; every line is "event: X", "id: N",
#           "data: ...", "data:", "retry: N" or a ": comment"
#         - every event block ends with a blank line (also the last one)
#         - the first event is hello: id = integer start, data = JSON
#           {"version": ...}
#         - ready: id = integer ready_seq, strictly increasing and above the
#           start; data = JSON {"number": string, "order_id": integer}
#         - with --expect-shutdown: the stream ends with exactly one
#           shutdown block whose first line is "retry: N" (before the event
#           line), whose data line is the empty "data:", then a blank line,
#           and nothing after it
#       Prints "ok ..." / "FAIL ..." lines; exit 0 only if all hold.
set -uo pipefail

case ${1:-} in
capture)
	[ $# -ge 3 ] || { echo "usage: $0 capture URL OUT_PREFIX [MAX_SECONDS]" >&2; exit 64; }
	url=$2 out=$3 max=${4:-300}
	# --no-buffer: write each chunk as it arrives; -sS: errors only.
	exec curl -sS -N --no-buffer -H 'Accept: text/event-stream' --max-time "$max" -D "$out.headers" -o "$out.raw" "$url"
	;;
check)
	[ $# -ge 2 ] || { echo "usage: $0 check FILE.raw [--expect-shutdown] [--json FILE]" >&2; exit 64; }
	file=$2
	shift 2
	expect=0 json=""
	while [ $# -gt 0 ]; do
		case $1 in
		--expect-shutdown) expect=1; shift ;;
		--json) json=$2; shift 2 ;;
		*) echo "unknown argument $1" >&2; exit 64 ;;
		esac
	done
	python3 -I - "$file" "$expect" "$json" <<'PY'
import json, re, sys
path, expect, out = sys.argv[1], sys.argv[2] == "1", sys.argv[3]
raw = open(path, "rb").read()
FAILED = []
def ok(cond, text):
    print(("ok   " if cond else "FAIL ") + text)
    if not cond:
        FAILED.append(text)
ok(len(raw) > 0, "the stream is not empty (%d bytes)" % len(raw))
ok(b"\r" not in raw, "LF line endings only (no CR)")
text = raw.decode("utf-8", errors="replace")
ended_with_newline = text.endswith("\n")
lines = text.split("\n")
if ended_with_newline:
    lines = lines[:-1]
field = re.compile(r"^(event|id|data|retry): ?(.*)$")
blocks, cur, bad = [], [], []
for i, l in enumerate(lines):
    if l == "":
        if cur:
            blocks.append(cur)
            cur = []
        continue
    if l.startswith(":"):
        continue  # comment (heartbeat, filler)
    if l == "data:":
        cur.append(("data", ""))
        continue
    m = field.match(l)
    if not m:
        bad.append((i + 1, l[:80]))
        continue
    cur.append((m.group(1), m.group(2)))
unterminated = cur
ok(not bad, "every line is a known field or a comment%s" % ("" if not bad else ": %r" % bad[:3]))
ok(not unterminated, "every event block ends with a blank line%s" % ("" if not unterminated else ": open block %r" % unterminated))
events = []
for b in blocks:
    d = {"event": None, "id": None, "data": [], "retry": None, "order": [k for k, _ in b]}
    for k, v in b:
        if k == "data":
            d["data"].append(v)
        else:
            d[k] = v
    events.append(d)
ok(events and events[0]["event"] == "hello", "the first event is hello")
start = None
if events and events[0]["event"] == "hello":
    h = events[0]
    ok(h["id"] is not None and re.fullmatch(r"\d+", h["id"]) is not None, "hello id is the start point (integer): %r" % h["id"])
    try:
        hd = json.loads("\n".join(h["data"]))
        ok(isinstance(hd, dict) and isinstance(hd.get("version"), str), "hello data is JSON with version: %r" % hd)
    except ValueError:
        ok(False, "hello data is JSON: %r" % h["data"])
    if h["id"] and h["id"].isdigit():
        start = int(h["id"])
readies = [e for e in events if e["event"] == "ready"]
seqs, bad_ready = [], []
for e in readies:
    try:
        rd = json.loads("\n".join(e["data"]))
        good = (e["id"] is not None and e["id"].isdigit() and isinstance(rd.get("number"), str)
                and isinstance(rd.get("order_id"), int) and len(e["data"]) == 1)
    except ValueError:
        good = False
    if not good:
        bad_ready.append(e)
    elif e["id"].isdigit():
        seqs.append(int(e["id"]))
ok(readies, "the stream has ready events: %d" % len(readies))
ok(not bad_ready, "every ready has id = ready_seq and one data line {number, order_id}%s" % ("" if not bad_ready else ": %r" % bad_ready[:2]))
ok(all(b > a for a, b in zip(seqs, seqs[1:])), "ready ids strictly increase")
ok(start is None or all(s > start for s in seqs), "ready ids are above the hello start")
others = [e["event"] for e in events if e["event"] not in ("hello", "ready", "shutdown")]
ok(not others, "no other event names: %r" % others)
shut = [i for i, e in enumerate(events) if e["event"] == "shutdown"]
if expect:
    ok(len(shut) == 1, "exactly one shutdown event: %d" % len(shut))
    if shut:
        s = events[shut[0]]
        ok(shut[0] == len(events) - 1, "shutdown is the last event")
        ok(s["order"][:1] == ["retry"] and s["retry"] is not None and s["retry"].isdigit(),
           "a retry: N line comes before the shutdown event line: %r (retry %r)" % (s["order"], s["retry"]))
        ok(s["data"] == [""], "shutdown has exactly one, empty data: line: %r" % s["data"])
        ok("\nretry: %s\nevent: shutdown\ndata:\n\n" % s["retry"] in "\n" + text and text.endswith("event: shutdown\ndata:\n\n"),
           "the stream ends with retry, event: shutdown, data:, blank line")
else:
    ok(not shut, "no shutdown (the Pod did not terminate): %d" % len(shut))
res = {"file": path, "bytes": len(raw), "events": len(events), "ready": len(readies), "start": start,
       "first_seq": seqs[0] if seqs else None, "last_seq": seqs[-1] if seqs else None, "seqs": seqs,
       "shutdown": len(shut), "retry": events[shut[0]]["retry"] if shut else None,
       "expect_shutdown": expect, "failed": FAILED}
if out:
    json.dump(res, open(out, "w"), indent=2)
print("%s: %d events, %d ready (%s..%s), shutdown %d" % ("PASS" if not FAILED else "FAIL", len(events), len(readies), res["first_seq"], res["last_seq"], len(shut)))
sys.exit(1 if FAILED else 0)
PY
	;;
*)
	sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 64
	;;
esac
