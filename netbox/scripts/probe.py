"""Probe 0 — the C9 gate for NetBox.

Runs inside a NetBox container, so it measures the same process against whatever
datastore that container is pointed at. Path A and path B differ only in
DB_HOST/DB_PORT, so running this in netbox-a and then in netbox-b is the A/B
comparison with nothing else moving.

The method is the study's own (STUDY section 2): django.test.Client driving the
real view stack, with CaptureQueriesContext counting statements. Deliberately
not HTTP+token — NetBox 4.6's v2 token would not validate through the
Authorization header, and authentication is not what C9 asks about.

    docker compose exec -T netbox-a /opt/netbox/netbox/manage.py shell < scripts/probe.py
"""
import os
import statistics
import time

from django.test import Client
from django.test.utils import CaptureQueriesContext
from django.db import connection
from users.models import User

N = int(os.environ.get("PROBE_N", "30"))
LABEL = os.environ.get("PROBE_LABEL", "A")

user = User.objects.filter(username="lab").first()
if user is None:
    user = User.objects.create_superuser(
        username="lab", email="lab@lab.local", password="labpass")
user.is_superuser = True
user.is_active = True
user.save()

client = Client()
client.force_login(user)

CASES = [
    ("N1 api device list  ", "/api/dcim/devices/?limit=50"),
    ("N4 api device detail", "/api/dcim/devices/1/"),
    ("N5 api brief        ", "/api/dcim/devices/?limit=50&brief=true"),
    ("   api prefix list  ", "/api/ipam/prefixes/?limit=50"),
    ("   api iface list   ", "/api/dcim/interfaces/?limit=50"),
    ("N2 ui  prefix list  ", "/ipam/prefixes/?per_page=50"),
    ("   ui  device list  ", "/dcim/devices/?per_page=50"),
]

print(f"=== PROBE 0 · path {LABEL} · n={N} ===")
print(f"{'case':22} {'status':>6} {'ms/req':>8} {'q/req':>7} {'us/query':>9}")

for name, url in CASES:
    # Warm the process: per-request memoization, content types, config revision.
    for _ in range(3):
        client.get(url)

    with CaptureQueriesContext(connection) as ctx:
        client.get(url)
        nq = len(ctx.captured_queries)

    lat = []
    for _ in range(N):
        t0 = time.perf_counter()
        r = client.get(url)
        lat.append((time.perf_counter() - t0) * 1000.0)

    p50 = statistics.median(lat)
    per_q = (p50 * 1000.0 / nq) if nq else 0.0
    print(f"{name:22} {r.status_code:>6} {p50:8.2f} {nq:7d} {per_q:9.1f}")

print("PROBE_OK")
