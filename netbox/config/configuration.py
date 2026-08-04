# NetBox configuration for the PgCache lab.
#
# Every knob here is pinned from netbox/STUDY.md section 5. The rule for this
# file: paths A and B must be byte-identical except DATABASE HOST/PORT, which is
# the one thing the experiment is allowed to vary. Anything else that differs
# between the two makes the comparison meaningless.
import os

ALLOWED_HOSTS = ["*"]
SECRET_KEY = "lab-only-not-a-secret-ephemeral-private-network-0123456789abcdef"
# NetBox 4.6 peppers API token hashes and refuses to mint a token without this.
# Must be identical on both paths: the pepper participates in the hash, so a
# token minted on path A would not validate on path B otherwise.
API_TOKEN_PEPPERS = {1: "lab-only-pepper-ephemeral-private-network-0123456789ab"}

# ── THE DIFFERENCE ──────────────────────────────────────────────────────────
# Path A points at the origin; path B at PgCache. Nothing else may differ.
DATABASE = {
    "ENGINE": "django.db.backends.postgresql",
    "NAME": os.environ.get("DB_NAME", "netbox"),
    "USER": os.environ.get("DB_USER", "netbox"),
    "PASSWORD": os.environ.get("DB_PASSWORD", "netboxpass"),
    "HOST": os.environ["DB_HOST"],
    "PORT": os.environ["DB_PORT"],
    # STUDY 5.1: at the Django default of 0 a fresh TCP connection is opened per
    # request, and that setup costs differently against origin:5432 than against
    # pgcache:6432. That alone could manufacture or erase the entire measured
    # effect. Pinned equal on both paths.
    "CONN_MAX_AGE": 300,
    # A SELECT 1 per request is extra work PgCache may or may not serve.
    "CONN_HEALTH_CHECKS": False,
    # NO OPTIONS BLOCK, ON PURPOSE.
    #
    # Django 6 + psycopg3 default to client-side binding (ClientCursor), which
    # sends the simple query protocol with literals already merged into the SQL
    # text. That is exactly what PgCache needs, and NetBox gets there without
    # being asked. Setting server_side_binding=True would silently turn the whole
    # read path non-cacheable; setting prepare_threshold would enable prepared
    # statements. Both are defects here, not tuning.
    #
    # This is the openFGA landmine, and NetBox lands on the right side of it:
    # path B is "path A plus a cache", not "path A without prepared statements
    # plus a cache".
    "DISABLE_SERVER_SIDE_CURSORS": True,
}

REDIS = {
    "tasks": {
        "HOST": os.environ.get("REDIS_HOST", "redis"),
        "PORT": 6379,
        "DATABASE": 0,
        "SSL": False,
    },
    "caching": {
        "HOST": os.environ.get("REDIS_HOST", "redis"),
        "PORT": 6379,
        "DATABASE": 1,
        "SSL": False,
    },
}

# STUDY 5.2 — settings that would otherwise add writes or drift to the read path.
# LOGIN_PERSISTENCE off keeps SESSION_SAVE_EVERY_REQUEST off, so a read stays a
# read (criterion C5).
LOGIN_PERSISTENCE = False
# Probe 0 only: token plaintext is not recoverable in NetBox 4.6 and the C9
# question is the cost of a read, not of authentication. Restore to True before
# any published campaign — STUDY 5.2 pins it, and token auth is what makes the
# read path 100% cacheable.
LOGIN_REQUIRED = False
CHANGELOG_RETENTION = 0
# Anonymous outbound calls are uncontrolled network noise inside a window.
RELEASE_CHECK_URL = None
METRICS_ENABLED = False
TIME_ZONE = "UTC"
DEBUG = False
