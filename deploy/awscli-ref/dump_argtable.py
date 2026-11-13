#!/usr/bin/env python3
"""Dump the argument tables of `aws s3 <subcommand>` and the global options
of the installed AWS CLI as JSON.

gs3 keeps its own flag table; a unit test compares it with this dump so a
flag can never be silently missing or mistyped.

Run inside the reference image:
    docker run --rm --entrypoint python3 gs3-awscli-ref /usr/local/bin/dump_argtable.py
"""
import json
import sys

import awscli
from awscli.clidriver import create_clidriver


def describe(name, arg):
    """Reduce an argument object to the fields that matter for parsing."""
    out = {
        "name": name,
        "positional": bool(getattr(arg, "positional_arg", False)),
        "action": getattr(arg, "_action", None) or "store",
        "nargs": getattr(arg, "nargs", None),
        "choices": getattr(arg, "choices", None) or None,
        "cli_type_name": getattr(arg, "cli_type_name", None),
        "required": bool(getattr(arg, "required", False)),
        "group_name": getattr(arg, "group_name", None),
        "no_paramfile": bool(getattr(arg, "no_paramfile", False)),
    }
    for attr in ("_default", "default"):
        if hasattr(arg, attr):
            out["default"] = getattr(arg, attr)
            break
    for attr in ("_const", "const"):
        if hasattr(arg, attr):
            out["const"] = getattr(arg, attr)
            break
    dest = getattr(arg, "_dest", None) or getattr(arg, "py_name", None)
    out["dest"] = dest
    if out["choices"] is not None:
        out["choices"] = list(out["choices"])
    # Custom argparse Action classes (e.g. AppendFilter) are not JSON
    # serialisable; record their name instead.
    if not isinstance(out["action"], str):
        out["action"] = getattr(out["action"], "__name__", repr(out["action"]))
    return out


def main():
    driver = create_clidriver()
    result = {"version": awscli.__version__, "global": [], "s3": {}}

    for name, arg in driver._get_argument_table().items():
        result["global"].append(describe(name, arg))

    command_table = driver._get_command_table()
    s3 = command_table["s3"]
    for sub_name, sub in sorted(s3.subcommand_table.items()):
        # Building the (empty) subcommand table first fires
        # `building-command-table.<name>`, which is what registers the sync
        # strategies that add --size-only / --exact-timestamps / --delete.
        # BasicCommand.__call__ does it in this order too.
        sub.subcommand_table
        result["s3"][sub_name] = [
            describe(name, arg) for name, arg in sub.arg_table.items()
        ]

    json.dump(result, sys.stdout, indent=2, sort_keys=True, default=str)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
