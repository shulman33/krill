FROM node:22-slim
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 && rm -rf /var/lib/apt/lists/*
WORKDIR /srv/app
COPY package.json ./
RUN npm install --omit=dev
COPY app.js ./
COPY init.sh /init.sh
RUN chmod +x /init.sh
