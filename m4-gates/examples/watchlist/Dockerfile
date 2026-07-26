# The F4 demo app. A plain Dockerfile — no krill-specific anything.
#
# Base image only, nothing installed at build time. That is deliberate twice
# over: it builds inside the builder microVM with only the container-registry
# allowlist (no PyPI widening needed), and F4 is a usability test on a real
# person, so the fewer moving parts on the day it runs, the better.
#
# debian-slim ships `mount`, which the generated init needs for /data. No
# iproute2: kernel ip= autoconfig carries the network contract (the M2
# finding).
FROM python:3.12-slim
WORKDIR /srv
COPY app.py krill_identity.py ./
EXPOSE 8000
CMD ["python", "app.py"]
