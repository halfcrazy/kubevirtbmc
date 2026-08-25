#!/usr/bin/env python3
"""Diff two Redfish-Interop-Validator run directories.

Usage: interop-diff.py HEAD_DIR [BASE_DIR]

Each directory holds one validator run: stdout.txt (captured validator stdout,
carrying the Results Summary table) and ConformanceLog_*.txt (carrying the
verdict lines). FAIL verdicts are logged at ERROR level as
"ERROR - <msg> ... <verdict> at <uri>", each appearing both inline and in the
closing listing, hence the set dedupe.

Prints a markdown summary to stdout: result counts side by side, then the
ERROR lines unique to each run ("introduced" / "fixed"). Always exits 0:
this report is informational, gating is the workflow's business.
"""

import glob
import os
import re
import sys

SUMMARY_RE = re.compile(r"^\|\s*(Pass|Fail|Warning|Not Tested)\s*\|\s*(\d+)\s*\|", re.M)
ERROR_RE = re.compile(r"^ERROR - (.*?)(?:\s*\.\.\.\s*)?$")
RESULT_ORDER = ["Pass", "Fail", "Warning", "Not Tested"]


def parse(logdir):
    counts, errors = {}, set()

    stdout_path = os.path.join(logdir, "stdout.txt")
    if os.path.exists(stdout_path):
        with open(stdout_path, errors="replace") as f:
            for name, count in SUMMARY_RE.findall(f.read()):
                counts[name] = int(count)

    for path in glob.glob(os.path.join(logdir, "ConformanceLog_*.txt")):
        with open(path, errors="replace") as f:
            for line in f:
                m = ERROR_RE.match(line)
                if m:
                    errors.add(m.group(1))
    return counts, errors


def fmt_count(counts, key):
    return str(counts[key]) if key in counts else "n/a"


def fmt_delta(head, base, key):
    if key not in head or key not in base:
        return ""
    delta = head[key] - base[key]
    return f"{delta:+d}" if delta else "0"


def main():
    head_dir = sys.argv[1]
    base_dir = sys.argv[2] if len(sys.argv) > 2 else None

    head_counts, head_errors = parse(head_dir)
    base_counts, base_errors = parse(base_dir) if base_dir else ({}, set())

    print("## Redfish Interop Validator\n")
    if base_dir:
        print("| Result | base | PR head | delta |")
        print("|--------|-----:|--------:|------:|")
        for key in RESULT_ORDER:
            print(f"| {key} | {fmt_count(base_counts, key)} | "
                  f"{fmt_count(head_counts, key)} | {fmt_delta(head_counts, base_counts, key)} |")
        print()

        introduced = sorted(head_errors - base_errors)
        fixed = sorted(base_errors - head_errors)
        print(f"### Introduced by this PR ({len(introduced)})\n")
        print("\n".join(f"- `{e}`" for e in introduced) or "- none", end="\n\n")
        print(f"### Fixed by this PR ({len(fixed)})\n")
        print("\n".join(f"- `{e}`" for e in fixed) or "- none", end="\n\n")
    else:
        print("| Result | Count |")
        print("|--------|------:|")
        for key in RESULT_ORDER:
            print(f"| {key} | {fmt_count(head_counts, key)} |")
        print()
        print(f"### Failures ({len(head_errors)})\n")
        print("\n".join(f"- `{e}`" for e in sorted(head_errors)) or "- none", end="\n\n")

    print("Full detail: see the uploaded `redfish-interop-logs` artifact "
          "(InteropHtmlLog per run).")


if __name__ == "__main__":
    main()
