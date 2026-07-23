FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir fastapi uvicorn
COPY app.py /app.py
COPY init.sh /init.sh
RUN chmod +x /init.sh
