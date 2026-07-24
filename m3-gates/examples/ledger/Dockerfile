FROM python:3.12-slim
# No iproute2 on purpose: kernel ip= autoconfig carries the network contract
# (the M2 finding). debian-slim ships `mount`, which the generated init needs
# for /data.
WORKDIR /srv
COPY app.py .
EXPOSE 8000
CMD ["python", "app.py"]
