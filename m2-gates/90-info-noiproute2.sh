#!/usr/bin/env bash
# Informational (NOT a gate): can an image without iproute2 network via the
# kernel ip= autoconfig fallback? Whatever happens gets recorded; the builder
# already warns on such images.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

dir=$(mktemp -d)
cp examples/guestbook/app.py "$dir/"
cat > "$dir/Dockerfile" <<'EOF'
FROM python:3.12-slim
RUN pip install --no-cache-dir fastapi uvicorn
WORKDIR /srv
COPY app.py .
EXPOSE 8000
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
EOF

krill delete noip >/dev/null 2>&1 || true
set +e
out=$(krill deploy "$dir" --name noip --json)
rc=$?
set -e
echo "$out" > "$RESULTS_DIR/90-noiproute2.json"
ready=$(echo "$out" | jq -r .ready 2>/dev/null)
warns=$(echo "$out" | jq -c .warnings 2>/dev/null)
echo "INFO: no-iproute2 image: deploy rc=$rc ready=$ready warnings=$warns"
if [ "$ready" = "true" ]; then
  echo "INFO: kernel ip= autoconfig CARRIES the network contract — images need no iproute2"
else
  echo "INFO: kernel ip= autoconfig did NOT work here — images must ship iproute2 (builder warns)"
fi
krill delete noip >/dev/null 2>&1 || true
rm -rf "$dir"
