"""Shared test configuration for the integration scripts in this directory.

Credentials are read from the environment and are NEVER stored in the
repository. An earlier revision hard-coded a live administrator password and a
working API key in every script here; because this repository is public, those
values were disclosed to anyone who cloned or forked it. Treat any credential
that ever appeared in this directory as compromised and rotate it.

Required variables:

    M365_TEST_ADMIN_PW   gateway administrator password
    M365_TEST_API_KEY    an API key created under "API Keys" in the console

Optional:

    M365_TEST_BASE       gateway base URL (default http://127.0.0.1:4141)

Run the scripts as ``python tests/<name>.py`` so that this directory is on
``sys.path``. See tests/.env.example for a template; note that a plain ``.env``
is git-ignored, but these scripts do not load it for you.
"""

import os
import sys

DEFAULT_BASE = "http://127.0.0.1:4141"

_HINTS = {
    "M365_TEST_ADMIN_PW": (
        "the gateway administrator password. Export it first, e.g.\n"
        "    bash:        export M365_TEST_ADMIN_PW='...'\n"
        "    PowerShell:  $env:M365_TEST_ADMIN_PW='...'"
    ),
    "M365_TEST_API_KEY": (
        "an API key from the gateway console (API Keys page). Export it first, e.g.\n"
        "    bash:        export M365_TEST_API_KEY='m365_...'\n"
        "    PowerShell:  $env:M365_TEST_API_KEY='m365_...'"
    ),
}


def _required(name):
    value = os.environ.get(name, "").strip()
    if not value:
        sys.stderr.write(
            "error: %s is not set - %s\n"
            "See tests/.env.example.\n" % (name, _HINTS[name])
        )
        raise SystemExit(2)
    return value


def __getattr__(name):
    """Resolve credentials lazily (PEP 562).

    Lazy resolution matters: test-codex-responses.py only needs an API key and
    must not start demanding an administrator password it never uses.
    """
    if name == "BASE":
        return os.environ.get("M365_TEST_BASE", "").strip() or DEFAULT_BASE
    if name == "ADMIN_PW":
        return _required("M365_TEST_ADMIN_PW")
    if name == "API_KEY":
        return _required("M365_TEST_API_KEY")
    raise AttributeError("module %r has no attribute %r" % (__name__, name))
