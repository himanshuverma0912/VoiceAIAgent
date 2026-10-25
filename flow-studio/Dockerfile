# 1. Build Flow Studio (React + Vite) into static files, running its unit tests.
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
# --legacy-peer-deps: npm 10 crashes ("reading edgesOut") resolving vitest's
# optional peer packages; none of them are used here.
RUN npm ci --legacy-peer-deps --no-audit --no-fund
COPY . .
RUN npm test && npm run build

# 2. Caddy serves those files and is the platform's HTTPS entry point. It gets the
#    certificate for PUBLIC_HOST by itself (Let's Encrypt), so no secrets here.
FROM caddy:2.11
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/dist/ /srv/studio/
