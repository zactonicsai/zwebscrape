FROM node:20-bookworm-slim

WORKDIR /app

COPY package.json ./
RUN npm install --omit=dev

# Install the chromium build that matches whatever playwright version
# npm just resolved, plus all of its system library dependencies.
# This avoids version mismatches between a pre-baked image and the
# actual installed npm version.
RUN npx playwright install --with-deps chromium \
 && rm -rf /var/lib/apt/lists/*

COPY server.js ./

EXPOSE 3000

CMD ["node", "server.js"]
