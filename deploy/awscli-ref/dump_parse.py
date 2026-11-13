#!/usr/bin/env python3
"""Run argv cases through the real AWS CLI argument parsers and record what
they produce, without executing any command.

Input (stdin):  JSON list of argv lists, each starting with "s3".
Output (stdout): JSON list of {"argv", "ok"/"error"} records.

The three passes mirror clidriver.main and BasicCommand.__call__:
  1. the main parser (global options + command),
  2. the `s3` command parser (subcommand),
  3. the subcommand parser (its own arguments).

gs3's Go parser is tested against this output (internal/cli/parse_ref_test.go).

Regenerate with:
    docker run --rm -i --entrypoint python3 gs3-awscli-ref:1.31.13 \
        /usr/local/bin/dump_parse.py < test/compat/parse_cases.json \
        > test/compat/parse_expected.json
"""
import contextlib
import io
import json
import sys

from awscli.argparser import ArgTableArgParser
from awscli.clidriver import create_clidriver


def plain(namespace):
    out = {}
    for key, value in vars(namespace).items():
        if isinstance(value, (str, bool, int, list)) or value is None:
            out[key] = value
        else:
            out[key] = repr(value)
    return out


def run(driver, argv):
    command_table = driver._get_command_table()
    main_parser = driver._create_parser(command_table)
    main_ns, remaining = main_parser.parse_known_args(argv)
    if main_ns.command != "s3":
        return {"skipped": "command is %r" % main_ns.command}

    s3 = command_table["s3"]
    s3_parser = ArgTableArgParser(s3.arg_table, s3.subcommand_table)
    s3_ns, remaining = s3_parser.parse_known_args(remaining)
    if getattr(s3_ns, "help", None) or s3_ns.subcommand is None:
        return {"globals": plain(main_ns), "subcommand": None,
                "args": None, "remaining": remaining}

    sub = s3.subcommand_table[s3_ns.subcommand]
    # Fires building-command-table.<name>, which registers the sync
    # strategies' extra arguments; BasicCommand.__call__ does the same.
    sub_table = sub.subcommand_table
    sub_parser = ArgTableArgParser(sub.arg_table, sub_table)
    sub_ns, remaining = sub_parser.parse_known_args(remaining)
    if getattr(sub_ns, "help", None):
        return {"globals": plain(main_ns), "subcommand": s3_ns.subcommand,
                "args": None, "remaining": remaining, "help": True}
    return {"globals": plain(main_ns), "subcommand": s3_ns.subcommand,
            "args": plain(sub_ns), "remaining": remaining}


def main():
    cases = json.load(sys.stdin)
    driver = create_clidriver()
    results = []
    for argv in cases:
        stderr = io.StringIO()
        stdout = io.StringIO()
        record = {"argv": argv}
        try:
            with contextlib.redirect_stderr(stderr), contextlib.redirect_stdout(stdout):
                record["ok"] = run(driver, list(argv))
        except SystemExit as exc:
            text = stderr.getvalue()
            # argparse prints "<prog>: error: <message>" after the usage text;
            # prog is "aws" for the main parser and this script's name for
            # the others.
            marker = ": error: "
            idx = text.rfind(marker)
            message = text[idx + len(marker):].rstrip("\n") if idx >= 0 else text
            record["error"] = {"exit": exc.code, "message": message,
                               "stdout": stdout.getvalue()}
        results.append(record)
    json.dump(results, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
