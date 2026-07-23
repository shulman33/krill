FROM python:3.12-slim
# iproute2 gives the generated init an `ip` binary for the network contract.
# (Images without it fall back to kernel-level ip= autoconfig — informational
# test, not gated.)
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir fastapi uvicorn
WORKDIR /srv
COPY app.py .
EXPOSE 8000
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
