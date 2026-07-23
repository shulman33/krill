#!/bin/sh
# PID 1 inside the microVM: mounts, static network, then the app. No init system.
mount -t proc proc /proc
mount -t sysfs sys /sys 2>/dev/null
ip addr add 172.16.0.2/24 dev eth0
ip link set eth0 up
ip route add default via 172.16.0.1
exec python3 -m uvicorn app:app --host 0.0.0.0 --port 8000
