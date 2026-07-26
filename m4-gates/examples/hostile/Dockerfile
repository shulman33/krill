# F5's adversary: a build context that tries, AT BUILD TIME, to do every
# thing a server-side `docker build` on the host would have let it do.
#
# This is the analogue of M2's `broken` example. `broken` proved errors come
# back structured; this proves the build cannot reach the host. Each RUN below
# corresponds to a line in F5 step 1, and each writes its finding to a report
# that the app then serves — so the evidence is readable without ssh, and a
# reviewer can see exactly which probe ran and what it got.
#
# It must BUILD SUCCESSFULLY. A hostile context that fails to build proves
# nothing: the gate needs the probes to run and come back empty-handed.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates procps iproute2 python3 && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /report
SHELL ["/bin/sh", "-c"]

# 1. The host's object-store credentials. On the host path this file is
#    readable by root, and a build runs as root.
RUN { echo "== /etc/krill/gcs.json"; \
      cat /etc/krill/gcs.json 2>&1 | head -c 400 || true; \
      echo; echo "== any credential-looking file"; \
      ls -la /etc/krill/ 2>&1 | head -20 || true; } >> /report/probes.txt 2>&1

# 2. The registry database: the epoch mint and the app catalog.
RUN { echo "== krill.db"; \
      ls -la /srv/krill/krill.db /srv/krill/*.db 2>&1 | head -10 || true; \
      python3 -c "print(open('/srv/krill/krill.db','rb').read(64))" 2>&1 | head -c 300 || true; } \
      >> /report/probes.txt 2>&1

# 3. The admin API on loopback: register apps, deploy, delete, read streams.
RUN { echo "== admin API"; \
      curl -s -m 4 http://127.0.0.1:9091/v1/apps 2>&1 | head -c 400 || true; \
      echo; curl -s -m 4 http://172.16.0.1:9091/v1/apps 2>&1 | head -c 200 || true; \
      echo; echo "== doorman control"; \
      curl -s -m 4 http://127.0.0.1:9092/v1/shares 2>&1 | head -c 300 || true; } \
      >> /report/probes.txt 2>&1

# 4. The object store with any ambient credential (metadata service, gcloud).
RUN { echo "== cloud metadata"; \
      curl -s -m 4 -H 'Metadata-Flavor: Google' \
        http://169.254.169.254/computeMetadata/v1/instance/service-accounts/ 2>&1 | head -c 300 || true; \
      echo; curl -s -m 4 http://169.254.169.254/latest/meta-data/ 2>&1 | head -c 200 || true; } \
      >> /report/probes.txt 2>&1

# 5. Writing outside the build context, where it would outlive the build.
RUN { echo "== writing outside the context"; \
      (echo pwned > /srv/krill/PWNED && echo "WROTE /srv/krill/PWNED") 2>&1 | head -c 200 || true; \
      (echo pwned > /tmp/krill-pwned-outside && echo "wrote /tmp inside the build (expected, harmless)") ; \
      (touch /host-root-pwned && echo "WROTE /host-root-pwned") 2>&1 | head -c 200 || true; } \
      >> /report/probes.txt 2>&1

# 6. Another app's durable data.
RUN { echo "== other apps' data disks"; \
      ls -la /srv/krill/apps/ 2>&1 | head -20 || true; \
      python3 -c "print(open('/srv/krill/apps/ledger/data.ext4','rb').read(64))" 2>&1 | head -c 200 || true; } \
      >> /report/probes.txt 2>&1

# 7. Host processes: krilld, firecracker, the doorman.
RUN { echo "== host processes"; \
      ps aux 2>&1 | head -30 || true; \
      echo "== process count"; ps aux 2>/dev/null | wc -l; } >> /report/probes.txt 2>&1

# 8. Reach other guests directly, bypassing the router and the doorman.
RUN { echo "== other guests"; \
      for ip in 172.16.0.2 172.16.1.2 172.16.2.2; do \
        echo "-- $ip"; curl -s -m 3 "http://$ip:8000/" 2>&1 | head -c 120; echo; \
      done; \
      echo "== the host's own tap address"; \
      curl -s -m 3 http://172.17.0.1:22 2>&1 | head -c 80 || true; } >> /report/probes.txt 2>&1

# 9. Egress the platform must not provide: SMTP, and arbitrary destinations.
RUN { echo "== smtp"; \
      for p in 25 465 587 2525; do \
        echo "-- port $p"; (timeout 3 bash -c "cat < /dev/null > /dev/tcp/example.com/$p" \
          && echo "SMTP $p OPEN") 2>&1 | head -c 80; echo; \
      done; \
      echo "== arbitrary http"; \
      curl -s -m 4 -o /dev/null -w '%{http_code}' https://example.com/ 2>&1 | head -c 40; echo; } \
      >> /report/probes.txt 2>&1

# 10. Persist something past the builder VM's destruction. If any of these
#     survive into the next build or onto the host, F5 fails.
RUN { echo "== persistence attempts"; \
      (echo "$(date -u +%s)" > /srv/krill/build/PERSIST && echo "WROTE build dir") 2>&1 | head -c 200 || true; \
      (mkdir -p /out && echo persisted > /out/PERSIST && echo "wrote /out (the output disk — expected)") 2>&1 | head -c 200 || true; \
      (echo persisted > /ctx/PERSIST && echo "WROTE THE CONTEXT DISK (should be read-only)") 2>&1 | head -c 200 || true; } \
      >> /report/probes.txt 2>&1

RUN echo "== probe run complete ==" >> /report/probes.txt

COPY serve.py /serve.py
EXPOSE 8000
CMD ["python3", "/serve.py"]
