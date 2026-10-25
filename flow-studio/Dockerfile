# Caddy with the Flow Studio files baked in. Caddy gets the HTTPS certificate
# for PUBLIC_HOST by itself (Let's Encrypt), so the image needs no secrets.
FROM caddy:2.11
COPY Caddyfile /etc/caddy/Caddyfile
COPY site/ /srv/studio/
