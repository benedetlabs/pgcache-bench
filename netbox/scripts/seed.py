"""Seed a NetBox rung for the PgCache lab.

Run with:
    docker compose exec -T netbox-a /opt/netbox/netbox/manage.py shell < scripts/seed.py

Composition per device is fixed across rungs (STUDY 6.4): 8 interfaces, 2 IP
addresses, 0.5 prefixes. That ratio is not decoration — Django's
prefetch_related skips a level entirely when the parent FK set is all-NULL, so
amplification on NetBox is a property of the seed, not of the endpoint. A run
against sparsely-populated data is not comparable with one against dense data.

Every FK the read path prefetches is populated, for the same reason.
"""
import os
import ipaddress

from dcim.models import (Device, DeviceRole, DeviceType, Interface,
                         Manufacturer, Site)
from ipam.models import IPAddress, Prefix

RUNGS = {"S0": 1_000, "S1": 20_000, "S2": 200_000, "S3": 1_000_000}
RUNG = os.environ.get("RUNG", "S0")
N_DEV = RUNGS[RUNG]
N_SITE = max(1, N_DEV // 500)
N_KIND = 8  # device types / roles / manufacturers, constant across rungs

print(f"seeding rung {RUNG}: {N_DEV} devices, {N_SITE} sites")

for model in (IPAddress, Interface, Device, Prefix, DeviceType, DeviceRole,
              Manufacturer, Site):
    model.objects.all().delete()

# NOT bulk_create for these. DeviceRole is an MPTT model — the nested-set
# columns (level, lft, rght, tree_id) are NOT NULL and are populated by MPTT's
# save(), which bulk_create bypasses:
#   IntegrityError: null value in column "level" of relation "dcim_devicerole"
# Counts here are tiny and constant across rungs (8 of each, N_DEV/500 sites),
# so the per-row save costs nothing. The bulk paths below are the ones that
# matter, and none of those models is nested.
mfrs = [Manufacturer.objects.create(name=f"mfr{i}", slug=f"mfr{i}")
        for i in range(N_KIND)]
types = [DeviceType.objects.create(manufacturer=mfrs[i], model=f"model{i}",
                                   slug=f"model{i}")
         for i in range(N_KIND)]
roles = [DeviceRole.objects.create(name=f"role{i}", slug=f"role{i}")
         for i in range(N_KIND)]
sites = [Site.objects.create(name=f"site{i}", slug=f"site{i}", status="active")
         for i in range(N_SITE)]

devices = Device.objects.bulk_create(
    [Device(name=f"dev{i}",
            device_type=types[i % N_KIND],
            role=roles[i % N_KIND],
            site=sites[i % N_SITE],
            status="active")
     for i in range(N_DEV)],
    batch_size=5_000)
print(f"  devices: {Device.objects.count()}")

ifaces = []
for d in devices:
    ifaces += [Interface(device=d, name=f"eth{j}", type="1000base-t")
               for j in range(8)]
Interface.objects.bulk_create(ifaces, batch_size=10_000)
print(f"  interfaces: {Interface.objects.count()}")

# A /24 per two devices, and two host addresses per device, so the IPAM read
# path has real containment to resolve instead of an empty table.
prefixes = [Prefix(prefix=str(ipaddress.ip_network(
                f"10.{(i >> 8) & 255}.{i & 255}.0/24")),
                status="active")
            for i in range(N_DEV // 2)]
Prefix.objects.bulk_create(prefixes, batch_size=5_000)
print(f"  prefixes: {Prefix.objects.count()}")

ips = []
for i in range(N_DEV):
    net = i // 2
    host = 10 + (i % 2) * 100
    ips.append(IPAddress(address=f"10.{(net >> 8) & 255}.{net & 255}.{host}/24",
                         status="active"))
    ips.append(IPAddress(address=f"10.{(net >> 8) & 255}.{net & 255}.{host + 1}/24",
                         status="active"))
IPAddress.objects.bulk_create(ips, batch_size=10_000)
print(f"  ips: {IPAddress.objects.count()}")

print("SEED_OK")
