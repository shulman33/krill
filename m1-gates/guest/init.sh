#!/bin/sh
# PID 1 inside the microVM. The M1 network contract: krilld appends
# krill_ip=<cidr> and krill_gw=<ip> to the kernel command line; the guest
# brings up eth0 from them. (M2's image builder will inject this init into
# every built rootfs.)
mount -t proc proc /proc
mount -t sysfs sys /sys 2>/dev/null
IP=$(sed -n 's/.*krill_ip=\([^ ]*\).*/\1/p' /proc/cmdline)
GW=$(sed -n 's/.*krill_gw=\([^ ]*\).*/\1/p' /proc/cmdline)
[ -n "$IP" ] || IP=172.16.0.2/30
[ -n "$GW" ] || GW=172.16.0.1
ip addr add "$IP" dev eth0
ip link set eth0 up
ip route add default via "$GW"
exec python3 -m uvicorn app:app --host 0.0.0.0 --port 8000
